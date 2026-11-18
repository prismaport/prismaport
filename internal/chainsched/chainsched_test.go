package chainsched

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prismaport/prismaport/internal/alert"
	"github.com/prismaport/prismaport/internal/scan"
	"github.com/prismaport/prismaport/internal/store"
	"github.com/prismaport/prismaport/internal/worker"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// scanner counts a fake chain's RPC work.
type scanner struct{ scans, lates, beats atomic.Int64 }

func (s *scanner) Scan(context.Context, scan.Tick) (bool, error) { s.scans.Add(1); return false, nil }
func (s *scanner) Late(context.Context) (bool, error)            { s.lates.Add(1); return false, nil }
func (s *scanner) Heartbeat(context.Context) error               { s.beats.Add(1); return nil }
func (s *scanner) Rest(context.Context) error                    { return nil }

func (s *scanner) rpc() int64 { return s.scans.Load() + s.lates.Load() + s.beats.Load() }

// fleet is n fake chains with real pacers: guards counts the chain steps that ran (each takes the
// chain's lock, a database round trip), queries the state reads.
type fleet struct {
	clock   *clock
	sched   *Scheduler
	scs     []*scanner
	guards  atomic.Int64
	sends   atomic.Int64
	queries atomic.Int64
	state   map[string]store.ChainScanState
}

func newFleet(n int, quiet time.Duration) *fleet {
	f := &fleet{clock: &clock{t: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)}, state: map[string]store.ChainScanState{}}
	var chains []Chain
	for i := range n {
		sc := &scanner{}
		f.scs = append(f.scs, sc)
		p := scan.NewPacer(scan.Config{Chain: fmt.Sprint("c", i), Intervals: scan.DefaultIntervals(), QuietAfter: quiet, Now: f.clock.now},
			nil, sc)
		chains = append(chains, Chain{ID: fmt.Sprint("c", i), Due: p.Due, Next: p.Next, SenderEvery: 2 * time.Second,
			Detector: func(ctx context.Context, st scan.State) (int, error) {
				f.guards.Add(1)
				return p.StepState(ctx, st)
			},
			Sender: func(context.Context) (int, error) { f.sends.Add(1); return 0, nil }})
	}
	f.sched = New(Config{Chains: chains, Now: f.clock.now, State: func(context.Context) (map[string]store.ChainScanState, error) {
		f.queries.Add(1)
		return f.state, nil
	}})
	return f
}

// tick runs one ModeCheck and waits for what it started.
func (f *fleet) tick(t *testing.T) {
	t.Helper()
	if _, err := f.sched.Step(t.Context()); err != nil && !errors.Is(err, worker.ErrSkipped) {
		t.Fatal(err)
	}
	f.sched.Wait()
	f.clock.advance(time.Second)
}

func (f *fleet) rpc() (n int64) {
	for _, s := range f.scs {
		n += s.rpc()
	}
	return n
}

// With nothing pending, the scheduler reads the state once per tick whatever the number of
// chains, and no chain takes its lock (a database round trip) or makes a request after the
// first tick, which sets each pacer's mode. Quiet chains (no reveal for a day) make no heartbeat.
func TestIdleCostDoesNotGrowWithTheNumberOfChains(t *testing.T) {
	for _, n := range []int{1, 10, 200} {
		f := newFleet(n, 24*time.Hour)
		f.tick(t)
		if f.guards.Load() != int64(n) || f.rpc() != 0 {
			t.Fatalf("%d chains, first tick: %d steps, %d requests", n, f.guards.Load(), f.rpc())
		}
		for range 3 * 3600 {
			f.tick(t)
		}
		if q := f.queries.Load(); q != 3*3600+1 {
			t.Fatalf("%d chains: %d state queries in %d ticks", n, q, 3*3600+1)
		}
		if f.guards.Load() != int64(n) || f.rpc() != 0 || f.sends.Load() != 0 {
			t.Fatalf("%d idle chains over 3 h: %d steps, %d requests, %d sender passes", n, f.guards.Load()-int64(n), f.rpc(), f.sends.Load())
		}
	}
}

// Without the quiet rule (or on a chain revealed on within the day) the only requests of an idle
// chain are its heartbeats: one an hour, from one provider.
func TestIdleChainsOnlyHeartbeat(t *testing.T) {
	f := newFleet(20, 0)
	for range 3600 {
		f.tick(t)
	}
	f.tick(t)
	for i, sc := range f.scs {
		if sc.beats.Load() != 2 || sc.scans.Load() != 0 || sc.lates.Load() != 0 {
			t.Fatalf("chain %d over an hour: %d beats, %d scans, %d late passes", i, sc.beats.Load(), sc.scans.Load(), sc.lates.Load())
		}
	}
	if f.guards.Load() != 40 {
		t.Fatalf("%d chain steps, want the 40 heartbeats", f.guards.Load())
	}
}

