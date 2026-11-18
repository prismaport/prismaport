package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/session"
)

// ChainPayment is the on-chain position of a recorded payment.
type ChainPayment struct {
	PaymentID     string
	SessionID     string
	Chain         string
	Asset         string
	Kind          string
	BlockNumber   uint64
	BlockHash     string
	SplitLogIndex *uint32
	SplitFee      *money.Units
	SplitNet      *money.Units
	// SplitPayouts is a signed-path or wallet payment's amount per recipient, in recipient order.
	SplitPayouts []money.Units
	// FinalityCheckAt is when the next finalized-head read for this payment is due (D49); nil is
	// due now. UpsertChainPayment sets it on insert and when the block changes.
	FinalityCheckAt *time.Time
}

// Chain payment kinds.
const (
	KindTransfer = "transfer"
	KindGateway  = "gateway"
)

// UpsertChainPayment inserts a chain payment or updates it when one with the same payment id exists.
func (s *Store) UpsertChainPayment(ctx context.Context, c ChainPayment) error {
	var idx *int32
	if c.SplitLogIndex != nil {
		v := int32(*c.SplitLogIndex)
		idx = &v
	}
	_, err := s.db.Exec(ctx, `INSERT INTO chain_payments (payment_id, session_id, chain, asset, kind, block_number, block_hash,
		split_log_index, split_fee, split_net, split_payouts, finality_check_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::numeric, $10::numeric, $11::text[]::base_units[], $12)
		ON CONFLICT (payment_id) DO UPDATE SET block_number = EXCLUDED.block_number, block_hash = EXCLUDED.block_hash,
			finality_check_at = CASE WHEN chain_payments.block_hash = EXCLUDED.block_hash THEN chain_payments.finality_check_at
				ELSE EXCLUDED.finality_check_at END`,
		c.PaymentID, c.SessionID, c.Chain, c.Asset, c.Kind, int64(c.BlockNumber), c.BlockHash, idx,
		optUnitsArg(c.SplitFee), optUnitsArg(c.SplitNet), unitsArrayArg(c.SplitPayouts), c.FinalityCheckAt)
	return opErr("upsert chain payment", err)
}

// TrackedPayment is a chain payment joined with its current payment row.
type TrackedPayment struct {
	ChainPayment
	Status   session.PaymentStatus
	TxHash   string
	LogIndex uint32
	Amount   money.Units
	Decimals uint8
}

func (s *Store) trackedPayments(ctx context.Context, where string, args ...any) ([]TrackedPayment, error) {
	rows, err := s.db.Query(ctx, `SELECT c.payment_id, c.session_id, c.chain, c.asset, c.kind, c.block_number, c.block_hash,
		c.split_log_index, c.split_fee::text, c.split_net::text, c.split_payouts::text[], p.status, p.tx_hash, p.log_index,
		p.amount::text, p.decimals, c.finality_check_at
		FROM chain_payments c JOIN payments p ON p.id = c.payment_id WHERE `+where+` ORDER BY c.block_number, p.log_index`, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (TrackedPayment, error) {
		var t TrackedPayment
		var bn int64
		var idx *int32
		var sf, sn *string
		var sp []string
		var st, amt string
		var li int32
		var dec int16
		if err := r.Scan(&t.PaymentID, &t.SessionID, &t.Chain, &t.Asset, &t.Kind, &bn, &t.BlockHash, &idx, &sf, &sn, &sp,
			&st, &t.TxHash, &li, &amt, &dec, &t.FinalityCheckAt); err != nil {
			return t, err
		}
		for _, x := range sp {
			u, err := parseUnits("chain_payments.split_payouts", x)
			if err != nil {
				return t, err
			}
			t.SplitPayouts = append(t.SplitPayouts, u)
		}
		t.BlockNumber, t.Status, t.LogIndex, t.Decimals = uint64(bn), session.PaymentStatus(st), uint32(li), uint8(dec)
		if idx != nil {
			v := uint32(*idx)
			t.SplitLogIndex = &v
		}
		var err error
		if t.SplitFee, err = parseOptUnits("chain_payments.split_fee", sf); err != nil {
			return t, err
		}
		if t.SplitNet, err = parseOptUnits("chain_payments.split_net", sn); err != nil {
			return t, err
		}
		t.Amount, err = parseUnits("payments.amount", amt)
		return t, err
	})
}

// PendingChainPayments lists detected and confirmed payments of a chain (not yet final).
func (s *Store) PendingChainPayments(ctx context.Context, chainID string) ([]TrackedPayment, error) {
	out, err := s.trackedPayments(ctx, `c.chain = $1 AND p.status IN ('detected', 'confirmed')`, chainID)
	return out, opErr("pending chain payments", err)
}

// SetPaymentFinalityCheck schedules the next finalized-head read for a payment.
func (s *Store) SetPaymentFinalityCheck(ctx context.Context, paymentID string, at time.Time) error {
	_, err := s.db.Exec(ctx, `UPDATE chain_payments SET finality_check_at = $2 WHERE payment_id = $1`, paymentID, at)
	return opErr("set payment finality check", err)
}

// ChainPaymentsOfSession lists every tracked payment of a session.
func (s *Store) ChainPaymentsOfSession(ctx context.Context, sessionID string) ([]TrackedPayment, error) {
	out, err := s.trackedPayments(ctx, `c.session_id = $1`, sessionID)
	return out, opErr("chain payments of session", err)
}
