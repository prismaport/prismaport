package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// SandboxInbox is a merchant's hosted test endpoint. Its URL carries Token; Endpoint is the
// test-mode webhook endpoint the inbox is registered as.
type SandboxInbox struct {
	ID         string
	MerchantID string
	Token      string
	CreatedAt  time.Time
	Endpoint   WebhookEndpoint
}

// SandboxRequest is one request an inbox received, with the signature check made on arrival.
type SandboxRequest struct {
	// Seq orders arrivals; it is the list cursor.
	Seq        int64
	ID         string
	InboxID    string
	ReceivedAt time.Time
	Method     string
	// Headers are [name, value] pairs in arrival order.
	Headers            [][2]string
	Body               []byte
	SignatureValid     bool
	SignatureError     string
	SignatureTimestamp *time.Time
}

// ErrInboxExists is returned when another request created the merchant's inbox first.
var ErrInboxExists = errors.New("store: the merchant already has an active sandbox inbox")

const inboxColumns = `i.id, i.merchant_id, i.token, i.created_at, ` + endpointColumns

func scanInbox(r pgx.Row) (SandboxInbox, error) {
	var i SandboxInbox
	err := r.Scan(append([]any{&i.ID, &i.MerchantID, &i.Token, &i.CreatedAt}, i.Endpoint.fields()...)...)
	return i, notFound(err)
}

// ActiveSandboxInbox is the merchant's inbox while it and its endpoint are active.
func (s *Store) ActiveSandboxInbox(ctx context.Context, merchantID string) (SandboxInbox, error) {
	out, err := scanInbox(s.db.QueryRow(ctx, `SELECT `+inboxColumns+` FROM sandbox_inboxes i JOIN webhook_endpoints e ON e.id = i.endpoint_id
		WHERE i.merchant_id = $1 AND i.disabled_at IS NULL AND e.deleted_at IS NULL`, merchantID))
	return out, opErr("active sandbox inbox", err)
}

// SandboxInboxByToken finds an active inbox by the token in its URL.
func (s *Store) SandboxInboxByToken(ctx context.Context, token string) (SandboxInbox, error) {
	out, err := scanInbox(s.db.QueryRow(ctx, `SELECT `+inboxColumns+` FROM sandbox_inboxes i JOIN webhook_endpoints e ON e.id = i.endpoint_id
		WHERE i.token = $1 AND i.disabled_at IS NULL AND e.deleted_at IS NULL`, token))
	return out, opErr("sandbox inbox by token", err)
}

// CreateSandboxInbox registers the inbox's test-mode endpoint and the inbox in one transaction.
// A concurrent create that won the merchant's single active slot gives ErrInboxExists.
func (s *Store) CreateSandboxInbox(ctx context.Context, in SandboxInbox, actor string) (SandboxInbox, error) {
	now := s.now()
	in.CreatedAt, in.Endpoint.CreatedAt, in.Endpoint.Status, in.Endpoint.Inbox = now, now, EndpointEnabled, true
	if in.Endpoint.EnabledEvents == nil {
		in.Endpoint.EnabledEvents = []string{}
	}
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		e := in.Endpoint
		if _, err := tx.Exec(ctx, `INSERT INTO webhook_endpoints (id, merchant_id, livemode, url, enabled_events, secret_ciphertext, created_at)
			VALUES ($1, $2, false, $3, $4, $5, $6)`, e.ID, in.MerchantID, e.URL, e.EnabledEvents, e.SecretSealed, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO sandbox_inboxes (id, merchant_id, token, endpoint_id, created_at) VALUES ($1, $2, $3, $4, $5)`,
			in.ID, in.MerchantID, in.Token, e.ID, now); err != nil {
			if isUniqueViolation(err) {
				return ErrInboxExists
			}
			return err
		}
		if err := audit(ctx, tx, in.MerchantID, actor, "webhook_endpoint.created", map[string]any{"endpoint_id": e.ID, "url": e.URL}); err != nil {
			return err
		}
		return audit(ctx, tx, in.MerchantID, actor, "sandbox_inbox.created", map[string]any{"inbox_id": in.ID, "endpoint_id": e.ID})
	})
	return in, opErr("create sandbox inbox", err)
}

// DisableSandboxInbox closes the merchant's active inbox: its endpoint is deleted (pending
// deliveries are dead-lettered) and its stored requests are deleted.
func (s *Store) DisableSandboxInbox(ctx context.Context, merchantID, actor string) error {
	return opErr("disable sandbox inbox", s.inTx(ctx, func(tx pgx.Tx) error {
		if err := lockSequencer(ctx, tx); err != nil {
			return err
		}
		var id, endpointID string
		err := tx.QueryRow(ctx, `UPDATE sandbox_inboxes SET disabled_at = $2 WHERE merchant_id = $1 AND disabled_at IS NULL
			RETURNING id, endpoint_id`, merchantID, s.now()).Scan(&id, &endpointID)
		if err != nil {
			return notFound(err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM sandbox_requests WHERE inbox_id = $1`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE webhook_endpoints SET deleted_at = $2 WHERE id = $1 AND deleted_at IS NULL`, endpointID, s.now()); err != nil {
			return err
		}
		if err := deadLetterPending(ctx, tx, endpointID, ErrTextEndpointDeleted); err != nil {
			return err
		}
		return audit(ctx, tx, merchantID, actor, "sandbox_inbox.deleted", map[string]any{"inbox_id": id, "endpoint_id": endpointID})
	}))
}

// disableInboxOfEndpoint closes the inbox registered as endpointID, if any, inside tx.
func disableInboxOfEndpoint(ctx context.Context, tx pgx.Tx, endpointID string, now time.Time) error {
	var id string
	err := tx.QueryRow(ctx, `UPDATE sandbox_inboxes SET disabled_at = $2 WHERE endpoint_id = $1 AND disabled_at IS NULL RETURNING id`,
		endpointID, now).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `DELETE FROM sandbox_requests WHERE inbox_id = $1`, id)
	return err
}

// InsertSandboxRequest stores a received request and trims the inbox to its newest max requests
// and to the retention window, in one transaction.
func (s *Store) InsertSandboxRequest(ctx context.Context, r SandboxRequest, max int, retention time.Duration) error {
	headers, err := json.Marshal(r.Headers)
	if err != nil {
		return opErr("insert sandbox request", err)
	}
	var sigErr *string
	if r.SignatureError != "" {
		sigErr = &r.SignatureError
	}
	return opErr("insert sandbox request", s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO sandbox_requests (id, inbox_id, received_at, method, headers, body, signature_valid,
			signature_error, signature_timestamp) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			r.ID, r.InboxID, r.ReceivedAt, r.Method, string(headers), r.Body, r.SignatureValid, sigErr, r.SignatureTimestamp); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM sandbox_requests WHERE inbox_id = $1 AND (received_at < $3 OR seq NOT IN (
			SELECT seq FROM sandbox_requests WHERE inbox_id = $1 ORDER BY seq DESC LIMIT $2))`, r.InboxID, max, r.ReceivedAt.Add(-retention))
		return err
	}))
}

