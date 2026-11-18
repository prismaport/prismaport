// Package worker runs the background loops: the event sequencer, the webhook dispatcher, the
// emails about failing webhook endpoints, the expiry sweeper, the sandbox inbox retention, the end of rolled API keys' overlaps, the removal of two-factor authentication after its wait and, per
// chain, the paced detector (scan modes and late checks) and the gas wallet.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/prismaport/prismaport/internal/alert"
	"github.com/prismaport/prismaport/internal/safe"
	"github.com/prismaport/prismaport/internal/service"
	"github.com/prismaport/prismaport/internal/store"
	"github.com/prismaport/prismaport/internal/tuning"
	"github.com/prismaport/prismaport/internal/webhook"
)

// Role picks which loops a worker process runs. The two parts coordinate through the database
// only, so they can be deployed apart (D53).
type Role string

const (
	// RoleAll runs every loop in one process.
	RoleAll Role = "all"
	// RoleChain runs only the chain layer (Config.Loops): payment detection and the gas wallets.
	RoleChain Role = "chain"
	// RoleEvents runs everything else: the sequencer, what calls merchant URLs and the sweepers.
	RoleEvents Role = "events"
)

// Config holds the worker's intervals and batch sizes; zero values get defaults in New. Every
// loop has an idle interval (...Every) and an active one (...Active, used while Activity is
// active, never slower than the idle one); see Activity. A loop whose active interval is not
// shorter than its idle one (the housekeeping loops by default) is not adaptive.
type Config struct {
	SequenceEvery, SequenceActive time.Duration
	DispatchEvery, DispatchActive time.Duration
	ExpireEvery, ExpireActive     time.Duration
	// NoticeEvery and NoticeActive pace the emails about failing webhook endpoints.
	NoticeEvery, NoticeActive time.Duration
	// TOTPRemoval* pace the sweep of pending removals of two-factor authentication, KeyExpiry*
	// the end of rolled API keys' overlaps, Sandbox* the sandbox inbox retention and Idempotency*
	// the idempotency key retention. Their active intervals default to the idle ones.
	TOTPRemovalEvery, TOTPRemovalActive time.Duration
	KeyExpiryEvery, KeyExpiryActive     time.Duration
	SandboxEvery, SandboxActive         time.Duration
	IdempotencyEvery, IdempotencyActive time.Duration
	// IdempotencyTTL is how long a stored idempotent response is replayed (default
	// DefaultIdempotencyTTL); older keys are deleted every IdempotencyEvery, IdempotencyBatch at a
	// time.
	IdempotencyTTL   time.Duration
	IdempotencyBatch int
	// Loops are extra loops (the chain layer) run with the same drain-then-tick behavior.
	Loops []Loop
	// Role picks the loops this process runs (default RoleAll).
	Role Role
	// Alerts receives loop_failing when every step of a loop has failed for FailingAfter
	// (default 5 minutes), and loop_recovered when one succeeds again.
	Alerts       alert.Sink
	FailingAfter time.Duration
	// StepTimeout bounds one step of a loop (default DefaultStepTimeout): a step stuck on a hung
	// request fails with the deadline instead of stalling its loop, so loop_failing can fire.
	StepTimeout time.Duration
	// Activity is the process's idle/active state, shared with the chain scheduler; nil gets one
	// that turns idle IdleAfter (default DefaultIdleAfter) after the last sign of work.
	Activity  *Activity
	IdleAfter time.Duration
	// Listen, when set, runs for the life of Run and calls wake with the payload of every
	// prismaport_wake notification (store.ListenWake). A wake for a table this Role watches
	// (WakeTopics) turns the process active and ticks every adaptive loop at once.
	Listen func(ctx context.Context, wake func(topic string))
	Now    func() time.Time
}

// ErrSkipped is returned by a step that had nothing due and made no request: it neither ends
// nor extends a failing streak, and is not logged.
var ErrSkipped = errors.New("worker: step skipped")

