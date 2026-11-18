//go:build integration

package api_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/checkout"
	"github.com/prismaport/prismaport/internal/service"
	"github.com/prismaport/prismaport/internal/store"
)

// stalledBlacklist answers every lookup at once until stalled; then a lookup hangs until its
// context ends (or the test releases it).
type stalledBlacklist struct {
	stalled atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func newStalledBlacklist() *stalledBlacklist {
	return &stalledBlacklist{entered: make(chan struct{}, 256), release: make(chan struct{})}
}

func (b *stalledBlacklist) IsBlacklisted(ctx context.Context, _, _, _ string) (bool, error) {
	if !b.stalled.Load() {
		return false, nil
	}
	b.entered <- struct{}{}
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-b.release:
		return false, errors.New("released by the test")
	}
}

func (b *stalledBlacklist) waitEntered(t *testing.T, n int) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for i := 0; i < n; i++ {
		select {
		case <-b.entered:
		case <-deadline:
			t.Fatalf("only %d of %d lookups started: the rest wait for a pool connection", i, n)
		}
	}
}

// A stalled blacklist provider holds no database connection or lock: more first reveals of an EVM
// network (D48) than the pool has connections wait on it at once while other requests still
// answer, and each reveal fails closed (retryable, nothing revealed) within the lookup's time
// bound.
func TestStalledBlacklistDoesNotExhaustThePool(t *testing.T) {
	bl := newStalledBlacklist()
	app := apptest.New(t, apptest.Options{Blacklist: bl,
		Checkout: func(svc *service.Service, st *store.Store) *checkout.Checkout { return checkout.New(svc, st, nil) }})
	t.Cleanup(func() { close(bl.release) })
	product := app.Link(app.TestKey)
	link := apiLink(t, app, app.TestKey, product, nil)
	n := int(app.Store.Pool().Config().MaxConns) + 2
	ids := make([]string, n)
	for i := range n {
		key := fmt.Sprintf("stalled-%d", i)
		var r apptest.Response
		if i%2 == 0 {
			r = app.Session(app.TestKey, map[string]any{"amount": "5.00"}, idem(key)...)
		} else {
			r = openLink(app, link, key, map[string]any{"amount": "5.00"})
		}
		if r.Status != http.StatusOK {
			t.Fatalf("session %d: %d %s", i, r.Status, r.Body)
		}
		ids[i] = r.JSON(t)["id"].(string)
	}

	bl.stalled.Store(true)
	start := time.Now()
	results := make([]apptest.Response, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i%2 == 0 {
				results[i] = app.Do("POST", "/v1/sessions/"+ids[i]+"/reveal", app.TestKey, map[string]any{"chain": "base-sepolia"})
			} else {
				results[i] = app.CheckoutReveal(ids[i], "base-sepolia")
			}
		}()
	}
	bl.waitEntered(t, n)
	other := make(chan apptest.Response, 1)
	go func() { other <- app.Do("GET", "/v1/sessions/"+ids[0], app.TestKey, nil) }()
	var got apptest.Response
	select {
	case got = <-other:
	case <-time.After(2 * time.Second):
		t.Fatal("a read waited behind the stalled reveals: the pool is exhausted")
	}
	wg.Wait()
	elapsed := time.Since(start)

	if got.Status != http.StatusOK {
		t.Fatalf("read during the stall: %d %s", got.Status, got.Body)
	}
	for i, r := range results {
		if r.Status != http.StatusServiceUnavailable || r.ErrorCode(t) != "service_unavailable" {
			t.Errorf("stalled reveal %d: %d %s", i, r.Status, r.Body)
		}
	}
	if elapsed > 8*time.Second {
		t.Fatalf("stalled reveals took %s; the lookup bound did not hold", elapsed)
	}
	if ses, err := app.Store.LoadSession(t.Context(), ids[0]); err != nil || len(ses.Reveals) != 0 || ses.PaymentStartedAt != nil {
		t.Fatalf("a failed reveal revealed: %+v %v", ses.Reveals, err)
	}
	bl.stalled.Store(false)
	if r := app.CheckoutReveal(ids[0], "base-sepolia"); r.Status != http.StatusOK {
		t.Fatalf("retry after the provider is back: %d %s", r.Status, r.Body)
	}
}

// A blacklist outage does not stop sessions from opening (nothing is read then, D48): only the
// first reveal of an EVM network fails closed. A network already revealed answers again without a
// new lookup, and Solana networks need none.
func TestBlacklistOutageOnlyBlocksFirstEVMReveals(t *testing.T) {
	app := apptest.New(t, apptest.Options{})
	shown := app.Session(app.TestKey, map[string]any{"amount": "5.00"}, idem("shown")...)
	if shown.Status != http.StatusOK {
		t.Fatalf("first: %d %s", shown.Status, shown.Body)
	}
	shownID := shown.JSON(t)["id"].(string)
	app.Reveal(app.TestKey, shownID, "base-sepolia")

	app.Blacklist.Down(true)
	fresh := app.Session(app.TestKey, map[string]any{"amount": "5.00"}, idem("fresh")...)
	if fresh.Status != http.StatusOK {
		t.Fatalf("a session during the outage: %d %s", fresh.Status, fresh.Body)
	}
	freshID := fresh.JSON(t)["id"].(string)
	reveal := func(id, chainID string) apptest.Response {
		return app.Do("POST", "/v1/sessions/"+id+"/reveal", app.TestKey, map[string]any{"chain": chainID})
	}
	if r := reveal(freshID, "base-sepolia"); r.Status != http.StatusServiceUnavailable || r.ErrorCode(t) != "service_unavailable" {
		t.Fatalf("a first EVM reveal during the outage must fail closed: %d %s", r.Status, r.Body)
	}
	if r := reveal(freshID, "solana-devnet"); r.Status != http.StatusOK {
		t.Fatalf("a Solana reveal during the outage: %d %s", r.Status, r.Body)
	}
	if r := reveal(shownID, "base-sepolia"); r.Status != http.StatusOK {
		t.Fatalf("a network already revealed during the outage: %d %s", r.Status, r.Body)
	}
}
