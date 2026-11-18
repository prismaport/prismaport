package config

import (
	"strings"
	"testing"
	"time"
)

func loadBase(t *testing.T) (Env, error) {
	t.Helper()
	t.Setenv("PRISMAPORT_DATABASE_URL", "postgres://x")
	return Load()
}

// The approved plan B numbers: idle every 2 minutes (5 for the notices), active within seconds,
// idle again 10 minutes after the last sign of work.
func TestWorkerDefaults(t *testing.T) {
	e, err := loadBase(t)
	if err != nil {
		t.Fatal(err)
	}
	want := WorkerEnv{SequencerIdle: 2 * time.Minute, SequencerActive: 10 * time.Second, WebhooksIdle: 2 * time.Minute,
		WebhooksActive: 5 * time.Second, NoticesIdle: 5 * time.Minute, NoticesActive: time.Minute, ExpiryIdle: 2 * time.Minute,
		ExpiryActive: 30 * time.Second, TOTPRemovalsIdle: 5 * time.Minute, TOTPRemovalsActive: 5 * time.Minute,
		KeyExpiryIdle: 5 * time.Minute, KeyExpiryActive: 5 * time.Minute, SandboxIdle: time.Hour, SandboxActive: time.Hour,
		IdempotencyIdle: time.Hour, IdempotencyActive: time.Hour, IdleAfter: 10 * time.Minute, WakePing: time.Minute,
		WakeMaxBackoff: time.Minute}
	if e.Worker != want {
		t.Fatalf("worker defaults %+v, want %+v", e.Worker, want)
	}
	if e.Scan.ModeCheck != 2*time.Minute || e.Scan.ModeCheckActive != 5*time.Second {
		t.Fatalf("mode check %s / %s", e.Scan.ModeCheck, e.Scan.ModeCheckActive)
	}
	c := e.Worker.Config()
	if c.SequenceEvery != 2*time.Minute || c.SequenceActive != 10*time.Second || c.DispatchEvery != 2*time.Minute ||
		c.DispatchActive != 5*time.Second || c.NoticeEvery != 5*time.Minute || c.NoticeActive != time.Minute ||
		c.ExpireEvery != 2*time.Minute || c.ExpireActive != 30*time.Second || c.IdleAfter != 10*time.Minute ||
		c.TOTPRemovalEvery != 5*time.Minute || c.TOTPRemovalActive != 5*time.Minute || c.KeyExpiryEvery != 5*time.Minute ||
		c.KeyExpiryActive != 5*time.Minute || c.SandboxEvery != time.Hour || c.SandboxActive != time.Hour ||
		c.IdempotencyEvery != time.Hour || c.IdempotencyActive != time.Hour {
		t.Fatalf("worker config %+v", c)
	}
	if w := e.Worker.Wake(); w.Ping != time.Minute || w.MaxBackoff != time.Minute {
		t.Fatalf("wake config %+v", w)
	}
}

func TestWorkerVariables(t *testing.T) {
	for k, v := range map[string]string{
		"PRISMAPORT_WORKER_SEQUENCER_IDLE": "1m", "PRISMAPORT_WORKER_SEQUENCER_ACTIVE": "1s",
		"PRISMAPORT_WORKER_WEBHOOKS_IDLE": "3m", "PRISMAPORT_WORKER_WEBHOOKS_ACTIVE": "3s",
		"PRISMAPORT_WORKER_NOTICES_IDLE": "10m", "PRISMAPORT_WORKER_NOTICES_ACTIVE": "2m",
		"PRISMAPORT_WORKER_EXPIRY_IDLE": "4m", "PRISMAPORT_WORKER_EXPIRY_ACTIVE": "20s",
		"PRISMAPORT_WORKER_IDLE_AFTER": "30m", "PRISMAPORT_SCAN_MODE_CHECK": "10m", "PRISMAPORT_SCAN_MODE_CHECK_ACTIVE": "2s",
		"PRISMAPORT_WORKER_TOTP_REMOVALS_IDLE": "10m", "PRISMAPORT_WORKER_TOTP_REMOVALS_ACTIVE": "1m",
		"PRISMAPORT_WORKER_KEY_EXPIRY_IDLE": "1m", "PRISMAPORT_WORKER_SANDBOX_RETENTION_IDLE": "6h",
		"PRISMAPORT_WORKER_SANDBOX_RETENTION_ACTIVE": "6h", "PRISMAPORT_WORKER_IDEMPOTENCY_RETENTION_IDLE": "24h",
		"PRISMAPORT_WORKER_IDEMPOTENCY_RETENTION_ACTIVE": "2h", "PRISMAPORT_WORKER_WAKE_PING": "30s",
		"PRISMAPORT_WORKER_WAKE_MAX_BACKOFF": "5m",
	} {
		t.Setenv(k, v)
	}
	e, err := loadBase(t)
	if err != nil {
		t.Fatal(err)
	}
	want := WorkerEnv{SequencerIdle: time.Minute, SequencerActive: time.Second, WebhooksIdle: 3 * time.Minute,
		WebhooksActive: 3 * time.Second, NoticesIdle: 10 * time.Minute, NoticesActive: 2 * time.Minute, ExpiryIdle: 4 * time.Minute,
		ExpiryActive: 20 * time.Second, TOTPRemovalsIdle: 10 * time.Minute, TOTPRemovalsActive: time.Minute,
		KeyExpiryIdle: time.Minute, KeyExpiryActive: time.Minute, SandboxIdle: 6 * time.Hour, SandboxActive: 6 * time.Hour,
		IdempotencyIdle: 24 * time.Hour, IdempotencyActive: 2 * time.Hour, IdleAfter: 30 * time.Minute, WakePing: 30 * time.Second,
		WakeMaxBackoff: 5 * time.Minute}
	if e.Worker != want || e.Scan.ModeCheck != 10*time.Minute || e.Scan.ModeCheckActive != 2*time.Second {
		t.Fatalf("worker %+v, mode check %s / %s", e.Worker, e.Scan.ModeCheck, e.Scan.ModeCheckActive)
	}
}

