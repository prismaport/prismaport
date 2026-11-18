//go:build integration

package soldetector_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/chainwatch"
	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/scan"
	"github.com/prismaport/prismaport/internal/solana"
	"github.com/prismaport/prismaport/internal/soldetector"
	"github.com/prismaport/prismaport/internal/solfake"
	"github.com/prismaport/prismaport/internal/solrpc"
	"github.com/prismaport/prismaport/internal/store"
	"github.com/prismaport/prismaport/internal/worker"
)

func (e *env) scan() {
	e.t.Helper()
	if _, err := e.det.Scan(e.t.Context(), scan.Tick{Mode: scan.Warm, Full: true}); err != nil {
		e.t.Fatalf("scan: %v", err)
	}
}

func (e *env) late() {
	e.t.Helper()
	if _, err := e.det.Late(e.t.Context()); err != nil {
		e.t.Fatalf("late: %v", err)
	}
}

func (e *env) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.app.Store.Pool().Exec(e.t.Context(), sql, args...); err != nil {
		e.t.Fatal(err)
	}
}

// pay mines a deposit into the forwarder's token account (the fake applies its token balances).
func (e *env) pay(seed string, pda solana.PublicKey, amount uint64) {
	e.cl.Mine(e.deposit(seed, pda, amount))
}

var methods = []string{"MultipleAccounts", "Signatures", "Transaction", "Block", "Slot", "BlockHeight", "SignatureStatuses"}

// counter counts both providers' requests since its last reading.
type counter struct {
	e    *env
	last map[string]int
}

func (e *env) counter() *counter {
	c := &counter{e: e, last: map[string]int{}}
	c.read()
	return c
}

// read returns the requests per method (and "total") since the last read.
func (c *counter) read() map[string]int {
	out := map[string]int{}
	now := map[string]int{"total": c.e.a.Total() + c.e.b.Total()}
	for _, m := range methods {
		now[m] = c.e.a.Calls(m) + c.e.b.Calls(m)
	}
	for k, v := range now {
		if d := v - c.last[k]; d != 0 {
			out[k] = d
		}
		c.last[k] = v
	}
	return out
}

func (c *counter) expect(what string, want map[string]int) {
	c.e.t.Helper()
	got := c.read()
	total := 0
	for _, v := range want {
		total += v
	}
	if total > 0 {
		want["total"] = total
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		c.e.t.Fatalf("%s: requests %v, want %v", what, got, want)
	}
}

// An open revealed session nobody pays costs one getMultipleAccounts per tick for all its
// deposit accounts (and the rent receiver, in the same request), from one provider, alternating.
func TestOneAccountsRequestPerTick(t *testing.T) {
	t.Parallel()
	e := setup(t, solrpc.Confirmed)
	for i := range 3 {
		e.openKey(fmt.Sprint("k", i))
	}
	c := e.counter()
	a, b := e.a.Calls("MultipleAccounts"), e.b.Calls("MultipleAccounts")
	for range 4 {
		e.scan()
	}
	c.expect("4 ticks over 3 sessions", map[string]int{"MultipleAccounts": 4})
	if da, db := e.a.Calls("MultipleAccounts")-a, e.b.Calls("MultipleAccounts")-b; da != 2 || db != 2 {
		t.Fatalf("providers not alternating: a %d, b %d", da, db)
	}
	// A provider down: the other answers every tick, nothing is lost.
	e.a.SetDown(true)
	e.scan()
	e.scan()
	if got := c.read(); got["MultipleAccounts"] < 2 || e.b.Calls("MultipleAccounts")-b != 4 {
		t.Fatalf("failover: %v", got)
	}
}

