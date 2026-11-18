package main

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

// The pooled host is unroutable on purpose: the guard must refuse it before any connection
// attempt, so the test fails fast instead of timing out on a dial.
func TestWorkerAndMigrateRefuseAPooledURL(t *testing.T) {
	t.Setenv("PRISMAPORT_DATABASE_URL", "postgresql://u:p@ep-x-pooler.invalid/neondb?sslmode=require&connect_timeout=1")
	for _, args := range [][]string{{"worker"}, {"worker", "chain"}, {"worker", "events"}, {"migrate", "up"}} {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		err := run(ctx, args, io.Discard)
		cancel()
		if err == nil || !strings.Contains(err.Error(), "transaction pooler") {
			t.Fatalf("%v: want a pooler error, got %v", args, err)
		}
	}
}

// A typo must not start a process running every loop: the role is read before anything connects.
func TestWorkerRefusesAnUnknownRole(t *testing.T) {
	for _, args := range [][]string{{"worker", "mail"}, {"worker", "all"}, {"worker", "chain", "events"}} {
		if err := run(t.Context(), args, io.Discard); err == nil || !strings.Contains(err.Error(), "usage: prismaport worker") {
			t.Fatalf("%v: want a usage error, got %v", args, err)
		}
	}
}
