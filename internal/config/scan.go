package config

import (
	"fmt"
	"os"
	"time"

	"github.com/prismaport/prismaport/internal/scan"
	"github.com/prismaport/prismaport/internal/tuning"
)

// ScanEnv is the adaptive scanner's global configuration.
type ScanEnv struct {
	// Intervals are the defaults of every chain (PRISMAPORT_SCAN_HOT, _WARM, _IDLE_HEARTBEAT);
	// a chain overrides them with PRISMAPORT_CHAIN_<ID>_SCAN_*.
	Intervals scan.Intervals
	// ModeCheck is how often each chain's mode is evaluated from the database (no RPC) while the
	// worker is idle (PRISMAPORT_SCAN_MODE_CHECK), ModeCheckActive while it is active
	// (PRISMAPORT_SCAN_MODE_CHECK_ACTIVE). An idle worker still wakes for a pacer's scheduled
	// late check or heartbeat.
	ModeCheck, ModeCheckActive time.Duration
	// PayerActive is how recent checkout_seen_at must be for a payer to count as present.
	PayerActive time.Duration
	// LateSchedule is when expired sessions are checked; WatchAfterExpiry how long. Zero turns
	// late checks off (the schedule is then empty).
	LateSchedule     scan.Schedule
	WatchAfterExpiry time.Duration
	// HeartbeatQuietAfter skips the idle heartbeat of a chain with no session revealed for that
	// long (PRISMAPORT_SCAN_HEARTBEAT_QUIET_AFTER, default 24h; 0 always beats).
	HeartbeatQuietAfter time.Duration
}

// DefaultHeartbeatQuietAfter is PRISMAPORT_SCAN_HEARTBEAT_QUIET_AFTER's default.
const DefaultHeartbeatQuietAfter = 24 * time.Hour

// SessionExpiry bounds checkout session lifetimes (PRISMAPORT_SESSION_*_EXPIRY): the pay window
// after the first reveal (D48). SelectWindow (PRISMAPORT_SESSION_SELECT_WINDOW) is how long a
// session waits for the payer to pick a network before anything is revealed.
type SessionExpiry struct {
	Default, Min, Max time.Duration
	SelectWindow      time.Duration
}

// Bounds of PRISMAPORT_SESSION_SELECT_WINDOW.
const (
	minSelectWindow = time.Minute
	maxSelectWindow = 24 * time.Hour
)

// DefaultScan is the scanner configuration when no variable is set.
func DefaultScan() ScanEnv {
	sch, _ := scan.ParseSchedule(scan.DefaultLateSchedule)
	return ScanEnv{Intervals: scan.DefaultIntervals(), ModeCheck: scan.DefaultModeCheck, ModeCheckActive: scan.DefaultModeCheckActive,
		PayerActive: scan.DefaultPayerActive, LateSchedule: sch, WatchAfterExpiry: scan.DefaultWatchAfterExpiry,
		HeartbeatQuietAfter: DefaultHeartbeatQuietAfter}
}

// duration reads a Go duration variable; ok is false when it is unset.
func duration(name string) (time.Duration, bool, error) {
	v := os.Getenv(name)
	if v == "" {
		return 0, false, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, false, fmt.Errorf("%s must be a duration such as 30s or 1h: %w", name, err)
	}
	return d, true, nil
}

// positiveDuration reads the duration variable name into *dst when it is set; zero or less is
// an error.
func positiveDuration(name string, dst *time.Duration) error {
	v, ok, err := duration(name)
	if err != nil {
		return err
	}
	if ok {
		if v <= 0 {
			return fmt.Errorf("%s must be positive", name)
		}
		*dst = v
	}
	return nil
}

func durationOr(name string, def time.Duration) (time.Duration, error) {
	d, ok, err := duration(name)
	if !ok || err != nil {
		return def, err
	}
	return d, nil
}

