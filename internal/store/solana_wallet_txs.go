package store

import (
	"context"
	"time"

	"github.com/prismaport/prismaport/internal/money"
)

// WalletTx is the last connected-wallet transaction the fee payer signed for a session.
type WalletTx struct {
	SessionID   string
	Chain       string
	Asset       string
	Payer       string
	Gross       money.Units
	Transaction string
	// LastValidHeight is the last block height at which the transaction can land.
	LastValidHeight uint64
	IssuedAt        time.Time
}

// Outstanding reports whether the transaction may still land: it was issued less than ttl ago,
// or the chain has not passed its last valid height.
func (w WalletTx) Outstanding(now time.Time, height uint64, ttl time.Duration) bool {
	return now.Before(w.IssuedAt.Add(ttl)) || height <= w.LastValidHeight
}

// GetWalletTx returns the session's last wallet transaction (ErrNotFound when none was issued).
func (s *Store) GetWalletTx(ctx context.Context, sessionID string) (WalletTx, error) {
	w := WalletTx{SessionID: sessionID}
	var gross string
	var height int64
	err := s.db.QueryRow(ctx, `SELECT chain, asset, payer, gross::text, transaction, last_valid_height, issued_at
		FROM solana_wallet_txs WHERE session_id = $1`, sessionID).Scan(&w.Chain, &w.Asset, &w.Payer, &gross, &w.Transaction, &height, &w.IssuedAt)
	if err != nil {
		return w, opErr("get wallet tx", notFound(err))
	}
	w.LastValidHeight = uint64(height)
	if w.Gross, err = parseUnits("solana_wallet_txs.gross", gross); err != nil {
		return w, opErr("get wallet tx", err)
	}
	return w, nil
}

// ReserveWalletTx records w (issued now) as the session's wallet transaction unless the one
// recorded is still Outstanding at height; it reports false, writing nothing, in that case. One
// statement decides, so of concurrent builds on any API instance only one is recorded.
func (s *Store) ReserveWalletTx(ctx context.Context, w WalletTx, height uint64, ttl time.Duration) (bool, error) {
	now := s.now()
	tag, err := s.db.Exec(ctx, `INSERT INTO solana_wallet_txs
			(session_id, chain, asset, payer, gross, transaction, last_valid_height, issued_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (session_id) DO UPDATE SET chain = EXCLUDED.chain, asset = EXCLUDED.asset, payer = EXCLUDED.payer,
			gross = EXCLUDED.gross, transaction = EXCLUDED.transaction, last_valid_height = EXCLUDED.last_valid_height,
			issued_at = EXCLUDED.issued_at
		WHERE solana_wallet_txs.issued_at <= $9 AND solana_wallet_txs.last_valid_height < $10`,
		w.SessionID, w.Chain, w.Asset, w.Payer, w.Gross.String(), w.Transaction, int64(w.LastValidHeight), now, now.Add(-ttl), int64(height))
	if err != nil {
		return false, opErr("reserve wallet tx", err)
	}
	return tag.RowsAffected() == 1, nil
}
