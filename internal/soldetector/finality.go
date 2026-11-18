package soldetector

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/prismaport/prismaport/internal/alert"
	"github.com/prismaport/prismaport/internal/chainsettle"
	"github.com/prismaport/prismaport/internal/session"
	"github.com/prismaport/prismaport/internal/solrpc"
	"github.com/prismaport/prismaport/internal/solwatch"
	"github.com/prismaport/prismaport/internal/store"
)

// final reports whether a payment's transaction is final where it was recorded: both providers
// return it at the finalized commitment in the same slot, and the finalized block there has the
// hash recorded at detection. gone is true when the finalized fork proves it is not there.
func (d *Detector) final(ctx context.Context, p store.TrackedPayment) (ok, gone bool, err error) {
	hash, err := d.q.BlockHash(ctx, p.BlockNumber, solrpc.Finalized)
	switch {
	case errors.Is(err, solrpc.ErrSkipped):
		return false, true, nil
	case errors.Is(err, solwatch.ErrLag) || errors.Is(err, solrpc.ErrNotFound):
		return false, false, nil
	case err != nil:
		return false, false, err
	case hash != p.BlockHash:
		return false, true, nil
	}
	tx, err := d.q.Transaction(ctx, p.TxHash, solrpc.Finalized)
	switch {
	case errors.Is(err, solwatch.ErrLag):
		return false, false, nil
	case errors.Is(err, solrpc.ErrNotFound):
		// The block is final with the recorded hash but neither provider has the transaction:
		// keep waiting and raise a critical alert.
		d.alerts.Alert(ctx, alert.Alert{Severity: alert.Critical, Chain: d.chainID(), Kind: "finalized_tx_missing",
			Message: "finalized block has the recorded hash but the transaction is not served", Fields: map[string]string{"tx": p.TxHash}})
		return false, false, nil
	case err != nil:
		return false, false, err
	}
	if tx.Slot != p.BlockNumber || tx.Failed() {
		return false, true, nil
	}
	return true, false, nil
}

// reverse undoes a payment whose slot is not on the finalized fork.
func (d *Detector) reverse(ctx context.Context, p store.TrackedPayment) error {
	if _, err := d.svc.ReversePayment(ctx, p.SessionID, p.PaymentID); err != nil && !errors.Is(err, session.ErrInvalidTransition) {
		return err
	}
	d.alerts.Alert(ctx, alert.Alert{Severity: alert.Warning, Chain: d.chainID(), Kind: "reorg",
		Message: fmt.Sprintf("payment in slot %d is not on the finalized fork; reversed", p.BlockNumber),
		Fields:  map[string]string{"session": p.SessionID, "tx": p.TxHash}})
	// The transaction may land again in another block: rescan the session's accounts (the
	// balance seen is forgotten too, so the next read rescans), and keep rescanning the payment's
	// deposit account for a while even if no balance shows it (already flushed).
	if err := d.st.ResetSolanaCursors(ctx, d.chainID(), p.SessionID); err != nil {
		return err
	}
	entries, err := d.st.SessionWatchEntries(ctx, d.chainID(), p.SessionID)
	if err != nil {
		return err
	}
	for _, tg := range d.targets(entries) {
		if tg.kind == store.SolanaDeposit && tg.entries[0].Asset == p.Asset {
			d.reorged[tg.address] = d.st.Now().Add(ReorgRescan)
		}
	}
	return nil
}

// settle credits detected payments under the confirmed policy (the transaction was agreed at
// confirmed, no slot read needed) and runs the finality checks that are due: one finalized slot
// from one provider; a payment whose slot it covers has its block hash and transaction re-checked
// at finalized through two-provider agreement and is finalized (a wallet payment records its
// split first) or, when its slot did not survive, reversed. Any other is checked again
// FinalityRetry later.
func (d *Detector) settle(ctx context.Context, now time.Time) error {
	pays, err := d.st.PendingChainPayments(ctx, d.chainID())
	if err != nil || len(pays) == 0 {
		return err
	}
	due, err := d.creditConfirmed(ctx, pays, now)
	if err != nil {
		return err
	}
	if len(due) == 0 {
		return nil
	}
	fin, err := d.rot.Slot(ctx, solrpc.Finalized)
	if err != nil {
		return err
	}
	d.finalized = max(d.finalized, fin)
	later := now.Add(d.cfg.FinalityRetry)
	for _, p := range due {
		if err := d.finalizeDue(ctx, p, fin, later); err != nil {
			return err
		}
	}
	return nil
}

// creditConfirmed confirms the detected payments under the confirmed policy and returns the
// payments whose finality check is due.
func (d *Detector) creditConfirmed(ctx context.Context, pays []store.TrackedPayment, now time.Time) ([]store.TrackedPayment, error) {
	var due []store.TrackedPayment
	for _, p := range pays {
		if p.Status == session.Detected && d.cfg.Credit == solrpc.Confirmed {
			if err := d.confirmPayment(ctx, p); err != nil {
				return nil, err
			}
			p.Status = session.Confirmed
		}
		if p.FinalityCheckAt == nil || !p.FinalityCheckAt.After(now) {
			due = append(due, p)
		}
	}
	return due, nil
}

// finalizeDue runs one due finality check against the finalized slot fin: a payment above it, or
// whose transaction is not agreed at finalized yet, is checked again at later; one whose slot did
// not survive is reversed; any other is confirmed if needed, its wallet split recorded and
// finalized.
func (d *Detector) finalizeDue(ctx context.Context, p store.TrackedPayment, fin uint64, later time.Time) error {
	if p.BlockNumber > fin {
		return d.st.SetPaymentFinalityCheck(ctx, p.PaymentID, later)
	}
	ok, gone, err := d.final(ctx, p)
	switch {
	case err != nil:
		return err
	case gone:
		return d.reverse(ctx, p)
	case !ok:
		return d.st.SetPaymentFinalityCheck(ctx, p.PaymentID, later)
	}
	if p.Status == session.Detected {
		if err := d.confirmPayment(ctx, p); err != nil {
			return err
		}
	}
	if p.Kind == store.KindGateway {
		if err := d.settleWallet(ctx, p); err != nil {
			return err
		}
	}
	if _, err := d.svc.FinalizePayment(ctx, p.SessionID, p.PaymentID); err != nil &&
		!errors.Is(err, session.ErrInvalidTransition) && !errors.Is(err, session.ErrUnknownPayment) {
		return err
	}
	return nil
}

func (d *Detector) confirmPayment(ctx context.Context, p store.TrackedPayment) error {
	_, err := d.svc.ConfirmPayment(ctx, p.SessionID, p.PaymentID)
	if errors.Is(err, session.ErrInvalidTransition) || errors.Is(err, session.ErrUnknownPayment) {
		return nil
	}
	return err
}

// settleWallet records a connected-wallet payment's split as the settlement once final.
func (d *Detector) settleWallet(ctx context.Context, p store.TrackedPayment) error {
	return chainsettle.RecordSplit(ctx, d.st, d.alerts, d.chainID(), "soldetector: wallet", p)
}
