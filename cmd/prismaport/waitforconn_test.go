package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestWaitForConn(t *testing.T) {
	defer func(w, r time.Duration) { connWait, connRetry = w, r }(connWait, connRetry)
	// A long connWait for every case but the one that waits it out: a short one fails the others
	// when the machine is loaded and the scheduler stalls the retry loop past it.
	connWait, connRetry = 30*time.Second, time.Millisecond
	full := &pgconn.PgError{Code: "XX000", Message: "(EMAXCONNSESSION) max clients reached in session mode"}
	other := errors.New("password authentication failed")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	calls := 0
	err := waitForConn(ctx, log, "test", func() error {
		if calls++; calls < 3 {
			return full
		}
		return nil
	})
	if err != nil || calls != 3 {
		t.Fatalf("frees up on the third try: err %v after %d calls", err, calls)
	}

	calls = 0
	if err := waitForConn(ctx, log, "test", func() error { calls++; return other }); !errors.Is(err, other) || calls != 1 {
		t.Fatalf("another error returns at once: err %v after %d calls", err, calls)
	}

	connWait = 500 * time.Millisecond
	calls = 0
	if err := waitForConn(ctx, log, "test", func() error { calls++; return full }); !errors.Is(err, full) || calls < 2 {
		t.Fatalf("a limit that never frees up returns its error after connWait: err %v after %d calls", err, calls)
	}

	connWait = 30 * time.Second
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	calls = 0
	if err := waitForConn(cctx, log, "test", func() error { calls++; return full }); !errors.Is(err, full) || calls != 1 {
		t.Fatalf("a cancelled context stops the wait: err %v after %d calls", err, calls)
	}
}
