package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// GetLogo returns a stored logo PNG by its content id.
func (s *Store) GetLogo(ctx context.Context, id string) ([]byte, error) {
	var b []byte
	err := s.db.QueryRow(ctx, `SELECT png FROM logos WHERE id = $1`, id).Scan(&b)
	return b, opErr("get logo", notFound(err))
}

// SetMerchantLogo points the merchant at the logo (stored once per content id) and deletes the
// one it replaces when no merchant points at it any more.
func (s *Store) SetMerchantLogo(ctx context.Context, merchantID, logoID string, png []byte, actor string) error {
	return opErr("set merchant logo", s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO logos (id, png, created_at) VALUES ($1, $2, $3) ON CONFLICT (id) DO NOTHING`,
			logoID, png, s.now()); err != nil {
			return err
		}
		old, err := swapLogo(ctx, tx, merchantID, &logoID)
		if err != nil {
			return err
		}
		if err := dropUnusedLogo(ctx, tx, old, logoID); err != nil {
			return err
		}
		return audit(ctx, tx, merchantID, actor, "merchant.logo_set", map[string]any{"logo_id": logoID})
	}))
}

// RemoveMerchantLogo clears the merchant's logo; it reports false when there was none.
func (s *Store) RemoveMerchantLogo(ctx context.Context, merchantID, actor string) (bool, error) {
	removed := false
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		old, err := swapLogo(ctx, tx, merchantID, nil)
		if err != nil || old == nil {
			return err
		}
		removed = true
		if err := dropUnusedLogo(ctx, tx, old, ""); err != nil {
			return err
		}
		return audit(ctx, tx, merchantID, actor, "merchant.logo_removed", map[string]any{"logo_id": *old})
	})
	return removed, opErr("remove merchant logo", err)
}

// swapLogo sets the merchant's logo_id and returns the previous one.
func swapLogo(ctx context.Context, tx pgx.Tx, merchantID string, logoID *string) (*string, error) {
	var old *string
	if err := tx.QueryRow(ctx, `SELECT logo_id FROM merchants WHERE id = $1 FOR UPDATE`, merchantID).Scan(&old); err != nil {
		return nil, notFound(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE merchants SET logo_id = $2 WHERE id = $1`, merchantID, logoID); err != nil {
		return nil, err
	}
	return old, nil
}

// dropUnusedLogo deletes the released logo unless it is the one being kept or another merchant
// still points at it. Another merchant uploading the same image at that moment may fail on the
// merchants.logo_id foreign key (and upload again); no merchant ever points at a deleted logo.
func dropUnusedLogo(ctx context.Context, tx pgx.Tx, old *string, keep string) error {
	if old == nil || *old == keep {
		return nil
	}
	_, err := tx.Exec(ctx, `DELETE FROM logos WHERE id = $1 AND NOT EXISTS (SELECT 1 FROM merchants WHERE logo_id = $1)`, *old)
	return err
}
