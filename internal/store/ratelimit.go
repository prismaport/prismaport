package store

import (
	"context"
	"fmt"
	"time"
)

// tokenUnits is one token in rate_limits.tokens: with it, N requests per minute refill N units
// per microsecond.
const tokenUnits = 60_000_000

// refill is the bucket after the time elapsed since its last request, capped at the burst and
// at one day of refill (so the product cannot overflow). It reads only the conflicting row and
// EXCLUDED, so concurrent requests on one key serialize on the row lock and each sees the
// previous one's result.
const refill = `least($2::bigint, b.tokens + least(86400000000, greatest(0,
	(extract(epoch FROM excluded.updated_at - b.updated_at) * 1000000)::bigint)) * $4::bigint)`

var takeToken = fmt.Sprintf(`INSERT INTO rate_limits AS b (key, tokens, updated_at, allowed)
	VALUES ($1, $2::bigint - $3::bigint, coalesce($5::timestamptz, clock_timestamp()), true)
	ON CONFLICT (key) DO UPDATE SET
		tokens = CASE WHEN %[1]s >= $3::bigint THEN %[1]s - $3::bigint ELSE %[1]s END,
		updated_at = greatest(b.updated_at, excluded.updated_at),
		allowed = %[1]s >= $3::bigint
	RETURNING allowed, tokens`, refill)

// TakeRateToken takes one token from key's bucket (burst tokens, refilled at perMinute) in a
// single atomic statement, so every API instance shares the limit. When none is left it
// returns how long until one is. at is nil in production (the database clock, the same for
// every instance) and set by tests.
func (s *Store) TakeRateToken(ctx context.Context, key string, perMinute, burst int, at *time.Time) (bool, time.Duration, error) {
	if perMinute <= 0 || burst <= 0 {
		return false, 0, fmt.Errorf("store: rate limit %d/min burst %d", perMinute, burst)
	}
	var allowed bool
	var tokens int64
	err := s.db.QueryRow(ctx, takeToken, key, int64(burst)*tokenUnits, int64(tokenUnits), int64(perMinute), at).Scan(&allowed, &tokens)
	if err != nil {
		return false, 0, opErr("take rate token", err)
	}
	if allowed {
		return true, 0, nil
	}
	missing := tokenUnits - tokens
	wait := (missing + int64(perMinute) - 1) / int64(perMinute)
	return false, time.Duration(wait) * time.Microsecond, nil
}

// PurgeRateLimits deletes buckets idle for longer than idle, measured on the same clock as
// TakeRateToken (at nil: the database clock). A deleted bucket is a full one, so this only drops
// state once every limit has had time to refill completely.
func (s *Store) PurgeRateLimits(ctx context.Context, idle time.Duration, at *time.Time) (int64, error) {
	tag, err := s.db.Exec(ctx, `DELETE FROM rate_limits WHERE updated_at < coalesce($2::timestamptz, clock_timestamp()) - $1::interval`,
		idle.String(), at)
	if err != nil {
		return 0, opErr("purge rate limits", err)
	}
	return tag.RowsAffected(), nil
}
