package testutil

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// fakeT records the failure Eventually reports and stops its goroutine the way t.Fatalf does.
type fakeT struct {
	testing.TB
	failed string
}

func (f *fakeT) Helper() {}

func (f *fakeT) Fatalf(format string, args ...any) {
	f.failed = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

// run calls Eventually on a fakeT in its own goroutine, so the fake's Goexit ends only that one.
func run(timeout time.Duration, cond func() bool) string {
	f := &fakeT{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		Eventually(f, timeout, "the flag", cond)
	}()
	<-done
	return f.failed
}

func TestEventuallyReturnsOnceTheConditionHolds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		start := time.Now()
		if failed := run(time.Second, func() bool { calls++; return calls == 3 }); failed != "" {
			t.Fatalf("failed: %s", failed)
		}
		if calls != 3 || time.Since(start) != 2*PollEvery {
			t.Fatalf("%d calls in %s", calls, time.Since(start))
		}
	})
}

func TestEventuallyFailsNamingWhatItWaitedFor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		failed := run(time.Second, func() bool { return false })
		if !strings.Contains(failed, "timed out after 1s waiting for the flag") {
			t.Fatalf("failure %q", failed)
		}
		if took := time.Since(start); took < time.Second || took > time.Second+PollEvery {
			t.Fatalf("gave up after %s", took)
		}
	})
}
