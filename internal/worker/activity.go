package worker

import (
	"log/slog"
	"sync"
	"time"
)

// DefaultIdleAfter is how long a worker stays active after the last sign of work.
const DefaultIdleAfter = 10 * time.Minute

// Activity is a worker process's one idle/active decision, shared by its loops (plan B). The
// process is active for IdleAfter after the last sign of work: a step of an adaptive loop that
// found some (n > 0), a chain state with something pending (chainsched) or a database wake
// (pg_notify on prismaport_wake). While active, each adaptive loop ticks at its active interval;
// once idle, at its idle one. Turning active wakes every adaptive loop at once, so none waits out
// an idle interval it started before.
type Activity struct {
	idleAfter time.Duration
	now       func() time.Time
	log       *slog.Logger

	mu     sync.Mutex
	last   time.Time // the last sign of work (zero: none yet)
	active bool      // the state last reported, for the transition logs
	woke   chan struct{}
}

// NewActivity returns an idle activity state; zero idleAfter is DefaultIdleAfter, nil now is
// time.Now and nil log discards.
func NewActivity(idleAfter time.Duration, now func() time.Time, log *slog.Logger) *Activity {
	if idleAfter <= 0 {
		idleAfter = DefaultIdleAfter
	}
	if now == nil {
		now = time.Now
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Activity{idleAfter: idleAfter, now: now, log: log, woke: make(chan struct{})}
}

// Mark records a sign of work now; an idle process turns active and its loops are woken.
func (a *Activity) Mark(reason string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	was := a.activeAt(now)
	a.last = now
	if !was {
		a.lapse()
		a.report(true, reason)
		a.wake()
	}
}

// Wake records a sign of work and wakes every adaptive loop for a tick now, active or not.
func (a *Activity) Wake(reason string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	if !a.activeAt(now) {
		a.lapse()
		a.report(true, reason)
	}
	a.last = now
	a.wake()
}

// Active reports whether a sign of work was seen within IdleAfter.
func (a *Activity) Active() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	active := a.activeAt(a.now())
	if !active && a.active {
		a.report(false, "")
	}
	return active
}

// Woken is closed at the next wake. Take it before a step, so a wake during the step is not lost.
func (a *Activity) Woken() <-chan struct{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.woke
}

func (a *Activity) activeAt(now time.Time) bool {
	return !a.last.IsZero() && now.Sub(a.last) < a.idleAfter
}

// lapse reports the idle turn no Active call saw.
func (a *Activity) lapse() {
	if a.active {
		a.report(false, "")
	}
}

func (a *Activity) wake() {
	close(a.woke)
	a.woke = make(chan struct{})
}

func (a *Activity) report(active bool, reason string) {
	a.active = active
	if active {
		a.log.Info("worker active", "reason", reason)
		return
	}
	a.log.Info("worker idle", "after", a.idleAfter.String())
}
