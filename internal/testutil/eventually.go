// Package testutil holds the small helpers tests share across packages. It imports only the
// standard library, so any package's tests can use it without an import cycle.
package testutil

import (
	"testing"
	"time"
)

// PollEvery is how often Eventually checks its condition.
const PollEvery = 10 * time.Millisecond

// Eventually checks cond every PollEvery until it holds, and fails the test naming what it
// waited for when timeout passes first. It is for waiting on work another goroutine or process
// does; a test that drives its own fakes should sync on them instead.
func Eventually(t testing.TB, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	EventuallyEvery(t, timeout, PollEvery, what, cond)
}

// EventuallyEvery is Eventually with its own poll interval, for a cond that does work on each
// call (mines a block, ticks a loop) and so sets the pace of an external process.
func EventuallyEvery(t testing.TB, timeout, every time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		time.Sleep(every)
	}
}
