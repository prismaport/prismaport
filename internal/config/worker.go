package config

import (
	"fmt"
	"time"

	"github.com/prismaport/prismaport/internal/store"
	"github.com/prismaport/prismaport/internal/worker"
)

// WorkerEnv is the worker's idle/active pacing (plan B): each loop ticks
// at its idle interval, or its active one for IdleAfter after the last sign of work. Every loop's
// two intervals and the wake listener are set by variable, so a deployment can trade database
// statements for latency without a release.
type WorkerEnv struct {
	SequencerIdle, SequencerActive       time.Duration
	WebhooksIdle, WebhooksActive         time.Duration
	NoticesIdle, NoticesActive           time.Duration
	ExpiryIdle, ExpiryActive             time.Duration
	TOTPRemovalsIdle, TOTPRemovalsActive time.Duration
	KeyExpiryIdle, KeyExpiryActive       time.Duration
	SandboxIdle, SandboxActive           time.Duration
	IdempotencyIdle, IdempotencyActive   time.Duration
	IdleAfter                            time.Duration
	// WakePing is how long the wake listener's connection may stay silent before it is pinged;
	// WakeMaxBackoff bounds the wait before it listens again after a failure.
	WakePing, WakeMaxBackoff time.Duration
}

// Bounds of the worker's intervals. The idle interval of a loop that reacts to payments is also
// the safety net while the wake listener is down, so it stays within maxIdleInterval; the
// housekeeping loops (retention, expiries of keys and two-factor removals) may wait up to a day.
// An active interval is at least the shortest and at most the idle one.
const (
	minLoopInterval     = 100 * time.Millisecond
	maxIdleInterval     = 10 * time.Minute
	maxHousekeepingIdle = 24 * time.Hour
	minIdleAfter        = time.Minute
	maxIdleAfter        = time.Hour
	minWakePing         = 10 * time.Second
	maxWakePing         = 10 * time.Minute
	minWakeBackoff      = time.Second
	maxWakeBackoff      = 10 * time.Minute
)

// Defaults of the wake listener (store.WakeConfig's).
const (
	DefaultWakePing       = time.Minute
	DefaultWakeMaxBackoff = time.Minute
)

// DefaultWorker is the worker pacing when no variable is set.
func DefaultWorker() WorkerEnv {
	return WorkerEnv{
		SequencerIdle: worker.DefaultSequenceEvery, SequencerActive: worker.DefaultSequenceActive,
		WebhooksIdle: worker.DefaultDispatchEvery, WebhooksActive: worker.DefaultDispatchActive,
		NoticesIdle: worker.DefaultNoticeEvery, NoticesActive: worker.DefaultNoticeActive,
		ExpiryIdle: worker.DefaultExpireEvery, ExpiryActive: worker.DefaultExpireActive,
		TOTPRemovalsIdle: worker.DefaultTOTPRemovalEvery, TOTPRemovalsActive: worker.DefaultTOTPRemovalEvery,
		KeyExpiryIdle: worker.DefaultKeyExpiryEvery, KeyExpiryActive: worker.DefaultKeyExpiryEvery,
		SandboxIdle: worker.DefaultSandboxEvery, SandboxActive: worker.DefaultSandboxEvery,
		IdempotencyIdle: worker.DefaultIdempotencyEvery, IdempotencyActive: worker.DefaultIdempotencyEvery,
		IdleAfter: worker.DefaultIdleAfter, WakePing: DefaultWakePing, WakeMaxBackoff: DefaultWakeMaxBackoff,
	}
}

// loadPair reads one loop's idle and active intervals over the defaults in *idle and *active and
// applies the bounds (idle at most maxIdle). With the active variable unset, the active interval
// follows a set idle one when followIdle (the housekeeping loops: one interval unless both are
// set), and is otherwise taken down to an idle one set below it.
func loadPair(idleName, activeName string, maxIdle time.Duration, followIdle bool, idle, active *time.Duration) error {
	var err error
	if *idle, err = durationOr(idleName, *idle); err != nil {
		return err
	}
	v, set, err := duration(activeName)
	switch {
	case err != nil:
		return err
	case set:
		*active = v
	case followIdle:
		*active = *idle
	default:
		*active = min(*active, *idle)
	}
	switch {
	case *idle < minLoopInterval || *idle > maxIdle:
		return fmt.Errorf("%s (%s) must be between %s and %s", idleName, *idle, minLoopInterval, maxIdle)
	case *active < minLoopInterval || *active > *idle:
		return fmt.Errorf("%s (%s) must be between %s and %s (%s)", activeName, *active, minLoopInterval, idleName, *idle)
	}
	return nil
}