// A balance that rose rescans the account (signatures from two providers, the transaction and
// its block agreed) and credits at confirmed without any slot read. Finality is scheduled: one
// finalized slot from one provider FinalityDelay later, then the agreed re-check; a slot not
// final yet is checked again one FinalityRetry later. A paid session with final payments leaves
// the watch (D47).
func TestBalanceRiseCreditsAndFinalityIsScheduled(t *testing.T) {
	t.Parallel()
	e := setup(t, solrpc.Confirmed)
	id, pda := e.open()
	c := e.counter()
	e.scan()
	c.expect("empty account", map[string]int{"MultipleAccounts": 1})

	e.cl.FinalDepth = 50
	e.pay("pay", pda, 25_000_000)
	e.scan()
	// Slot: the one finalized-slot read of a fresh detector, on its first scan (ADR-0008 H-5).
	c.expect("rise", map[string]int{"MultipleAccounts": 1, "Signatures": 2, "Transaction": 2, "Block": 2, "Slot": 1})
	if v := e.get(id); v.Status != "paid" || len(v.Payments) != 1 || v.Payments[0].Status != "confirmed" {
		t.Fatalf("credit at confirmed: %+v", v)
	}
	e.scan()
	c.expect("unchanged balance", map[string]int{"MultipleAccounts": 1})

	e.app.Clock.Advance(soldetector.DefaultFinalityDelay)
	e.scan()
	c.expect("finality due, slot not final", map[string]int{"MultipleAccounts": 1, "Slot": 1})
	e.app.Clock.Advance(30 * time.Second)
	e.scan()
	c.expect("before the retry", map[string]int{"MultipleAccounts": 1})

	e.cl.FinalDepth = 3
	e.cl.MineN(5)
	e.app.Clock.Advance(30 * time.Second)
	e.scan()
	c.expect("finality", map[string]int{"MultipleAccounts": 1, "Slot": 1, "Block": 2, "Transaction": 2})
	if v := e.get(id); v.Payments[0].Status != "finalized" {
		t.Fatalf("not finalized: %+v", v)
	}
	e.scan()
	c.expect("settled session", map[string]int{})
}

// Our own flush empties (and closes) the account: a lower balance is stored, nothing rescanned.
// A later deposit of the same amount is a rise again.
func TestDecreaseOnlyUpdatesTheBalance(t *testing.T) {
	t.Parallel()
	e := setup(t, solrpc.Confirmed)
	id, pda := e.open()
	e.pay("pay", pda, 10_000_000)
	e.scan()
	c := e.counter()
	e.cl.CloseAccount(ata(t, pda))
	e.scan()
	c.expect("closed by a flush", map[string]int{"MultipleAccounts": 1})
	e.pay("again", pda, 10_000_000)
	e.scan()
	if got := c.read(); got["Signatures"] != 2 {
		t.Fatalf("second deposit not rescanned: %v", got)
	}
	if v := e.get(id); v.AmountReceived != "20.00" || len(v.Payments) != 2 {
		t.Fatalf("second deposit: %+v", v)
	}
}

// A flush that emptied the account and a smaller deposit after it, both between two reads: the
// balance reads lower than the stored one but not zero, which scans the account and credits the
// deposit. Before, it was stored as a decrease and never scanned.
func TestLowerNonZeroBalanceScans(t *testing.T) {
	t.Parallel()
	e := setup(t, solrpc.Confirmed)
	id, pda := e.open()
	e.pay("pay", pda, 25_000_000)
	e.scan()
	e.cl.CloseAccount(ata(t, pda)) // a flush
	e.pay("again", pda, 10_000_000)
	e.scan()
	if v := e.get(id); v.AmountReceived != "35.00" || len(v.Payments) != 2 {
		t.Fatalf("deposit after a flush, below the stored balance: %+v", v)
	}
}

// Our flush landing stores a zero balance at its slot. A provider behind that slot still shows the
// balance from before the flush: its reading is older than the stored one and is ignored
// (neither scanned nor stored), so a deposit of the same amount later still reads as a rise and is
// credited. Storing b's reading would have made the deposit look like nothing new.
func TestReadingBeforeOurFlushLandedIsIgnored(t *testing.T) {
	t.Parallel()
	e := setup(t, solrpc.Confirmed)
	id, pda := e.open()
	acc := ata(t, pda)
	e.pay("pay", pda, 25_000_000)
	e.scan()
	e.cl.CloseAccount(acc) // our flush
	e.cl.MineN(3)
	if err := e.app.Store.SolanaFlushLanded(t.Context(), chainID, acc.String(), e.cl.Tip()); err != nil {
		t.Fatal(err)
	}
	// Both providers answer two slots behind the flush and show the account as it was before it.
	e.a.SetBehind(2)
	e.b.SetBehind(2)
	e.cl.SetTokenAccount(acc, usdc, pda, 25_000_000)
	c := e.counter()
	e.scan()
	c.expect("a reading from before our flush", map[string]int{"MultipleAccounts": 1})
	e.a.SetBehind(0)
	e.b.SetBehind(0)
	e.cl.CloseAccount(acc)
	e.cl.MineN(1)
	e.pay("again", pda, 25_000_000)
	e.scan()
	e.scan()
	if v := e.get(id); v.AmountReceived != "50.00" || len(v.Payments) != 2 {
		t.Fatalf("deposit after a stale reading: %+v", v)
	}
}

