package worker

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// probe is a loop step that records when it ran (since start) and reports work while work > 0.
type probe struct {
	start time.Time
	work  atomic.Int32
	mu    sync.Mutex
	at    []time.Duration
}

func newProbe() *probe { return &probe{start: time.Now()} }

func (p *probe) step(context.Context) (int, error) {
	p.mu.Lock()
	p.at = append(p.at, time.Since(p.start))
	p.mu.Unlock()
	if p.work.Load() > 0 {
		p.work.Add(-1)
		return 1, nil
	}
	return 0, nil
}

func (p *probe) ticks() []time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.at)
}

// run starts a worker with loops (bubble time) and returns its stop.
func run(t *testing.T, act *Activity, loops ...Loop) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		New(nil, nil, nil, Config{Role: RoleChain, Loops: loops, Activity: act}, nil).Run(ctx)
		close(done)
	}()
	synctest.Wait()
	return func() { cancel(); <-done }
}

func wantTicks(t *testing.T, p *probe, want ...time.Duration) {
	t.Helper()
	if got := p.ticks(); !slices.Equal(got, want) {
		t.Fatalf("ticks %v, want %v", got, want)
	}
}

// An idle loop waits its idle interval; a step that finds work turns the process active (the
// step drains at once) and the loop ticks at its active interval from there.
func TestWorkTurnsTheProcessActive(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := newProbe()
		act := NewActivity(10*time.Minute, nil, nil)
		stop := run(t, act, Loop{Name: "l", Every: 2 * time.Minute, Active: 2 * time.Second, Step: p.step})
		defer stop()
		wantTicks(t, p, 0)
		time.Sleep(time.Minute)
		synctest.Wait()
		wantTicks(t, p, 0)
		if act.Active() {
			t.Fatal("active before any work")
		}
		p.work.Store(1)
		time.Sleep(time.Minute)
		synctest.Wait()
		wantTicks(t, p, 0, 2*time.Minute, 2*time.Minute)
		if !act.Active() {
			t.Fatal("work did not turn the process active")
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		wantTicks(t, p, 0, 2*time.Minute, 2*time.Minute, 2*time.Minute+2*time.Second)
	})
}

// A wake ticks every adaptive loop at once, mid idle interval, and turns the process active; a
// housekeeping loop (no Active) keeps its interval.
func TestWakeTicksAnIdleLoopAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, h := newProbe(), newProbe()
		act := NewActivity(10*time.Minute, nil, nil)
		stop := run(t, act, Loop{Name: "l", Every: 2 * time.Minute, Active: 2 * time.Second, Step: p.step},
			Loop{Name: "housekeeping", Every: 5 * time.Minute, Step: h.step})
		defer stop()
		time.Sleep(30 * time.Second)
		act.Wake("test")
		synctest.Wait()
		wantTicks(t, p, 0, 30*time.Second)
		if !act.Active() {
			t.Fatal("a wake did not turn the process active")
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		wantTicks(t, p, 0, 30*time.Second, 32*time.Second)
		wantTicks(t, h, 0)
		time.Sleep(5*time.Minute - 32*time.Second)
		synctest.Wait()
		wantTicks(t, h, 0, 5*time.Minute)
	})
}

// A wake that comes while a step runs ticks the loop again right after it: the step may have
// read the database before the change it announces.
func TestWakeDuringAStepIsNotLost(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		act := NewActivity(10*time.Minute, nil, nil)
		var mu sync.Mutex
		var at []time.Duration
		step := func(context.Context) (int, error) {
			mu.Lock()
			at = append(at, time.Since(start))
			first := len(at) == 2
			mu.Unlock()
			if first {
				act.Wake("during") // the second tick is woken while it runs
				time.Sleep(time.Second)
			}
			return 0, nil
		}
		stop := run(t, act, Loop{Name: "l", Every: 2 * time.Minute, Active: 10 * time.Second, Step: step})
		defer stop()
		act.Wake("first")
		time.Sleep(2 * time.Second) // the woken step sleeps a second; the loop then ticks again at once
		synctest.Wait()
		mu.Lock()
		got := slices.Clone(at)
		mu.Unlock()
		if want := []time.Duration{0, 0, time.Second}; !slices.Equal(got, want) {
			t.Fatalf("ticks %v, want %v", got, want)
		}
	})
}

// The process turns idle IdleAfter after the last sign of work: the loop ticks at its active
// interval until then and at its idle one after.
func TestIdleAgainAfterIdleAfter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := newProbe()
		act := NewActivity(10*time.Minute, nil, nil)
		act.Mark("start")
		stop := run(t, act, Loop{Name: "l", Every: 2 * time.Minute, Active: 2 * time.Second, Step: p.step})
		defer stop()
		time.Sleep(15 * time.Minute)
		synctest.Wait()
		got := p.ticks()
		var want []time.Duration
		for d := time.Duration(0); d <= 10*time.Minute; d += 2 * time.Second {
			want = append(want, d)
		}
		want = append(want, 12*time.Minute, 14*time.Minute)
		if !slices.Equal(got, want) {
			t.Fatalf("%d ticks ending %v, want %d ending %v", len(got), got[max(0, len(got)-4):], len(want), want[len(want)-4:])
		}
		if act.Active() {
			t.Fatal("still active 15 minutes after the last work")
		}
	})
}

// Each sign of work extends the active window; Mark on an active process wakes nobody.
func TestMarkExtendsTheWindowAndWakesOnlyOnTheTransition(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		act := NewActivity(10*time.Minute, nil, nil)
		w := act.Woken()
		act.Mark("a")
		select {
		case <-w:
		default:
			t.Fatal("turning active woke nobody")
		}
		w = act.Woken()
		time.Sleep(9 * time.Minute)
		act.Mark("b")
		select {
		case <-w:
			t.Fatal("a Mark on an active process woke the loops")
		default:
		}
		time.Sleep(9 * time.Minute)
		if !act.Active() {
			t.Fatal("the second Mark did not extend the window")
		}
		time.Sleep(time.Minute)
		if act.Active() {
			t.Fatal("active 10 minutes after the last Mark")
		}
	})
}
