package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prismaport/prismaport/internal/ids"
)

// Merchant is an account that owns products, keys and wallets.
type Merchant struct {
	ID        string
	Name      string
	CreatedAt time.Time
	// LiveEnabledAt is when live mode opened; nil while it is closed (a self-serve account from
	// before sign-up opened it).
	LiveEnabledAt *time.Time
	// LogoID is the merchant's checkout logo (logos.id); nil when it has none.
	LogoID *string
}

// ChainAsset names one asset on one chain.
type ChainAsset struct {
	Chain string
	Asset string
}

func audit(ctx context.Context, tx pgx.Tx, merchantID, actor, action string, details map[string]any) error {
	if details == nil {
		details = map[string]any{}
	}
	_, err := tx.Exec(ctx, `INSERT INTO audit_log (merchant_id, actor, action, details) VALUES ($1, $2, $3, $4)`,
		merchantID, actor, action, details)
	return err
}

// CreateMerchant adds a merchant whose live mode is open at once; a self-serve sign-up starts
// with it closed.
func (s *Store) CreateMerchant(ctx context.Context, name, actor string) (Merchant, error) {
	now := s.now()
	m := Merchant{ID: ids.NewAt(ids.Merchant, now), Name: name, CreatedAt: now}
	m.LiveEnabledAt = &m.CreatedAt
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO merchants (id, name, created_at, live_enabled_at) VALUES ($1, $2, $3, $3)`, m.ID, m.Name, m.CreatedAt); err != nil {
			return err
		}
		return audit(ctx, tx, m.ID, actor, "merchant.created", map[string]any{"name": name})
	})
	return m, opErr("create merchant", err)
}

// GetMerchant returns the merchant with the given id, or ErrNotFound when none exists.
func (s *Store) GetMerchant(ctx context.Context, id string) (Merchant, error) {
	var m Merchant
	err := s.db.QueryRow(ctx, `SELECT id, name, created_at, live_enabled_at, logo_id FROM merchants WHERE id = $1`, id).Scan(&m.ID, &m.Name,
		&m.CreatedAt, &m.LiveEnabledAt, &m.LogoID)
	return m, opErr("get merchant", notFound(err))
}

// EnableLive opens a merchant's closed live mode and accepts the given live assets
// when it accepts none yet. It reports false when live mode was already open.
func (s *Store) EnableLive(ctx context.Context, merchantID string, assets []ChainAsset, actor string) (bool, error) {
	changed := false
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE merchants SET live_enabled_at = $2 WHERE id = $1 AND live_enabled_at IS NULL`, merchantID, s.now())
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			if _, err := s.Bind(tx).GetMerchant(ctx, merchantID); err != nil {
				return err
			}
			return nil
		}
		changed = true
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM merchant_assets WHERE merchant_id = $1 AND livemode`, merchantID).Scan(&n); err != nil {
			return err
		}
		if n == 0 && len(assets) > 0 {
			if err := s.Bind(tx).SetAcceptedAssets(ctx, merchantID, true, assets, actor); err != nil {
				return err
			}
		}
		return audit(ctx, tx, merchantID, actor, "merchant.live_enabled", nil)
	})
	return changed, opErr("enable live", err)
}

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// SetAcceptedAssets replaces the merchant's accepted (chain, asset) pairs for one mode.
func (s *Store) SetAcceptedAssets(ctx context.Context, merchantID string, livemode bool, assets []ChainAsset, actor string) error {
	return opErr("set accepted assets", s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM merchant_assets WHERE merchant_id = $1 AND livemode = $2`, merchantID, livemode); err != nil {
			return err
		}
		var list []string
		for _, a := range assets {
			if _, err := tx.Exec(ctx, `INSERT INTO merchant_assets (merchant_id, livemode, chain, asset) VALUES ($1, $2, $3, $4)`,
				merchantID, livemode, a.Chain, a.Asset); err != nil {
				return err
			}
			list = append(list, a.Chain+":"+a.Asset)
		}
		return audit(ctx, tx, merchantID, actor, "accepted_assets.set", map[string]any{"livemode": livemode, "assets": list})
	}))
}

// AcceptedAssets lists the assets a merchant accepts in the given mode, ordered by chain and asset.
func (s *Store) AcceptedAssets(ctx context.Context, merchantID string, livemode bool) ([]ChainAsset, error) {
	rows, err := s.db.Query(ctx, `SELECT chain, asset FROM merchant_assets WHERE merchant_id = $1 AND livemode = $2
		ORDER BY chain, asset`, merchantID, livemode)
	if err != nil {
		return nil, opErr("accepted assets", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ChainAsset, error) {
		var a ChainAsset
		err := r.Scan(&a.Chain, &a.Asset)
		return a, err
	})
	return out, opErr("accepted assets", err)
}

// AuditEntry is one row of a merchant's audit log.
type AuditEntry struct {
	Actor     string
	Action    string
	CreatedAt time.Time
}

// AuditLog returns a merchant's audit entries in the order they were written.
func (s *Store) AuditLog(ctx context.Context, merchantID string) ([]AuditEntry, error) {
	rows, err := s.db.Query(ctx, `SELECT actor, action, created_at FROM audit_log WHERE merchant_id = $1 ORDER BY id`, merchantID)
	if err != nil {
		return nil, opErr("audit log", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (AuditEntry, error) {
		var a AuditEntry
		err := r.Scan(&a.Actor, &a.Action, &a.CreatedAt)
		return a, err
	})
	return out, opErr("audit log", err)
}
