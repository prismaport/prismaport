// Package scan paces a chain's detector by what is pending on the chain (adaptive
// scanner, D43). Every ModeCheck the pacer reads the chain's state from the
// database (no RPC) and picks the fastest mode any entry needs: hot while a payer is on the
// checkout page or a payment waits for credit, warm while a session is open or a payment waits
// for finality, late while expired sessions still get scheduled late checks, idle otherwise. It
// runs the chain's RPC step only when the interval of that mode is due.
package scan

import (
	"fmt"
	"strings"
	"time"

	"github.com/prismaport/prismaport/internal/tuning"
)

// Mode is how fast a chain is scanned. A higher mode is faster.
type Mode int

// The scan modes, from not yet known to the fastest.
const (
	// Unknown is the pacer's mode before its first evaluation.
	Unknown Mode = iota - 1
	Idle
	Late
	Warm
	Hot
)

func (m Mode) String() string {
	switch m {
	case Idle:
		return "idle"
	case Late:
		return "late"
	case Warm:
		return "warm"
	case Hot:
		return "hot"
	}
	return "none"
}

// Scanning reports whether the mode reads new blocks (the block cursor must stay continuous).
func (m Mode) Scanning() bool { return m >= Warm }

// Intervals are a chain's tick intervals.
type Intervals struct {
	Hot       time.Duration
	Warm      time.Duration
	Heartbeat time.Duration
}

// Defaults (D43, owner updates 2026-10-30 and 2026-11-02 / D46).
const (
	DefaultHot              = time.Minute
	DefaultWarm             = time.Minute
	DefaultHeartbeat        = time.Hour
	DefaultModeCheck        = 2 * time.Minute // idle; worker plan B (2026-11-17)
	DefaultModeCheckActive  = 5 * time.Second
	DefaultPayerActive      = 30 * time.Second
	DefaultWatchAfterExpiry = tuning.DefaultWatchAfterExpiry // one default for worker and API
	DefaultLateSchedule     = "30m:1m,23h30m:1h"
)

// DefaultIntervals returns the default interval of each scan mode.
func DefaultIntervals() Intervals {
	return Intervals{Hot: DefaultHot, Warm: DefaultWarm, Heartbeat: DefaultHeartbeat}
}

// Validate applies the configuration rules: hot at least 500ms, warm at least hot, heartbeat at
// least warm.
func (i Intervals) Validate() error {
	switch {
	case i.Hot < 500*time.Millisecond:
		return fmt.Errorf("hot interval %s is below 500ms", i.Hot)
	case i.Warm < i.Hot:
		return fmt.Errorf("warm interval %s is below the hot interval %s", i.Warm, i.Hot)
	case i.Heartbeat < i.Warm:
		return fmt.Errorf("idle heartbeat %s is below the warm interval %s", i.Heartbeat, i.Warm)
	}
	return nil
}

func (i Intervals) of(m Mode) time.Duration {
	switch m {
	case Hot:
		return i.Hot
	case Warm:
		return i.Warm
	}
	return i.Heartbeat
}

// Step is one step of a late-check schedule: checks every Every until Until after expiry.
type Step struct {
	Until time.Duration
	Every time.Duration
}

// Schedule is the late-check schedule, measured from a session's expires_at.
type Schedule []Step

// ParseSchedule reads "<until>:<every>,..." (Go durations). Steps must be strictly increasing
// and every interval positive. An empty string is an empty schedule (no late checks).
func ParseSchedule(s string) (Schedule, error) {
	var out Schedule
	if strings.TrimSpace(s) == "" {
		return out, nil
	}
	for _, part := range strings.Split(s, ",") {
		until, every, ok := strings.Cut(strings.TrimSpace(part), ":")
		if !ok {
			return nil, fmt.Errorf("late schedule step %q is not <until>:<every>", part)
		}
		u, err := time.ParseDuration(strings.TrimSpace(until))
		if err != nil {
			return nil, fmt.Errorf("late schedule step %q: %w", part, err)
		}
		e, err := time.ParseDuration(strings.TrimSpace(every))
		if err != nil {
			return nil, fmt.Errorf("late schedule step %q: %w", part, err)
		}
		if e <= 0 {
			return nil, fmt.Errorf("late schedule step %q: the interval must be positive", part)
		}
		if u <= 0 || (len(out) > 0 && u <= out[len(out)-1].Until) {
			return nil, fmt.Errorf("late schedule step %q: steps must be strictly increasing", part)
		}
		out = append(out, Step{Until: u, Every: e})
	}
	return out, nil
}

func (s Schedule) String() string {
	parts := make([]string, len(s))
	for i, st := range s {
		parts[i] = st.Until.String() + ":" + st.Every.String()
	}
	return strings.Join(parts, ",")
}

// End is the last step's Until: no check is scheduled later than this after expiry.
func (s Schedule) End() time.Duration {
	if len(s) == 0 {
		return 0
	}
	return s[len(s)-1].Until
}

// First is the delay of the first check after expiry.
func (s Schedule) First() time.Duration {
	if len(s) == 0 {
		return 0
	}
	return min(s[0].Every, s[0].Until)
}

// Next is the next check after one made at elapsed (time since expiry). ok is false once the
// schedule is over.
func (s Schedule) Next(elapsed time.Duration) (time.Duration, bool) {
	for _, st := range s {
		if elapsed < st.Until {
			return min(elapsed+st.Every, s.End()), true
		}
	}
	return 0, false
}

// Count is the number of checks the schedule makes when every check runs on time.
func (s Schedule) Count() int {
	n := 0
	at, ok := s.First(), len(s) > 0
	for ok {
		n++
		at, ok = s.Next(at)
	}
	return n
}

// NextCheck is the next scheduled late check of a session expiring at expires after a check at
// now: nil before expiry (the first check is due a step after it), and just past the watch end
// (which no late pass selects) once the schedule is over.
func NextCheck(sch Schedule, watch time.Duration, expires, now time.Time) *time.Time {
	elapsed := now.Sub(expires)
	if elapsed < 0 {
		return nil
	}
	t := expires.Add(watch).Add(time.Second)
	if n, ok := sch.Next(elapsed); ok {
		t = expires.Add(n)
	}
	return &t
}
