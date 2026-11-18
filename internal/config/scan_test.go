package config

import (
	"strings"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/scan"
)

func TestScanDefaults(t *testing.T) {
	t.Setenv("PRISMAPORT_DATABASE_URL", "postgres://x")
	t.Setenv("PRISMAPORT_CHAIN_BASE_RPC_URLS", "https://a.example,https://b.example")
	t.Setenv("PRISMAPORT_CHAIN_SOLANA_DEVNET_RPC_URLS", "https://a.example,https://b.example")
	e, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	s := e.Scan
	want := scan.Intervals{Hot: time.Minute, Warm: time.Minute, Heartbeat: time.Hour}
	if s.Intervals != want || s.ModeCheck != 2*time.Minute || s.ModeCheckActive != 5*time.Second || s.PayerActive != 30*time.Second || s.WatchAfterExpiry != 23*time.Hour+30*time.Minute ||
		s.LateSchedule.String() != "30m0s:1m0s,23h30m0s:1h0m0s" || s.HeartbeatQuietAfter != 24*time.Hour {
		t.Fatalf("scan defaults %+v", s)
	}
	if n := s.LateSchedule.Count(); n != 53 {
		t.Fatalf("default schedule makes %d checks, want 53", n)
	}
	b := e.Chains["base"]
	if b.Scan != want || b.BlockTime != 2*time.Second || b.ReanchorMargin != 30 {
		t.Fatalf("base scan %+v block time %s margin %d", b.Scan, b.BlockTime, b.ReanchorMargin)
	}
	if e.Solana["solana-devnet"].Scan != want {
		t.Fatalf("solana-devnet scan %+v", e.Solana["solana-devnet"].Scan)
	}
	if e.Sessions != (SessionExpiry{Default: 30 * time.Minute, Min: time.Minute, Max: 24 * time.Hour, SelectWindow: time.Hour}) {
		t.Fatalf("session expiry %+v", e.Sessions)
	}
}

func TestScanVariables(t *testing.T) {
	t.Setenv("PRISMAPORT_DATABASE_URL", "postgres://x")
	t.Setenv("PRISMAPORT_CHAIN_BASE_RPC_URLS", "https://a.example,https://b.example")
	t.Setenv("PRISMAPORT_CHAIN_SOLANA_RPC_URLS", "https://a.example,https://b.example")
	for k, v := range map[string]string{
		"PRISMAPORT_SCAN_HOT": "5s", "PRISMAPORT_SCAN_WARM": "20s", "PRISMAPORT_SCAN_IDLE_HEARTBEAT": "30m",
		"PRISMAPORT_SCAN_MODE_CHECK": "500ms", "PRISMAPORT_SCAN_PAYER_ACTIVE": "45s", "PRISMAPORT_WATCH_AFTER_EXPIRY": "48h",
		"PRISMAPORT_LATE_SCHEDULE": "30m:10m,48h:6h", "PRISMAPORT_SCAN_HEARTBEAT_QUIET_AFTER": "0s",
		"PRISMAPORT_CHAIN_BASE_SCAN_HOT": "1s", "PRISMAPORT_CHAIN_BASE_SCAN_WARM": "10s", "PRISMAPORT_CHAIN_BASE_SCAN_IDLE_HEARTBEAT": "2h",
		"PRISMAPORT_CHAIN_BASE_BLOCK_TIME": "250ms", "PRISMAPORT_CHAIN_BASE_REANCHOR_MARGIN": "0",
		"PRISMAPORT_CHAIN_SOLANA_SCAN_WARM": "40s",
		"PRISMAPORT_SESSION_DEFAULT_EXPIRY": "30m", "PRISMAPORT_SESSION_MIN_EXPIRY": "5m", "PRISMAPORT_SESSION_MAX_EXPIRY": "48h",
		"PRISMAPORT_SESSION_SELECT_WINDOW": "2h",
	} {
		t.Setenv(k, v)
	}
	e, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	s := e.Scan
	if s.Intervals != (scan.Intervals{Hot: 5 * time.Second, Warm: 20 * time.Second, Heartbeat: 30 * time.Minute}) ||
		s.ModeCheck != 500*time.Millisecond || s.ModeCheckActive != 500*time.Millisecond || s.PayerActive != 45*time.Second || s.WatchAfterExpiry != 48*time.Hour ||
		len(s.LateSchedule) != 2 || s.LateSchedule[1] != (scan.Step{Until: 48 * time.Hour, Every: 6 * time.Hour}) ||
		s.HeartbeatQuietAfter != 0 {
		t.Fatalf("scan %+v", s)
	}
	b := e.Chains["base"]
	if b.Scan != (scan.Intervals{Hot: time.Second, Warm: 10 * time.Second, Heartbeat: 2 * time.Hour}) || b.BlockTime != 250*time.Millisecond ||
		b.ReanchorMargin != 0 {
		t.Fatalf("base %+v %s %d", b.Scan, b.BlockTime, b.ReanchorMargin)
	}
	// A chain inherits every interval it does not override.
	if got := e.Solana["solana"].Scan; got != (scan.Intervals{Hot: 5 * time.Second, Warm: 40 * time.Second, Heartbeat: 30 * time.Minute}) {
		t.Fatalf("solana %+v", got)
	}
	if e.Sessions != (SessionExpiry{Default: 30 * time.Minute, Min: 5 * time.Minute, Max: 48 * time.Hour, SelectWindow: 2 * time.Hour}) {
		t.Fatalf("sessions %+v", e.Sessions)
	}
}

