package worker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// A step stuck on a call that never returns fails at StepTimeout, saying so, instead of stalling
// its loop forever.
func TestStepWithinCutsAHungStep(t *testing.T) {
	hung := func(ctx context.Context) (int, error) {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	_, err := StepWithin(t.Context(), 20*time.Millisecond, nil, nil, "hung", hung)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("err %v", err)
	}
	// A shutdown is not reported as a timeout.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := StepWithin(ctx, time.Minute, nil, nil, "hung", hung); !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "timeout") {
		t.Fatalf("err %v", err)
	}
}

// The worker runs every step under its timeout, except a background step: its work outlives the
// step and bounds itself.
func TestLoopsRunUnderTheStepTimeout(t *testing.T) {
	deadlines := make(chan bool, 2)
	probe := func(ctx context.Context) (int, error) {
		_, ok := ctx.Deadline()
		deadlines <- ok
		return 0, ErrSkipped
	}
	ctx, cancel := context.WithCancel(t.Context())
	w := New(nil, nil, nil, Config{Role: RoleChain, StepTimeout: time.Minute, Loops: []Loop{
		{Name: "plain", Every: time.Hour, Step: probe},
		{Name: "background", Every: time.Hour, Step: probe, Background: true},
	}}, nil)
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	got := map[bool]int{}
	for range 2 {
		got[<-deadlines]++
	}
	cancel()
	<-done
	if got[true] != 1 || got[false] != 1 {
		t.Fatalf("deadlines %v", got)
	}
}
