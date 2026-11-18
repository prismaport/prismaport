package store

import (
	"bytes"
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// IdempotencyScope identifies an idempotency key within one merchant and mode.
type IdempotencyScope struct {
	MerchantID string
	Livemode   bool
	Key        string
}

// StoredResponse is the response saved for an idempotency key so it can be replayed.
type StoredResponse struct {
	Status int
	Body   []byte
}

// Idempotent runs fn at most once per scope. The first successful response is stored with the
// request hash; later calls with the same key replay it, or fail with ErrIdempotencyConflict when
// the request differs. Concurrent calls with one key serialize on a transaction-scoped advisory
// lock, so fn never runs twice. Failed calls store nothing and may be retried with the same key.
func (s *Store) Idempotent(ctx context.Context, scope IdempotencyScope, requestHash [32]byte,
	fn func(ctx context.Context, tx pgx.Tx) (StoredResponse, error)) (StoredResponse, bool, error) {
	var resp StoredResponse
	replayed := false
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
			scope.MerchantID+"/"+boolKey(scope.Livemode)+"/"+scope.Key); err != nil {
			return err
		}
		var storedHash []byte
		err := tx.QueryRow(ctx, `SELECT request_hash, response_status, response_body FROM idempotency_keys
			WHERE merchant_id = $1 AND livemode = $2 AND key = $3`, scope.MerchantID, scope.Livemode, scope.Key).
			Scan(&storedHash, &resp.Status, &resp.Body)
		switch {
		case err == nil:
			if !bytes.Equal(storedHash, requestHash[:]) {
				return ErrIdempotencyConflict
			}
			replayed = true
			return nil
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		resp, err = fn(ctx, tx)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO idempotency_keys (merchant_id, livemode, key, request_hash, response_status, response_body, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`, scope.MerchantID, scope.Livemode, scope.Key, requestHash[:], resp.Status, resp.Body, s.now())
		return err
	})
	if err != nil {
		return StoredResponse{}, false, opErr("idempotent", err)
	}
	return resp, replayed, nil
}

// IdempotencyKeyUsed reports whether a response is already stored under scope, without taking the
// key's lock: a caller can skip work that only a first run needs.
func (s *Store) IdempotencyKeyUsed(ctx context.Context, scope IdempotencyScope) (bool, error) {
	var used bool
	err := s.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM idempotency_keys WHERE merchant_id = $1 AND livemode = $2 AND key = $3)`,
		scope.MerchantID, scope.Livemode, scope.Key).Scan(&used)
	return used, opErr("idempotency key used", err)
}

// PurgeIdempotencyKeys deletes up to limit stored responses created before cutoff, oldest first
// (index idempotency_keys_created), and returns how many it deleted. Each call is one short
// statement, so a large backlog is cleared in batches without holding locks on the table.
func (s *Store) PurgeIdempotencyKeys(ctx context.Context, cutoff time.Time, limit int) (int, error) {
	tag, err := s.db.Exec(ctx, `DELETE FROM idempotency_keys WHERE ctid = ANY (ARRAY(
		SELECT ctid FROM idempotency_keys WHERE created_at < $1 ORDER BY created_at LIMIT $2)) AND created_at < $1`, cutoff, limit)
	if err != nil {
		return 0, opErr("purge idempotency keys", err)
	}
	return int(tag.RowsAffected()), nil
}

func boolKey(b bool) string {
	if b {
		return "live"
	}
	return "test"
}