// Loop is a named step run every Every; a step returning n > 0 runs again at once. Wait, when
// set, is called once the loop stopped, to wait for work its steps started in the background.
type Loop struct {
	Name  string
	Every time.Duration
	// Active, when shorter than Every, makes the loop adaptive: it ticks every Active while the
	// process is active (Every stays the idle interval), a step that found work (n > 0) marks the
	// process active, and a wake ticks it at once. Otherwise it ticks every Every in both modes.
	Active time.Duration
	// Next, when set, is when the loop has something scheduled (zero: nothing); an idle loop
	// wakes for it instead of waiting out Every (the chain scheduler's late checks and heartbeats).
	Next func() time.Time
	Step func(context.Context) (int, error)
	Wait func()
	// Background marks a step that starts its work in the background and returns at once: the
	// work keeps the step's context (the process's) and bounds itself, as a step deadline would
	// cancel it as soon as the step returned.
	Background bool
}

// DefaultStepTimeout bounds one loop step (tuning.DefaultStepTimeout).
const DefaultStepTimeout = tuning.DefaultStepTimeout

// DefaultFailingAfter is how long every step of a loop must fail before loop_failing.
const DefaultFailingAfter = 5 * time.Minute

// DefaultIdempotencyTTL is how long an idempotency key's stored response is kept.
const DefaultIdempotencyTTL = tuning.DefaultIdempotencyTTL

// The loops' default intervals, idle and active (plan B, owner-approved 2026-11-17): an idle
// deployment makes a few statements a minute, an active one reacts within seconds. The sequencer
// polls every 10 s while active (2026-11-17): every new event wakes it at once (pg_notify), so its
// poll is only the safety net. The housekeeping loops keep one interval in both modes.
const (
	DefaultSequenceEvery    = 2 * time.Minute
	DefaultSequenceActive   = 10 * time.Second
	DefaultDispatchEvery    = 2 * time.Minute
	DefaultDispatchActive   = 5 * time.Second
	DefaultExpireEvery      = 2 * time.Minute
	DefaultExpireActive     = 30 * time.Second
	DefaultNoticeEvery      = 5 * time.Minute
	DefaultNoticeActive     = time.Minute
	DefaultTOTPRemovalEvery = 5 * time.Minute
	DefaultKeyExpiryEvery   = 5 * time.Minute
	DefaultSandboxEvery     = time.Hour
	DefaultIdempotencyEvery = time.Hour
)

// The batch sizes.
const (
	sequenceBatch = 500
	expireBatch   = 200
)

func orDefault(d *time.Duration, def time.Duration) {
	if *d == 0 {
		*d = def
	}
}

func (c *Config) defaults() {
	orDefault(&c.SequenceEvery, DefaultSequenceEvery)
	orDefault(&c.SequenceActive, DefaultSequenceActive)
	orDefault(&c.DispatchEvery, DefaultDispatchEvery)
	orDefault(&c.DispatchActive, DefaultDispatchActive)
	orDefault(&c.ExpireEvery, DefaultExpireEvery)
	orDefault(&c.ExpireActive, DefaultExpireActive)
	orDefault(&c.NoticeEvery, DefaultNoticeEvery)
	orDefault(&c.NoticeActive, DefaultNoticeActive)
	orDefault(&c.TOTPRemovalEvery, DefaultTOTPRemovalEvery)
	orDefault(&c.TOTPRemovalActive, c.TOTPRemovalEvery)
	orDefault(&c.KeyExpiryEvery, DefaultKeyExpiryEvery)
	orDefault(&c.KeyExpiryActive, c.KeyExpiryEvery)
	orDefault(&c.SandboxEvery, DefaultSandboxEvery)
	orDefault(&c.SandboxActive, c.SandboxEvery)
	orDefault(&c.IdempotencyEvery, DefaultIdempotencyEvery)
	orDefault(&c.IdempotencyActive, c.IdempotencyEvery)
	if c.FailingAfter == 0 {
		c.FailingAfter = DefaultFailingAfter
	}
	if c.StepTimeout == 0 {
		c.StepTimeout = DefaultStepTimeout
	}
	if c.IdempotencyTTL == 0 {
		c.IdempotencyTTL = DefaultIdempotencyTTL
	}
	if c.IdempotencyBatch == 0 {
		c.IdempotencyBatch = 1000
	}
	if c.Role == "" {
		c.Role = RoleAll
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Activity == nil {
		c.Activity = NewActivity(c.IdleAfter, c.Now, nil)
	}
}

// Worker runs the periodic jobs that are not tied to one chain, webhook delivery among them.
type Worker struct {
	svc        *service.Service
	st         *store.Store
	dispatcher *webhook.Dispatcher
	cfg        Config
	log        *slog.Logger
}

// New returns a worker, filling unset config fields with their defaults.
func New(svc *service.Service, st *store.Store, d *webhook.Dispatcher, cfg Config, log *slog.Logger) *Worker {
	cfg.defaults()
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if cfg.Alerts == nil {
		cfg.Alerts = alert.Log{Logger: log}
	}
	return &Worker{svc: svc, st: st, dispatcher: d, cfg: cfg, log: log}
}

// Run blocks until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) {
	var wg sync.WaitGroup
	loops := w.loops()
	for _, l := range loops {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.run(ctx, l)
		}()
	}
	if w.cfg.Listen != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.cfg.Listen(ctx, w.wake)
		}()
	}
	wg.Wait()
	for _, l := range loops {
		if l.Wait != nil {
			l.Wait()
		}
	}
}

