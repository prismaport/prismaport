//go:build integration

package detector_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/chainsettle"
	"github.com/prismaport/prismaport/internal/chainwatch"
	"github.com/prismaport/prismaport/internal/detector"
	"github.com/prismaport/prismaport/internal/evm"
	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/payout"
	"github.com/prismaport/prismaport/internal/scan"
	"github.com/prismaport/prismaport/internal/store"
	"github.com/prismaport/prismaport/internal/testutil"
	"github.com/prismaport/prismaport/internal/worker"
)

func (e *env) late() {
	e.t.Helper()
	if _, err := e.det.Late(e.t.Context()); err != nil {
		e.t.Fatalf("late: %v", err)
	}
}

func (e *env) expire() {
	e.t.Helper()
	if _, err := e.app.Svc.ExpireDue(e.t.Context(), 10); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) recheckRequested(id string) bool {
	e.t.Helper()
	var at *time.Time
	if err := e.app.Store.Pool().QueryRow(e.t.Context(), `SELECT recheck_requested_at FROM sessions WHERE id = $1`, id).Scan(&at); err != nil {
		e.t.Fatal(err)
	}
	return at != nil
}

// An expired session leaves the per-tick read; a scheduled late check (one aggregate) finds a
// deposit through the bounded rescan and records it as a late payment. Checks follow the
// schedule, stop at the end of the watch, and "Check again" credits a deposit sent after it.
func TestLateCheckFindsADepositUntilTheWatchEnds(t *testing.T) {
	e := setup(t, func(c *detector.Config) { c.MaxRange = 500 })
	ctx := t.Context()
	s := e.create("25.00", "a")
	to := addr(t, s, "USDC")
	e.chain.MineN(2)
	e.tick() // registers the address while the session is open
	expires := e.app.Clock.Now().Add(30 * time.Minute)
	e.app.Clock.Advance(time.Hour)
	e.expire()
	e.chain.Mine(transfer(usdc, payer, to, 25_000_000))
	e.chain.MineN(2)
	if n := e.cost(e.tick); n != 0 || len(e.get(s.ID).Payments) != 0 {
		t.Fatalf("an expired session is still read by the ticks: %d requests", n)
	}

	// aggregate 1 + agreed head 2 + getLogs 2 + block hash 2
	if n := e.cost(e.late); n != 7 {
		t.Fatalf("late check with a deposit made %d requests, want 7", n)
	}
	e.drain()
	e.chain.MineN(4)
	e.app.Clock.Advance(2 * time.Minute)
	e.tick() // the payment becomes final
	if got := e.get(s.ID); got.Status != "late_payment" || len(got.Payments) != 1 || got.Payments[0].Status != "finalized" {
		t.Fatalf("late payment: %+v", got)
	}

	// Not due again until the next step of the schedule (hourly once 30 min have passed).
	if n := e.cost(e.late); n != 0 {
		t.Fatalf("checked again before the next scheduled check: %d requests", n)
	}
	e.app.Clock.Set(expires.Add(2 * time.Hour))
	if n := e.cost(e.late); n != 1 {
		t.Fatalf("scheduled check of an unchanged balance made %d requests, want 1", n)
	}

	// Run the schedule to its end, then send again: nothing checks the address any more.
	checks := 0
	for at := expires.Add(2 * time.Hour); !at.After(expires.Add(scan.DefaultWatchAfterExpiry)); at = at.Add(30 * time.Minute) {
		e.app.Clock.Set(at)
		if n := e.cost(e.late); n == 1 {
			checks++
		} else if n != 0 {
			t.Fatalf("a scheduled check made %d requests", n)
		}
	}
	if checks != 22 { // 3h..23h hourly (21), then the last at 23h30
		t.Fatalf("%d scheduled checks between 2h and 23h30 after expiry, want 22", checks)
	}
	e.app.Clock.Set(expires.Add(scan.DefaultWatchAfterExpiry + time.Minute))
	e.chain.Mine(transfer(usdc, payer, to, 5_000_000))
	e.chain.MineN(2)
	st, err := scan.FromStore(e.app.Store, "anvil", chain.EVM, 30*time.Second, e.cfgSchedule(), scan.DefaultWatchAfterExpiry)(ctx)
	if err != nil || st.LateDue != nil || st.Recheck {
		t.Fatalf("state after the watch: %+v %v", st, err)
	}
	if n := e.cost(func() { e.late(); e.tick() }); n != 0 || len(e.get(s.ID).Payments) != 1 {
		t.Fatalf("checked after the watch ended: %d requests", n)
	}

	// "Check again" queues one more check, which credits the deposit and clears the request.
	e.exec(`UPDATE sessions SET recheck_requested_at = $2 WHERE id = $1`, s.ID, e.app.Clock.Now())
	if st, _ := scan.FromStore(e.app.Store, "anvil", chain.EVM, 30*time.Second, e.cfgSchedule(), scan.DefaultWatchAfterExpiry)(ctx); !st.Recheck {
		t.Fatalf("recheck not seen: %+v", st)
	}
	e.late()
	e.drain()
	if got := e.get(s.ID); len(got.Payments) != 2 || got.Payments[1].Status == "detected" || got.Payments[1].Amount != "5.000000" {
		t.Fatalf("after check again: %+v", got)
	}
	if e.recheckRequested(s.ID) {
		t.Fatal("recheck request not cleared")
	}
	if n := e.cost(e.late); n != 0 {
		t.Fatalf("checked again after the recheck was served: %d requests", n)
	}
}

