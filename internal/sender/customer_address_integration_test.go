//go:build integration

package sender_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/contracts"
	"github.com/prismaport/prismaport/internal/evm"
	"github.com/prismaport/prismaport/internal/evmfake"
	"github.com/prismaport/prismaport/internal/evmrpc"
	"github.com/prismaport/prismaport/internal/sender"
	"github.com/prismaport/prismaport/internal/store"
)

// payOn opens a session of 25.00 on a customer address and pays it on the fake chain.
func (e *env) payOn(addr, key string) (string, evm.Address) {
	e.t.Helper()
	r := e.app.Do("POST", "/v1/sessions", e.app.TestKey, map[string]any{"customer_address_id": addr, "amount": "25.00"}, "Idempotency-Key", key)
	if r.Status != 200 {
		e.t.Fatalf("session on %s: %d %s", addr, r.Status, r.Body)
	}
	var s struct {
		ID    string                     `json:"id"`
		PayTo []struct{ Address string } `json:"pay_to"`
	}
	_ = json.Unmarshal(r.Body, &s)
	to, _ := evm.ParseAddress(s.PayTo[0].Address)
	w := evm.Uint64Word(25_000_000)
	e.chain.Mine(evmfake.LogSpec{Address: usdc, Topics: []evm.Word{contracts.TransferTopic, evm.AddressWord(payer), evm.AddressWord(to)}, Data: w[:]})
	e.chain.MineN(1)
	for range 3 {
		if _, err := e.det.Step(e.t.Context()); err != nil {
			e.t.Fatal(err)
		}
	}
	e.bal[to] += 25_000_000
	return s.ID, to
}

// Sessions on one customer address pay into the same forwarder: the Factory derives it from each
// session's stored salt and inputs (no flush_address_mismatch), and two sessions that confirmed
// the same total each get their own flush.
func TestSessionsOnACustomerAddressFlushTheSameForwarder(t *testing.T) {
	t.Parallel()
	e := setup(t, nil, nil)
	r := e.app.Do("POST", "/v1/customer_addresses", e.app.TestKey, map[string]any{"customer_id": "user_1", "product_id": e.app.Link(e.app.TestKey)})
	if r.Status != 201 {
		t.Fatalf("customer address: %d %s", r.Status, r.Body)
	}
	addr := r.JSON(t)["id"].(string)
	s1, to1 := e.payOn(addr, "a")
	if _, err := e.snd.Plan(t.Context()); err != nil {
		t.Fatal(err)
	}
	e.app.Clock.Advance(2 * time.Hour)
	s2, to2 := e.payOn(addr, "b")
	if to1 != to2 {
		t.Fatalf("two sessions on one address pay %s and %s", to1.Hex(), to2.Hex())
	}
	if _, err := e.snd.Plan(t.Context()); err != nil {
		t.Fatal(err)
	}
	if e.alerts.Has("flush_address_mismatch") {
		t.Fatalf("alerts %v", e.alerts.Kinds())
	}
	j1, j2 := e.job(s1), e.job(s2)
	if j1.Ref == j2.Ref || j1.PlannedTotal.String() != "25000000" || j2.PlannedTotal.String() != "25000000" ||
		j1.Address != to1.Hex() || j2.Address != to1.Hex() {
		t.Fatalf("jobs %+v / %+v", j1, j2)
	}
}

// mineSharedFlush mines the job's flush in a block whose logs (served by eth_getLogs too) are the
// forwarder's Payout events then Settled, with the matching receipt, and lets it reach finality.
func (e *env) mineSharedFlush(j store.TxJob, fwd evm.Address, gross, feeAmt uint64, payouts map[evm.Address]uint64, order []evm.Address) {
	e.t.Helper()
	hash, _ := evm.ParseWord(j.TxHashes[0])
	treasury, _ := evm.ParseAddress(apptest.EVMTreasury)
	var specs []evmfake.LogSpec
	net := uint64(0)
	for _, r := range order {
		net += payouts[r]
		specs = append(specs, evmfake.LogSpec{Address: fwd, Topics: []evm.Word{contracts.PayoutTopic, evm.AddressWord(usdc), evm.AddressWord(r)},
			Data: word(payouts[r]), TxHash: hash})
	}
	tw := evm.AddressWord(treasury)
	specs = append(specs, evmfake.LogSpec{Address: fwd, Topics: []evm.Word{contracts.SettledTopic, evm.AddressWord(usdc)},
		Data: append(append(append(word(gross), word(feeAmt)...), word(net)...), tw[:]...), TxHash: hash})
	b := e.chain.Mine(specs...)
	e.chain.SetReceipt(evmrpc.Receipt{TxHash: hash, Status: 1, BlockNumber: b.Number, BlockHash: b.Hash, GasUsed: 210_000, Logs: b.Logs})
	e.bal[fwd] = 0
	e.chain.MineN(5)
}