// run is one loop: a tick drains while there is work, so a backlog clears without waiting for
// ticks, then the loop waits for the next tick (Interval) or, if it is adaptive, a wake.
func (w *Worker) run(ctx context.Context, l Loop) {
	act := w.cfg.Activity
	failing := alert.Sustained{For: w.cfg.FailingAfter}
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		// Taken before the step, so a wake that comes while it runs ticks the loop again.
		woken := act.Woken()
		for {
			// A panic costs only this step: it is logged, alerted and counted as a failure.
			n, err := w.step(ctx, l.Name, l.Step, l.Background)
			if err != nil && ctx.Err() != nil || errors.Is(err, ErrSkipped) {
				break
			}
			w.watch(ctx, l.Name, &failing, err)
			if err != nil {
				w.log.Error("worker step failed", "loop", l.Name, "err", err)
				break
			}
			if n == 0 {
				break
			}
			if l.adaptive() {
				act.Mark("work:" + l.Name)
				// The step runs again now, so the wake this Mark may have sent needs no tick of its own.
				woken = act.Woken()
			}
		}
		if !l.adaptive() {
			woken = nil // a loop with one interval (housekeeping) is never woken
		}
		timer.Reset(l.Interval(act.Active(), w.cfg.Now()))
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-woken:
			timer.Stop()
		}
	}
}

// adaptive reports whether the loop has an active interval shorter than its idle one: only then
// does a wake tick it and its work mark the process active.
func (l Loop) adaptive() bool { return l.Active > 0 && l.Active < l.Every }

// Interval is how long the loop waits after a tick: Every, Active while the process is active
// (never slower than Every), and no later than Next (not below the shorter of the two).
func (l Loop) Interval(active bool, now time.Time) time.Duration {
	d, floor := l.Every, l.Every
	if l.adaptive() {
		floor = l.Active
		if active {
			d = l.Active
		}
	}
	if l.Next != nil {
		if next := l.Next(); !next.IsZero() && next.Sub(now) < d {
			d = max(next.Sub(now), floor)
		}
	}
	return d
}

// WakeTopics are the prismaport_wake payloads (the tables of migration 00118's triggers) that
// wake a process of the role: a new session or event for the events loops, a new reveal or a
// "Check again" request for the chain scheduler.
func WakeTopics(r Role) []string {
	switch r {
	case RoleChain:
		return []string{"session_reveals", "recheck"}
	case RoleEvents:
		return []string{"sessions", "events"}
	}
	return []string{"sessions", "events", "session_reveals", "recheck"}
}

// wake handles one prismaport_wake notification; the empty topic (the listener reconnected and
// may have missed some) wakes every role.
func (w *Worker) wake(topic string) {
	if topic == "" || slices.Contains(WakeTopics(w.cfg.Role), topic) {
		w.cfg.Activity.Wake("wake:" + topic)
	}
}

// step runs one step under StepTimeout (a background step under ctx alone).
func (w *Worker) step(ctx context.Context, name string, step func(context.Context) (int, error), background bool) (int, error) {
	if background {
		return safe.Step(ctx, w.cfg.Alerts, w.log, name, step)
	}
	return StepWithin(ctx, w.cfg.StepTimeout, w.cfg.Alerts, w.log, name, step)
}

