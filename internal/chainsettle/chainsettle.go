// Package chainsettle records the split a payment's own transaction made (the signed-path
// Gateway on EVM, the connected wallet on Solana) as its session's settlement, once the
// payment is final. The EVM and Solana detectors share it.
package chainsettle

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/prismaport/prismaport/internal/alert"
	"github.com/prismaport/prismaport/internal/payout"
	"github.com/prismaport/prismaport/internal/store"
)

// Store is what RecordSplit writes through; *store.Store satisfies it.
type Store interface {
	RecordSettlement(ctx context.Context, sessionID string, s store.Settlement, rule store.SettlementRule) (*store.Session, error)
	Now() time.Time
}

// Verify is the settlement rule (store.SettlementRule) every writer of a settlement passes: the
// fee and every recipient's amount must be exactly what the pay_to entry's fee rule and payout
// snapshot give for the gross, so fee + sum(payouts) == gross.
func Verify(pt store.PayToRecord, s store.Settlement) error {
	return payout.Verify(s.Gross, pt.Decimals, pt.Fee, payout.Bps(pt.Recipients), s.Fee, s.Shares)
}

// RecordSplit stores p's split as the settlement of its session. It runs before the payment is
// finalized, so a crash in between retries it: a duplicate is a conflict and ignored. A
// settlement that differs from the one already stored for the same transaction raises a
// critical settlement_mismatch alert and is not retried. who names the caller and the payment
// kind in the error for a payment without a split ("detector: gateway").
func RecordSplit(ctx context.Context, st Store, alerts alert.Sink, chainID, who string, p store.TrackedPayment) error {
	if p.SplitLogIndex == nil || p.SplitFee == nil || p.SplitNet == nil {
		return fmt.Errorf("%s payment %s has no split", who, p.PaymentID)
	}
	_, err := st.RecordSettlement(ctx, p.SessionID, store.Settlement{Chain: p.Chain, Asset: p.Asset, Gross: p.Amount,
		Fee: *p.SplitFee, Net: *p.SplitNet, Shares: p.SplitPayouts, TxHash: p.TxHash, LogIndex: *p.SplitLogIndex, SettledAt: st.Now(),
		Direct: true}, Verify)
	if errors.Is(err, store.ErrConflict) {
		return nil
	}
	if errors.Is(err, store.ErrSettlementMismatch) {
		alerts.Alert(ctx, alert.Alert{Severity: alert.Critical, Chain: chainID, Kind: "settlement_mismatch",
			Message: err.Error(), Fields: map[string]string{"session": p.SessionID, "tx": p.TxHash}})
		return nil
	}
	return err
}
