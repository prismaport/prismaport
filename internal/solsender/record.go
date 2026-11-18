package solsender

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/prismaport/prismaport/internal/alert"
	"github.com/prismaport/prismaport/internal/chainsettle"
	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/solana"
	"github.com/prismaport/prismaport/internal/solattr"
	"github.com/prismaport/prismaport/internal/solrpc"
	"github.com/prismaport/prismaport/internal/solwatch"
	"github.com/prismaport/prismaport/internal/store"
)

// recordFlush stores the settlement a finalized flush transaction made. solattr already holds
// the token movements to the fee and split rule; the store checks them again against the
// session's snapshot. ok is true when the settlement is the session's (stored now or before); a
// settlement already recorded on another session (sessions on one customer address share the
// account) is reported in by instead.
func (s *Sender) recordFlush(ctx context.Context, j store.TxJob, f solana.Forwarder, mint, ata solana.PublicKey, tx *solrpc.Tx) (ok bool, by string, err error) {
	sp, ok, err := solattr.FlushSettlement(tx, f, ata, mint, s.assets[j.Asset].Decimals)
	var rej *solattr.Rejection
	if errors.As(err, &rej) {
		s.jobs.Alert(ctx, alert.Critical, "settlement_mismatch", rej.Reason, map[string]string{"session": j.SessionID, "tx": tx.Signatures[0]})
		return false, "", nil
	}
	if err != nil || !ok {
		return false, "", err
	}
	s.crossCheckEvents(ctx, j, f, mint, ata, tx, sp)
	return s.storeSettlement(ctx, j, tx, sp)
}

// crossCheckEvents alerts when the flush's Settled or Payout events differ from its token
// movements. The events are unauthenticated and only cross-checked.
func (s *Sender) crossCheckEvents(ctx context.Context, j store.TxJob, f solana.Forwarder, mint, ata solana.PublicKey, tx *solrpc.Tx,
	sp solattr.Settlement) {
	for _, ev := range solana.SettledEvents(tx.Logs) {
		if ev.Source == ata && (ev.Gross != uint64Of(sp.Gross) || ev.Fee != uint64Of(sp.Fee) || ev.Net != uint64Of(sp.Net)) {
			s.jobs.Alert(ctx, alert.Critical, "settled_event_mismatch", "Settled event differs from the token movements",
				map[string]string{"session": j.SessionID, "tx": tx.Signatures[0]})
		}
	}
	var paid []solana.PayoutEvent
	for _, ev := range solana.PayoutEvents(tx.Logs) {
		if ev.Forwarder == f.Address && ev.Mint == mint {
			paid = append(paid, ev)
		}
	}
	nonZero := 0
	for _, p := range sp.Payouts {
		if !p.IsZero() {
			nonZero++
		}
	}
	if len(paid) != nonZero {
		s.jobs.Alert(ctx, alert.Critical, "settled_event_mismatch", "Payout events differ from the token movements",
			map[string]string{"session": j.SessionID, "tx": tx.Signatures[0]})
	}
}

// storeSettlement records the flush's settlement on the job's session (recordFlush's ok and by).
func (s *Sender) storeSettlement(ctx context.Context, j store.TxJob, tx *solrpc.Tx, sp solattr.Settlement) (ok bool, by string, err error) {
	ses, err := s.st.RecordSettlement(ctx, j.SessionID, store.Settlement{Chain: s.chainID(), Asset: j.Asset, Gross: sp.Gross,
		Fee: sp.Fee, Net: sp.Net, Shares: sp.Payouts, TxHash: tx.Signatures[0], LogIndex: uint32(sp.Index), SettledAt: s.st.Now()}, chainsettle.Verify)
	switch {
	case errors.Is(err, store.ErrConflict):
		owner, err := s.st.SettlementSession(ctx, s.chainID(), tx.Signatures[0], uint32(sp.Index))
		if err != nil {
			return false, "", err
		}
		if owner != "" && owner != j.SessionID {
			return false, owner, nil
		}
		return true, "", nil
	case errors.Is(err, store.ErrSettlementMismatch):
		s.jobs.Alert(ctx, alert.Critical, "settlement_mismatch", err.Error(), map[string]string{"session": j.SessionID, "tx": tx.Signatures[0]})
		return false, "", nil
	case err != nil:
		return false, "", err
	}
	s.jobs.ReconcileFlushFees(ctx, j, ses)
	return true, "", nil
}

func uint64Of(u money.Units) uint64 {
	b := u.Big()
	if !b.IsUint64() {
		return 0
	}
	return b.Uint64()
}

// recordFlushes records every finalized flush of the account found in its history. On a customer
// address the account is shared: history before the session's first counted deposit (from_slot)
// is skipped, and a flush another session's job sent is left to that job and reported as covering.
func (s *Sender) recordFlushes(ctx context.Context, j store.TxJob, f solana.Forwarder, mint, ata solana.PublicKey) (sweep, error) {
	var sw sweep
	var fromSlot uint64
	if v, ok := j.Details["from_slot"]; ok {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return sw, fmt.Errorf("flush job %s: from_slot %q", j.ID, v)
		}
		fromSlot = n
	}
	others, err := s.st.OtherSessionFlushes(ctx, j)
	if err != nil {
		return sw, err
	}
	// One finalized slot from one provider (D49); the history itself through both.
	finalized, err := s.rot.Slot(ctx, solrpc.Finalized)
	if err != nil {
		return sw, err
	}
	sigs, err := s.q.Signatures(ctx, ata.String(), "", 0, 0)
	if err != nil {
		return sw, err
	}
	for _, sig := range sigs {
		if sig.Failed() || sig.Slot > finalized || sig.Slot < fromSlot {
			continue
		}
		if sid, ok := others[sig.Signature]; ok {
			sw.coveredTx, sw.coveredBy = sig.Signature, sid
			continue
		}
		tx, err := s.q.Transaction(ctx, sig.Signature, solrpc.Finalized)
		if errors.Is(err, solwatch.ErrLag) || errors.Is(err, solrpc.ErrNotFound) {
			continue
		}
		if err != nil {
			return sw, err
		}
		ok, by, err := s.recordFlush(ctx, j, f, mint, ata, tx)
		if err != nil {
			return sw, err
		}
		if ok {
			sw.recorded++
		}
		if by != "" {
			sw.coveredTx, sw.coveredBy = sig.Signature, by
		}
	}
	return sw, nil
}