// The reference key (connected wallet, Solana Pay) is listed from one provider only while a
// wallet transaction built for the session can land: until wallet_tx_at + 2 x WalletWindow, and
// never past expiry + WalletWindow. Without wallet_tx_at it is never read.
func TestReferenceScannedOnlyInsideTheWalletWindow(t *testing.T) {
	t.Parallel()
	e := setup(t, solrpc.Confirmed)
	id, _ := e.open()
	c := e.counter()
	e.scan()
	c.expect("no wallet transaction", map[string]int{"MultipleAccounts": 1})

	e.exec(`UPDATE sessions SET wallet_tx_at = $2 WHERE id = $1`, id, e.app.Clock.Now())
	e.scan()
	c.expect("wallet transaction built", map[string]int{"MultipleAccounts": 1, "Signatures": 1})
	e.app.Clock.Advance(2*store.SolanaWalletWindow - time.Second)
	e.scan()
	c.expect("end of the window", map[string]int{"MultipleAccounts": 1, "Signatures": 1})
	e.app.Clock.Advance(time.Second)
	e.scan()
	c.expect("after the window", map[string]int{"MultipleAccounts": 1})

	// Built just before expiry: scanned until expiry + WalletWindow, deposits stop at expiry.
	var expires time.Time
	if err := e.app.Store.Pool().QueryRow(t.Context(), `SELECT expires_at FROM sessions WHERE id = $1`, id).Scan(&expires); err != nil {
		t.Fatal(err)
	}
	e.app.Clock.Set(expires.Add(-time.Minute))
	e.exec(`UPDATE sessions SET wallet_tx_at = $2 WHERE id = $1`, id, e.app.Clock.Now())
	e.app.Clock.Set(expires.Add(time.Second))
	if _, err := e.app.Svc.ExpireDue(t.Context(), 10); err != nil {
		t.Fatal(err)
	}
	sch, _ := scan.ParseSchedule(scan.DefaultLateSchedule)
	state := scan.FromStore(e.app.Store, chainID, chain.Solana, 30*time.Second, sch, scan.DefaultWatchAfterExpiry)
	if st, err := state(t.Context()); err != nil || !st.Open {
		t.Fatalf("chain not kept scanning inside the wallet window: %+v %v", st, err)
	}
	e.scan()
	c.expect("expired, inside expiry + window", map[string]int{"Signatures": 1})
	e.app.Clock.Set(expires.Add(store.SolanaWalletWindow))
	e.scan()
	c.expect("expiry + window", map[string]int{})
	if st, err := state(t.Context()); err != nil || st.Open {
		t.Fatalf("chain still scanning after the wallet window: %+v %v", st, err)
	}
}

// Late checks read the expired session's deposit account on the schedule (one request each); a
// risen balance finds the deposit as a late payment. Nothing is read after the watch ends, and
// "Check again" rescans the account even with an unchanged balance.
func TestLateChecksOnTheScheduleAndCheckAgain(t *testing.T) {
	t.Parallel()
	e := setup(t, solrpc.Confirmed)
	id, pda := e.open()
	expires := e.app.Clock.Now().Add(30 * time.Minute)
	e.app.Clock.Set(expires.Add(30 * time.Second))
	if _, err := e.app.Svc.ExpireDue(t.Context(), 10); err != nil {
		t.Fatal(err)
	}
	c := e.counter()
	e.late()
	c.expect("before the first step of the schedule", map[string]int{})
	e.app.Clock.Set(expires.Add(20 * time.Minute))
	e.pay("late", pda, 25_000_000)
	e.late()
	// Slot: the one finalized-slot read of a fresh detector, on its first scan (ADR-0008 H-5).
	c.expect("first late check", map[string]int{"MultipleAccounts": 1, "Signatures": 2, "Transaction": 2, "Block": 2, "Slot": 1})
	e.scan() // the payment is credited by the next tick (no account read: the session expired)
	if v := e.get(id); v.Status != "late_payment" || len(v.Payments) != 1 || v.Payments[0].Status != "confirmed" {
		t.Fatalf("late payment: %+v", v)
	}
	c.read()
	checks := 0
	for at := expires.Add(20 * time.Minute); !at.After(expires.Add(scan.DefaultWatchAfterExpiry)); at = at.Add(10 * time.Minute) {
		e.app.Clock.Set(at)
		e.late()
		checks += c.read()["MultipleAccounts"]
	}
	if checks != 24 { // 30m, then hourly 1h30 to 23h30 (23)
		t.Fatalf("%d checks after the first, want 24", checks)
	}
	e.app.Clock.Set(expires.Add(scan.DefaultWatchAfterExpiry + time.Minute))
	e.pay("after", pda, 5_000_000)
	e.late()
	c.expect("after the watch", map[string]int{})
	sch, _ := scan.ParseSchedule(scan.DefaultLateSchedule)
	st, err := scan.FromStore(e.app.Store, chainID, chain.Solana, 30*time.Second, sch, scan.DefaultWatchAfterExpiry)(t.Context())
	if err != nil || st.LateDue != nil {
		t.Fatalf("state after the watch %+v %v", st, err)
	}

	e.exec(`UPDATE sessions SET recheck_requested_at = $2 WHERE id = $1`, id, e.app.Clock.Now())
	e.late()
	e.scan()
	if v := e.get(id); len(v.Payments) != 2 {
		t.Fatalf("after check again: %+v", v)
	}
	var pending *time.Time
	if err := e.app.Store.Pool().QueryRow(t.Context(), `SELECT recheck_requested_at FROM sessions WHERE id = $1`, id).Scan(&pending); err != nil || pending != nil {
		t.Fatalf("recheck not cleared: %v %v", pending, err)
	}
	// "Check again" with nothing new still rescans (a foreign flush leaves no balance).
	c.read()
	e.exec(`UPDATE sessions SET recheck_requested_at = $2 WHERE id = $1`, id, e.app.Clock.Now().Add(time.Second))
	e.late()
	if got := c.read(); got["Signatures"] != 2 || got["MultipleAccounts"] != 1 {
		t.Fatalf("check again without a balance change: %v", got)
	}
}

