package detector

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/prismaport/prismaport/internal/alert"
	"github.com/prismaport/prismaport/internal/chainwatch"
	"github.com/prismaport/prismaport/internal/evmrpc"
	"github.com/prismaport/prismaport/internal/session"
	"github.com/prismaport/prismaport/internal/store"
)

// canonicalHashes re-reads payments' block hashes through two-provider agreement, once per block.
type canonicalHashes struct {
	d      *Detector
	hashes map[uint64]string
}

// check returns known=false when the providers cannot both serve the block yet.
func (c *canonicalHashes) check(ctx context.Context, p store.TrackedPayment) (ok, known bool, err error) {
	h, cached := c.hashes[p.BlockNumber]
	if !cached {
		w, err := c.d.q.BlockHash(ctx, p.BlockNumber)
		if errors.Is(err, evmrpc.ErrNotFound) {
			return false, false, nil
		}
		if err != nil {
			return false, false, err
		}
		h = w.Hex()
		c.hashes[p.BlockNumber] = h
	}
	return h == p.BlockHash, true, nil
}

// reorged reverses a payment whose block hash changed and forgets what checks saw on its
// session's addresses, so the next balance read rescans them from their first block (the
// transfer may be mined again). A signed-path payment is found only from its settle job's block,
// never by those rescans: the job that recorded it is reopened, so the sender follows its
// transaction again and hands the block it mines in to GatewaySettled.
func (d *Detector) reorged(ctx context.Context, p store.TrackedPayment, stage string) error {
	if _, err := d.svc.ReversePayment(ctx, p.SessionID, p.PaymentID); err != nil && !errors.Is(err, session.ErrInvalidTransition) {
		return err
	}
	if p.Kind == store.KindGateway {
		n, err := d.st.ReopenSettleJobs(ctx, d.chainID(), p.SessionID, p.TxHash)
		if err != nil {
			return err
		}
		d.log.Warn("signed-path payment reorged out; settle job followed again", "chain", d.chainID(),
			"session", p.SessionID, "payment", p.PaymentID, "jobs", n)
	}
	d.alerts.Alert(ctx, alert.Alert{Severity: alert.Warning, Chain: d.chainID(), Kind: "reorg",
		Message: fmt.Sprintf("block %d of payment %s is no longer canonical at %s; payment reversed", p.BlockNumber, p.PaymentID, stage),
		Fields:  map[string]string{"session": p.SessionID, "payment": p.PaymentID, "block": fmt.Sprint(p.BlockNumber)}})
	return d.st.ResetLateCheck(ctx, d.chainID(), p.SessionID, p.BlockNumber)
}

// credit confirms the detected payments deep enough under the policy at head (a tick's aggregate
// block), each after its block hash is re-read through two-provider agreement and found
// unchanged. A changed hash reverses the payment. head (one provider's) is capped by the agreed
// latest head, read once a payment would confirm at it. With the safe-head policy one safe header
// is read from one provider. head is stored for the checkout's confirmation count.
func (d *Detector) credit(ctx context.Context, head uint64, pays []store.TrackedPayment) error {
	waiting := false
	for _, p := range pays {
		waiting = waiting || p.Status == session.Detected
	}
	if !waiting {
		return nil
	}
	if err := d.st.SetCreditHead(ctx, d.chainID(), head); err != nil {
		return err
	}
	h := chainwatch.Head{Latest: head}
	if d.cfg.Policy.UseSafeHead {
		safe, err := d.rot.Header(ctx, evmrpc.Safe)
		if err != nil && !errors.Is(err, evmrpc.ErrNotFound) {
			return err
		}
		h.Safe = min(safe.Number, head)
	}
	c := &canonicalHashes{d: d, hashes: map[uint64]string{}}
	agreed := false
	for _, p := range pays {
		if p.Status != session.Detected || !d.cfg.Policy.Confirmed(p.BlockNumber, h) {
			continue
		}
		if !agreed {
			// head came from one provider: before it confirms anything it is capped by the
			// agreed latest head, so one provider ahead (or on a fork) cannot count depth alone.
			latest, err := d.q.AgreedLatest(ctx)
			if err != nil {
				return err
			}
			h.Latest = min(h.Latest, latest)
			h.Safe = min(h.Safe, h.Latest)
			agreed = true
			if !d.cfg.Policy.Confirmed(p.BlockNumber, h) {
				continue
			}
		}
		ok, known, err := c.check(ctx, p)
		if err != nil {
			return err
		}
		if !known {
			continue
		}
		if !ok {
			if err := d.reorged(ctx, p, "credit"); err != nil {
				return err
			}
			continue
		}
		if _, err := d.svc.ConfirmPayment(ctx, p.SessionID, p.PaymentID); err != nil &&
			!errors.Is(err, session.ErrInvalidTransition) && !errors.Is(err, session.ErrUnknownPayment) {
			return err
		}
	}
	return nil
}

// finality runs the due finality checks of confirmed payments: one finalized header from one
// provider; a payment whose block it covers has its hash re-checked through two-provider
// agreement and is finalized (a signed-path one records its split first) or, when the hash
// changed, reversed. Any other is checked again FinalityRetry later.
func (d *Detector) finality(ctx context.Context, pays []store.TrackedPayment, now time.Time) error {
	var due []store.TrackedPayment
	for _, p := range pays {
		if p.Status == session.Confirmed && (p.FinalityCheckAt == nil || !p.FinalityCheckAt.After(now)) {
			due = append(due, p)
		}
	}
	if len(due) == 0 {
		return nil
	}
	fin, ferr := d.rot.Header(ctx, evmrpc.Finalized)
	if ferr != nil && !errors.Is(ferr, evmrpc.ErrNotFound) {
		return ferr
	}
	later := now.Add(d.cfg.FinalityRetry)
	c := &canonicalHashes{d: d, hashes: map[uint64]string{}}
	for _, p := range due {
		if ferr != nil || fin.Number < p.BlockNumber {
			if err := d.st.SetPaymentFinalityCheck(ctx, p.PaymentID, later); err != nil {
				return err
			}
			continue
		}
		ok, known, err := c.check(ctx, p)
		switch {
		case err != nil:
			return err
		case !known:
			if err := d.st.SetPaymentFinalityCheck(ctx, p.PaymentID, later); err != nil {
				return err
			}
			continue
		case !ok:
			if err := d.reorged(ctx, p, "finality"); err != nil {
				return err
			}
			continue
		}
		if p.Kind == store.KindGateway {
			if err := d.settleGateway(ctx, p); err != nil {
				return err
			}
		}
		if _, err := d.svc.FinalizePayment(ctx, p.SessionID, p.PaymentID); err != nil &&
			!errors.Is(err, session.ErrInvalidTransition) && !errors.Is(err, session.ErrUnknownPayment) {
			return err
		}
	}
	return nil
}
