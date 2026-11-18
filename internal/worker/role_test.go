package worker

import (
	"context"
	"slices"
	"testing"
	"time"
)

func loopNames(role Role) []string {
	step := func(context.Context) (int, error) { return 0, nil }
	w := New(nil, nil, nil, Config{Role: role, Loops: []Loop{{Name: "chains", Every: time.Second, Step: step}}}, nil)
	var names []string
	for _, l := range w.loops() {
		names = append(names, l.Name)
	}
	slices.Sort(names)
	return names
}

func TestEachRoleRunsItsOwnLoops(t *testing.T) {
	events := []string{"api-key-expiry", "expiry", "idempotency-retention", "sandbox-retention", "sequencer", "totp-removals", "webhook-notices", "webhooks"}
	if got := loopNames(RoleEvents); !slices.Equal(got, events) {
		t.Fatalf("events: got %v, want %v", got, events)
	}
	if got := loopNames(RoleChain); !slices.Equal(got, []string{"chains"}) {
		t.Fatalf("chain: got %v", got)
	}
}

// A loop in neither role would silently stop when the worker is deployed as two processes.
func TestTheTwoRolesTogetherRunEveryLoopOnce(t *testing.T) {
	both := append(loopNames(RoleChain), loopNames(RoleEvents)...)
	slices.Sort(both)
	if all := loopNames(RoleAll); !slices.Equal(both, all) {
		t.Fatalf("chain + events = %v, all = %v", both, all)
	}
	if empty := loopNames(""); !slices.Equal(empty, loopNames(RoleAll)) {
		t.Fatalf("the default role runs %v, want every loop", empty)
	}
}