// StepWithin runs step (panics recovered, as safe.Step) under a deadline of timeout. A step cut
// by its deadline fails saying so.
func StepWithin(ctx context.Context, timeout time.Duration, alerts alert.Sink, log *slog.Logger, name string,
	step func(context.Context) (int, error)) (int, error) {
	sctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	n, err := safe.Step(sctx, alerts, log, name, step)
	if err != nil && ctx.Err() == nil && errors.Is(sctx.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("step exceeded its %s timeout: %w", timeout, err)
	}
	return n, err
}

// loops lists what this process runs for its Role.
func (w *Worker) loops() []Loop {
	var out []Loop
	if w.cfg.Role != RoleChain {
		out = append(out,
			Loop{Name: "sequencer", Every: w.cfg.SequenceEvery, Active: w.cfg.SequenceActive, Step: func(ctx context.Context) (int, error) {
				return w.st.SequenceEvents(ctx, sequenceBatch)
			}},
			// Deliveries run in the background, one goroutine per endpoint; wait for them on the way out.
			Loop{Name: "webhooks", Every: w.cfg.DispatchEvery, Active: w.cfg.DispatchActive, Step: w.dispatcher.Start, Wait: w.dispatcher.Wait,
				Background: true},
			Loop{Name: "webhook-notices", Every: w.cfg.NoticeEvery, Active: w.cfg.NoticeActive, Step: w.svc.SendWebhookNotices},
			Loop{Name: "sandbox-retention", Every: w.cfg.SandboxEvery, Active: w.cfg.SandboxActive, Step: w.svc.PurgeSandbox},
			Loop{Name: "idempotency-retention", Every: w.cfg.IdempotencyEvery, Active: w.cfg.IdempotencyActive, Step: w.PurgeIdempotencyKeys},
			Loop{Name: "api-key-expiry", Every: w.cfg.KeyExpiryEvery, Active: w.cfg.KeyExpiryActive, Step: w.svc.ExpireRolledAPIKeys},
			Loop{Name: "totp-removals", Every: w.cfg.TOTPRemovalEvery, Active: w.cfg.TOTPRemovalActive, Step: w.svc.SweepTOTPRemovals},
			Loop{Name: "expiry", Every: w.cfg.ExpireEvery, Active: w.cfg.ExpireActive, Step: func(ctx context.Context) (int, error) {
				// Sessions with pending payments stay due, so draining would spin; one pass per tick.
				// An open or partially paid session keeps the process active.
				n, open, err := w.svc.ExpirePass(ctx, expireBatch)
				if n > 0 || open {
					w.cfg.Activity.Mark("expiry")
				}
				return 0, err
			}},
		)
	}
	if w.cfg.Role != RoleEvents {
		out = append(out, w.cfg.Loops...)
	}
	return out
}

// PurgeIdempotencyKeys deletes one batch of idempotency keys older than IdempotencyTTL; the loop
// runs it again at once while a batch was full.
func (w *Worker) PurgeIdempotencyKeys(ctx context.Context) (int, error) {
	st := w.st
	n, err := st.PurgeIdempotencyKeys(ctx, st.Now().Add(-w.cfg.IdempotencyTTL), w.cfg.IdempotencyBatch)
	if err != nil || n < w.cfg.IdempotencyBatch {
		return 0, err
	}
	return n, nil
}

// watch alerts when a loop has failed on every step for FailingAfter, and when it recovers.
func (w *Worker) watch(ctx context.Context, name string, failing *alert.Sustained, err error) {
	Watch(ctx, w.cfg.Alerts, name, failing, err, w.cfg.Now())
}

// Watch records one step of the loop called name (a chain's loops are "<kind>:<chain id>") in
// its failing streak, whose For is how long every step must fail: loop_failing when the streak
// reaches it, loop_recovered at the first success after.
func Watch(ctx context.Context, alerts alert.Sink, name string, failing *alert.Sustained, err error, now time.Time) {
	raise, cleared := failing.Update(err != nil, now)
	_, chain, _ := strings.Cut(name, ":")
	switch {
	case raise:
		alerts.Alert(ctx, alert.Alert{Severity: alert.Critical, Chain: chain, Kind: "loop_failing",
			Message: fmt.Sprintf("every %s step has failed for %s", name, failing.For), Fields: map[string]string{"loop": name, "err": err.Error()}})
	case cleared:
		alerts.Alert(ctx, alert.Alert{Severity: alert.Info, Chain: chain, Kind: "loop_recovered",
			Message: name + " steps succeed again", Fields: map[string]string{"loop": name}})
	}
}