// Only the chains with something pending run: an open session scans its chain every warm
// interval, a sender with work runs every SenderEvery, and the others stay untouched.
func TestOnlyChainsWithSomethingDueRun(t *testing.T) {
	f := newFleet(5, 24*time.Hour)
	f.tick(t)
	f.state["c2"] = store.ChainScanState{Open: true}
	f.state["c4"] = store.ChainScanState{SenderDue: true}
	for range 120 {
		f.tick(t)
	}
	for i, sc := range f.scs {
		want := int64(0)
		if i == 2 {
			want = 2 // woken at once, then one warm interval (1 min) later
		}
		if sc.scans.Load() != want || sc.beats.Load() != 0 {
			t.Fatalf("chain c%d: %d scans, %d beats", i, sc.scans.Load(), sc.beats.Load())
		}
	}
	if s := f.sends.Load(); s != 60 {
		t.Fatalf("%d sender passes in 2 min, want one every 2 s", s)
	}
}

// A step whose lock another process holds is not asked again before LockRetry and is not a
// failure; a failing chain raises loop_failing for itself after FailingAfter, and
// loop_recovered once it succeeds.
func TestLockHeldElsewhereAndPerChainFailures(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)}
	rec := &alert.Recorder{}
	var heldTries, failTries atomic.Int64
	var failing atomic.Bool
	failing.Store(true)
	always := func(scan.State, time.Time) bool { return true }
	s := New(Config{Now: c.now, Alerts: rec, FailingAfter: time.Minute, LockRetry: 10 * time.Second,
		State: func(context.Context) (map[string]store.ChainScanState, error) { return nil, nil },
		Chains: []Chain{
			{ID: "held", Due: always, Detector: func(context.Context, scan.State) (int, error) { heldTries.Add(1); return 0, ErrNotHeld }},
			{ID: "bad", Due: always, Detector: func(context.Context, scan.State) (int, error) {
				failTries.Add(1)
				if failing.Load() {
					return 0, errors.New("provider down")
				}
				return 0, nil
			}},
		}})
	tick := func() {
		_, _ = s.Step(t.Context())
		s.Wait()
		c.advance(time.Second)
	}
	for range 61 {
		tick()
	}
	if heldTries.Load() != 7 { // at 0, 10, ..., 60 s
		t.Fatalf("lock asked %d times in 61 s", heldTries.Load())
	}
	raised := rec.All()
	if len(raised) != 1 || raised[0].Kind != "loop_failing" || raised[0].Chain != "bad" || raised[0].Fields["loop"] != "detector:bad" {
		t.Fatalf("alerts %+v", raised)
	}
	failing.Store(false)
	tick()
	if all := rec.All(); len(all) != 2 || all[1].Kind != "loop_recovered" || all[1].Chain != "bad" {
		t.Fatalf("alerts after recovery %+v", all)
	}
}

// A slow chain does not hold the others back, and is never started twice at once.
func TestASlowChainDoesNotBlockTheOthers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := &clock{t: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)}
		release := make(chan struct{})
		var slow, fast atomic.Int64
		always := func(scan.State, time.Time) bool { return true }
		s := New(Config{Now: c.now, State: func(context.Context) (map[string]store.ChainScanState, error) { return nil, nil },
			Chains: []Chain{
				{ID: "slow", Due: always, Detector: func(context.Context, scan.State) (int, error) { slow.Add(1); <-release; return 0, nil }},
				{ID: "fast", Due: always, Detector: func(context.Context, scan.State) (int, error) { fast.Add(1); return 0, nil }},
			}})
		for range 5 {
			_, _ = s.Step(t.Context())
			synctest.Wait() // the fast chain's step returns; the slow one blocks on release
			c.advance(time.Second)
		}
		close(release)
		s.Wait()
		if slow.Load() != 1 || fast.Load() != 5 {
			t.Fatalf("slow ran %d times, fast %d", slow.Load(), fast.Load())
		}
	})
}

// A detector that panics costs only its own step: the process goes on, the panic is alerted,
// it counts as a failed step (loop_failing after FailingAfter) and the other chains keep running.
func TestAPanickingDetectorIsRecoveredAndAlerted(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)}
	rec := &alert.Recorder{}
	var healthy atomic.Int64
	always := func(scan.State, time.Time) bool { return true }
	s := New(Config{Now: c.now, Alerts: rec, FailingAfter: 2 * time.Second,
		State: func(context.Context) (map[string]store.ChainScanState, error) { return nil, nil },
		Chains: []Chain{
			{ID: "bad", Due: always, Detector: func(context.Context, scan.State) (int, error) { panic("bad row") }},
			{ID: "good", Due: always, Detector: func(context.Context, scan.State) (int, error) { healthy.Add(1); return 0, nil }},
		}})
	for range 3 {
		_, _ = s.Step(t.Context())
		s.Wait()
		c.advance(time.Second)
	}
	if healthy.Load() != 3 {
		t.Fatalf("the healthy chain ran %d times, want 3", healthy.Load())
	}
	var panicked, failing int
	for _, a := range rec.All() {
		switch {
		case a.Kind == "step_panicked" && a.Chain == "bad" && a.Fields["loop"] == "detector:bad" && a.Fields["panic"] == "bad row":
			panicked++
		case a.Kind == "loop_failing" && a.Chain == "bad":
			failing++
		default:
			t.Fatalf("unexpected alert %+v", a)
		}
	}
	if panicked != 3 || failing != 1 {
		t.Fatalf("step_panicked %d, loop_failing %d in %v", panicked, failing, rec.Kinds())
	}
}

