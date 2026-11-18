// Package chainsched runs every chain's paced detector and gas-wallet sender from one worker
// loop. Each ModeCheck it reads the state of every chain in one database query (the pacers'
// inputs and whether each sender has work), asks each pacer whether anything is due (no I/O) and
// starts only the chains and senders with something to do. A chain with nothing pending costs
// neither a query of its own nor a request: the database load of the idle loop does not grow
// with the number of chains, and the only requests are the pacers' heartbeats.
//
// The state read also drives the worker's idle/active decision (plan B): a chain with anything
// pending (a payer, an open session, a payment or job in flight, a "Check again") marks the
// process active, and while it is idle the loop wakes for the earliest late check or heartbeat a
// pacer has scheduled (Pacer.Next), so the idle mode check never delays the RPC schedule.
//
// Each started step runs in its own goroutine, so a slow provider of one chain never delays
// another; a chain's detector (or sender) never runs twice at once. The steps keep their
// advisory locks (single writer per chain across replicas): a step whose lock another process
// holds reports ErrNotHeld and is not tried again before LockRetry. Failures are tracked per
// "detector:<chain>" and "sender:<chain>", with the worker's loop_failing and loop_recovered
// alerts.
package chainsched

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/prismaport/prismaport/internal/alert"
	"github.com/prismaport/prismaport/internal/scan"
	"github.com/prismaport/prismaport/internal/store"
	"github.com/prismaport/prismaport/internal/worker"
)

// ErrNotHeld is returned by a step whose advisory lock another process holds: that process runs
// the chain.
var ErrNotHeld = errors.New("chainsched: lock held by another process")

// DefaultLockRetry is how long a step whose lock another process holds waits before asking for
// it again.
const DefaultLockRetry = 10 * time.Second

// Chain is one chain's paced work.
type Chain struct {
	ID string
	// Due reports whether the chain's pacer has anything to do with this state (scan.Pacer.Due).
	Due func(st scan.State, now time.Time) bool
	// Next is when the chain's pacer has RPC work scheduled (scan.Pacer.Next; nil: unknown).
	Next func(st scan.State, now time.Time) time.Time
	// Detector runs the pacer with the state read (scan.Pacer.StepState under the chain's lock).
	Detector func(ctx context.Context, st scan.State) (int, error)
	// Sender runs one gas-wallet pass (plan and step under its lock); nil when the chain sends
	// nothing. It runs only while the state shows sender work, at most every SenderEvery unless
	// its last pass changed something.
	Sender      func(ctx context.Context) (int, error)
	SenderEvery time.Duration
}

// Config configures a Scheduler.
type Config struct {
	Chains []Chain
	// State reads every chain's state in one query (store.Store.ChainStates).
	State func(ctx context.Context) (map[string]store.ChainScanState, error)
	// Alerts receives loop_failing when every step of a chain's detector or sender has failed
	// for FailingAfter (default worker.DefaultFailingAfter), and loop_recovered after.
	Alerts       alert.Sink
	FailingAfter time.Duration
	LockRetry    time.Duration
	// StepTimeout bounds each detector and sender step (default worker.DefaultStepTimeout).
	StepTimeout time.Duration
	// Activity is the worker process's idle/active state; a chain with anything pending marks it.
	Activity *worker.Activity
	Now      func() time.Time
	Log      *slog.Logger
}

// task is one chain's detector or sender.
type task struct {
	name      string
	busy      bool
	lockAfter time.Time // not before: the lock was held elsewhere
	last      time.Time // the last sender pass started
	again     bool      // the last pass changed something: run at the next tick
	failed    bool      // the last step failed or did nothing (unverified, lock elsewhere)
	failing   alert.Sustained
}

type entry struct {
	c        Chain
	det, snd *task
}

// Scheduler is the chains' single worker loop (Step every ModeCheck).
type Scheduler struct {
	cfg    Config
	chains []entry
	mu     sync.Mutex
	wg     sync.WaitGroup
	next   time.Time // the earliest scheduled work the last Step saw (zero: none)
}