// D50 opens the next session on a customer address once the previous one is paid, so both
// sessions' funds can sit in the shared forwarder when the first flush runs. That flush sweeps
// both: its settlement (gross 50.00) is recorded once, on the session whose job sent it. The other
// session's job waits while that flush is in flight, then finds the forwarder empty and ends as
// covered, naming the flush and the session: no second transaction, no retry, no alert, no
// second settlement.
func TestSharedForwarderFlushSettlesBothSessionsOnce(t *testing.T) {
	t.Parallel()
	e := setup(t, nil, nil)
	r := e.app.Do("POST", "/v1/customer_addresses", e.app.TestKey, map[string]any{"customer_id": "user_9", "product_id": e.app.Link(e.app.TestKey)})
	if r.Status != 201 {
		t.Fatalf("customer address: %d %s", r.Status, r.Body)
	}
	addr := r.JSON(t)["id"].(string)
	s1, fwd := e.payOn(addr, "a")
	s2, to2 := e.payOn(addr, "b")
	if s1 == s2 || to2 != fwd || e.bal[fwd] != 50_000_000 {
		t.Fatalf("sessions %s %s pay %s and %s, balance %d", s1, s2, fwd.Hex(), to2.Hex(), e.bal[fwd])
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
	if sent.ID == "" || other.Status != store.JobPending || other.Attempts != 0 {
		t.Fatalf("jobs after the first step: sent %+v, other %+v", sent, other)
	}
	broadcasts := len(e.sent)

	main, _ := evm.ParseAddress(apptest.EVMDestination)
	// 50.00 at the pilot fee: 0.25 + 0.10 to the treasury, 49.65 to the merchant.
	e.mineSharedFlush(sent, fwd, 50_000_000, 350_000, map[evm.Address]uint64{main: 49_650_000}, []evm.Address{main})
	// Mined, the flush waits for its scheduled finality check (D49); the other job waits too.
	e.step()
	if j := e.job(sent.SessionID); j.Status != store.JobMined {
		t.Fatalf("sending job after its receipt: %+v", j)
	}
	if j := e.job(other.SessionID); j.Status != store.JobPending || j.Attempts != 0 {
		t.Fatalf("the other job did not wait for the flush in flight: %+v", j)
	}
	e.app.Clock.Advance(16 * time.Minute)
	e.step()
	if j := e.job(sent.SessionID); j.Status != store.JobDone || j.Outcome != "settled" {
		t.Fatalf("sending job %+v", j)
	}
	e.app.Clock.Advance(2 * time.Second)
	e.step()

	j := e.job(other.SessionID)
	if j.Status != store.JobDone || j.Outcome != "covered" || j.Attempts != 0 || j.LastError != "" ||
		j.Details["covered_by_session"] != sent.SessionID || j.Details["covered_by_tx"] != sent.TxHashes[0] {
		t.Fatalf("covered job %+v", j)
	}
	if len(e.sent) != broadcasts {
		t.Fatalf("%d more transactions broadcast for an empty forwarder", len(e.sent)-broadcasts)
	}
	for _, k := range []string{"flush_failing", "flush_gave_up", "flush_bad_job", "settlement_mismatch", "flush_address_mismatch"} {
		if e.alerts.Has(k) {
			t.Fatalf("alert %s: %v", k, e.alerts.Kinds())
		}
	}
	a, _ := e.app.Store.LoadSession(t.Context(), sent.SessionID)
	b, _ := e.app.Store.LoadSession(t.Context(), other.SessionID)
	if len(a.Settlements) != 1 || a.Settlements[0].Gross.String() != "50000000" || len(b.Settlements) != 0 {
		t.Fatalf("settlements: sending session %+v, other %+v", a.Settlements, b.Settlements)
	}
	// Later steps change nothing.
	e.app.Clock.Advance(time.Hour)
	e.step()
	if js, _ := e.app.Store.SessionTxJobs(t.Context(), other.SessionID); len(js) != 1 || js[0].Outcome != "covered" {
		t.Fatalf("jobs %+v", js)
	}
}

// Session 1 on a customer address was flushed once (its own settlement S1) and then received a
// second deposit, which the next session's flush swept with that session's own funds. Session 1's
// second job finds the forwarder empty: its scan reads S1 again (already its own, nothing new) and
// the other session's flush. It ends covered, naming that flush and session (the dashboard links
// it), not settled_elsewhere because of the older S1.
func TestCoveredWinsOverAnOlderOwnSettlement(t *testing.T) {
	t.Parallel()
	e := setup(t, nil, nil)
	ctx := t.Context()
	r := e.app.Do("POST", "/v1/customer_addresses", e.app.TestKey, map[string]any{"customer_id": "user_7", "product_id": e.app.Link(e.app.TestKey)})
	if r.Status != 201 {
		t.Fatalf("customer address: %d %s", r.Status, r.Body)
	}
	addr := r.JSON(t)["id"].(string)
	main, _ := evm.ParseAddress(apptest.EVMDestination)
	s1, fwd := e.payOn(addr, "a")
	e.step()
	e.mineSharedFlush(e.job(s1), fwd, 25_000_000, 225_000, map[evm.Address]uint64{main: 24_775_000}, []evm.Address{main})
	e.step()
	e.app.Clock.Advance(16 * time.Minute)
	e.step()
	if j := e.job(s1); j.Outcome != "settled" {
		t.Fatalf("first flush %+v", j)
	}
	w := evm.Uint64Word(25_000_000)
	e.chain.Mine(evmfake.LogSpec{Address: usdc, Topics: []evm.Word{contracts.TransferTopic, evm.AddressWord(payer), evm.AddressWord(fwd)}, Data: w[:]})
	e.chain.MineN(1)
	e.bal[fwd] = 25_000_000
	for range 2 {
		if _, err := e.det.Step(ctx); err != nil {
			t.Fatal(err)
		}
	}
	s2, _ := e.payOn(addr, "b")
	if _, err := e.snd.Plan(ctx); err != nil {
		t.Fatal(err)
	}
	js, _ := e.app.Store.SessionTxJobs(ctx, s1)
	if len(js) != 2 || js[1].PlannedTotal.String() != "50000000" {
		t.Fatalf("session 1's second flush: %+v", js)
	}
	second := js[1]
	if _, err := e.app.Store.Pool().Exec(ctx, `UPDATE chain_tx_jobs SET next_attempt_at = $2 WHERE id = $1`, second.ID,
		e.app.Clock.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	e.step() // sends session 2's flush, which sweeps 50.00
	j2 := e.job(s2)
	e.mineSharedFlush(j2, fwd, 50_000_000, 350_000, map[evm.Address]uint64{main: 49_650_000}, []evm.Address{main})
	e.step()
	e.app.Clock.Advance(16 * time.Minute)
	e.step()
	e.app.Clock.Advance(time.Hour)
	e.step()
	got, err := e.app.Store.GetTxJob(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.JobDone || got.Outcome != "covered" || got.Details["covered_by_session"] != s2 || got.Details["covered_by_tx"] != j2.TxHashes[0] {
		t.Fatalf("session 1's second job %+v", got)
	}
}

// On a customer address, session 2's flush T2 swept both sessions' first deposits. Then a deposit
// of 30 arrived unseen and a stranger's flush T1 moved it: session 1's job records T1 (30, above
// its planned 25) and its rescan fails once. The retry sees T1 as already the session's and T2 as
// covering: it still rescans (the 30 is credited, to the address's current session) before ending
// covered. Before, the covering flush ended the retry at once and the 30 was never credited.
func TestOverflowRescanRetriedWhenAnotherFlushCovers(t *testing.T) {
	t.Parallel()
	rec := &failOnce{failed: true} // armed below
	e := setup(t, nil, func(c *sender.Config) { rec.Recorder = c.Recorder; c.Recorder = rec })
	ctx := t.Context()
	r := e.app.Do("POST", "/v1/customer_addresses", e.app.TestKey, map[string]any{"customer_id": "user_8", "product_id": e.app.Link(e.app.TestKey)})
	if r.Status != 201 {
		t.Fatalf("customer address: %d %s", r.Status, r.Body)
	}
	addr := r.JSON(t)["id"].(string)
	main, _ := evm.ParseAddress(apptest.EVMDestination)
	s1, fwd := e.payOn(addr, "a")
	s2, _ := e.payOn(addr, "b")
	if _, err := e.snd.Plan(ctx); err != nil {
		t.Fatal(err)
	}
	j1 := e.job(s1)
	if _, err := e.app.Store.Pool().Exec(ctx, `UPDATE chain_tx_jobs SET next_attempt_at = $2 WHERE id = $1`, j1.ID,
		e.app.Clock.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	e.step() // sends session 2's flush T2, which sweeps 50.00
	j2 := e.job(s2)
	e.mineSharedFlush(j2, fwd, 50_000_000, 350_000, map[evm.Address]uint64{main: 49_650_000}, []evm.Address{main})
	e.step()
	e.app.Clock.Advance(16 * time.Minute)
	e.step()
	if j := e.job(s2); j.Outcome != "settled" {
		t.Fatalf("session 2's flush %+v", j)
	}
	w := evm.Uint64Word(30_000_000)
	e.chain.Mine(evmfake.LogSpec{Address: usdc, Topics: []evm.Word{contracts.TransferTopic, evm.AddressWord(payer), evm.AddressWord(fwd)}, Data: w[:]})
	e.settledLogs(evm.Word{0x57, 0x7b}, fwd, 30_000_000, 250_000, false) // the stranger's T1
	e.bal[fwd] = 0
	e.chain.MineN(5)
	e.app.Clock.Advance(time.Hour)
	rec.failed = false
	if _, err := e.snd.Step(ctx); err == nil || !rec.failed {
		t.Fatalf("session 1's first rescan should have failed: %v", err)
	}
	e.step()
	got, err := e.app.Store.GetTxJob(ctx, j1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.JobDone || got.Outcome != "covered" || got.Details["covered_by_tx"] != j2.TxHashes[0] {
		t.Fatalf("session 1's job %+v", got)
	}
	ses, err := e.app.Store.LoadSession(ctx, s2)
	if err != nil {
		t.Fatal(err)
	}
	if len(ses.State.Payments) != 2 || ses.State.Payments[1].Amount.String() != "30000000" {
		t.Fatalf("the deposit T1 moved, on the current session: %+v", ses.State.Payments)
	}
}
