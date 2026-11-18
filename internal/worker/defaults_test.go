package worker

import (
	"context"
	"testing"
	"time"
)

// The defaults keep an idle deployment to a few database round trips a minute: the hosted
// database's egress allowance is spent by polling, not by traffic.
func TestDefaultIntervals(t *testing.T) {
	var c Config
	c.defaults()
	if c.SequenceEvery != 2*time.Minute || c.SequenceActive != 10*time.Second || c.DispatchEvery != 2*time.Minute ||
		c.DispatchActive != 5*time.Second || c.ExpireEvery != 2*time.Minute || c.ExpireActive != 30*time.Second ||
		c.NoticeEvery != 5*time.Minute || c.NoticeActive != time.Minute {
		t.Fatalf("defaults %+v", c)
	}
	if c.TOTPRemovalEvery != 5*time.Minute || c.TOTPRemovalActive != 5*time.Minute || c.KeyExpiryEvery != 5*time.Minute ||
		c.KeyExpiryActive != 5*time.Minute || c.SandboxEvery != time.Hour || c.SandboxActive != time.Hour ||
		c.IdempotencyEvery != time.Hour || c.IdempotencyActive != time.Hour {
		t.Fatalf("housekeeping defaults %+v", c)
	}
	if c.Activity == nil || c.Activity.idleAfter != 10*time.Minute {
		t.Fatalf("activity %+v", c.Activity)
	}
}

func TestSetIntervalsAreKept(t *testing.T) {
	c := Config{SequenceEvery: time.Second, DispatchEvery: 2 * time.Second, ExpireEvery: 3 * time.Second, TOTPRemovalEvery: 4 * time.Second,
		SequenceActive: 100 * time.Millisecond, NoticeEvery: 5 * time.Second, IdleAfter: time.Minute}
	c.defaults()
	if c.SequenceEvery != time.Second || c.DispatchEvery != 2*time.Second || c.ExpireEvery != 3*time.Second || c.TOTPRemovalEvery != 4*time.Second ||
		c.SequenceActive != 100*time.Millisecond || c.NoticeEvery != 5*time.Second || c.Activity.idleAfter != time.Minute {
		t.Fatalf("set intervals overwritten: %+v", c)
	}
}

// The interval of every loop in each mode with the defaults (chains: the scan mode check, built
// by app.ChainLoops from PRISMAPORT_SCAN_MODE_CHECK and _ACTIVE).
func TestEffectiveIntervalOfEachLoop(t *testing.T) {
	step := func(context.Context) (int, error) { return 0, nil }
	w := New(nil, nil, nil, Config{Loops: []Loop{{Name: "chains", Every: 2 * time.Minute, Active: 5 * time.Second, Step: step}}}, nil)
	want := map[string][2]time.Duration{
		"sequencer":             {2 * time.Minute, 10 * time.Second},
		"webhooks":              {2 * time.Minute, 5 * time.Second},
		"chains":                {2 * time.Minute, 5 * time.Second},
		"webhook-notices":       {5 * time.Minute, time.Minute},
		"expiry":                {2 * time.Minute, 30 * time.Second},
		"api-key-expiry":        {5 * time.Minute, 5 * time.Minute},
		"totp-removals":         {5 * time.Minute, 5 * time.Minute},
		"sandbox-retention":     {time.Hour, time.Hour},
		"idempotency-retention": {time.Hour, time.Hour},
	}
	now := time.Now()
	loops := w.loops()
	if len(loops) != len(want) {
		t.Fatalf("%d loops, want %d", len(loops), len(want))
	}
	for _, l := range loops {
		w, ok := want[l.Name]
		if !ok {
			t.Fatalf("unexpected loop %s", l.Name)
		}
		if idle, active := l.Interval(false, now), l.Interval(true, now); idle != w[0] || active != w[1] {
			t.Errorf("%s: idle %s active %s, want %s %s", l.Name, idle, active, w[0], w[1])
		}
	}
}

// The short intervals tests pass (an idle interval below the default active one) hold in both
// modes: active is never slower than idle.
func TestShortIntervalsHoldInBothModes(t *testing.T) {
	w := New(nil, nil, nil, Config{Role: RoleEvents, SequenceEvery: 20 * time.Millisecond, DispatchEvery: 20 * time.Millisecond,
		ExpireEvery: 20 * time.Millisecond}, nil)
	now := time.Now()
	for _, l := range w.loops() {
		switch l.Name {
		case "sequencer", "webhooks", "expiry":
			if a, i := l.Interval(true, now), l.Interval(false, now); a != 20*time.Millisecond || i != 20*time.Millisecond {
				t.Errorf("%s: active %s idle %s", l.Name, a, i)
			}
		}
	}
}