// A detector stuck on a request that never returns fails at StepTimeout and the chain runs again,
// instead of staying busy forever (and never reaching loop_failing).
func TestAHungStepIsCutAtItsTimeout(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)}
	var runs atomic.Int64
	var cut atomic.Bool
	always := func(scan.State, time.Time) bool { return true }
	s := New(Config{Now: c.now, StepTimeout: 50 * time.Millisecond,
		State: func(context.Context) (map[string]store.ChainScanState, error) { return nil, nil },
		Chains: []Chain{{ID: "hung", Due: always, Detector: func(ctx context.Context, _ scan.State) (int, error) {
			runs.Add(1)
			<-ctx.Done()
			cut.Store(errors.Is(ctx.Err(), context.DeadlineExceeded))
			return 0, ctx.Err()
		}}}})
	_, _ = s.Step(t.Context())
	s.Wait()
	c.advance(time.Second)
	_, _ = s.Step(t.Context())
	s.Wait()
	if runs.Load() != 2 || !cut.Load() {
		t.Fatalf("runs %d, cut by its deadline %v", runs.Load(), cut.Load())
	}
}

// The state the scheduler reads is the worker's activity signal (plan B): a chain with anything
// pending marks the process active; late checks alone do not, but NextDue holds the earliest
// scheduled RPC work so an idle loop wakes for it.
func TestPendingWorkMarksTheProcessActiveAndNextDueSchedulesTheRest(t *testing.T) {
	f := newFleet(1, 0)
	act := worker.NewActivity(10*time.Minute, f.clock.now, nil)
	f.sched.cfg.Activity = act
	f.tick(t) // the first tick sets the mode and beats; the detector was running at Step time
	start := f.clock.now().Add(-time.Second)
	if got := f.sched.NextDue(); !got.Equal(start) {
		t.Fatalf("next due %s while the detector ran, want %s (look again)", got, start)
	}
	f.tick(t)
	if got, want := f.sched.NextDue(), start.Add(time.Hour); !got.Equal(want) {
		t.Fatalf("idle chain: next due %s, want the heartbeat at %s", got, want)
	}
	late := f.clock.now().Add(20 * time.Minute)
	f.state["c0"] = store.ChainScanState{LateDue: &late}
	f.tick(t) // the mode changes to late: the pacer runs (no late check yet)
	f.tick(t)
	if got := f.sched.NextDue(); !got.Equal(late) {
		t.Fatalf("late checks: next due %s, want %s", got, late)
	}
	if act.Active() {
		t.Fatal("late checks alone turned the process active")
	}
	for _, cs := range []store.ChainScanState{{Open: true}, {PayerActive: true}, {Detected: true}, {Confirmed: true}, {Submitted: true},
		{Mined: true}, {SenderDue: true}, {Recheck: true}} {
		act = worker.NewActivity(10*time.Minute, f.clock.now, nil)
		f.sched.cfg.Activity = act
		f.state["c0"] = cs
		f.tick(t)
		if !act.Active() {
			t.Fatalf("%+v did not turn the process active", cs)
		}
	}
}

// A chain whose steps keep failing (an unverified provider, a lock held elsewhere) never pulls
// the idle loop forward: its pacer's mode stays unknown, so its Next would say "now" every tick.
func TestAFailingChainDoesNotSpeedUpTheIdleLoop(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	s := New(Config{Now: func() time.Time { return now },
		Chains: []Chain{{ID: "c", Due: func(scan.State, time.Time) bool { return true },
			Next:     func(_ scan.State, at time.Time) time.Time { return at },
			Detector: func(context.Context, scan.State) (int, error) { return 0, errors.New("rpc: chain id mismatch") }}},
		State: func(context.Context) (map[string]store.ChainScanState, error) {
			return map[string]store.ChainScanState{}, nil
		}})
	for i := range 3 {
		if _, err := s.Step(t.Context()); err != nil {
			t.Fatal(err)
		}
		s.Wait()
		if got := s.NextDue(); i > 0 && !got.IsZero() {
			t.Fatalf("tick %d: next due %s after failed steps, want none", i, got)
		}
	}
	s.cfg.State = func(context.Context) (map[string]store.ChainScanState, error) { return nil, errors.New("db down") }
	s.next = now
	if _, err := s.Step(t.Context()); err == nil || !s.NextDue().IsZero() {
		t.Fatalf("a failed state read kept next due %s (err %v)", s.NextDue(), err)
	}
}
