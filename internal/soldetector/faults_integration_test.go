//go:build integration

package soldetector_test

import (
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/chainwatch"
	"github.com/prismaport/prismaport/internal/soldetector"
	"github.com/prismaport/prismaport/internal/solrpc"
	"github.com/prismaport/prismaport/internal/solwatch"
)

// Rollback, finality and RPC-error paths against solfake's rolled-back slots, commitment levels
// and per-method error injection (ADR 0008 E-3). The rule under test is fail closed: a payment
// counts only from what two providers agree on at the credit commitment, an error is never an
// answer, and nothing is lost once the providers answer again.

// onClock rebuilds the quorum and the detector on the test clock, so a provider's cooldown after
// an injected error can be waited out.
func (e *env) onClock(credit solrpc.Commitment) {
	e.t.Helper()
	e.q = solwatch.NewQuorum(chainID, []solwatch.Provider{e.a, e.b}, chainwatch.Options{Now: e.app.Clock.Now})
	det, err := soldetector.New(soldetector.Config{Chain: e.chain, Credit: credit}, e.q, e.app.Svc, e.app.Store, e.alerts, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	e.det = det
}

func (e *env) paid(id string) bool {
	e.t.Helper()
	v := e.get(id)
	return v.Status == "paid" && len(v.Payments) == 1
}

func TestProcessedOnlyDepositIsNotCredited(t *testing.T) {
	t.Parallel()
	e := setup(t, solrpc.Confirmed)
	e.cl.ConfirmDepth = 2
	id, pda := e.open()
	e.mustStep()
	slot := e.cl.Mine(e.deposit("pay", pda, 25_000_000))

	e.mustStep()
	if v := e.get(id); len(v.Payments) != 0 || v.Status != "open" {
		t.Fatalf("credited from a processed-only slot: %+v", v)
	}
	e.cl.Confirm(slot)
	e.mustStep()
	if !e.paid(id) {
		t.Fatalf("not credited once confirmed: %+v", e.get(id))
	}
}

func TestRolledBackDepositIsReversedAndNotCreditedAgain(t *testing.T) {
	t.Parallel()
	e := setup(t, solrpc.Confirmed)
	id, pda := e.open()
	e.mustStep()
	slot := e.cl.Mine(e.deposit("pay", pda, 25_000_000))
	e.mustStep()
	if !e.paid(id) {
		t.Fatalf("not credited at confirmed: %+v", e.get(id))
	}

	e.cl.Rollback(slot) // the deposit and the balance it wrote are gone
	e.cl.Finalize(slot)
	e.app.Clock.Advance(soldetector.DefaultFinalityDelay)
	e.mustStep()
	v := e.get(id)
	if len(v.Payments) != 1 || v.Payments[0].Status != "reversed" || v.Status == "paid" {
		t.Fatalf("rolled-back payment not reversed: %+v", v)
	}
	if !e.alerts.Has("reorg") {
		t.Fatalf("alerts %v", e.alerts.Kinds())
	}
	for range 3 {
		e.cl.MineN(1)
		e.mustStep()
	}
	if v := e.get(id); len(v.Payments) != 1 || v.Payments[0].Status != "reversed" || v.Status == "paid" {
		t.Fatalf("rolled-back deposit credited again: %+v", v)
	}
}

func TestRollbackBeforeFinalityNeverCreditsUnderFinalizedCredit(t *testing.T) {
	t.Parallel()
	e := setup(t, solrpc.Finalized)
	id, pda := e.open()
	e.mustStep()
	slot := e.cl.Mine(e.deposit("pay", pda, 25_000_000))
	e.mustStep()
	if v := e.get(id); v.Status == "paid" {
		t.Fatalf("credited before finality under finalized credit: %+v", v)
	}

	e.cl.Rollback(slot)
	e.cl.Finalize(slot + 1)
	for range 3 {
		e.app.Clock.Advance(soldetector.DefaultFinalityDelay)
		e.mustStep()
	}
	v := e.get(id)
	if v.Status == "paid" {
		t.Fatalf("a rolled-back deposit was credited: %+v", v)
	}
	for _, p := range v.Payments {
		if p.Status == "confirmed" || p.Status == "finalized" {
			t.Fatalf("a rolled-back deposit counts: %+v", v)
		}
	}
}

// Every RPC method the credit path uses, failing on both providers for one call: nothing is
// credited on that tick, and the deposit is credited exactly once after the cooldown.
func TestRPCErrorsCreditNothingAndLoseNothing(t *testing.T) {
	t.Parallel()
	for _, method := range []string{"MultipleAccounts", "Signatures", "Transaction", "Block"} {
		t.Run(method, func(t *testing.T) {
			e := setup(t, solrpc.Confirmed)
			e.onClock(solrpc.Confirmed)
			id, pda := e.open()
			e.mustStep()
			e.pay("pay", pda, 25_000_000)
			before := e.a.Calls(method) + e.b.Calls(method)
			e.a.FailNext(method, 1, nil)
			e.b.FailNext(method, 1, nil)

			_ = e.step() // the error is the point; what matters is that nothing counted
			if got := e.a.Calls(method) + e.b.Calls(method) - before; got < 2 {
				t.Fatalf("%s asked %d times on the failing tick: the fault was not exercised", method, got)
			}
			if v := e.get(id); len(v.Payments) != 0 || v.Status != "open" {
				t.Fatalf("credited while %s failed: %+v", method, v)
			}

			e.app.Clock.Advance(10 * time.Minute) // past every cooldown
			for range 2 {
				e.cl.MineN(1)
				if err := e.step(); err != nil {
					t.Fatalf("step after recovery: %v", err)
				}
			}
			if !e.paid(id) {
				t.Fatalf("deposit lost after %s recovered: %+v", method, e.get(id))
			}
		})
	}
}

// One provider without the transaction yet is lag, not an answer: no credit until both agree.
func TestOneProviderMissingTheTransactionWaits(t *testing.T) {
	t.Parallel()
	e := setup(t, solrpc.Confirmed)
	e.onClock(solrpc.Confirmed)
	id, pda := e.open()
	e.mustStep()
	e.pay("pay", pda, 25_000_000)
	e.b.FailNext("Transaction", 1, solrpc.ErrNotFound)

	_ = e.step()
	if v := e.get(id); len(v.Payments) != 0 {
		t.Fatalf("credited from one provider: %+v", v)
	}
	e.cl.MineN(1)
	e.mustStep()
	if !e.paid(id) {
		t.Fatalf("not credited once both providers have it: %+v", e.get(id))
	}
}

// The finality check reads one finalized slot and then re-checks the block hash and the
// transaction with two providers; an error there leaves the payment confirmed (never reversed,
// never finalized on one answer) and the next check finalizes it.
func TestFinalityCheckErrorsKeepThePaymentConfirmed(t *testing.T) {
	t.Parallel()
	for _, method := range []string{"Slot", "Block", "Transaction"} {
		t.Run(method, func(t *testing.T) {
			e := setup(t, solrpc.Confirmed)
			e.onClock(solrpc.Confirmed)
			id, pda := e.open()
			e.mustStep()
			slot := e.cl.Mine(e.deposit("pay", pda, 25_000_000))
			e.mustStep()
			if !e.paid(id) {
				t.Fatalf("not credited: %+v", e.get(id))
			}
			e.cl.Finalize(slot)
			e.app.Clock.Advance(soldetector.DefaultFinalityDelay)
			e.a.FailNext(method, 1, nil)
			e.b.FailNext(method, 1, nil)
			_ = e.step()
			if v := e.get(id); len(v.Payments) != 1 || v.Payments[0].Status != "confirmed" {
				t.Fatalf("finality check with %s failing changed the payment: %+v", method, v)
			}
			if e.alerts.Has("reorg") {
				t.Fatalf("an RPC error was taken for a reorg: %v", e.alerts.Kinds())
			}

			e.app.Clock.Advance(10 * time.Minute)
			for range 2 {
				e.mustStep()
				e.app.Clock.Advance(soldetector.DefaultFinalityDelay)
			}
			if v := e.get(id); len(v.Payments) != 1 || v.Payments[0].Status != "finalized" {
				t.Fatalf("not finalized after %s recovered: %+v", method, v)
			}
		})
	}
}