// New returns a scheduler, filling unset config fields with their defaults.
func New(cfg Config) *Scheduler {
	if cfg.FailingAfter == 0 {
		cfg.FailingAfter = worker.DefaultFailingAfter
	}
	if cfg.LockRetry == 0 {
		cfg.LockRetry = DefaultLockRetry
	}
	if cfg.StepTimeout == 0 {
		cfg.StepTimeout = worker.DefaultStepTimeout
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if cfg.Alerts == nil {
		cfg.Alerts = alert.Log{Logger: cfg.Log}
	}
	s := &Scheduler{cfg: cfg}
	for _, c := range cfg.Chains {
		e := entry{c: c, det: &task{name: "detector:" + c.ID, failing: alert.Sustained{For: cfg.FailingAfter}}}
		if c.Sender != nil {
			e.snd = &task{name: "sender:" + c.ID, failing: alert.Sustained{For: cfg.FailingAfter}}
		}
		s.chains = append(s.chains, e)
	}
	return s
}

// Loop is the worker loop that runs the scheduler every idle mode check, every active one while
// the process is active, and no later than the earliest scheduled work (NextDue).
func (s *Scheduler) Loop(idle, active time.Duration) worker.Loop {
	// Background: Step starts the chains' steps and returns; each runs under its own StepTimeout.
	return worker.Loop{Name: "chains", Every: idle, Active: active, Next: s.NextDue, Step: s.Step, Wait: s.Wait, Background: true}
}

// NextDue is the earliest scheduled work the last Step saw (zero: none): a pacer's next late
// check, scan or heartbeat, or now for a chain whose step was running (its next is read again
// at the following tick).
func (s *Scheduler) NextDue() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.next
}

// pending reports whether a chain's state needs the worker active: a payer, an open session, a
// payment or job in flight, sender work or a "Check again". Late checks alone do not (NextDue
// schedules them).
func pending(cs store.ChainScanState) bool {
	return cs.PayerActive || cs.Open || cs.Recheck || cs.Detected || cs.Confirmed || cs.Submitted || cs.Mined || cs.SenderDue
}

// Step reads every chain's state in one query and starts the detectors and senders that have
// something due. It returns worker.ErrSkipped when it started nothing, and the query's error.
func (s *Scheduler) Step(ctx context.Context) (int, error) {
	states, err := s.cfg.State(ctx)
	if err != nil {
		s.mu.Lock()
		s.next = time.Time{} // retry at the loop's own interval, not at a stale schedule
		s.mu.Unlock()
		return 0, err
	}
	now := s.cfg.Now()
	started := 0
	busy := false
	var next time.Time
	soonest := func(t time.Time) {
		if !t.IsZero() && (next.IsZero() || t.Before(next)) {
			next = t
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.chains {
		cs := states[e.c.ID]
		st := scan.FromChainState(cs)
		busy = busy || pending(cs)
		if s.ready(e.det, now) && e.c.Due(st, now) {
			s.start(ctx, e.det, func(ctx context.Context) (int, error) { return e.c.Detector(ctx, st) })
			started++
		}
		switch {
		case e.det.busy:
			// Look again soon for what the running step schedules next, unless its previous step
			// failed or did nothing (provider not verified, lock held by another worker): its
			// pacer's mode stays unknown and would say "now" at every tick, so it is retried at the
			// loop's own interval.
			if !e.det.failed {
				soonest(now)
			}
		case e.c.Next != nil:
			if t := e.c.Next(st, now); t.After(now) {
				soonest(maxTime(t, e.det.lockAfter))
			}
		}
		if e.snd != nil && cs.SenderDue && s.ready(e.snd, now) && (e.snd.again || !now.Before(e.snd.last.Add(e.c.SenderEvery))) {
			e.snd.last = now
			s.start(ctx, e.snd, e.c.Sender)
			started++
		}
	}
	s.next = next
	if busy && s.cfg.Activity != nil {
		s.cfg.Activity.Mark("chains")
	}
	if started == 0 {
		return 0, worker.ErrSkipped
	}
	return 0, nil
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// ready reports whether t may start now (s.mu held).
func (s *Scheduler) ready(t *task, now time.Time) bool {
	return !t.busy && !now.Before(t.lockAfter)
}

// start runs step in the background as t (s.mu held).
func (s *Scheduler) start(ctx context.Context, t *task, step func(context.Context) (int, error)) {
	t.busy = true
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		// A panic costs only this step: it is logged, alerted and counted as a failure. A step stuck
		// past StepTimeout fails with the deadline instead of keeping the chain busy forever.
		n, err := worker.StepWithin(ctx, s.cfg.StepTimeout, s.cfg.Alerts, s.cfg.Log, t.name, step)
		now := s.cfg.Now()
		held := !errors.Is(err, ErrNotHeld)
		if held && !errors.Is(err, worker.ErrSkipped) && (err == nil || ctx.Err() == nil) {
			if err != nil {
				s.cfg.Log.Error("chain step failed", "loop", t.name, "err", err)
			}
			// t is still busy: no other goroutine touches its streak.
			worker.Watch(ctx, s.cfg.Alerts, t.name, &t.failing, err, now)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		t.busy, t.again, t.failed = false, n > 0 && err == nil, err != nil
		if !held {
			t.lockAfter = now.Add(s.cfg.LockRetry)
		}
	}()
}

// Wait waits for the steps started in the background to return.
func (s *Scheduler) Wait() { s.wg.Wait() }