// The idle heartbeat is one getSlot from one provider, alternating; a provider down is failed
// over (and the chain shows Degraded), and only every provider down fails it.
func TestHeartbeatIsOneSlotRequestAlternating(t *testing.T) {
	t.Parallel()
	e := setup(t, solrpc.Confirmed)
	c := e.counter()
	for range 2 {
		if err := e.det.Heartbeat(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	c.expect("two heartbeats", map[string]int{"Slot": 2})
	if e.a.Calls("Slot") != 1 || e.b.Calls("Slot") != 1 {
		t.Fatalf("not alternating: a %d, b %d", e.a.Calls("Slot"), e.b.Calls("Slot"))
	}
	e.a.SetDown(true)
	for range 2 {
		if err := e.det.Heartbeat(t.Context()); err != nil {
			t.Fatalf("one provider down: %v", err)
		}
	}
	if st := e.q.Status(); st.State != chainwatch.Degraded {
		t.Fatalf("status with a provider down: %+v", st)
	}
	e.b.SetDown(true)
	e.app.Clock.Advance(time.Hour)
	if err := e.det.Heartbeat(t.Context()); err == nil {
		t.Fatal("every provider down and the heartbeat succeeded")
	}
}

// A session revealed and never paid, driven by the pacer from the reveal to the end of its late
// checks: one request per tick of its 30-minute pay window and one per late check.
func TestNeverPaidSessionRequestBudget(t *testing.T) {
	t.Parallel()
	e := setup(t, solrpc.Confirmed)
	sch, _ := scan.ParseSchedule(scan.DefaultLateSchedule)
	p := scan.NewPacer(scan.Config{Chain: chainID, Intervals: scan.DefaultIntervals(), Now: e.app.Clock.Now},
		scan.FromStore(e.app.Store, chainID, chain.Solana, 30*time.Second, sch, scan.DefaultWatchAfterExpiry), e.det)
	ctx := t.Context()
	if _, err := p.Step(ctx); err != nil { // the startup heartbeat
		t.Fatal(err)
	}
	e.open()
	c := e.counter()
	end := e.app.Clock.Now().Add(24*time.Hour + 5*time.Minute)
	for e.app.Clock.Now().Before(end) {
		e.app.Clock.Advance(30 * time.Second)
		if _, err := e.app.Svc.ExpireDue(ctx, 10); err != nil {
			t.Fatal(err)
		}
		if _, err := p.Step(ctx); err != nil && !errors.Is(err, worker.ErrSkipped) {
			t.Fatal(err)
		}
	}
	got := c.read()
	heartbeats := got["Slot"] // idle heartbeats after the watch are not the session's
	t.Logf("never-paid Solana session: %v", got)
	if got["MultipleAccounts"] != 30+sch.Count() || got["total"]-heartbeats != 30+sch.Count() {
		t.Fatalf("%v, want %d getMultipleAccounts (30 ticks + %d late checks) and nothing else but heartbeats", got, 30+sch.Count(), sch.Count())
	}
}

// foreignFlush is someone else's flush of the deposit account: it touches the account and pays
// the rent receiver. close leaves the account closed (the rent goes to the rent receiver), else
// empty.
func (e *env) foreignFlush(seed string, pda solana.PublicKey, close bool) {
	e.t.Helper()
	acc := ata(e.t, pda)
	e.cl.Mine(solfake.Tx(solfake.Signature(seed), solfake.Key("stranger"),
		[]solrpc.Instruction{{ProgramID: program.String(), Accounts: []string{pda.String(), acc.String(), rent.String()}, Data: "flush"}}, nil, nil, nil))
	if !close {
		e.cl.SetTokenAccount(acc, usdc, pda, 0)
		return
	}
	e.cl.CloseAccount(acc)
	e.rentLamports += 2_039_280
	e.cl.SetAccount(rent.String(), solrpc.Account{Exists: true, Lamports: e.rentLamports})
}

// Flush is permissionless: a deposit flushed and closed by someone else between two reads leaves
// no balance, but the deposit account's rent reaching the rent receiver (read in the same
// request) lists its new signatures and rescans the account the foreign transaction closed.
func TestForeignFlushAndCloseBetweenTwoReadsIsFound(t *testing.T) {
	t.Parallel()
	e := setup(t, solrpc.Confirmed)
	e.rentLamports = 1_000_000_000
	e.cl.SetAccount(rent.String(), solrpc.Account{Exists: true, Lamports: e.rentLamports})
	id, pda := e.open()
	c := e.counter()
	e.scan()
	c.expect("baseline", map[string]int{"MultipleAccounts": 1})

	e.pay("pay", pda, 25_000_000)
	e.foreignFlush("stranger flush", pda, true)
	e.scan()
	// rent receiver signatures 1 + the foreign transaction 1 (one provider each), then the
	// account's rescan: signatures 2, the deposit's transaction and block 4, the flush's
	// transaction 2; the finalized-slot read of a fresh detector 1 (ADR-0008 H-5).
	c.expect("foreign flush and close", map[string]int{"MultipleAccounts": 1, "Signatures": 3, "Transaction": 5, "Block": 2, "Slot": 1})
	if v := e.get(id); v.Status != "paid" || len(v.Payments) != 1 {
		t.Fatalf("payment flushed by someone else not found: %+v", v)
	}
	e.scan()
	c.expect("nothing new", map[string]int{"MultipleAccounts": 1})
}

// Our own flush raises the rent receiver too: one signature list, no transaction read.
func TestOwnFlushRentIsOneSignatureList(t *testing.T) {
	t.Parallel()
	e := setup(t, solrpc.Confirmed)
	e.rentLamports = 1_000_000_000
	e.cl.SetAccount(rent.String(), solrpc.Account{Exists: true, Lamports: e.rentLamports})
	id, pda := e.open()
	e.scan()
	e.pay("pay", pda, 25_000_000)
	e.scan()
	sig := solfake.Signature("our flush")
	total := money.FromUint64(25_000_000)
	j, _, err := e.app.Store.InsertTxJob(t.Context(), store.TxJob{Chain: chainID, Kind: store.JobFlush, Ref: "own", SessionID: id, Asset: "USDC", Address: ata(t, pda).String(), Calldata: []byte{0}, PlannedTotal: &total})
	if err != nil {
		t.Fatal(err)
	}
	j.Status, j.TxHashes = store.JobDone, []string{sig}
	if err := e.app.Store.UpdateTxJob(t.Context(), &j); err != nil {
		t.Fatal(err)
	}
	c := e.counter()
	e.foreignFlush("our flush", pda, true)
	e.scan()
	c.expect("own flush", map[string]int{"MultipleAccounts": 1, "Signatures": 1})
}

// Someone else's flush that leaves the account open (empty) shows as an account that appeared:
// it is rescanned and the deposit found.
func TestForeignFlushLeavingTheAccountOpenIsFound(t *testing.T) {
	t.Parallel()
	e := setup(t, solrpc.Confirmed)
	id, pda := e.open()
	e.scan()
	e.pay("pay", pda, 25_000_000)
	e.foreignFlush("stranger flush", pda, false)
	c := e.counter()
	e.scan()
	if got := c.read(); got["Signatures"] != 2 {
		t.Fatalf("appeared account not rescanned: %v", got)
	}
	if v := e.get(id); v.Status != "paid" || len(v.Payments) != 1 {
		t.Fatalf("payment flushed by someone else not found: %+v", v)
	}
}

// A watch set larger than one getMultipleAccounts batch, read while one provider trails the
// other by a slot: every batch asks for the slot seen before the read (not the one an earlier
// batch of the same read answered), and a provider behind that slot hands the batch to the other
// one instead of failing the tick. Every tick succeeds and the deposit is found.
func TestBatchedReadWithAProviderOneSlotBehind(t *testing.T) {
	t.Parallel()
	e := setup(t, solrpc.Confirmed)
	det, err := soldetector.New(soldetector.Config{Chain: e.chain, Credit: solrpc.Confirmed, AccountBatch: 2}, e.q, e.app.Svc, e.app.Store, e.alerts, nil)
	if err != nil {
		t.Fatal(err)
	}
	e.det = det
	var last string
	var pda solana.PublicKey
	for i := range 3 {
		last, pda = e.openKey(fmt.Sprint("k", i)) // 3 deposit accounts + the rent receiver: 2 batches
	}
	e.b.SetBehind(1)
	c := e.counter()
	for range 4 {
		e.cl.MineN(1)
		e.scan()
	}
	if got := c.read(); got["MultipleAccounts"] < 8 {
		t.Fatalf("4 ticks of 2 batches: %v", got)
	}
	e.pay("pay", pda, 25_000_000)
	e.scan()
	e.scan()
	if v := e.get(last); v.Status != "paid" || len(v.Payments) != 1 {
		t.Fatalf("deposit with a provider behind: %+v", v)
	}
}

// A balance read that sees a deposit before both providers' signature index lists it: the risen
// balance is kept back (the listing did not reach the read's slot and found nothing), so the next
// tick rescans and credits the deposit once the index caught up. Storing it would have left the
// balance unchanged from then on, and the deposit unseen.
func TestSignatureIndexBehindTheBalanceReadRetries(t *testing.T) {
	t.Parallel()
	e := setup(t, solrpc.Confirmed)
	id, pda := e.open()
	e.scan()
	e.a.SetSignaturesBehind(1)
	e.b.SetSignaturesBehind(1)
	e.pay("pay", pda, 25_000_000)
	e.scan()
	if v := e.get(id); len(v.Payments) != 0 {
		t.Fatalf("credited from an index that does not list it: %+v", v)
	}
	e.scan()
	e.a.SetSignaturesBehind(0)
	e.b.SetSignaturesBehind(0)
	c := e.counter()
	e.scan()
	if got := c.read(); got["Signatures"] != 2 {
		t.Fatalf("the risen balance was stored without its transfer: %v", got)
	}
	if v := e.get(id); v.Status != "paid" || len(v.Payments) != 1 {
		t.Fatalf("deposit after the index caught up: %+v", v)
	}
}

// With an expiry hold, an incomplete first late check (the signature index behind the balance
// read) leaves next_check_at unset: the session stays open, the pass reports it, and the next
// check records the deposit mined before expiry on time. It used to schedule a retry a minute
// later, which ended the hold and made the deposit a late payment (ADR-0008 H-3).
func TestIncompleteFirstLateCheckKeepsTheExpiryHold(t *testing.T) {
	t.Parallel()
	e := setupHold(t, solrpc.Confirmed, 5*time.Minute)
	ctx := t.Context()
	id, pda := e.open()
	e.scan()
	e.pay("pay", pda, 25_000_000) // after the last scan, before expiry
	ses, err := e.app.Store.LoadSession(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	e.app.Clock.Set(ses.State.ExpiresAt.Add(time.Minute)) // the first late check is due

	e.a.SetSignaturesBehind(1)
	e.b.SetSignaturesBehind(1)
	_, err = e.det.Late(ctx)
	if _, err := e.app.Svc.ExpireDue(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if v := e.get(id); v.Status != "open" || len(v.Payments) != 0 {
		t.Fatalf("expiry did not wait for an incomplete late check: %+v", v)
	}
	if err == nil {
		t.Fatal("an incomplete first late check was not reported")
	}
	e.a.SetSignaturesBehind(0)
	e.b.SetSignaturesBehind(0)
	e.late()
	e.scan() // credits the payment (confirmed policy)
	if _, err := e.app.Svc.ExpireDue(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if v := e.get(id); v.Status != "paid" || len(v.Payments) != 1 {
		t.Fatalf("deposit mined before expiry: %+v", v)
	}
}