// ListSandboxRequests pages an inbox's requests newest first (seq below before, when before > 0),
// only those received since.
func (s *Store) ListSandboxRequests(ctx context.Context, inboxID string, before int64, since time.Time, limit int) ([]SandboxRequest, bool, error) {
	rows, err := s.db.Query(ctx, `SELECT seq, id, inbox_id, received_at, method, headers::text, body, signature_valid,
		coalesce(signature_error, ''), signature_timestamp FROM sandbox_requests
		WHERE inbox_id = $1 AND ($2 = 0 OR seq < $2) AND received_at >= $3 ORDER BY seq DESC LIMIT $4`, inboxID, before, since, limit+1)
	if err != nil {
		return nil, false, opErr("list sandbox requests", err)
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (SandboxRequest, error) {
		var q SandboxRequest
		var headers string
		if err := r.Scan(&q.Seq, &q.ID, &q.InboxID, &q.ReceivedAt, &q.Method, &headers, &q.Body, &q.SignatureValid, &q.SignatureError,
			&q.SignatureTimestamp); err != nil {
			return q, err
		}
		return q, json.Unmarshal([]byte(headers), &q.Headers)
	})
	if err != nil {
		return nil, false, opErr("list sandbox requests", err)
	}
	more := len(list) > limit
	if more {
		list = list[:limit]
	}
	return list, more, nil
}

// PurgeSandboxRequests deletes up to limit requests received before cutoff.
func (s *Store) PurgeSandboxRequests(ctx context.Context, cutoff time.Time, limit int) (int, error) {
	tag, err := s.db.Exec(ctx, `DELETE FROM sandbox_requests WHERE id IN (
		SELECT id FROM sandbox_requests WHERE received_at < $1 ORDER BY received_at LIMIT $2)`, cutoff, limit)
	if err != nil {
		return 0, opErr("purge sandbox requests", err)
	}
	return int(tag.RowsAffected()), nil
}

// ExpireNowForSimulation moves an open or partially paid test-mode session's expiry to now, so
// the ordinary expiry command can close it. Live sessions are never touched.
func (s *Store) ExpireNowForSimulation(ctx context.Context, merchantID, sessionID string, now time.Time) error {
	_, err := s.db.Exec(ctx, `UPDATE sessions SET expires_at = $3 WHERE id = $1 AND merchant_id = $2 AND livemode = false
		AND status IN ('open', 'partially_paid') AND expires_at > $3`, sessionID, merchantID, now)
	return opErr("expire now for simulation", err)
}
