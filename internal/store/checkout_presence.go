package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// PresenceThrottle is the youngest checkout_seen_at a payer-side request rewrites: a page polling
// every few seconds costs one write per PresenceThrottle.
const PresenceThrottle = 10 * time.Second

// TouchCheckoutSeen records that a payer has the session's checkout open now, unless the stored
// mark is younger than PresenceThrottle (one conditional UPDATE, nothing else of the session
// changes). It reports whether it wrote.
func (s *Store) TouchCheckoutSeen(ctx context.Context, id string) (bool, error) {
	now := s.now()
	tag, err := s.db.Exec(ctx, `UPDATE sessions SET checkout_seen_at = $2
		WHERE id = $1 AND (checkout_seen_at IS NULL OR checkout_seen_at <= $3)`, id, now, now.Add(-PresenceThrottle))
	if err != nil {
		return false, opErr("touch checkout seen", err)
	}
	return tag.RowsAffected() == 1, nil
}

// CheckoutSeenAt is the session's last recorded payer presence (nil: never seen).
func (s *Store) CheckoutSeenAt(ctx context.Context, id string) (*time.Time, error) {
	var at *time.Time
	err := s.db.QueryRow(ctx, `SELECT checkout_seen_at FROM sessions WHERE id = $1`, id).Scan(&at)
	return at, opErr("checkout seen at", notFound(err))
}

// RecheckRequestedAt is when "Check again" was asked for the session and not yet done by the
// worker (nil: none pending).
func (s *Store) RecheckRequestedAt(ctx context.Context, id string) (*time.Time, error) {
	var at *time.Time
	err := s.db.QueryRow(ctx, `SELECT recheck_requested_at FROM sessions WHERE id = $1`, id).Scan(&at)
	return at, opErr("recheck requested at", notFound(err))
}

// RequestRecheck marks the session for one late check by the worker (which sets the column back
// to NULL when done) and writes the session.recheck_requested audit row, both only when no
// recheck is pending. It reports whether it marked the session; false means one was pending.
func (s *Store) RequestRecheck(ctx context.Context, merchantID, id, actor string, details map[string]any) (bool, error) {
	marked := false
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE sessions SET recheck_requested_at = $3
			WHERE id = $1 AND merchant_id = $2 AND recheck_requested_at IS NULL`, id, merchantID, s.now())
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		marked = true
		return audit(ctx, tx, merchantID, actor, "session.recheck_requested", details)
	})
	return marked, opErr("request recheck", err)
}

// TakeRateTokenPer is TakeRateToken for limits slower than one request a minute: a bucket of
// count tokens that refills completely over period. One token is period's microseconds, refilled
// count units per microsecond, so the same statement and table serve both.
func (s *Store) TakeRateTokenPer(ctx context.Context, key string, count int, period time.Duration, at *time.Time) (bool, time.Duration, error) {
	unit := period.Microseconds()
	if count <= 0 || unit <= 0 {
		return false, 0, fmt.Errorf("store: rate limit %d per %s", count, period)
	}
	var allowed bool
	var tokens int64
	err := s.db.QueryRow(ctx, takeToken, key, int64(count)*unit, unit, int64(count), at).Scan(&allowed, &tokens)
	if err != nil {
		return false, 0, opErr("take rate token per", err)
	}
	if allowed {
		return true, 0, nil
	}
	missing := unit - tokens
	wait := (missing + int64(count) - 1) / int64(count)
	return false, time.Duration(wait) * time.Microsecond, nil
}