// "Check again" rescans even when the balance did not change: a deposit someone else flushed
// right away leaves no balance behind.
func TestCheckAgainRescansWithoutABalanceChange(t *testing.T) {
	e := setup(t, nil)
	s := e.create("25.00", "a")
	to := addr(t, s, "USDC")
	e.tick()
	e.app.Clock.Advance(30 * time.Hour)
	e.expire()
	e.chain.Mine(transfer(usdc, payer, to, 25_000_000))
	b := e.chain.Mine(flush(usdc, to, 25_000_000)...)
	e.chain.Deploy(to, b.Number)
	e.chain.MineN(2)
	e.exec(`UPDATE sessions SET recheck_requested_at = $2 WHERE id = $1`, s.ID, e.app.Clock.Now())
	e.late()
	if got := e.get(s.ID); len(got.Payments) != 1 || e.recheckRequested(s.ID) {
		t.Fatalf("after check again: %+v", got)
	}
}

func (e *env) cfgSchedule() scan.Schedule {
	s, _ := scan.ParseSchedule(scan.DefaultLateSchedule)
	return s
}

// TestEqualLateDepositAfterAFlushFoundByLateCheck sends a late deposit, flushes it, and sends
// the same amount again before the next check: the balance the check sees equals the one it saw
// before the flush, and the second deposit must still be found.
func TestEqualLateDepositAfterAFlushFoundByLateCheck(t *testing.T) {
	e := setup(t, nil)
	ctx := t.Context()
	s := e.create("25.00", "a")
	to := addr(t, s, "USDC")
	e.chain.MineN(2)
	e.tick()
	e.app.Clock.Advance(90 * time.Minute)
	e.expire()
	e.chain.Mine(transfer(usdc, payer, to, 25_000_000))
	e.chain.MineN(2)
	e.late()
	e.drain()
	ses, err := e.app.Store.LoadSession(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	pt := ses.PayTo[0]
	for _, p := range ses.PayTo {
		if p.Asset == "USDC" {
			pt = p
		}
	}
	gross := money.FromUint64(25_000_000)
	rule, err := payout.Compute(gross, pt.Decimals, pt.Fee, payout.Bps(pt.Recipients))
	if err != nil {
		t.Fatal(err)
	}
	e.chain.Mine(transfer(usdc, to, evm3e, 25_000_000)) // the flush
	if _, err := e.app.Store.RecordSettlement(ctx, s.ID, store.Settlement{Chain: "anvil", Asset: "USDC", Gross: gross, Fee: rule.Fee,
		Net: rule.Net, Shares: rule.Shares, TxHash: "0x" + strings.Repeat("f1", 32), SettledAt: e.app.Clock.Now()}, chainsettle.Verify); err != nil {
		t.Fatal(err)
	}
	e.chain.Mine(transfer(usdc, payer, to, 25_000_000))
	e.chain.MineN(2)
	e.app.Clock.Advance(time.Hour)

	e.late()

	e.drain()
	if got := e.get(s.ID); len(got.Payments) != 2 || got.Payments[1].Status == "detected" {
		t.Fatalf("second late deposit: %+v", got)
	}
}

// The bounded rescan asks for ranges that fit the provider's cap, both providers alike.
func TestLateCheckWithinProviderLogRangeCap(t *testing.T) {
	e := setup(t, func(c *detector.Config) { c.MaxRange = 7 })
	e.pa.SetMaxLogRange(7)
	s := e.create("25.00", "a")
	to := addr(t, s, "USDC")
	e.chain.MineN(2)
	e.tick()
	e.app.Clock.Advance(3 * time.Hour)
	e.expire()
	e.chain.MineN(30)
	e.chain.Mine(transfer(usdc, payer, to, 25_000_000))
	e.chain.MineN(2)
	before := len(e.pa.LogRanges())
	e.late()
	if got := e.get(s.ID); len(got.Payments) != 1 {
		t.Fatalf("late check found nothing: %+v", got)
	}
	ra, rb := e.pa.LogRanges()[before:], e.pb.LogRanges()[before:]
	if len(ra) < 5 || len(ra) != len(rb) {
		t.Fatalf("late check asked a %d and b %d ranges", len(ra), len(rb))
	}
	for i := range ra {
		if ra[i] != rb[i] || ra[i][1]-ra[i][0]+1 > 7 {
			t.Fatalf("range %d: a %v, b %v", i, ra[i], rb[i])
		}
	}
}

// An address no tick saw (the session expired before any tick) is rescanned from the first block
// it could have received anything in: the head when its network was revealed, minus the margin.
func TestLateCheckOfAnAddressNeverRead(t *testing.T) {
	e := setup(t, func(c *detector.Config) { c.ReanchorMargin = 2; c.MaxRange = 500 })
	e.chain.MineN(100)
	s := e.create("25.00", "a")
	to := addr(t, s, "USDC")
	e.app.Clock.Advance(2 * time.Hour) // 3600 blocks at 2s: the whole fake chain
	e.expire()
	e.chain.Mine(transfer(usdc, payer, to, 25_000_000))
	e.chain.MineN(2)
	e.late()
	e.drain()
	if got := e.get(s.ID); got.Status != "late_payment" || len(got.Payments) != 1 {
		t.Fatalf("late payment: %+v", got)
	}
}

// The first tick after a reveal registers the address from the head at the reveal (estimated
// from its age) minus the margin, so a transfer sent between the reveal and that tick is found.
func TestFirstRescanCoversEveryBlockSinceTheReveal(t *testing.T) {
	e := setup(t, func(c *detector.Config) { c.ReanchorMargin = 3; c.Policy = chainwatch.CreditPolicy{Confirmations: 1} })
	e.chain.MineN(400) // before the session
	s := e.create("25.00", "a")
	to := addr(t, s, "USDC")
	e.app.Clock.Advance(20 * time.Second) // 10 blocks of 2s
	e.chain.MineN(8)
	e.chain.Mine(transfer(usdc, payer, to, 25_000_000))
	e.chain.Mine()
	tip := e.chain.Tip().Number
	e.tick()
	ranges := e.pa.LogRanges()
	if len(ranges) == 0 || ranges[0][0] != tip-13 {
		t.Fatalf("first range %v, want it to start at %d (tip %d - 10 blocks - margin 3)", ranges, tip-13, tip)
	}
	if got := e.get(s.ID); got.Status != "paid" {
		t.Fatalf("payment made before the first tick: %+v", got)
	}
}

// A tick never asks to run again at once, and a tick with nothing changed reads no header.
func TestScanNeverAsksForMoreAndReadsNoHeader(t *testing.T) {
	e := setup(t, nil)
	ctx := t.Context()
	e.create("25.00", "a")
	e.chain.MineN(12)
	for range 3 {
		more, err := e.det.Scan(ctx, scan.Tick{Mode: scan.Warm})
		if err != nil || more {
			t.Fatalf("scan: more %v, %v", more, err)
		}
	}
	if e.method("eth_getBlockByNumber") != 0 || e.method("eth_call") != 3 {
		t.Fatalf("%d header reads and %d calls for three quiet ticks", e.method("eth_getBlockByNumber"), e.method("eth_call"))
	}
}

// fastHot scans hot every 30 s (the defaults have hot = warm = 1 min) so the tests see both speeds.
var fastHot = scan.Intervals{Hot: 30 * time.Second, Warm: time.Minute, Heartbeat: time.Hour}

// The paced detector reads the chain only when its mode says so: an idle chain makes one
// heartbeat request per provider an hour, a new session wakes it on the next ModeCheck tick,
// a payer on the page makes it hot, a payment waiting for credit keeps it hot, and while it
// waits for finality the ticks read nothing but the one aggregate until the check is due.
func TestPacedChainIdlesAndWakes(t *testing.T) {
	e := setup(t, nil)
	ctx := t.Context()
	sch, _ := scan.ParseSchedule(scan.DefaultLateSchedule)
	p := scan.NewPacer(scan.Config{Chain: "anvil", Intervals: fastHot, Now: e.app.Clock.Now},
		scan.FromStore(e.app.Store, "anvil", chain.EVM, 30*time.Second, sch, scan.DefaultWatchAfterExpiry), e.det)
	tick := func() bool {
		t.Helper()
		n, err := p.Step(ctx)
		if errors.Is(err, worker.ErrSkipped) {
			return false
		}
		if err != nil {
			t.Fatal(err)
		}
		for n > 0 { // the worker drains
			if n, err = p.Step(ctx); err != nil && !errors.Is(err, worker.ErrSkipped) {
				t.Fatal(err)
			}
		}
		return true
	}
	if !tick() || p.Mode() != scan.Idle || e.calls() != 1 {
		t.Fatalf("first tick: mode %s, %d calls (want one latest header from one provider)", p.Mode(), e.calls())
	}
	for range 359 {
		e.app.Clock.Advance(10 * time.Second)
		e.chain.Mine()
		if tick() {
			t.Fatalf("idle chain made requests at %s", e.app.Clock.Now())
		}
	}
	e.app.Clock.Advance(10 * time.Second)
	if !tick() || e.calls() != 2 {
		t.Fatalf("hourly heartbeat: %d calls", e.calls())
	}

	s := e.create("25.00", "a")
	to := addr(t, s, "USDC")
	e.app.Clock.Advance(time.Second)
	if n := e.cost(func() {
		if !tick() || p.Mode() != scan.Warm {
			t.Fatalf("a new session did not wake the chain on the next tick: %s", p.Mode())
		}
	}); n != 1 {
		t.Fatalf("waking tick made %d requests, want the one aggregate", n)
	}
	e.app.Clock.Advance(time.Second)
	if tick() {
		t.Fatal("warm chain scanned again a second later")
	}

	e.exec(`UPDATE sessions SET checkout_seen_at = $2 WHERE id = $1`, s.ID, e.app.Clock.Now())
	e.chain.Mine(transfer(usdc, payer, to, 25_000_000))
	if !tick() || p.Mode() != scan.Hot || len(e.get(s.ID).Payments) != 1 {
		t.Fatalf("payer on the page: mode %s, %+v", p.Mode(), e.get(s.ID))
	}
	// The payer leaves; the payment waiting for credit keeps the chain hot.
	e.app.Clock.Advance(31 * time.Second)
	e.chain.MineN(2)
	if !tick() || p.Mode() != scan.Hot || e.get(s.ID).Status != "paid" {
		t.Fatalf("detected payment: mode %s, %+v", p.Mode(), e.get(s.ID))
	}
	// Paid and waiting for finality: warm until final, then idle.
	headers := e.method("eth_getBlockByNumber")
	for range 10 {
		e.app.Clock.Advance(time.Minute)
		e.chain.Mine()
		tick()
	}
	if e.get(s.ID).Payments[0].Status != "finalized" || p.Mode() != scan.Idle {
		t.Fatalf("after finality: mode %s, %+v", p.Mode(), e.get(s.ID))
	}
	// Two finalized headers (the first one did not cover the block yet), then the block hash
	// from both providers.
	if n := e.method("eth_getBlockByNumber") - headers; n != 4 {
		t.Fatalf("%d header reads while waiting for finality, want 4", n)
	}
}

// Mode selection from the database: each condition gives its mode, payer presence expires
// after PayerActive, and the watch end turns late checks off.
func TestModeFromDatabaseState(t *testing.T) {
	e := setup(t, nil)
	ctx := t.Context()
	sch, _ := scan.ParseSchedule(scan.DefaultLateSchedule)
	state := scan.FromStore(e.app.Store, "anvil", chain.EVM, 30*time.Second, sch, scan.DefaultWatchAfterExpiry)
	mode := func() (scan.Mode, string) {
		t.Helper()
		st, err := state(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return scan.Select(st)
	}
	check := func(what string, want scan.Mode, reason string) {
		t.Helper()
		if m, r := mode(); m != want || r != reason {
			t.Fatalf("%s: %s %s, want %s %s", what, m, r, want, reason)
		}
	}
	check("empty", scan.Idle, "nothing_pending")
	// A session nobody revealed a network of keeps the chain idle (D48).
	if r := e.app.Session(e.app.TestKey, `{"amount":"25.00"}`, "Idempotency-Key", "unrevealed"); r.Status != 200 {
		t.Fatalf("create: %d %s", r.Status, r.Body)
	}
	check("unrevealed session", scan.Idle, "nothing_pending")
	s := e.create("25.00", "a")
	check("open session", scan.Warm, "session_open")
	e.exec(`UPDATE sessions SET checkout_seen_at = $2 WHERE id = $1`, s.ID, e.app.Clock.Now())
	check("payer on the page", scan.Hot, "payer_active")
	e.app.Clock.Advance(31 * time.Second)
	check("payer gone", scan.Warm, "session_open")
	e.app.Clock.Advance(time.Hour)
	e.expire()
	m, _ := mode()
	if m != scan.Late {
		t.Fatalf("expired session in its watch: %s", m)
	}
	st, _ := state(ctx)
	if want := e.app.Clock.Now().Add(-31*time.Second - time.Hour).Add(30*time.Minute + time.Minute); st.LateDue == nil || !st.LateDue.Equal(want) {
		t.Fatalf("first late check due %v, want %s", st.LateDue, want)
	}
	// Never checked: the owed check still runs a while after the watch end, then nothing.
	e.app.Clock.Advance(48 * time.Hour)
	check("past the watch", scan.Idle, "nothing_pending")

	o := e.create("10.00", "b")
	to := addr(t, o, "USDC")
	e.chain.Mine(transfer(usdc, payer, to, 10_000_000))
	e.tick()
	check("payment detected", scan.Hot, "payment_detected")
	e.chain.MineN(2)
	e.tick()
	e.app.Clock.Advance(2 * time.Hour) // the session is paid and expired: only finality is pending
	check("payment confirmed", scan.Warm, "payment_confirmed")
	e.chain.MineN(5)
	e.tick()
	// Paid exactly with its payment final: no late check (D47).
	check("finalized, paid and expired", scan.Idle, "nothing_pending")
	e.exec(`INSERT INTO chain_tx_jobs (id, chain, kind, ref, session_id, asset, address, planned_total, to_address, calldata, sender, status,
		nonce, next_attempt_at, created_at, updated_at) VALUES ('txj_1', 'anvil', 'flush', 'r1', $1, 'USDC', $2, 1, $2, '\x00', 's', 'submitted',
		1, now(), now(), now())`, o.ID, strings.ToLower(to.Hex()))
	check("job submitted", scan.Hot, "job_submitted")
	e.exec(`UPDATE chain_tx_jobs SET status = 'mined' WHERE id = 'txj_1'`)
	check("job mined", scan.Warm, "job_mined")
	e.exec(`UPDATE chain_tx_jobs SET status = 'done' WHERE id = 'txj_1'`)
	e.exec(`UPDATE sessions SET recheck_requested_at = $2 WHERE id = $1`, s.ID, e.app.Clock.Now())
	e.app.Clock.Advance(48 * time.Hour)
	check("check again", scan.Late, "recheck")
}

// The real worker loop runs the pacer every ModeCheck: an idle chain makes no request between
// heartbeats, and a new session is read within about one ModeCheck.
func TestWorkerWakesAnIdleChainWithinModeCheck(t *testing.T) {
	e := setup(t, nil)
	sch, _ := scan.ParseSchedule(scan.DefaultLateSchedule)
	p := scan.NewPacer(scan.Config{Chain: "anvil", Intervals: fastHot, Now: e.app.Clock.Now},
		scan.FromStore(e.app.Store, "anvil", chain.EVM, 30*time.Second, sch, scan.DefaultWatchAfterExpiry), e.det)
	const modeCheck = 50 * time.Millisecond
	var ticks atomic.Int64 // pacer steps that returned
	step := func(ctx context.Context) (int, error) {
		defer ticks.Add(1)
		return p.Step(ctx)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		worker.New(e.app.Svc, e.app.Store, e.app.Dispatcher, worker.Config{SequenceEvery: time.Hour, DispatchEvery: time.Hour, ExpireEvery: time.Hour,
			Loops: []worker.Loop{{Name: "detector:anvil", Every: modeCheck, Step: step}}}, nil).Run(ctx)
		close(done)
	}()
	defer func() { cancel(); <-done }()
	headers := func() int { return e.pa.Calls("eth_getBlockByNumber") }
	testutil.Eventually(t, 5*time.Second, "the startup heartbeat", func() bool { return headers() > 0 })
	if headers() != 1 {
		t.Fatalf("startup heartbeat: %d header reads", headers())
	}
	idleFrom := ticks.Load()
	testutil.Eventually(t, 5*time.Second, "twenty idle ticks", func() bool { return ticks.Load() >= idleFrom+20 })
	if headers() != 1 || e.method("eth_call") != 0 || e.method("eth_getLogs") != 0 {
		t.Fatalf("idle chain made requests: %d header reads, %d calls, %d getLogs", headers(), e.method("eth_call"), e.method("eth_getLogs"))
	}
	e.create("25.00", "a")
	start := time.Now()
	testutil.Eventually(t, 5*time.Second, "the chain to wake for the new session", func() bool { return e.method("eth_call") > 0 })
	if woke := time.Since(start); woke > 10*modeCheck {
		t.Fatalf("woke after %s (ModeCheck %s)", woke, modeCheck)
	}
}

// A session generated on one network and never paid costs one request per tick while open and
// one per late check after (D49), from reveal to the end of its watch: 30 ticks of 1 minute and
// the 53 checks of the default schedule.
func TestNeverPaidSessionRequestBudget(t *testing.T) {
	e := setup(t, nil)
	sch, _ := scan.ParseSchedule(scan.DefaultLateSchedule)
	p := scan.NewPacer(scan.Config{Chain: "anvil", Intervals: scan.DefaultIntervals(), Now: e.app.Clock.Now},
		scan.FromStore(e.app.Store, "anvil", chain.EVM, 30*time.Second, sch, scan.DefaultWatchAfterExpiry), e.det)
	ctx := t.Context()
	if _, err := p.Step(ctx); err != nil { // the startup heartbeat
		t.Fatal(err)
	}
	e.create("25.00", "a")
	before := e.calls()
	end := e.app.Clock.Now().Add(24*time.Hour + 5*time.Minute)
	for e.app.Clock.Now().Before(end) {
		e.app.Clock.Advance(30 * time.Second)
		e.chain.Mine()
		if _, err := e.app.Svc.ExpireDue(ctx, 10); err != nil {
			t.Fatal(err)
		}
		if _, err := p.Step(ctx); err != nil && !errors.Is(err, worker.ErrSkipped) {
			t.Fatal(err)
		}
	}
	// Idle heartbeats (1 per hour after the watch) are not the session's: count them apart.
	got, heartbeats := e.calls()-before, e.method("eth_getBlockByNumber")-1
	t.Logf("never-paid session: %d requests (%d of them idle heartbeats)", got, heartbeats)
	if got-heartbeats != 30+sch.Count() || e.method("eth_getLogs") != 0 {
		t.Fatalf("%d requests besides %d heartbeat reads, want %d (30 ticks + %d late checks)", got-heartbeats, heartbeats, 30+sch.Count(), sch.Count())
	}
}

// evm3e stands for the merchant a flush pays.
var evm3e = evm.Address{0x3e}

// With an expiry hold, a transfer mined just before expires_at is found by the first late check
// through the real path and is on time, even when that check's rescan fails once. The address's
// other asset (unchanged) used to write next_check_at before the rescan, so the failed rescan
// ended the hold and the transfer became a late payment (ADR-0008 H-3).
func TestFailedFirstLateRescanKeepsTheExpiryHold(t *testing.T) {
	e := setupHold(t, 5*time.Minute, func(c *detector.Config) { c.MaxRange = 500 })
	// Provider cool-downs follow the test clock, so b can serve again once it is back.
	e.q = chainwatch.NewQuorum("anvil", []chainwatch.Provider{e.pa, e.pb}, chainwatch.Options{Now: e.app.Clock.Now})
	e.det = e.newDetector()
	ctx := t.Context()
	s := e.create("25.00", "a")
	to := addr(t, s, "USDT") // one address for every asset
	e.chain.MineN(2)
	e.tick()
	e.chain.Mine(transfer(usdt, payer, to, 25_000_000)) // after the last tick, before expiry
	e.chain.MineN(2)
	e.app.Clock.Set(e.expiresAt(s.ID).Add(time.Minute)) // the first late check is due

	e.pb.SetDown(true) // the balance read fails over to a; the rescan needs both
	if _, err := e.det.Late(ctx); err == nil {
		t.Fatal("late check without a quorum succeeded")
	}
	e.expire()
	if got := e.get(s.ID); got.Status != "open" || len(got.Payments) != 0 {
		t.Fatalf("expiry did not wait for the failed late check: %+v", got)
	}
	e.pb.SetDown(false)
	e.app.Clock.Advance(time.Minute) // past b's cool-down, inside the hold
	e.late()
	e.drain()
	e.expire()
	if got := e.get(s.ID); got.Status != "paid" || len(got.Payments) != 1 {
		t.Fatalf("transfer mined before expiry: %+v", got)
	}
}

// A first late check whose balance read is stale (both providers behind the stored balance) says
// nothing about the last minute before expiry: the session stays held, the pass reports it, and
// the next check finds the deposit on time (ADR-0008 H-3).
func TestStaleFirstLateCheckKeepsTheExpiryHold(t *testing.T) {
	e := setupHold(t, 5*time.Minute, func(c *detector.Config) { c.MaxRange = 500 })
	ctx := t.Context()
	s := e.create("25.00", "a")
	to := addr(t, s, "USDC")
	e.chain.Mine(transfer(usdc, payer, to, 10_000_000))
	e.chain.MineN(2)
	stored := e.chain.Tip().Number // the block the next tick reads (and stores) the balance at
	e.tick()
	e.tick()
	if got := e.get(s.ID); got.Status != "partially_paid" {
		t.Fatalf("first deposit: %+v", got)
	}
	e.chain.Mine(transfer(usdc, payer, to, 15_000_000)) // after the last tick, before expiry
	e.chain.MineN(2)
	e.app.Clock.Set(e.expiresAt(s.ID).Add(time.Minute))

	behind := e.chain.Tip().Number - (stored - 1)
	e.pa.SetLag(behind)
	e.pb.SetLag(behind)
	_, err := e.det.Late(ctx)
	e.expire()
	if got := e.get(s.ID); got.Status != "partially_paid" || len(got.Payments) != 1 {
		t.Fatalf("expiry did not wait for a stale late check: %+v", got)
	}
	if err == nil {
		t.Fatal("a stale first late check was not reported")
	}
	e.pa.SetLag(0)
	e.pb.SetLag(0)
	e.late()
	e.drain()
	e.expire()
	if got := e.get(s.ID); got.Status != "paid" || len(got.Payments) != 2 {
		t.Fatalf("deposit mined before expiry: %+v", got)
	}
}

// A full late batch never cuts an address's entries in two: its assets share one watch row, so a
// rescan for the first asset alone moved the address past the second asset's transfer for good.
func TestLateBatchKeepsAnAddressWhole(t *testing.T) {
	e := setup(t, func(c *detector.Config) { c.MaxRange = 500; c.LateBatch = 3 })
	x := e.create("25.00", "x")
	y := e.create("25.00", "y")
	e.chain.MineN(2)
	e.tick()
	e.app.Clock.Advance(time.Hour)
	e.expire()
	for _, s := range []sessionView{x, y} {
		to := addr(t, s, "USDC")
		e.chain.Mine(transfer(usdc, payer, to, 10_000_000), transfer(usdt, payer, to, 15_000_000))
	}
	e.chain.MineN(2)
	for range 3 { // two batches (one address each), then nothing due
		e.late()
	}
	e.app.Clock.Advance(5 * time.Minute)
	e.late()
	e.drain()
	for _, s := range []sessionView{x, y} {
		if got := e.get(s.ID); len(got.Payments) != 2 || got.Received != "25.00" {
			t.Fatalf("session %s: %+v", s.ID, got)
		}
	}
}