func TestScanInvalidValues(t *testing.T) {
	t.Setenv("PRISMAPORT_DATABASE_URL", "postgres://x")
	t.Setenv("PRISMAPORT_CHAIN_BASE_RPC_URLS", "https://a.example,https://b.example")
	for _, c := range []struct {
		vars map[string]string
		want string
	}{
		{map[string]string{"PRISMAPORT_SCAN_HOT": "3"}, "must be a duration"},
		{map[string]string{"PRISMAPORT_SCAN_HOT": "499ms"}, "below 500ms"},
		{map[string]string{"PRISMAPORT_SCAN_WARM": "10s"}, "below the hot interval"},
		{map[string]string{"PRISMAPORT_SCAN_IDLE_HEARTBEAT": "30s"}, "below the warm interval"},
		{map[string]string{"PRISMAPORT_SCAN_MODE_CHECK": "50ms"}, "PRISMAPORT_SCAN_MODE_CHECK (50ms) must be between 100ms and 10m0s"},
		{map[string]string{"PRISMAPORT_SCAN_MODE_CHECK": "11m"}, "PRISMAPORT_SCAN_MODE_CHECK (11m0s) must be between 100ms and 10m0s"},
		{map[string]string{"PRISMAPORT_SCAN_MODE_CHECK_ACTIVE": "3m"}, "PRISMAPORT_SCAN_MODE_CHECK_ACTIVE (3m0s) must be between 100ms and PRISMAPORT_SCAN_MODE_CHECK (2m0s)"},
		{map[string]string{"PRISMAPORT_SCAN_MODE_CHECK_ACTIVE": "50ms"}, "PRISMAPORT_SCAN_MODE_CHECK_ACTIVE (50ms) must be between"},
		{map[string]string{"PRISMAPORT_SCAN_MODE_CHECK_ACTIVE": "x"}, "PRISMAPORT_SCAN_MODE_CHECK_ACTIVE must be a duration"},
		{map[string]string{"PRISMAPORT_SCAN_PAYER_ACTIVE": "9s"}, "at least 10s"},
		{map[string]string{"PRISMAPORT_SCAN_HEARTBEAT_QUIET_AFTER": "-1h"}, "0 (always beat) or positive"},
		{map[string]string{"PRISMAPORT_WATCH_AFTER_EXPIRY": "-1h"}, "between 0"},
		{map[string]string{"PRISMAPORT_WATCH_AFTER_EXPIRY": "169h"}, "between 0"},
		{map[string]string{"PRISMAPORT_WATCH_AFTER_EXPIRY": "12h"}, "after PRISMAPORT_WATCH_AFTER_EXPIRY"},
		{map[string]string{"PRISMAPORT_LATE_SCHEDULE": "1h:20m,1h:1h"}, "strictly increasing"},
		{map[string]string{"PRISMAPORT_LATE_SCHEDULE": "2h:20m,1h:1h"}, "strictly increasing"},
		{map[string]string{"PRISMAPORT_LATE_SCHEDULE": "1h:0s"}, "must be positive"},
		{map[string]string{"PRISMAPORT_LATE_SCHEDULE": "1h"}, "<until>:<every>"},
		{map[string]string{"PRISMAPORT_LATE_SCHEDULE": "1x:20m"}, "late schedule step"},
		{map[string]string{"PRISMAPORT_LATE_SCHEDULE": " , "}, "<until>:<every>"},
		{map[string]string{"PRISMAPORT_CHAIN_BASE_SCAN_HOT": "100ms"}, "PRISMAPORT_CHAIN_BASE_SCAN_*"},
		{map[string]string{"PRISMAPORT_CHAIN_BASE_SCAN_WARM": "1s"}, "below the hot interval"},
		{map[string]string{"PRISMAPORT_CHAIN_BASE_SCAN_IDLE_HEARTBEAT": "x"}, "must be a duration"},
		{map[string]string{"PRISMAPORT_CHAIN_BASE_BLOCK_TIME": "0s"}, "BLOCK_TIME must be positive"},
		{map[string]string{"PRISMAPORT_CHAIN_BASE_REANCHOR_MARGIN": "-1"}, "REANCHOR_MARGIN"},
		{map[string]string{"PRISMAPORT_SESSION_MIN_EXPIRY": "2h", "PRISMAPORT_SESSION_DEFAULT_EXPIRY": "3h", "PRISMAPORT_SESSION_MAX_EXPIRY": "1h"}, "is above"},
		{map[string]string{"PRISMAPORT_SESSION_DEFAULT_EXPIRY": "30s"}, "must be between"},
		{map[string]string{"PRISMAPORT_SESSION_DEFAULT_EXPIRY": "25h"}, "must be between"},
		{map[string]string{"PRISMAPORT_SESSION_MAX_EXPIRY": "0s"}, "must be positive"},
		{map[string]string{"PRISMAPORT_SESSION_SELECT_WINDOW": "59s"}, "PRISMAPORT_SESSION_SELECT_WINDOW (59s) must be between"},
		{map[string]string{"PRISMAPORT_SESSION_SELECT_WINDOW": "25h"}, "PRISMAPORT_SESSION_SELECT_WINDOW (25h0m0s) must be between"},
		{map[string]string{"PRISMAPORT_SESSION_SELECT_WINDOW": "1x"}, "PRISMAPORT_SESSION_SELECT_WINDOW"},
	} {
		t.Run(strings.Join(keys(c.vars), "+"), func(t *testing.T) {
			for k, v := range c.vars {
				t.Setenv(k, v)
			}
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("%v: %v, want %q", c.vars, err, c.want)
			}
		})
	}
}

