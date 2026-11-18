package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/prismaport/prismaport/internal/store"
)

// idemRequest names one idempotent request: the key's scope, and the endpoint and raw body that
// make its fingerprint. The same key with another fingerprint is a conflict.
type idemRequest struct {
	scope store.IdempotencyScope
	// route is the endpoint, such as "POST /v1/sessions". Stored keys hold the fingerprint made
	// with it, so an endpoint's route string never changes.
	route string
	body  []byte
}

// fingerprint is the sha256 of the route, a newline and the raw body.
func (r idemRequest) fingerprint() [32]byte {
	return sha256.Sum256(append([]byte(r.route+"\n"), r.body...))
}

// A stored response (store.StoredResponse, and the customer address result it holds) keeps the
// HTTP status the request was first answered with, so a replay answers alike. These are the
// statuses the service answers with; their values are net/http's and must never change.
const (
	statusOK      = 200
	statusCreated = 201
)

func idempotencyConflict() *Error {
	return newErr(KindConflict, CodeIdempotencyConflict, "this Idempotency-Key was already used with a different request")
}

// idempotent runs fn at most once per request scope, inside the transaction that records the key,
// and keeps its result as JSON with the HTTP status fn gives. It returns the stored result decoded
// again, with its status: the first run's, or on a replay the one stored then, so both read
// alike.
func idempotent[T any](ctx context.Context, st *store.Store, req idemRequest,
	fn func(ctx context.Context, tx pgx.Tx) (T, int, error)) (out T, status int, replayed bool, err error) {
	resp, replayed, err := st.Idempotent(ctx, req.scope, req.fingerprint(), func(ctx context.Context, tx pgx.Tx) (store.StoredResponse, error) {
		v, status, err := fn(ctx, tx)
		if err != nil {
			return store.StoredResponse{}, err
		}
		body, err := json.Marshal(v)
		return store.StoredResponse{Status: status, Body: body}, err
	})
	if errors.Is(err, store.ErrIdempotencyConflict) {
		return out, 0, false, idempotencyConflict()
	}
	if err != nil {
		return out, 0, false, err
	}
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return out, 0, false, errors.New("stored idempotent response is malformed")
	}
	return out, resp.Status, replayed, nil
}
