//go:build integration

package service_test

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/service"
)

// slowEndpoint holds every request until released, then answers 200.
type slowEndpoint struct {
	calls   atomic.Int32
	arrived chan struct{}
	release chan struct{}
	srv     *httptest.Server
}

func newSlowEndpoint(t *testing.T) *slowEndpoint {
	s := &slowEndpoint{arrived: make(chan struct{}, 10), release: make(chan struct{})}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.calls.Add(1)
		s.arrived <- struct{}{}
		select {
		case <-s.release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { s.unblock(); s.srv.Close() })
	return s
}

func (s *slowEndpoint) unblock() {
	select {
	case <-s.release:
	default:
		close(s.release)
	}
}

// fastEndpoint answers 200 at once and reports each request.
func fastEndpoint(t *testing.T) (*httptest.Server, <-chan struct{}) {
	got := make(chan struct{}, 10)
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { got <- struct{}{} }))
	t.Cleanup(srv.Close)
	return srv, got
}

// otherMerchant creates a second merchant and registers its webhook endpoint at url.
func otherMerchant(t *testing.T, app *apptest.App, url string) (key string) {
	t.Helper()
	seed, err := app.Svc.SeedMerchant(t.Context(), service.SeedInput{Name: "Other shop",
		EVMDestination: "0x1111111111111111111111111111111111111111", Actor: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if r := app.Do("POST", "/v1/webhook_endpoints", seed.Plaintext, map[string]any{"url": url}); r.Status != http.StatusOK {
		t.Fatalf("endpoint: %d %s", r.Status, r.Body)
	}
	return seed.Plaintext
}

// paid creates a session with key, records a payment on it and sequences its event.
func paid(t *testing.T, app *apptest.App, key, tx string) {
	t.Helper()
	s := createSession(t, app, key, `{"amount":"5.00"}`, "parallel-"+tx)
	pay(t, app, s, "base-sepolia", tx, usdc("1"), false)
	sequence(t, app)
}

func arrives(t *testing.T, got <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s never arrived", what)
	}
}

// A merchant whose endpoint hangs does not hold up another merchant's webhooks claimed in the
// same batch (ADR-0008 A-5): endpoints are sent to in parallel.
func TestWebhookSlowEndpointDoesNotDelayAnotherMerchantInTheBatch(t *testing.T) {
	app := apptest.New(t, apptest.Options{})
	slow := newSlowEndpoint(t)
	registerEndpoint(t, app, slow.srv.URL)
	fast, got := fastEndpoint(t)
	other := otherMerchant(t, app, fast.URL)
	paid(t, app, app.TestKey, "0x1") // claimed first: due first
	app.Clock.Advance(time.Second)
	paid(t, app, other, "0x2")
	app.Clock.Advance(time.Second)

	done := make(chan runResult, 1)
	go func() {
		n, err := app.Dispatcher.RunOnce(t.Context())
		done <- runResult{n, err}
	}()
	arrives(t, slow.arrived, "the slow merchant's delivery")
	arrives(t, got, "the other merchant's delivery, while the slow endpoint hangs")
	slow.unblock()
	r := <-done

	if r.err != nil || r.n != 2 {
		t.Fatalf("RunOnce: %d %v", r.n, r.err)
	}
}

// In the worker, Start never waits for a delivery: while one endpoint hangs, the next steps
// deliver other merchants' new webhooks, and never send a second request to the busy endpoint.
func TestWebhookStartKeepsDeliveringWhileAnEndpointHangs(t *testing.T) {
	app := apptest.New(t, apptest.Options{})
	slow := newSlowEndpoint(t)
	registerEndpoint(t, app, slow.srv.URL)
	fast, got := fastEndpoint(t)
	other := otherMerchant(t, app, fast.URL)
	d := app.Dispatcher
	paid(t, app, app.TestKey, "0x1")

	if n, err := d.Start(t.Context()); err != nil || n != 1 {
		t.Fatalf("first Start: %d %v", n, err)
	}
	arrives(t, slow.arrived, "the slow merchant's delivery")
	paid(t, app, app.TestKey, "0x2") // a second delivery to the busy endpoint
	paid(t, app, other, "0x3")

	start := time.Now()
	if n, err := d.Start(t.Context()); err != nil || n != 1 {
		t.Fatalf("Start while the endpoint hangs: %d %v, want only the other merchant's delivery", n, err)
	}
	arrives(t, got, "the other merchant's delivery")
	if waited := time.Since(start); waited > 2*time.Second {
		t.Fatalf("the other merchant's delivery waited %v", waited)
	}
	if n, err := d.Start(t.Context()); err != nil || n != 0 {
		t.Fatalf("Start with only the busy endpoint due: %d %v", n, err)
	}
	if c := slow.calls.Load(); c != 1 {
		t.Fatalf("the busy endpoint got %d requests at once", c)
	}

	slow.unblock()
	d.Wait()
	if n, err := d.Start(t.Context()); err != nil || n != 1 {
		t.Fatalf("Start once the endpoint answered: %d %v", n, err)
	}
	arrives(t, slow.arrived, "the busy endpoint's second delivery")
	d.Wait()
}