// A housekeeping loop given only its idle interval keeps one interval (not adaptive): its active
// one follows instead of staying at the default.
func TestHousekeepingIdleAloneKeepsOneInterval(t *testing.T) {
	t.Setenv("PRISMAPORT_WORKER_SANDBOX_RETENTION_IDLE", "24h")
	t.Setenv("PRISMAPORT_WORKER_KEY_EXPIRY_IDLE", "1h")
	t.Setenv("PRISMAPORT_WORKER_TOTP_REMOVALS_IDLE", "1m")
	e, err := loadBase(t)
	if err != nil {
		t.Fatal(err)
	}
	w := e.Worker
	if w.SandboxActive != 24*time.Hour || w.KeyExpiryActive != time.Hour || w.TOTPRemovalsActive != time.Minute ||
		w.IdempotencyActive != time.Hour {
		t.Fatalf("housekeeping active %s %s %s %s", w.SandboxActive, w.KeyExpiryActive, w.TOTPRemovalsActive, w.IdempotencyActive)
	}
}

// An idle interval set below the default active one takes it down when the active one is unset:
// the short intervals of local runs keep working with one variable.
func TestWorkerShortIdleTakesTheActiveDown(t *testing.T) {
	t.Setenv("PRISMAPORT_WORKER_SEQUENCER_IDLE", "500ms")
	t.Setenv("PRISMAPORT_SCAN_MODE_CHECK", "1s")
	e, err := loadBase(t)
	if err != nil {
		t.Fatal(err)
	}
	if e.Worker.SequencerActive != 500*time.Millisecond || e.Scan.ModeCheckActive != time.Second {
		t.Fatalf("sequencer active %s, mode check active %s", e.Worker.SequencerActive, e.Scan.ModeCheckActive)
	}
}

func TestWorkerInvalidValues(t *testing.T) {
	for _, c := range []struct {
		vars map[string]string
		want string
	}{
		{map[string]string{"PRISMAPORT_WORKER_SEQUENCER_IDLE": "2"}, "PRISMAPORT_WORKER_SEQUENCER_IDLE must be a duration"},
		{map[string]string{"PRISMAPORT_WORKER_SEQUENCER_IDLE": "50ms"}, "PRISMAPORT_WORKER_SEQUENCER_IDLE (50ms) must be between 100ms and 10m0s"},
		{map[string]string{"PRISMAPORT_WORKER_WEBHOOKS_IDLE": "11m"}, "PRISMAPORT_WORKER_WEBHOOKS_IDLE (11m0s) must be between 100ms and 10m0s"},
		{map[string]string{"PRISMAPORT_WORKER_NOTICES_ACTIVE": "6m"},
			"PRISMAPORT_WORKER_NOTICES_ACTIVE (6m0s) must be between 100ms and PRISMAPORT_WORKER_NOTICES_IDLE (5m0s)"},
		{map[string]string{"PRISMAPORT_WORKER_EXPIRY_ACTIVE": "0s"}, "PRISMAPORT_WORKER_EXPIRY_ACTIVE (0s) must be between"},
		{map[string]string{"PRISMAPORT_WORKER_EXPIRY_ACTIVE": "x"}, "PRISMAPORT_WORKER_EXPIRY_ACTIVE must be a duration"},
		{map[string]string{"PRISMAPORT_WORKER_IDLE_AFTER": "59s"}, "PRISMAPORT_WORKER_IDLE_AFTER (59s) must be between 1m0s and 1h0m0s"},
		{map[string]string{"PRISMAPORT_WORKER_IDLE_AFTER": "2h"}, "PRISMAPORT_WORKER_IDLE_AFTER (2h0m0s) must be between"},
		{map[string]string{"PRISMAPORT_WORKER_SANDBOX_RETENTION_IDLE": "25h"},
			"PRISMAPORT_WORKER_SANDBOX_RETENTION_IDLE (25h0m0s) must be between 100ms and 24h0m0s"},
		{map[string]string{"PRISMAPORT_WORKER_KEY_EXPIRY_ACTIVE": "6m"},
			"PRISMAPORT_WORKER_KEY_EXPIRY_ACTIVE (6m0s) must be between 100ms and PRISMAPORT_WORKER_KEY_EXPIRY_IDLE (5m0s)"},
		{map[string]string{"PRISMAPORT_WORKER_TOTP_REMOVALS_IDLE": "x"}, "PRISMAPORT_WORKER_TOTP_REMOVALS_IDLE must be a duration"},
		{map[string]string{"PRISMAPORT_WORKER_IDEMPOTENCY_RETENTION_ACTIVE": "50ms"}, "PRISMAPORT_WORKER_IDEMPOTENCY_RETENTION_ACTIVE (50ms) must be between"},
		{map[string]string{"PRISMAPORT_WORKER_WAKE_PING": "5s"}, "PRISMAPORT_WORKER_WAKE_PING (5s) must be between 10s and 10m0s"},
		{map[string]string{"PRISMAPORT_WORKER_WAKE_MAX_BACKOFF": "11m"}, "PRISMAPORT_WORKER_WAKE_MAX_BACKOFF (11m0s) must be between 1s and 10m0s"},
	} {
		t.Run(strings.Join(keys(c.vars), "+"), func(t *testing.T) {
			for k, v := range c.vars {
				t.Setenv(k, v)
			}
			if _, err := loadBase(t); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("%v: %v, want %q", c.vars, err, c.want)
			}
		})
	}
}
