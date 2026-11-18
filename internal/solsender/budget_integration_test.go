//go:build integration

package solsender_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/scan"
	"github.com/prismaport/prismaport/internal/solana"
	"github.com/prismaport/prismaport/internal/solfake"
	"github.com/prismaport/prismaport/internal/solrpc"
	"github.com/prismaport/prismaport/internal/store"
	"github.com/prismaport/prismaport/internal/worker"
)

var budgetMethods = []string{"MultipleAccounts", "Signatures", "Transaction", "Block", "Slot", "BlockHeight", "SignatureStatuses",
	"LatestBlockhash", "Simulate", "Balance", "SendTransaction", "IsBlockhashValid", "MinimumBalanceForRentExemption"}

// calls is both providers' requests per method.
func (e *env) calls() map[string]int {
	out := map[string]int{}
	for _, m := range budgetMethods {
		out[m] = e.pa.Calls(m) + e.pb.Calls(m)
	}
	out["total"] = e.pa.Total() + e.pb.Total()
	return out
}

func addDelta(into, before, after map[string]int) {
	for k, v := range after {
		if d := v - before[k]; d != 0 {
			into[k] += d
		}
	}
}

// A session paid exactly on Solana 20 s after it was generated, from the reveal until the
// payment is final, the deposit flushed and the flush final (then 30 quiet minutes). The pacer
// drives the detector (hot = warm = 1 min) and the sender runs every 2 s; one slot every 2 s.
// The flush lands before the sender's next poll. Our flush closes the deposit account, whose rent
// reaches the rent receiver.
func TestPaidOnSolanaRequestBudget(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	ctx := t.Context()
	rentLamports := uint64(1_000_000_000)
	e.cl.SetAccount(rent.String(), solrpc.Account{Exists: true, Lamports: rentLamports})
	sch, _ := scan.ParseSchedule(scan.DefaultLateSchedule)
	p := scan.NewPacer(scan.Config{Chain: chainID, Intervals: scan.DefaultIntervals(), Now: e.app.Clock.Now},
		scan.FromStore(e.app.Store, chainID, chain.Solana, 30*time.Second, sch, scan.DefaultWatchAfterExpiry), e.det)
	if _, err := p.Step(ctx); err != nil { // the startup heartbeat
		t.Fatal(err)
	}

	r := e.app.RevealedSession(e.app.TestKey, `{"amount":"25.00"}`, "Idempotency-Key", "budget")
	var s struct {
		ID    string                            `json:"id"`
		PayTo []struct{ Chain, Address string } `json:"pay_to"`
	}
	_ = json.Unmarshal(r.Body, &s)
	var pda solana.PublicKey
	for _, pt := range s.PayTo {
		if pt.Chain == chainID {
			pda = solana.MustPublicKey(pt.Address)
		}
	}
	source := ata(t, pda)
	start := e.app.Clock.Now()
	det, snd := map[string]int{}, map[string]int{}
	landed := 0
	sentBefore := len(e.cl.Sent())
	for e.app.Clock.Now().Before(start.Add(40 * time.Minute)) {
		e.app.Clock.Advance(2 * time.Second)
		e.cl.Mine()
		if e.app.Clock.Now().Sub(start) == 20*time.Second {
			src := ata(t, payer)
			e.cl.Mine(solfake.Tx(solfake.Signature("budget pay"), payer, []solrpc.Instruction{solfake.Transfer(src, source, payer, 25_000_000)}, nil,
				[]solfake.TokenBalance{{Account: src, Mint: usdc, Owner: payer, Amount: 25_000_000}},
				[]solfake.TokenBalance{{Account: src, Mint: usdc, Owner: payer, Amount: 0}, {Account: source, Mint: usdc, Owner: pda, Amount: 25_000_000}}))
		}
		if sent := e.cl.Sent(); len(sent) > sentBefore && landed == 0 {
			// The flush lands: its transfers out, the deposit account closed, its rent paid.
			probe, err := solfake.FromRaw(sent[len(sent)-1], nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			m := split(t)
			i := solfake.InstructionIndex(probe, program, solana.IxFlush.Discriminator)
			var inner []solrpc.Instruction
			for _, mv := range m {
				inner = append(inner, solfake.Transfer(source, mv.to, pda, mv.amount))
			}
			tx, err := solfake.FromRaw(sent[len(sent)-1], map[int][]solrpc.Instruction{i: inner}, events(t, pda, source, 25_000_000, 225_000, 24_775_000, m[1]))
			if err != nil {
				t.Fatal(err)
			}
			e.cl.Mine(tx)
			e.cl.CloseAccount(source)
			rentLamports += 2_039_280
			e.cl.SetAccount(rent.String(), solrpc.Account{Exists: true, Lamports: rentLamports})
			landed++
		}
		if _, err := e.app.Svc.ExpireDue(ctx, 10); err != nil {
			t.Fatal(err)
		}
		before := e.calls()
		if _, err := p.Step(ctx); err != nil && !errors.Is(err, worker.ErrSkipped) {
			t.Fatal(err)
		}
		mid := e.calls()
		addDelta(det, before, mid)
		if d := mid["total"] - before["total"]; d > 0 {
			t.Logf("t+%s detector %d", e.app.Clock.Now().Sub(start), d)
		}
		if _, err := e.snd.Plan(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := e.snd.Step(ctx); err != nil {
			t.Fatal(err)
		}
		addDelta(snd, mid, e.calls())
	}
	ses, err := e.app.Store.LoadSession(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	j := e.job(s.ID)
	if ses.State.Status != "paid" || len(ses.Settlements) != 1 || j.Status != store.JobDone || j.Outcome != "settled" {
		t.Fatalf("session %s settlements %d job %s/%s", ses.State.Status, len(ses.Settlements), j.Status, j.Outcome)
	}
	for _, py := range ses.State.Payments {
		if py.Status != "finalized" {
			t.Fatalf("payment %+v not final", py)
		}
	}
	t.Logf("paid on Solana: detector %v, sender %v", det, snd)
	// Detector: the finalized slot read on its first scan (H-5) and the payment's finality check.
	if det["Slot"] != 2 || snd["Slot"] != 1 || det["BlockHeight"]+snd["BlockHeight"] != 0 {
		t.Fatalf("slot reads beyond the first scan and the two finality checks: detector %v, sender %v", det, snd)
	}
	// detector 17: getMultipleAccounts 4 (reveal, the tick that sees the deposit, the tick the
	// submitted flush wakes, the tick of the payment's finality), the finalized slot on the first
	// scan 1, rescan 6, rent receiver probe 1 (our flush's rent), payment finality 5; sender 18:
	// flush send 12, one status poll, flush finality 5.
	if got := det["total"] + snd["total"]; got != 35 || det["total"] != 17 {
		t.Fatalf("%d requests, want 35 (detector 17): detector %v, sender %v", got, det, snd)
	}
}

// While a flush is submitted the sender reads one signature status per poll (one provider) and
// broadcasts again; the agreed block height is read only once the blockhash may have expired.
// While it waits for finality nothing is read until its check is due, then one finalized slot.
func TestSenderMakesNoSlotReadsWhileSubmittedOrMined(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	id, pda, source := e.paid()
	e.step()
	if j := e.job(id); j.Status != store.JobSubmitted {
		t.Fatalf("job %+v", j)
	}
	diff := func(before map[string]int) map[string]int {
		out := map[string]int{}
		addDelta(out, before, e.calls())
		return out
	}
	before := e.calls()
	for range 5 {
		e.app.Clock.Advance(2 * time.Second)
		e.step()
	}
	if got := diff(before); got["SignatureStatuses"] != 5 || got["SendTransaction"] != 10 || got["total"] != 15 {
		t.Fatalf("5 polls while submitted: %v", got)
	}
	e.app.Clock.Advance(50 * time.Second)
	before = e.calls()
	e.step()
	if got := diff(before); got["BlockHeight"] != 2 || got["Slot"] != 0 {
		t.Fatalf("poll after the expiry horizon: %v", got)
	}

	m := split(t)
	sent := e.cl.Sent()
	probe, _ := solfake.FromRaw(sent[len(sent)-1], nil, nil)
	i := solfake.InstructionIndex(probe, program, solana.IxFlush.Discriminator)
	var inner []solrpc.Instruction
	for _, mv := range m {
		inner = append(inner, solfake.Transfer(source, mv.to, pda, mv.amount))
	}
	tx, err := solfake.FromRaw(sent[len(sent)-1], map[int][]solrpc.Instruction{i: inner}, events(t, pda, source, 25_000_000, 225_000, 24_775_000, m[1]))
	if err != nil {
		t.Fatal(err)
	}
	slot := e.cl.Mine(tx)
	e.cl.CloseAccount(source)
	before = e.calls()
	e.step()
	if j := e.job(id); j.Status != store.JobMined {
		t.Fatalf("job %+v", j)
	}
	for range 5 {
		e.app.Clock.Advance(2 * time.Second)
		e.step()
	}
	if got := diff(before); got["total"] != 1 || got["SignatureStatuses"] != 1 {
		t.Fatalf("landing then 10 s mined: %v", got)
	}
	e.cl.Finalize(slot)
	e.app.Clock.Advance(10 * time.Second)
	before = e.calls()
	e.step()
	if got := diff(before); got["Slot"] != 1 || got["total"] != 5 || e.job(id).Outcome != "settled" {
		t.Fatalf("finality check: %v, job %+v", got, e.job(id))
	}
}
