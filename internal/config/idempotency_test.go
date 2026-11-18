package config

import (
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/tuning"
)

func TestIdempotencyTTL(t *testing.T) {
	for _, c := range []struct {
		env  string
		want time.Duration
		err  bool
	}{
		{"", tuning.DefaultIdempotencyTTL, false},
		{"72h", 72 * time.Hour, false},
		{"24h", MinIdempotencyTTL, false},
		{"23h", 0, true},
		{"soon", 0, true},
	} {
		t.Setenv("PRISMAPORT_IDEMPOTENCY_TTL", c.env)
		got, err := loadIdempotencyTTL()
		if (err != nil) != c.err || (!c.err && got != c.want) {
			t.Errorf("PRISMAPORT_IDEMPOTENCY_TTL=%q: %s %v", c.env, got, err)
		}
	}
}

func TestStepTimeout(t *testing.T) {
	for _, c := range []struct {
		env  string
		want time.Duration
		err  bool
	}{
		{"", tuning.DefaultStepTimeout, false},
		{"10m", 10 * time.Minute, false},
		{"30s", MinStepTimeout, false},
		{"5s", 0, true},
		{"later", 0, true},
	} {
		t.Setenv("PRISMAPORT_WORKER_STEP_TIMEOUT", c.env)
		got, err := loadStepTimeout()
		if (err != nil) != c.err || (!c.err && got != c.want) {
			t.Errorf("PRISMAPORT_WORKER_STEP_TIMEOUT=%q: %s %v", c.env, got, err)
		}
	}
}
