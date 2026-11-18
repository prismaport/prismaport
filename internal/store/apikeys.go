package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prismaport/prismaport/internal/ids"
)

// ErrKeyNotActive refuses to roll a key that is revoked, expired or already rolled.
var ErrKeyNotActive = errors.New("store: API key is revoked, expired or already rolled")

// APIKey is the stored record of a merchant API key; the secret itself is not kept.
type APIKey struct {
	ID         string
	MerchantID string
	Livemode   bool
	Prefix     string
	Scopes     []string
	CreatedAt  time.Time
	RevokedAt  *time.Time
	LastUsedAt *time.Time
	// ExpiresAt is set when the key was rolled: it authenticates until then.
	ExpiresAt *time.Time
	// ReplacedBy is the key the roll issued.
	ReplacedBy *string
}

// Active reports whether the key authenticates at now.
func (k APIKey) Active(now time.Time) bool {
	return k.RevokedAt == nil && (k.ExpiresAt == nil || now.Before(*k.ExpiresAt))
}

// APIKeyRecord is a key as the dashboard lists it.
type APIKeyRecord struct {
	APIKey
	Name *string
}

const apiKeyColumns = `id, merchant_id, livemode, key_prefix, scopes, created_at, revoked_at, name, last_used_at, expires_at, replaced_by`

func scanAPIKey(row pgx.Row) (APIKeyRecord, error) {
	var k APIKeyRecord
	err := row.Scan(&k.ID, &k.MerchantID, &k.Livemode, &k.Prefix, &k.Scopes, &k.CreatedAt, &k.RevokedAt, &k.Name, &k.LastUsedAt,
		&k.ExpiresAt, &k.ReplacedBy)
	return k, err
}

