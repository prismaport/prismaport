package soldetector

import (
	"context"
	"errors"
	"fmt"

	"github.com/prismaport/prismaport/internal/alert"
	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/service"
	"github.com/prismaport/prismaport/internal/session"
	"github.com/prismaport/prismaport/internal/solana"
	"github.com/prismaport/prismaport/internal/solattr"
	"github.com/prismaport/prismaport/internal/solrpc"
	"github.com/prismaport/prismaport/internal/solwatch"
	"github.com/prismaport/prismaport/internal/store"
)

func (d *Detector) scanTargets(ctx context.Context, ts []target) (int, error) {
	if len(ts) == 0 {
		return 0, nil
	}
	cursors, err := d.ensure(ctx, ts)
	if err != nil {
		return 0, err
	}
	found := 0
	for _, t := range ts {
		n, _, err := d.scan(ctx, t, cursors[t.address], 0)
		found += n
		if err != nil {
			return found, err
		}
	}
	return found, nil
}

// scan processes the account's signatures newer than its cursor, oldest first. A deposit account
// is listed by two providers (merged); a reference key by one, every tick of its wallet window.
// The cursor only moves past transactions that are attributed and at or below the last
// finalized slot a finality check read, so a restart (or a provider that lagged) never skips
// one. complete is false when a transaction could not be processed yet, or when a deposit
// account's listing reached neither minSlot (the slot its balance was read at; 0: no bound) nor
// any transaction to record.
func (d *Detector) scan(ctx context.Context, t target, cur store.SolanaWatch, minSlot uint64) (int, bool, error) {
	var sigs []solrpc.SignatureInfo
	var err error
	reached := true
	if t.kind == store.SolanaReference {
		sigs, err = d.rot.Signatures(ctx, t.address, cur.CursorSignature, cur.CursorSlot, signaturesPage)
	} else {
		sigs, reached, err = d.q.SignaturesAt(ctx, t.address, cur.CursorSignature, cur.CursorSlot, signaturesPage, minSlot)
	}
	if err != nil {
		return 0, false, err
	}
	if d.finalized == 0 && len(sigs) > 0 {
		// Only finality checks refresh the finalized slot; after a restart with nothing pending
		// none runs, and no cursor could move past anything (ADR-0008 H-5). Read it once. A
		// failure only keeps the cursors where they are, as before.
		if fin, err := d.rot.Slot(ctx, solrpc.Finalized); err == nil {
			d.finalized = fin
		} else {
			d.log.Warn("finalized slot read failed; cursors wait", "chain", d.chainID(), "err", err)
		}
	}
	found := 0
	advance, complete := true, true
	var next *solrpc.SignatureInfo
	for i := len(sigs) - 1; i >= 0; i-- {
		s := sigs[i]
		if s.Signature == cur.CursorSignature {
			continue
		}
		done, n, err := d.process(ctx, t, s)
		if err != nil {
			return found, false, err
		}
		found += n
		complete = complete && done
		if !done || s.Slot > d.finalized {
			advance = false
		}
		if advance {
			next = &sigs[i]
		}
	}
	if !reached && found == 0 {
		complete = false
		d.log.Info("signature index behind the balance read; retrying", "chain", d.chainID(), "account", t.address, "slot", minSlot)
	}
	if next != nil {
		return found, complete, d.st.SetSolanaCursor(ctx, d.chainID(), t.address, next.Signature, next.Slot)
	}
	return found, complete, nil
}

// process attributes one transaction. done is false while the providers cannot both serve it
// or its slot.
func (d *Detector) process(ctx context.Context, t target, s solrpc.SignatureInfo) (done bool, recorded int, err error) {
	sig := s.Signature
	tx, err := d.q.Transaction(ctx, sig, solrpc.Confirmed)
	switch {
	case errors.Is(err, solwatch.ErrLag):
		return false, 0, nil
	case errors.Is(err, solrpc.ErrNotFound):
		// Neither provider has it: listed from a fork that did not survive once its slot is final.
		return s.Slot <= d.finalized, 0, nil
	case err != nil:
		return false, 0, err
	}
	if tx.Failed() {
		return true, 0, nil
	}
	switch t.kind {
	case store.SolanaDeposit:
		e := t.entries[0]
		dep, ok, err := solattr.DepositInflow(tx, t.address, t.owner, e.Token)
		if err != nil {
			return true, 0, d.rejected(ctx, e, sig, err)
		}
		if !ok {
			return true, 0, nil
		}
		ok, err = d.record(ctx, e, tx, sig, uint32(dep.AccountIndex), dep.Amount, store.KindTransfer, nil)
		if err != nil || !ok {
			return false, 0, err
		}
		return true, 1, nil
	case store.SolanaReference:
		n := 0
		done := true
		for _, e := range t.entries {
			split, ok := walletSplit(e)
			if !ok {
				continue
			}
			sp, ok, err := solattr.WalletPayment(tx, e.Token, split)
			if err != nil {
				if err := d.rejected(ctx, e, sig, err); err != nil {
					return true, n, err
				}
				continue
			}
			if !ok {
				continue
			}
			ok, err = d.record(ctx, e, tx, sig, uint32(sp.Index), sp.Gross, store.KindGateway, &sp)
			if err != nil {
				return false, n, err
			}
			if !ok {
				done = false
				continue
			}
			n++
		}
		return done, n, nil
	}
	return true, 0, nil
}