func loadScan() (ScanEnv, error) {
	s := DefaultScan()
	var err error
	for _, v := range []struct {
		name string
		dst  *time.Duration
	}{
		{"PRISMAPORT_SCAN_HOT", &s.Intervals.Hot}, {"PRISMAPORT_SCAN_WARM", &s.Intervals.Warm},
		{"PRISMAPORT_SCAN_IDLE_HEARTBEAT", &s.Intervals.Heartbeat},
		{"PRISMAPORT_SCAN_PAYER_ACTIVE", &s.PayerActive}, {"PRISMAPORT_WATCH_AFTER_EXPIRY", &s.WatchAfterExpiry},
		{"PRISMAPORT_SCAN_HEARTBEAT_QUIET_AFTER", &s.HeartbeatQuietAfter},
	} {
		if *v.dst, err = durationOr(v.name, *v.dst); err != nil {
			return s, err
		}
	}
	if err := s.Intervals.Validate(); err != nil {
		return s, fmt.Errorf("PRISMAPORT_SCAN_*: %w", err)
	}
	if err := loadPair("PRISMAPORT_SCAN_MODE_CHECK", "PRISMAPORT_SCAN_MODE_CHECK_ACTIVE", maxIdleInterval, false, &s.ModeCheck, &s.ModeCheckActive); err != nil {
		return s, err
	}
	if s.PayerActive < 10*time.Second {
		return s, fmt.Errorf("PRISMAPORT_SCAN_PAYER_ACTIVE must be at least 10s")
	}
	if s.HeartbeatQuietAfter < 0 {
		return s, fmt.Errorf("PRISMAPORT_SCAN_HEARTBEAT_QUIET_AFTER must be 0 (always beat) or positive")
	}
	if s.WatchAfterExpiry < 0 || s.WatchAfterExpiry > 7*24*time.Hour {
		return s, fmt.Errorf("PRISMAPORT_WATCH_AFTER_EXPIRY must be between 0 (no late checks) and 168h")
	}
	if v, ok := os.LookupEnv("PRISMAPORT_LATE_SCHEDULE"); ok && v != "" {
		if s.LateSchedule, err = scan.ParseSchedule(v); err != nil {
			return s, fmt.Errorf("PRISMAPORT_LATE_SCHEDULE: %w", err)
		}
	}
	switch {
	case s.WatchAfterExpiry == 0:
		s.LateSchedule = scan.Schedule{}
	case len(s.LateSchedule) == 0:
		return s, fmt.Errorf("PRISMAPORT_LATE_SCHEDULE needs a step while PRISMAPORT_WATCH_AFTER_EXPIRY is above 0")
	case s.LateSchedule.End() > s.WatchAfterExpiry:
		return s, fmt.Errorf("PRISMAPORT_LATE_SCHEDULE ends at %s, after PRISMAPORT_WATCH_AFTER_EXPIRY (%s)", s.LateSchedule.End(), s.WatchAfterExpiry)
	}
	return s, nil
}

func loadSessionExpiry() (SessionExpiry, error) {
	e := SessionExpiry{Default: tuning.DefaultSessionExpiry, Min: tuning.DefaultMinSessionExpiry, Max: tuning.DefaultMaxSessionExpiry,
		SelectWindow: tuning.DefaultSelectWindow}
	var err error
	if e.SelectWindow, err = durationOr("PRISMAPORT_SESSION_SELECT_WINDOW", e.SelectWindow); err != nil {
		return e, err
	}
	if e.SelectWindow < minSelectWindow || e.SelectWindow > maxSelectWindow {
		return e, fmt.Errorf("PRISMAPORT_SESSION_SELECT_WINDOW (%s) must be between %s and %s", e.SelectWindow, minSelectWindow, maxSelectWindow)
	}
	for _, v := range []struct {
		name string
		dst  *time.Duration
	}{{"PRISMAPORT_SESSION_DEFAULT_EXPIRY", &e.Default}, {"PRISMAPORT_SESSION_MIN_EXPIRY", &e.Min}, {"PRISMAPORT_SESSION_MAX_EXPIRY", &e.Max}} {
		if *v.dst, err = durationOr(v.name, *v.dst); err != nil {
			return e, err
		}
		if *v.dst <= 0 {
			return e, fmt.Errorf("%s must be positive", v.name)
		}
	}
	switch {
	case e.Min > e.Max:
		return e, fmt.Errorf("PRISMAPORT_SESSION_MIN_EXPIRY (%s) is above PRISMAPORT_SESSION_MAX_EXPIRY (%s)", e.Min, e.Max)
	case e.Default < e.Min || e.Default > e.Max:
		return e, fmt.Errorf("PRISMAPORT_SESSION_DEFAULT_EXPIRY (%s) must be between the min (%s) and the max (%s)", e.Default, e.Min, e.Max)
	}
	return e, nil
}

// chainIntervals reads a chain's PRISMAPORT_CHAIN_<ID>_SCAN_* overrides of the global intervals.
func chainIntervals(p string, def scan.Intervals) (scan.Intervals, error) {
	in := def
	var err error
	if in.Hot, err = durationOr(p+"SCAN_HOT", in.Hot); err != nil {
		return in, err
	}
	if in.Warm, err = durationOr(p+"SCAN_WARM", in.Warm); err != nil {
		return in, err
	}
	if in.Heartbeat, err = durationOr(p+"SCAN_IDLE_HEARTBEAT", in.Heartbeat); err != nil {
		return in, err
	}
	if err := in.Validate(); err != nil {
		return in, fmt.Errorf("%sSCAN_*: %w", p, err)
	}
	return in, nil
}

// expiryHoldSlack is how long after the first late check is due expiry still waits for it (a
// provider down, a late pass behind): service.Config.ExpiryHold is the first step plus this.
const expiryHoldSlack = 4 * time.Minute

// ExpiryHold is how long past expires_at a session's expiry may wait for its first late check
// (service.Config.ExpiryHold, ADR-0008 H-3); 0 with late checks off, since none would come.
func (s ScanEnv) ExpiryHold() time.Duration {
	if s.WatchAfterExpiry == 0 || len(s.LateSchedule) == 0 {
		return 0
	}
	return s.LateSchedule.First() + expiryHoldSlack
}