func insertAPIKey(ctx context.Context, tx pgx.Tx, k APIKeyRecord, hash [32]byte) error {
	_, err := tx.Exec(ctx, `INSERT INTO api_keys (id, merchant_id, livemode, key_hash, key_prefix, scopes, created_at, name)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, k.ID, k.MerchantID, k.Livemode, hash[:], k.Prefix, k.Scopes, k.CreatedAt, k.Name)
	return err
}

// InsertAPIKey stores a key by hash. The plaintext never reaches this layer.
func (s *Store) InsertAPIKey(ctx context.Context, merchantID string, livemode bool, hash [32]byte, prefix string, scopes []string, actor string) (APIKey, error) {
	k := APIKeyRecord{APIKey: APIKey{ID: ids.New(ids.APIKey), MerchantID: merchantID, Livemode: livemode, Prefix: prefix, Scopes: scopes,
		CreatedAt: s.now()}}
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if err := insertAPIKey(ctx, tx, k, hash); err != nil {
			return err
		}
		return audit(ctx, tx, merchantID, actor, "api_key.created", map[string]any{"key_id": k.ID, "livemode": livemode, "scopes": scopes})
	})
	return k.APIKey, opErr("insert api key", err)
}

// InsertNamedAPIKey stores a key by hash with a display name.
func (s *Store) InsertNamedAPIKey(ctx context.Context, merchantID string, livemode bool, hash [32]byte, prefix string, scopes []string,
	name, actor string) (APIKeyRecord, error) {
	k := APIKeyRecord{APIKey: APIKey{ID: ids.New(ids.APIKey), MerchantID: merchantID, Livemode: livemode, Prefix: prefix, Scopes: scopes,
		CreatedAt: s.now()}, Name: &name}
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if err := insertAPIKey(ctx, tx, k, hash); err != nil {
			return err
		}
		return audit(ctx, tx, merchantID, actor, "api_key.created", map[string]any{"key_id": k.ID, "livemode": livemode, "scopes": scopes, "name": name})
	})
	return k, opErr("insert named api key", err)
}

// AuthenticateAPIKey returns the key with this hash if it authenticates now: not revoked, and
// a rolled key only before its expiry.
func (s *Store) AuthenticateAPIKey(ctx context.Context, hash [32]byte) (APIKey, error) {
	k, err := scanAPIKey(s.db.QueryRow(ctx, `SELECT `+apiKeyColumns+` FROM api_keys
		WHERE key_hash = $1 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > $2)`, hash[:], s.now()))
	return k.APIKey, opErr("authenticate api key", notFound(err))
}

// TouchAPIKey records a use of the key at now, unless the recorded use is more recent than
// now - resolution. Concurrent requests on any number of instances write at most once per
// resolution window. It reports whether it wrote.
func (s *Store) TouchAPIKey(ctx context.Context, keyID string, resolution time.Duration) (bool, error) {
	now := s.now()
	tag, err := s.db.Exec(ctx, `UPDATE api_keys SET last_used_at = $2
		WHERE id = $1 AND (last_used_at IS NULL OR last_used_at <= $3)`, keyID, now, now.Add(-resolution))
	if err != nil {
		return false, opErr("touch api key", err)
	}
	return tag.RowsAffected() == 1, nil
}

// RevokeAPIKey revokes any active key by its id alone; RevokeMerchantAPIKey is the
// merchant-scoped form.
func (s *Store) RevokeAPIKey(ctx context.Context, keyID, actor string) error {
	return opErr("revoke api key", s.inTx(ctx, func(tx pgx.Tx) error {
		var merchantID string
		err := tx.QueryRow(ctx, `UPDATE api_keys SET revoked_at = $2 WHERE id = $1 AND revoked_at IS NULL RETURNING merchant_id`,
			keyID, s.now()).Scan(&merchantID)
		if err != nil {
			return notFound(err)
		}
		return audit(ctx, tx, merchantID, actor, "api_key.revoked", map[string]any{"key_id": keyID})
	}))
}

// ListAPIKeys lists the merchant's keys in one mode: keys that still authenticate first, newest
// first within each group.
func (s *Store) ListAPIKeys(ctx context.Context, merchantID string, livemode bool) ([]APIKeyRecord, error) {
	rows, err := s.db.Query(ctx, `SELECT `+apiKeyColumns+` FROM api_keys WHERE merchant_id = $1 AND livemode = $2
		ORDER BY (revoked_at IS NOT NULL OR (expires_at IS NOT NULL AND expires_at <= $3)), id DESC`, merchantID, livemode, s.now())
	if err != nil {
		return nil, opErr("list api keys", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (APIKeyRecord, error) { return scanAPIKey(r) })
	return out, opErr("list api keys", err)
}

// GetAPIKey returns one of the merchant's keys in a mode.
func (s *Store) GetAPIKey(ctx context.Context, merchantID string, livemode bool, keyID string) (APIKeyRecord, error) {
	k, err := scanAPIKey(s.db.QueryRow(ctx, `SELECT `+apiKeyColumns+` FROM api_keys WHERE id = $1 AND merchant_id = $2 AND livemode = $3`,
		keyID, merchantID, livemode))
	return k, opErr("get api key", notFound(err))
}

// RevokeMerchantAPIKey revokes a key only if it belongs to the merchant and mode. Revoking a
// rolled key ends its overlap at once.
func (s *Store) RevokeMerchantAPIKey(ctx context.Context, merchantID string, livemode bool, keyID, actor string) (APIKeyRecord, error) {
	var k APIKeyRecord
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		k, err = scanAPIKey(tx.QueryRow(ctx, `UPDATE api_keys SET revoked_at = coalesce(revoked_at, $4)
			WHERE id = $1 AND merchant_id = $2 AND livemode = $3 RETURNING `+apiKeyColumns, keyID, merchantID, livemode, s.now()))
		if err != nil {
			return notFound(err)
		}
		return audit(ctx, tx, merchantID, actor, "api_key.revoked", map[string]any{"key_id": keyID})
	})
	return k, opErr("revoke merchant api key", err)
}

// RollAPIKey replaces an active key with a new one of the same name and scopes (hash and prefix
// are the new secret's). The old key keeps authenticating for overlap, then is refused; with no
// overlap it is revoked in the same transaction. Both steps are audited: api_key.created and
// api_key.rolled now, api_key.expired when the overlap ends (here, or later by
// ExpireRolledAPIKeys).
func (s *Store) RollAPIKey(ctx context.Context, merchantID string, livemode bool, keyID string, hash [32]byte, prefix string,
	overlap time.Duration, actor string) (fresh, old APIKeyRecord, err error) {
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		now := s.now()
		var err error
		old, err = scanAPIKey(tx.QueryRow(ctx, `SELECT `+apiKeyColumns+` FROM api_keys
			WHERE id = $1 AND merchant_id = $2 AND livemode = $3 FOR UPDATE`, keyID, merchantID, livemode))
		if err != nil {
			return notFound(err)
		}
		if !old.Active(now) || old.ReplacedBy != nil {
			return ErrKeyNotActive
		}
		fresh = APIKeyRecord{APIKey: APIKey{ID: ids.New(ids.APIKey), MerchantID: merchantID, Livemode: livemode, Prefix: prefix,
			Scopes: old.Scopes, CreatedAt: now}, Name: old.Name}
		if err := insertAPIKey(ctx, tx, fresh, hash); err != nil {
			return err
		}
		created := map[string]any{"key_id": fresh.ID, "livemode": livemode, "scopes": fresh.Scopes, "rolled_from": old.ID}
		if old.Name != nil {
			created["name"] = *old.Name
		}
		if err := audit(ctx, tx, merchantID, actor, "api_key.created", created); err != nil {
			return err
		}
		expires := now.Add(overlap)
		var revoked *time.Time
		if overlap <= 0 {
			expires, revoked = now, &now
		}
		old, err = scanAPIKey(tx.QueryRow(ctx, `UPDATE api_keys SET expires_at = $2, replaced_by = $3, revoked_at = $4
			WHERE id = $1 RETURNING `+apiKeyColumns, old.ID, expires, fresh.ID, revoked))
		if err != nil {
			return err
		}
		if err := audit(ctx, tx, merchantID, actor, "api_key.rolled", map[string]any{"key_id": old.ID, "replaced_by": fresh.ID,
			"livemode": livemode, "overlap_seconds": int64(max(overlap, 0) / time.Second), "expires_at": expires.Format(time.RFC3339)}); err != nil {
			return err
		}
		if revoked != nil {
			return audit(ctx, tx, merchantID, actor, "api_key.expired", map[string]any{"key_id": old.ID, "replaced_by": fresh.ID,
				"livemode": livemode, "expired_at": expires.Format(time.RFC3339)})
		}
		return nil
	})
	return fresh, old, opErr("roll api key", err)
}

// ExpireRolledAPIKeys marks up to limit rolled keys whose overlap has ended as revoked (at their
// expiry) and audits each. Authentication refuses them from expires_at on regardless; this
// records the second step of the roll. It returns how many it marked.
func (s *Store) ExpireRolledAPIKeys(ctx context.Context, limit int) (int, error) {
	n := 0
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `UPDATE api_keys SET revoked_at = expires_at
			WHERE id IN (SELECT id FROM api_keys WHERE expires_at IS NOT NULL AND revoked_at IS NULL AND expires_at <= $1
				ORDER BY expires_at LIMIT $2 FOR UPDATE SKIP LOCKED)
			RETURNING id, merchant_id, livemode, replaced_by, expires_at`, s.now(), limit)
		if err != nil {
			return err
		}
		type expired struct {
			id, merchant, replacedBy string
			live                     bool
			at                       time.Time
		}
		list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (expired, error) {
			var e expired
			err := r.Scan(&e.id, &e.merchant, &e.live, &e.replacedBy, &e.at)
			return e, err
		})
		if err != nil {
			return err
		}
		for _, e := range list {
			if err := audit(ctx, tx, e.merchant, "system", "api_key.expired", map[string]any{"key_id": e.id, "replaced_by": e.replacedBy,
				"livemode": e.live, "expired_at": e.at.UTC().Format(time.RFC3339)}); err != nil {
				return err
			}
		}
		n = len(list)
		return nil
	})
	return n, opErr("expire rolled api keys", err)
}