// Next pulls an idle loop forward to its scheduled work, never below the active interval, and
// never pushes it back.
func TestNextPullsAnIdleLoopForward(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	var next time.Time
	l := Loop{Every: 2 * time.Minute, Active: 5 * time.Second, Next: func() time.Time { return next }}
	for _, c := range []struct {
		next   time.Time
		active bool
		want   time.Duration
	}{
		{time.Time{}, false, 2 * time.Minute},
		{now.Add(time.Minute), false, time.Minute},
		{now.Add(time.Second), false, 5 * time.Second},
		{now.Add(-time.Minute), false, 5 * time.Second},
		{now.Add(10 * time.Minute), false, 2 * time.Minute},
		{now.Add(time.Minute), true, 5 * time.Second},
	} {
		next = c.next
		if got := l.Interval(c.active, now); got != c.want {
			t.Errorf("next %s active %v: %s, want %s", c.next.Sub(now), c.active, got, c.want)
		}
	}
}

func TestWakeTopicsFollowTheRole(t *testing.T) {
	for _, c := range []struct {
		role  Role
		topic string
		wakes bool
	}{
		{RoleEvents, "sessions", true}, {RoleEvents, "events", true}, {RoleEvents, "session_reveals", false}, {RoleEvents, "recheck", false},
		{RoleChain, "session_reveals", true}, {RoleChain, "recheck", true}, {RoleChain, "sessions", false}, {RoleChain, "events", false},
		{RoleAll, "sessions", true}, {RoleAll, "events", true}, {RoleAll, "session_reveals", true}, {RoleAll, "recheck", true},
		{RoleAll, "other", false}, {RoleChain, "", true}, {RoleEvents, "", true},
	} {
		act := NewActivity(0, nil, nil)
		w := New(nil, nil, nil, Config{Role: c.role, Activity: act}, nil)
		w.wake(c.topic)
		if act.Active() != c.wakes {
			t.Errorf("%s woken by %q: %v, want %v", c.role, c.topic, act.Active(), c.wakes)
		}
	}
}

// A housekeeping loop keeps one interval by default; given an active interval shorter than its
// idle one (by variable) it becomes adaptive like the others.
func TestHousekeepingLoopsAreAdaptiveOnlyWithAShorterActive(t *testing.T) {
	now := time.Now()
	byName := func(c Config) map[string]Loop {
		out := map[string]Loop{}
		for _, l := range New(nil, nil, nil, c, nil).loops() {
			out[l.Name] = l
		}
		return out
	}
	def := byName(Config{Role: RoleEvents})
	for _, name := range []string{"api-key-expiry", "totp-removals", "sandbox-retention", "idempotency-retention"} {
		if def[name].adaptive() {
			t.Errorf("%s is adaptive by default", name)
		}
	}
	for _, name := range []string{"sequencer", "webhooks", "webhook-notices", "expiry"} {
		if !def[name].adaptive() {
			t.Errorf("%s is not adaptive by default", name)
		}
	}
	set := byName(Config{Role: RoleEvents, KeyExpiryActive: 30 * time.Second, SandboxEvery: 2 * time.Hour,
		TOTPRemovalEvery: 10 * time.Minute, IdempotencyEvery: 30 * time.Minute, IdempotencyActive: 5 * time.Minute})
	if l := set["api-key-expiry"]; !l.adaptive() || l.Interval(true, now) != 30*time.Second || l.Interval(false, now) != 5*time.Minute {
		t.Errorf("api-key-expiry with a 30s active: adaptive %v, %s / %s", l.adaptive(), l.Interval(false, now), l.Interval(true, now))
	}
	if l := set["sandbox-retention"]; l.adaptive() || l.Interval(true, now) != 2*time.Hour {
		t.Errorf("sandbox-retention at 2h: adaptive %v, active %s", l.adaptive(), l.Interval(true, now))
	}
	if l := set["totp-removals"]; l.adaptive() || l.Interval(false, now) != 10*time.Minute {
		t.Errorf("totp-removals at 10m: adaptive %v, idle %s", l.adaptive(), l.Interval(false, now))
	}
	if l := set["idempotency-retention"]; !l.adaptive() || l.Interval(true, now) != 5*time.Minute {
		t.Errorf("idempotency-retention 30m/5m: adaptive %v, active %s", l.adaptive(), l.Interval(true, now))
	}
}
