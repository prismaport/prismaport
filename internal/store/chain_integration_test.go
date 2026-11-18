//go:build integration

package store_test

import (
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/store"
)

func TestChainStatus(t *testing.T) {
	t.Parallel()
	st := newStore(t)
	ctx := t.Context()
	since := time.Date(2026, 10, 23, 18, 0, 0, 0, time.UTC)
	if err := st.SetChainStatus(ctx, store.ChainStatus{Chain: "base", State: "halted", Reason: "disagree", Since: since}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetChainStatus(ctx, store.ChainStatus{Chain: "base", State: "ok", Since: since.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetChainStatus(ctx, "base")
	if err != nil || got.State != "ok" || got.Reason != "" || !got.Since.Equal(since.Add(time.Minute)) {
		t.Fatalf("status %+v %v", got, err)
	}
	if err := st.SetChainStatus(ctx, store.ChainStatus{Chain: "base", State: "broken", Since: since}); err == nil {
		t.Fatal("unknown state accepted")
	}
}