// WATCH_AFTER_EXPIRY=0 turns scheduled late checks off whatever the schedule says.
func TestNoLateChecks(t *testing.T) {
	t.Setenv("PRISMAPORT_DATABASE_URL", "postgres://x")
	t.Setenv("PRISMAPORT_WATCH_AFTER_EXPIRY", "0s")
	e, err := Load()
	if err != nil || len(e.Scan.LateSchedule) != 0 || e.Scan.LateSchedule == nil {
		t.Fatalf("%v %#v", err, e.Scan.LateSchedule)
	}
	if h := e.Scan.ExpiryHold(); h != 0 {
		t.Fatalf("expiry hold %s with late checks off", h)
	}
}

// Expiry waits for the first late check (1 min after expiry by default) plus slack (ADR-0008 H-3).
func TestExpiryHoldFollowsTheLateSchedule(t *testing.T) {
	t.Setenv("PRISMAPORT_DATABASE_URL", "postgres://x")
	e, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if h := e.Scan.ExpiryHold(); h != 5*time.Minute {
		t.Fatalf("default expiry hold %s", h)
	}
	t.Setenv("PRISMAPORT_LATE_SCHEDULE", "30m:3m,23h30m:1h")
	if e, err = Load(); err != nil {
		t.Fatal(err)
	}
	if h := e.Scan.ExpiryHold(); h != 7*time.Minute {
		t.Fatalf("expiry hold %s for a 3m first step", h)
	}
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, strings.TrimPrefix(k, "PRISMAPORT_"))
	}
	return out
}
