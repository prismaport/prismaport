package store

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/prismaport/prismaport/internal/chainpause"
)

// ChainPauses lists the pauses in force (D64), oldest first.
func (s *Store) ChainPauses(ctx context.Context) ([]chainpause.Pause, error) {
	rows, err := s.db.Query(ctx, `SELECT chain, COALESCE(asset, ''), paused_at, paused_by FROM chain_pauses
		ORDER BY paused_at, chain, asset NULLS FIRST`)
	if err != nil {
		return nil, opErr("chain pauses", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (chainpause.Pause, error) {
		var p chainpause.Pause
		err := r.Scan(&p.Chain, &p.Asset, &p.PausedAt, &p.PausedBy)
		return p, err
	})
	return out, opErr("chain pauses", err)
}

// PauseChain pauses a chain (asset empty) or one asset on it, audited as chain.paused. It reports
// false, writing nothing, when that pause is already in force.
func (s *Store) PauseChain(ctx context.Context, chainID, asset, actor string) (bool, error) {
	changed := false
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO chain_pauses (chain, asset, paused_at, paused_by) VALUES ($1, NULLIF($2, ''), $3, $4)
			ON CONFLICT (chain, (COALESCE(asset, ''))) DO NOTHING`, chainID, asset, s.now(), actor)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		changed = true
		return auditNoMerchant(ctx, tx, actor, "chain.paused", pauseDetails(chainID, asset))
	})
	return changed, opErr("pause chain", err)
}

// ResumeChain ends the pause of a chain (asset empty) or of one asset on it, audited as
// chain.resumed. A pause of the whole chain and a pause of one of its assets are separate: each
// is resumed by its own call. It reports false when no such pause was in force.
func (s *Store) ResumeChain(ctx context.Context, chainID, asset, actor string) (bool, error) {
	changed := false
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM chain_pauses WHERE chain = $1 AND COALESCE(asset, '') = $2`, chainID, asset)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		changed = true
		return auditNoMerchant(ctx, tx, actor, "chain.resumed", pauseDetails(chainID, asset))
	})
	return changed, opErr("resume chain", err)
}

func pauseDetails(chainID, asset string) map[string]any {
	d := map[string]any{"chain": chainID, "asset": nil}
	if asset != "" {
		d["asset"] = asset
	}
	return d
}

// auditNoMerchant writes an audit entry that belongs to no merchant, inside tx.
func auditNoMerchant(ctx context.Context, tx pgx.Tx, actor, action string, details map[string]any) error {
	_, err := tx.Exec(ctx, `INSERT INTO audit_log (merchant_id, actor, action, details) VALUES (NULL, $1, $2, $3)`, actor, action, details)
	return err
}
