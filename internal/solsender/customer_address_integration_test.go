//go:build integration

package solsender_test

import (
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/solana"
	"github.com/prismaport/prismaport/internal/solfake"
	"github.com/prismaport/prismaport/internal/solrpc"
	"github.com/prismaport/prismaport/internal/solsender"
	"github.com/prismaport/prismaport/internal/store"
)

// openOn opens a 25.00 session on a customer address and returns its id and the address's
// Solana forwarder.
func (e *env) openOn(addr, key string) (string, solana.PublicKey) {
	e.t.Helper()
	r := e.app.Do("POST", "/v1/sessions", e.app.TestKey, map[string]any{"customer_address_id": addr, "amount": "25.00"}, "Idempotency-Key", key)
	if r.Status != 200 {
		e.t.Fatalf("session on %s: %d %s", addr, r.Status, r.Body)
	}
	body := r.JSON(e.t)
	for _, x := range body["pay_to"].([]any) {
		if p := x.(map[string]any); p["chain"] == chainID {
			return body["id"].(string), solana.MustPublicKey(p["address"].(string))
		}
	}
	e.t.Fatalf("no %s pay_to: %s", chainID, r.Body)
	return "", solana.PublicKey{}
}

// payInto mines a 25.00 deposit into the forwarder's token account, which then holds total.
func (e *env) payInto(pda solana.PublicKey, sig string, total uint64) solana.PublicKey {
	e.t.Helper()
	src, dst := ata(e.t, payer), ata(e.t, pda)
	e.cl.Mine(solfake.Tx(solfake.Signature(sig), payer, []solrpc.Instruction{solfake.Transfer(src, dst, payer, 25_000_000)}, nil,
		[]solfake.TokenBalance{{Account: src, Mint: usdc, Owner: payer, Amount: 25_000_000}, {Account: dst, Mint: usdc, Owner: pda, Amount: total - 25_000_000}},
		[]solfake.TokenBalance{{Account: src, Mint: usdc, Owner: payer, Amount: 0}, {Account: dst, Mint: usdc, Owner: pda, Amount: total}}))
	e.cl.SetTokenAccount(dst, usdc, pda, total)
	if _, err := e.det.Step(e.t.Context()); err != nil {
		e.t.Fatal(err)
	}
	return dst
}

// Two sessions on one customer address (the second opened once the first was paid, D50) hold
// funds in the same deposit account when the first flush is sent. That flush sweeps both and
// closes the account: its settlement is recorded once, on the session whose job sent it. The other
// job does not send a second flush while the first is in flight; once the first is final it finds
// the account empty and ends as covered, naming that flush and session, with no retry, no alert
// and no second settlement.
// mineFlush lands the last broadcast flush (as land does) without stepping the sender, and
// answers its slot.
func (e *env) mineFlush(pda, source solana.PublicKey, moves []move, logs []string) uint64 {
	e.t.Helper()
	sent := e.cl.Sent()
	if len(sent) == 0 {
		e.t.Fatal("nothing was broadcast")
	}
	raw := sent[len(sent)-1]
	probe, err := solfake.FromRaw(raw, nil, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	i := solfake.InstructionIndex(probe, program, solana.IxFlush.Discriminator)
	var inner []solrpc.Instruction
	for _, m := range moves {
		inner = append(inner, solfake.Transfer(source, m.to, pda, m.amount))
	}
	tx, err := solfake.FromRaw(raw, map[int][]solrpc.Instruction{i: inner}, logs)
	if err != nil {
		e.t.Fatal(err)
	}
	slot := e.cl.Mine(tx)
	e.cl.CloseAccount(source)
	return slot
}

func TestSharedDepositAccountFlushSettlesBothSessionsOnce(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	r := e.app.Do("POST", "/v1/customer_addresses", e.app.TestKey, map[string]any{"customer_id": "user_9", "product_id": e.app.Link(e.app.TestKey)})
	if r.Status != 201 {
		t.Fatalf("customer address: %d %s", r.Status, r.Body)
	}
	addr := r.JSON(t)["id"].(string)
	s1, pda := e.openOn(addr, "a")
	source := e.payInto(pda, "d1", 25_000_000)
	s2, pda2 := e.openOn(addr, "b")
	if s2 == s1 || pda2 != pda {
		t.Fatalf("second session %s pays %s, first %s pays %s", s2, pda2, s1, pda)
	}
	e.payInto(pda, "d2", 50_000_000)
	for _, id := range []string{s1, s2} {
		ses, _ := e.app.Store.LoadSession(t.Context(), id)
		if ses.State.Status != "paid" {
			t.Fatalf("session %s is %s", id, ses.State.Status)
		}
	}

	e.step() // plans both flushes, sends the oldest
	var sent, other store.TxJob
	for _, id := range []string{s1, s2} {
		if j := e.job(id); j.Status == store.JobSubmitted {
			sent = j
		} else {
			other = j
		}
	}
	if sent.ID == "" || other.Status != store.JobPending {
		t.Fatalf("jobs after the first step: sent %+v, other %+v", sent, other)
	}
	if other.Details["from_slot"] == "" {
		t.Fatalf("a shared account's job has no from_slot: %v", other.Details)
	}
	broadcasts := len(e.cl.Sent())
	// 50.00 at the pilot fee: 0.35 to the treasury, 49.65 to the merchant.
	m := []move{{ata(t, treasury()), 350_000}, {ata(t, merchant()), 49_650_000}}
	// The flush lands; while it waits for finality (scheduled, D49) the other job waits too.
	slot := e.mineFlush(pda, source, m, events(t, pda, source, 50_000_000, 350_000, 49_650_000, m[1]))
	e.step()
	if j := e.job(sent.SessionID); j.Status != store.JobMined {
		t.Fatalf("sending job after its signature landed: %+v", j)
	}
	if j := e.job(other.SessionID); j.Status != store.JobPending || j.Attempts != 0 {
		t.Fatalf("the other job did not wait for the flush in flight: %+v", j)
	}
	if n := len(e.cl.Sent()) - broadcasts; n != 0 {
		t.Fatalf("%d transactions broadcast while the first flush was in flight", n)
	}

	e.cl.Finalize(slot)
	e.app.Clock.Advance(solsender.DefaultFinalityDelay)
	e.step()
	if j := e.job(sent.SessionID); j.Status != store.JobDone || j.Outcome != "settled" {
		t.Fatalf("sending job %+v", j)
	}
	e.app.Clock.Advance(2 * time.Second)
	e.step()
	j := e.job(other.SessionID)
	if j.Status != store.JobDone || j.Outcome != "covered" || j.Attempts != 0 || j.LastError != "" ||
		j.Details["covered_by_session"] != sent.SessionID || j.Details["covered_by_tx"] != e.job(sent.SessionID).MinedTxHash {
		t.Fatalf("covered job %+v", j)
	}
	if n := len(e.cl.Sent()) - broadcasts; n != 0 {
		t.Fatalf("%d transactions broadcast for an empty account", n)
	}
	for _, k := range []string{"flush_failing", "flush_gave_up", "settlement_mismatch", "settled_event_mismatch", "flush_address_mismatch"} {
		if e.alerts.Has(k) {
			t.Fatalf("alert %s: %v", k, e.alerts.Kinds())
		}
	}
	if a, b := e.settlements(sent.SessionID), e.settlements(other.SessionID); len(a) != 1 || a[0].Gross.String() != "50000000" || len(b) != 0 {
		t.Fatalf("settlements: sending session %+v, other %+v", a, b)
	}
}
