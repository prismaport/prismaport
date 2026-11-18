package scan

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/worker"
)

func TestParseSchedule(t *testing.T) {
	s, err := ParseSchedule(DefaultLateSchedule)
	end := 23*time.Hour + 30*time.Minute
	if err != nil || len(s) != 2 || s[0] != (Step{30 * time.Minute, time.Minute}) || s.End() != end || s.First() != time.Minute {
		t.Fatalf("%v %v", s, err)
	}
	if s.Count() != 53 {
		t.Fatalf("default schedule makes %d checks, want 53", s.Count())
	}
	var at []time.Duration
	for d, ok := s.First(), true; ok; d, ok = s.Next(d) {
		at = append(at, d)
	}
	// Every minute for 30 min after expiry (1 h after creation), then hourly to 23h30 (24 h).
	var want []time.Duration
	for m := 1; m <= 30; m++ {
		want = append(want, time.Duration(m)*time.Minute)
	}
	for d := 90 * time.Minute; d <= end; d += time.Hour {
		want = append(want, d)
	}
	if len(at) != len(want) {
		t.Fatalf("checks at %v", at)
	}
	for i := range want {
		if at[i] != want[i] {
			t.Fatalf("check %d at %s, want %s (%v)", i, at[i], want[i], at)
		}
	}
	// A late check (worker down) moves the rest of the schedule; the last one stays at the end.
	if n, ok := s.Next(23 * time.Hour); !ok || n != end {
		t.Fatalf("after 23h: %s %v", n, ok)
	}
	if _, ok := s.Next(end); ok {
		t.Fatal("a check after the end")
	}
	if e, err := ParseSchedule(""); err != nil || len(e) != 0 || e.Count() != 0 {
		t.Fatalf("empty: %v %v", e, err)
	}
	for _, bad := range []string{"1h", "1h:0s", "1h:-1m", "0s:1m", "2h:1h,1h:1h", "1h:1h,1h:2h", "x:1m", "1h:y", "1h:20m,"} {
		if _, err := ParseSchedule(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}

func TestNextCheck(t *testing.T) {
	s, _ := ParseSchedule(DefaultLateSchedule)
	exp := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	watch := 23*time.Hour + 30*time.Minute
	if NextCheck(s, watch, exp, exp.Add(-time.Minute)) != nil {
		t.Fatal("a next check before expiry")
	}
	if n := NextCheck(s, watch, exp, exp.Add(time.Minute)); !n.Equal(exp.Add(2 * time.Minute)) {
		t.Fatalf("after the first check: %s", n)
	}
	if n := NextCheck(s, watch, exp, exp.Add(30*time.Minute)); !n.Equal(exp.Add(90 * time.Minute)) {
		t.Fatalf("after the last minute check: %s", n)
	}
	if n := NextCheck(s, watch, exp, exp.Add(watch)); !n.After(exp.Add(watch)) {
		t.Fatalf("after the last check: %s", n)
	}
}

func TestIntervalsValidate(t *testing.T) {
	if err := DefaultIntervals().Validate(); err != nil {
		t.Fatal(err)
	}
	for _, in := range []Intervals{
		{Hot: 499 * time.Millisecond, Warm: time.Second, Heartbeat: time.Hour},
		{Hot: time.Second, Warm: 999 * time.Millisecond, Heartbeat: time.Hour},
		{Hot: time.Second, Warm: time.Minute, Heartbeat: 59 * time.Second},
	} {
		if in.Validate() == nil {
			t.Fatalf("%+v accepted", in)
		}
	}
}

func TestSelect(t *testing.T) {
	due := time.Now()
	for _, c := range []struct {
		s      State
		mode   Mode
		reason string
	}{
		{State{}, Idle, "nothing_pending"},
		{State{LateDue: &due}, Late, "late_checks"},
		{State{Recheck: true}, Late, "recheck"},
		{State{Mined: true, LateDue: &due}, Warm, "job_mined"},
		{State{Confirmed: true}, Warm, "payment_confirmed"},
		{State{Open: true, Recheck: true}, Warm, "session_open"},
		{State{Submitted: true, Open: true}, Hot, "job_submitted"},
		{State{Detected: true, Confirmed: true}, Hot, "payment_detected"},
		{State{PayerActive: true, Open: true, Detected: true}, Hot, "payer_active"},
	} {
		t.Run(c.reason, func(t *testing.T) {
			if m, r := Select(c.s); m != c.mode || r != c.reason {
				t.Fatalf("%+v: %s %s, want %s %s", c.s, m, r, c.mode, c.reason)
			}
		})
	}
}

type fakeScanner struct {
	scans, fulls, lates, beats, rests int
	more                              int // Scan reports more this many times
	err, lateErr                      error
}

func (f *fakeScanner) Scan(_ context.Context, t Tick) (bool, error) {
	f.scans++
	if t.Full {
		f.fulls++
	}
	if f.more > 0 {
		f.more--
		return true, f.err
	}
	return false, f.err
}
func (f *fakeScanner) Late(context.Context) (bool, error) { f.lates++; return false, f.lateErr }
func (f *fakeScanner) Heartbeat(context.Context) error    { f.beats++; return f.err }
func (f *fakeScanner) Rest(context.Context) error         { f.rests++; return nil }

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newPacer(st *State) (*Pacer, *fakeScanner, *clock) {
	c := &clock{t: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)}
	f := &fakeScanner{}
	// A hot interval below warm (configurable, the defaults have both at 1 min) to cover both.
	p := NewPacer(Config{Chain: "base", Intervals: Intervals{Hot: 30 * time.Second, Warm: time.Minute, Heartbeat: time.Hour}, Now: c.now}, func(context.Context) (State, error) { return *st, nil }, f)
	return p, f, c
}

// step runs one ModeCheck tick and reports whether anything ran.
func step(t *testing.T, p *Pacer) (int, bool) {
	t.Helper()
	n, err := p.Step(t.Context())
	if errors.Is(err, worker.ErrSkipped) {
		return n, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return n, true
}

// An idle chain makes one heartbeat an hour and nothing else, and a new session wakes it at
// the next ModeCheck tick, even an hour into idle.
func TestIdleChainHeartbeatsAndWakesOnTheNextTick(t *testing.T) {
	st := &State{}
	p, f, c := newPacer(st)
	if _, ran := step(t, p); !ran || f.beats != 1 || f.rests != 1 || p.Mode() != Idle {
		t.Fatalf("first tick: beats %d rests %d mode %s", f.beats, f.rests, p.Mode())
	}
	for range 3599 {
		c.advance(time.Second)
		if _, ran := step(t, p); ran {
			t.Fatalf("idle tick at %s ran something: %+v", c.t, f)
		}
	}
	c.advance(time.Second)
	if _, ran := step(t, p); !ran || f.beats != 2 || f.scans != 0 {
		t.Fatalf("after an hour: %+v", f)
	}
	c.advance(10 * time.Minute)
	st.Open = true
	if _, ran := step(t, p); !ran || f.scans != 1 || !(f.fulls == 1) || p.Mode() != Warm {
		t.Fatalf("new session did not wake the chain: %+v %s", f, p.Mode())
	}
	// Warm: next scan one warm interval later.
	for range 59 {
		c.advance(time.Second)
		step(t, p)
	}
	if f.scans != 1 {
		t.Fatalf("warm scanned %d times within a minute", f.scans)
	}
	c.advance(time.Second)
	if step(t, p); f.scans != 2 {
		t.Fatalf("warm did not scan after a minute: %+v", f)
	}
	// A payer on the page speeds it up at once, then every hot interval.
	c.advance(time.Second)
	st.PayerActive = true
	if step(t, p); f.scans != 3 || p.Mode() != Hot {
		t.Fatalf("payer did not wake hot: %+v", f)
	}
	c.advance(29 * time.Second)
	if step(t, p); f.scans != 3 {
		t.Fatal("hot scanned before its interval")
	}
	c.advance(time.Second)
	if step(t, p); f.scans != 4 {
		t.Fatal("hot did not scan after its interval")
	}
	if f.beats != 2 || f.rests != 1 {
		t.Fatalf("heartbeat or rest while scanning: %+v", f)
	}
	// Back to idle: rest once, heartbeat an interval after the last request.
	st.PayerActive, st.Open = false, false
	c.advance(time.Second)
	if _, ran := step(t, p); ran || f.rests != 2 {
		t.Fatalf("idle again: %+v", f)
	}
}

// A scan that stopped short of the head runs again at once; one that caught up waits.
func TestDrainOnlyWhileBehind(t *testing.T) {
	st := &State{Open: true}
	p, f, _ := newPacer(st)
	f.more = 2
	for i, want := range []int{1, 1, 0} {
		n, ran := step(t, p)
		if !ran || n != want {
			t.Fatalf("step %d: n %d ran %v", i, n, ran)
		}
	}
	if _, ran := step(t, p); ran || f.scans != 3 {
		t.Fatalf("an empty round after catching up: %+v", f)
	}
}

// Late checks run when due whatever the mode; a recheck runs at once.
func TestLateChecksRunWhenDue(t *testing.T) {
	st := &State{}
	p, f, c := newPacer(st)
	step(t, p)
	due := c.t.Add(20 * time.Minute)
	st.LateDue = &due
	c.advance(time.Second)
	if _, ran := step(t, p); ran || f.lates != 0 || p.Mode() != Late {
		t.Fatalf("late before due: %+v %s", f, p.Mode())
	}
	c.advance(20 * time.Minute)
	if step(t, p); f.lates != 1 || f.beats != 1 {
		t.Fatalf("late due: %+v", f)
	}
	st.LateDue = nil
	st.Recheck, st.Open = true, true
	c.advance(time.Second)
	if step(t, p); f.lates != 2 || f.scans != 1 {
		t.Fatalf("recheck while warm: %+v", f)
	}
}

// A failing heartbeat is retried at the warm interval, so an idle chain's provider outage turns
// into loop_failing within minutes.
func TestFailedHeartbeatRetriesAtTheWarmInterval(t *testing.T) {
	st := &State{}
	p, f, c := newPacer(st)
	f.err = errors.New("down")
	if _, err := p.Step(t.Context()); err == nil {
		t.Fatal("heartbeat error swallowed")
	}
	c.advance(59 * time.Second)
	if _, err := p.Step(t.Context()); !errors.Is(err, worker.ErrSkipped) {
		t.Fatalf("retried early: %v", err)
	}
	c.advance(time.Second)
	if _, err := p.Step(t.Context()); err == nil || errors.Is(err, worker.ErrSkipped) || f.beats != 2 {
		t.Fatalf("retry: %v %+v", err, f)
	}
	f.err = nil
	c.advance(time.Minute)
	step(t, p)
	c.advance(time.Minute)
	if _, ran := step(t, p); ran || f.beats != 3 {
		t.Fatalf("after recovery the heartbeat is hourly again: %+v", f)
	}
}

// While hot, a full pass (Solana's balance trigger for the other open accounts) runs at the warm
// interval only.
func TestFullPassAtTheWarmInterval(t *testing.T) {
	st := &State{PayerActive: true}
	p, f, c := newPacer(st)
	p.cfg.Intervals.Hot = 3 * time.Second
	for range 40 {
		step(t, p)
		c.advance(time.Second)
	}
	if f.scans != 14 || f.fulls != 1 {
		t.Fatalf("40 s hot: %d scans, %d full", f.scans, f.fulls)
	}
	c.advance(20 * time.Second)
	if step(t, p); f.fulls != 2 {
		t.Fatalf("no full pass after a minute: %+v", f)
	}
}

// A failing late pass is retried at the warm interval, not on every ModeCheck tick.
func TestFailedLatePassBacksOff(t *testing.T) {
	due := time.Date(2026, 10, 30, 11, 0, 0, 0, time.UTC)
	st := &State{LateDue: &due}
	p, f, c := newPacer(st)
	f.lateErr = errors.New("revert")
	if _, err := p.Step(t.Context()); err == nil || f.lates != 1 {
		t.Fatalf("late error: %v %+v", err, f)
	}
	for range 59 {
		c.advance(time.Second)
		_, _ = p.Step(t.Context())
	}
	if f.lates != 1 {
		t.Fatalf("late retried %d times within the warm interval", f.lates-1)
	}
	c.advance(time.Second)
	f.lateErr = nil
	if step(t, p); f.lates != 2 {
		t.Fatalf("late not retried after the warm interval: %+v", f)
	}
}

// With QuietAfter, an idle chain with no reveal for that long makes no heartbeat; a reveal makes
// it beat again, and Due says so without running anything.
func TestQuietChainSkipsTheHeartbeat(t *testing.T) {
	st := &State{}
	c := &clock{t: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)}
	f := &fakeScanner{}
	p := NewPacer(Config{Chain: "base", Intervals: Intervals{Hot: 30 * time.Second, Warm: time.Minute, Heartbeat: time.Hour},
		QuietAfter: 24 * time.Hour, Now: c.now}, func(context.Context) (State, error) { return *st, nil }, f)
	if _, ran := step(t, p); ran || f.beats != 0 || f.rests != 1 || p.Mode() != Idle {
		t.Fatalf("never revealed: first tick %+v", f) // the mode is set, no RPC
	}
	for range 3 {
		c.advance(time.Hour)
		if p.Due(*st, c.now()) {
			t.Fatal("a quiet chain is due")
		}
		if _, ran := step(t, p); ran || f.beats != 0 {
			t.Fatalf("a quiet chain beat: %+v", f)
		}
	}
	at := c.now().Add(-23 * time.Hour)
	st.LastReveal = &at
	if !p.Due(*st, c.now()) {
		t.Fatal("a chain revealed on within the day is not due")
	}
	if _, ran := step(t, p); !ran || f.beats != 1 {
		t.Fatalf("revealed within the day: %+v", f)
	}
	c.advance(time.Hour)
	if p.Due(*st, c.now()) {
		t.Fatal("the last reveal is a day old: due")
	}
	if _, ran := step(t, p); ran || f.beats != 1 {
		t.Fatalf("quiet again: %+v", f)
	}
}

// Due agrees with StepState: an idle chain between two heartbeats is not due, a woken one is.
func TestDueMatchesStep(t *testing.T) {
	st := &State{}
	p, f, c := newPacer(st)
	step(t, p)
	for range 10 {
		c.advance(time.Minute)
		if p.Due(*st, c.now()) {
			t.Fatal("idle between heartbeats is due")
		}
	}
	st.Open = true
	if !p.Due(*st, c.now()) {
		t.Fatal("a woken chain is not due")
	}
	step(t, p)
	if f.scans != 1 || p.Due(*st, c.now()) {
		t.Fatalf("after the scan: %+v due %v", f, p.Due(*st, c.now()))
	}
}

// Next is when Due turns true with the same state: the heartbeat of an idle chain, the scan of a
// scanning one, the earliest late check, and now for a pending "Check again" or a mode change.
func TestNextIsWhenDueTurnsTrue(t *testing.T) {
	st := &State{}
	p, _, c := newPacer(st)
	if got := p.Next(*st, c.now()); !got.Equal(c.now()) {
		t.Fatalf("before the first step: %s, want now", got)
	}
	step(t, p)
	t0 := c.now()
	if got := p.Next(*st, c.now()); !got.Equal(t0.Add(time.Hour)) {
		t.Fatalf("idle: %s, want the heartbeat an hour on", got)
	}
	due := t0.Add(20 * time.Minute)
	st.LateDue = &due
	step(t, p) // mode change to late
	if got := p.Next(*st, c.now()); !got.Equal(due) {
		t.Fatalf("late: %s, want %s", got, due)
	}
	st.Recheck = true
	if got := p.Next(*st, c.now()); !got.Equal(c.now()) {
		t.Fatalf("recheck: %s, want now", got)
	}
	st.Recheck, st.LateDue, st.Open = false, nil, true
	step(t, p)
	scanned := c.now()
	c.advance(10 * time.Second)
	if got := p.Next(*st, c.now()); !got.Equal(scanned.Add(time.Minute)) {
		t.Fatalf("warm: %s, want the next scan at %s", got, scanned.Add(time.Minute))
	}
	// At every point Next names, Due holds; a second before it, it does not.
	for _, s := range []State{{}, {Open: true}, {LateDue: &due}} {
		*st = s
		step(t, p)
		next := p.Next(*st, c.now())
		if !next.After(c.now()) {
			continue
		}
		if p.Due(*st, next.Add(-time.Second)) || !p.Due(*st, next) {
			t.Fatalf("%+v: Due around Next %s disagrees", s, next)
		}
	}
}