// boundedDuration reads the duration variable name over *dst and checks it is within [lo, hi].
func boundedDuration(name string, dst *time.Duration, lo, hi time.Duration) error {
	var err error
	if *dst, err = durationOr(name, *dst); err != nil {
		return err
	}
	if *dst < lo || *dst > hi {
		return fmt.Errorf("%s (%s) must be between %s and %s", name, *dst, lo, hi)
	}
	return nil
}

// loadWorker reads PRISMAPORT_WORKER_<LOOP>_IDLE and _ACTIVE, PRISMAPORT_WORKER_IDLE_AFTER and
// PRISMAPORT_WORKER_WAKE_PING and _WAKE_MAX_BACKOFF.
func loadWorker() (WorkerEnv, error) {
	w := DefaultWorker()
	for _, l := range []struct {
		loop         string
		maxIdle      time.Duration
		followIdle   bool
		idle, active *time.Duration
	}{
		{"SEQUENCER", maxIdleInterval, false, &w.SequencerIdle, &w.SequencerActive},
		{"WEBHOOKS", maxIdleInterval, false, &w.WebhooksIdle, &w.WebhooksActive},
		{"NOTICES", maxIdleInterval, false, &w.NoticesIdle, &w.NoticesActive},
		{"EXPIRY", maxIdleInterval, false, &w.ExpiryIdle, &w.ExpiryActive},
		{"TOTP_REMOVALS", maxHousekeepingIdle, true, &w.TOTPRemovalsIdle, &w.TOTPRemovalsActive},
		{"KEY_EXPIRY", maxHousekeepingIdle, true, &w.KeyExpiryIdle, &w.KeyExpiryActive},
		{"SANDBOX_RETENTION", maxHousekeepingIdle, true, &w.SandboxIdle, &w.SandboxActive},
		{"IDEMPOTENCY_RETENTION", maxHousekeepingIdle, true, &w.IdempotencyIdle, &w.IdempotencyActive},
	} {
		if err := loadPair("PRISMAPORT_WORKER_"+l.loop+"_IDLE", "PRISMAPORT_WORKER_"+l.loop+"_ACTIVE", l.maxIdle, l.followIdle, l.idle, l.active); err != nil {
			return w, err
		}
	}
	for _, v := range []struct {
		name   string
		dst    *time.Duration
		lo, hi time.Duration
	}{
		{"PRISMAPORT_WORKER_IDLE_AFTER", &w.IdleAfter, minIdleAfter, maxIdleAfter},
		{"PRISMAPORT_WORKER_WAKE_PING", &w.WakePing, minWakePing, maxWakePing},
		{"PRISMAPORT_WORKER_WAKE_MAX_BACKOFF", &w.WakeMaxBackoff, minWakeBackoff, maxWakeBackoff},
	} {
		if err := boundedDuration(v.name, v.dst, v.lo, v.hi); err != nil {
			return w, err
		}
	}
	return w, nil
}

// Config is the worker pacing as worker.Config fields (the rest of the Config is left zero).
func (w WorkerEnv) Config() worker.Config {
	return worker.Config{SequenceEvery: w.SequencerIdle, SequenceActive: w.SequencerActive,
		DispatchEvery: w.WebhooksIdle, DispatchActive: w.WebhooksActive, NoticeEvery: w.NoticesIdle, NoticeActive: w.NoticesActive,
		ExpireEvery: w.ExpiryIdle, ExpireActive: w.ExpiryActive,
		TOTPRemovalEvery: w.TOTPRemovalsIdle, TOTPRemovalActive: w.TOTPRemovalsActive,
		KeyExpiryEvery: w.KeyExpiryIdle, KeyExpiryActive: w.KeyExpiryActive,
		SandboxEvery: w.SandboxIdle, SandboxActive: w.SandboxActive,
		IdempotencyEvery: w.IdempotencyIdle, IdempotencyActive: w.IdempotencyActive, IdleAfter: w.IdleAfter}
}

// Wake is the wake listener's configuration (store.ListenWake).
func (w WorkerEnv) Wake() store.WakeConfig {
	return store.WakeConfig{Ping: w.WakePing, MaxBackoff: w.WakeMaxBackoff}
}
