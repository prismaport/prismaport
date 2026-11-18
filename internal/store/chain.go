package store

import (
	"context"
	"time"
)

// ChainStatus is the persisted health of a chain's providers.
type ChainStatus struct {
	Chain  string
	State  string
	Reason string
	Since  time.Time
}

// SetChainStatus records a chain status, replacing any earlier one for the same chain.
func (s *Store) SetChainStatus(ctx context.Context, st ChainStatus) error {
	_, err := s.db.Exec(ctx, `INSERT INTO chain_status (chain, state, reason, since, updated_at) VALUES ($1, $2, $3, $4, now())
		ON CONFLICT (chain) DO UPDATE SET state = EXCLUDED.state, reason = EXCLUDED.reason, since = EXCLUDED.since, updated_at = now()`,
		st.Chain, st.State, st.Reason, st.Since)
	return opErr("set chain status", err)
}

// GetChainStatus returns the recorded status of a chain, or ErrNotFound when it has none.
func (s *Store) GetChainStatus(ctx context.Context, chainID string) (ChainStatus, error) {
	var st ChainStatus
	err := s.db.QueryRow(ctx, `SELECT chain, state, reason, since FROM chain_status WHERE chain = $1`, chainID).
		Scan(&st.Chain, &st.State, &st.Reason, &st.Since)
	return st, opErr("get chain status", notFound(err))
}
