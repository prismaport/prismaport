package solwatch

import (
	"sync"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/chainwatch"
	"github.com/prismaport/prismaport/internal/testutil"
)

// statusRecorder is an OnStatus whose Halted calls block until released, standing in for a slow
// write of the status; it keeps the statuses in the order their writes finished.
type statusRecorder struct {
	mu        sync.Mutex
	persisted []chainwatch.State
	entered   chan struct{}
	release   chan struct{}
}

func newStatusRecorder() *statusRecorder {
	return &statusRecorder{entered: make(chan struct{}, 1), release: make(chan struct{})}
}

func (r *statusRecorder) onStatus(s chainwatch.Status) {
	if s.State == chainwatch.Halted {
		r.entered <- struct{}{}
		<-r.release
	}
	r.mu.Lock()
	r.persisted = append(r.persisted, s.State)
	r.mu.Unlock()
}

func (r *statusRecorder) last() chainwatch.State {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.persisted[len(r.persisted)-1]
}

// A Halted whose write is slow is never persisted after the OK that followed it.
func TestStatusChangesArePersistedInOrder(t *testing.T) {
	r := newStatusRecorder()
	q := NewQuorum("base", nil, chainwatch.Options{OnStatus: r.onStatus})
	var wg sync.WaitGroup
	wg.Go(func() { q.setStatus(chainwatch.Halted, "providers disagree") })
	<-r.entered
	wg.Go(func() { q.setStatus(chainwatch.OK, "") })
	// The OK change is made (its write waits behind the Halted one) while the Halted write is in flight.
	testutil.Eventually(t, 5*time.Second, "the OK change", func() bool { return q.Status().State == chainwatch.OK })
	close(r.release)
	wg.Wait()
	if q.Status().State != chainwatch.OK || r.last() != chainwatch.OK {
		t.Fatalf("status %s, persisted %v", q.Status().State, r.persisted)
	}
}