// walletSplit is the split a connected-wallet payment for entry e must follow: its fee rule and
// payout snapshot, over the classic token accounts of the entry's mint.
func walletSplit(e store.WatchEntry) (solattr.Split, bool) {
	tre, err1 := solana.ParsePublicKey(e.Treasury)
	mint, err2 := solana.ParsePublicKey(e.Token)
	if err1 != nil || err2 != nil || len(e.Recipients) == 0 {
		return solattr.Split{}, false
	}
	treATA, err := solana.AssociatedTokenAddress(tre, mint)
	if err != nil {
		return solattr.Split{}, false
	}
	sp := solattr.Split{TreasuryATA: treATA.String(), Decimals: e.Decimals, Fee: e.Fee}
	for _, r := range e.Recipients {
		w, err := solana.ParsePublicKey(r.Address)
		if err != nil {
			return solattr.Split{}, false
		}
		ata, err := solana.AssociatedTokenAddress(w, mint)
		if err != nil {
			return solattr.Split{}, false
		}
		sp.RecipientATAs, sp.Bps = append(sp.RecipientATAs, ata.String()), append(sp.Bps, r.Bps)
	}
	return sp, true
}

func (d *Detector) rejected(ctx context.Context, e store.WatchEntry, sig string, err error) error {
	var r *solattr.Rejection
	if !errors.As(err, &r) {
		return err
	}
	d.alerts.Alert(ctx, alert.Alert{Severity: alert.Critical, Chain: d.chainID(), Kind: "solana_transfer_rejected", Message: r.Reason,
		Fields: map[string]string{"session": e.SessionID, "tx": sig}})
	return nil
}

// record registers a payment and its slot. It is idempotent; a transaction that landed again
// in a different block (same signature after a fork) replaces the stale payment. ok is false
// when the block is not served by both providers yet (retried on the next scan) or the payment
// cannot be recorded at all (alerted).
func (d *Detector) record(ctx context.Context, e store.WatchEntry, tx *solrpc.Tx, sig string, index uint32, amount money.Units, kind string, sp *solattr.Settlement) (bool, error) {
	hash, err := d.q.BlockHash(ctx, tx.Slot, solrpc.Confirmed)
	if errors.Is(err, solwatch.ErrLag) || errors.Is(err, solrpc.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	obs := service.Observation{SessionID: e.SessionID, Chain: e.Chain, Asset: e.Asset, Address: e.Address, TxHash: sig, LogIndex: index, Amount: amount}
	pid, _, err := d.svc.RecordPayment(ctx, obs)
	if err != nil {
		if errors.Is(err, session.ErrAmountOutOfRange) || errors.Is(err, session.ErrUnknownPayTo) {
			d.alerts.Alert(ctx, alert.Alert{Severity: alert.Warning, Chain: d.chainID(), Kind: "payment_not_recordable",
				Message: err.Error(), Fields: map[string]string{"session": e.SessionID, "tx": sig}})
			return true, nil
		}
		if errors.Is(err, service.ErrPaymentOnOtherSession) {
			// Another session on the same customer address recorded it first.
			d.log.Info("payment already credited to another session", "chain", d.chainID(), "session", e.SessionID, "tx", sig)
			return true, nil
		}
		if errors.Is(err, service.ErrSessionSuperseded) {
			// A newer session opened on the customer address meanwhile and owns its accounts: the
			// cursor stays before this transaction, so that session's scan reads it.
			d.log.Info("transfer left to the customer address's newer session", "chain", d.chainID(), "session", e.SessionID, "tx", sig)
			return false, nil
		}
		return false, fmt.Errorf("record payment %s/%d: %w", sig, index, err)
	}
	existing, err := d.st.ChainPaymentsOfSession(ctx, e.SessionID)
	if err != nil {
		return false, err
	}
	for _, p := range existing {
		if p.PaymentID == pid && (p.BlockHash != hash || p.BlockNumber != tx.Slot) {
			if _, err := d.svc.ReversePayment(ctx, e.SessionID, pid); err != nil {
				return false, err
			}
			if pid, _, err = d.svc.RecordPayment(ctx, obs); err != nil {
				return false, err
			}
		}
	}
	// The first finalized-slot read is due FinalityDelay after detection (D49).
	fin := d.st.Now().Add(d.cfg.FinalityDelay)
	cp := store.ChainPayment{PaymentID: pid, SessionID: e.SessionID, Chain: e.Chain, Asset: e.Asset, Kind: kind,
		BlockNumber: tx.Slot, BlockHash: hash, FinalityCheckAt: &fin}
	if sp != nil {
		idx := uint32(sp.Index)
		f, n := sp.Fee, sp.Net
		cp.SplitLogIndex, cp.SplitFee, cp.SplitNet, cp.SplitPayouts = &idx, &f, &n, sp.Payouts
	}
	d.log.Info("payment detected", "chain", e.Chain, "session", e.SessionID, "payment", pid, "kind", kind, "slot", tx.Slot)
	return true, d.st.UpsertChainPayment(ctx, cp)
}
