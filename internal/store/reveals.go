package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// Who revealed a network of a session (session_reveals.source).
const (
	RevealCheckout    = "checkout"
	RevealMerchantAPI = "merchant_api"
	RevealWallet      = "wallet"
	// RevealCustomerAddress: a session opened on a customer address. Its customer already holds
	// the address, so every network of the session is revealed when it is created.
	RevealCustomerAddress = "customer_address"
)

// Reveal is a network (registry chain id) of a session whose deposit details were shown (D48).
// Only revealed networks are watched; every asset of the network shares the reveal.
type Reveal struct {
	Chain      string
	RevealedAt time.Time
	Source     string
}

// Revealed is the reveal of a network of the session, nil while it was never shown.
func (s *Session) Revealed(chainID string) *Reveal {
	for i := range s.Reveals {
		if s.Reveals[i].Chain == chainID {
			return &s.Reveals[i]
		}
	}
	return nil
}

func sessionReveals(ctx context.Context, q querier, id string) ([]Reveal, error) {
	rows, err := q.Query(ctx, `SELECT chain, revealed_at, source FROM session_reveals WHERE session_id = $1
		ORDER BY revealed_at, chain`, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Reveal, error) {
		var v Reveal
		err := r.Scan(&v.Chain, &v.RevealedAt, &v.Source)
		return v, err
	})
}

// RevealInput asks for one network of a session. Actor, when set, writes the session.revealed
// audit row (merchant API reveals); MerchantID scopes it.
type RevealInput struct {
	SessionID  string
	Chain      string
	Source     string
	MerchantID string
	Actor      string
}

// RevealNetwork reveals one network of a session under the session's row lock. check runs first
// on the locked session and may refuse. The session's first reveal starts its pay window:
// payment_started_at = now and expires_at = now + the pay window, in the same transaction as the
// reveal row; later reveals leave expires_at alone. A network already revealed changes nothing,
// except that a wallet source always moves wallet_tx_at. It answers the session after the reveal
// and whether this call revealed the network.
func (s *Store) RevealNetwork(ctx context.Context, in RevealInput, check func(*Session) error) (*Session, bool, error) {
	var out *Session
	revealed := false
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		cur, err := loadSession(ctx, tx, in.SessionID, true)
		if err != nil {
			return err
		}
		if err := check(cur); err != nil {
			return err
		}
		now := s.now()
		if cur.Revealed(in.Chain) == nil {
			if _, err := tx.Exec(ctx, `INSERT INTO session_reveals (session_id, chain, revealed_at, source) VALUES ($1, $2, $3, $4)`,
				cur.ID, in.Chain, now, in.Source); err != nil {
				return err
			}
			if cur.PaymentStartedAt == nil {
				if _, err := tx.Exec(ctx, `UPDATE sessions SET payment_started_at = $2, expires_at = $3, updated_at = $2 WHERE id = $1`,
					cur.ID, now, now.Add(cur.PayWindow)); err != nil {
					return err
				}
			}
			if in.Actor != "" {
				if err := audit(ctx, tx, in.MerchantID, in.Actor, "session.revealed",
					map[string]any{"session_id": cur.ID, "livemode": cur.Livemode, "chain": in.Chain}); err != nil {
					return err
				}
			}
			revealed = true
		}
		if in.Source == RevealWallet {
			if _, err := tx.Exec(ctx, `UPDATE sessions SET wallet_tx_at = $2 WHERE id = $1`, cur.ID, now); err != nil {
				return err
			}
		}
		if !revealed {
			out = cur
			return nil
		}
		out, err = loadSession(ctx, tx, in.SessionID, false)
		return err
	})
	if err != nil {
		return nil, false, opErr("reveal network", err)
	}
	return out, revealed, nil
}

// WalletTxAt is when a wallet transaction was last built or submitted for the session (nil:
// never).
func (s *Store) WalletTxAt(ctx context.Context, id string) (*time.Time, error) {
	var at *time.Time
	err := s.db.QueryRow(ctx, `SELECT wallet_tx_at FROM sessions WHERE id = $1`, id).Scan(&at)
	return at, opErr("wallet tx at", notFound(err))
}
