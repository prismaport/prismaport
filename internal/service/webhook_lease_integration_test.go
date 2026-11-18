//go:build integration

package service_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/webhook"
)

// heldEndpoint holds its first request until released, then answers it 500; later requests get
// 200 at once.
type heldEndpoint struct {
	mu      sync.Mutex
	calls   int
	arrived chan struct{}
	release chan struct{}
	srv     *httptest.Server
}

func newHeldEndpoint(t *testing.T) *heldEndpoint {
	h := &heldEndpoint{arrived: make(chan struct{}), release: make(chan struct{})}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.calls++
		first := h.calls == 1
		h.mu.Unlock()
		if first {
			close(h.arrived)
			<-h.release
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *heldEndpoint) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

type runResult struct {
	n   int
	err error
}

// startHeldRun runs d.RunOnce in the background and returns once its request is in flight.
func startHeldRun(t *testing.T, d *webhook.Dispatcher, h *heldEndpoint) <-chan runResult {
	t.Helper()
	done := make(chan runResult, 1)
	go func() {
		n, err := d.RunOnce(t.Context())
		done <- runResult{n, err}
	}()
	select {
	case <-h.arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("the delivery never reached the endpoint")
	}
	return done
}

type deliveryRow struct {
	ID         string
	Status     string
	Attempts   int
	StatusCode *int
	LastError  *string
}

func endpointDelivery(t *testing.T, app *apptest.App, endpointID string) deliveryRow {
	t.Helper()
	var d deliveryRow
	if err := app.Store.Pool().QueryRow(t.Context(), `SELECT id, status, attempts, last_status_code, last_error
		FROM webhook_deliveries WHERE endpoint_id = $1`, endpointID).Scan(&d.ID, &d.Status, &d.Attempts, &d.StatusCode, &d.LastError); err != nil {
		t.Fatal(err)
	}
	return d
}

// A worker whose lease lapsed mid-request must not overwrite what the worker that claimed the
// delivery again wrote: here a late failure on the last attempt would turn a success into dead.
func TestWebhookLapsedLeaseDropsTheLateOutcome(t *testing.T) {
	app := apptest.New(t, apptest.Options{WebhookAttempts: 1})
	h := newHeldEndpoint(t)
	endpointID, _ := registerEndpoint(t, app, h.srv.URL)
	s := createSession(t, app, app.TestKey, `{"amount":"5.00"}`, "lease")
	pay(t, app, s, "base-sepolia", "0x1", usdc("1"), false)
	sequence(t, app)
	second := webhook.NewDispatcher(app.Svc, app.Store, webhook.Config{AllowPrivateNetworks: true, Timeout: 5 * time.Second}, nil)

	first := startHeldRun(t, app.Dispatcher, h)
	app.Clock.Advance(time.Hour)
	n, err := second.RunOnce(t.Context())
	close(h.release)
	late := <-first

	if err != nil || n != 1 {
		t.Fatalf("second dispatcher: %d %v", n, err)
	}
	if late.err != nil || late.n != 1 {
		t.Fatalf("first dispatcher: %d %v", late.n, late.err)
	}
	if h.count() != 2 {
		t.Fatalf("endpoint saw %d requests, want 2", h.count())
	}
	d := endpointDelivery(t, app, endpointID)
	if d.Status != "succeeded" || d.Attempts != 1 || d.StatusCode == nil || *d.StatusCode != http.StatusOK || d.LastError != nil {
		t.Fatalf("the late failure overwrote the success: %+v", d)
	}
}

// A resend while an attempt is in flight keeps its fresh retry budget: the in-flight outcome is
// dropped and the resent delivery goes out on the next run.
func TestWebhookResendDuringAnAttemptKeepsItsBudget(t *testing.T) {
	app := apptest.New(t, apptest.Options{})
	h := newHeldEndpoint(t)
	endpointID, _ := registerEndpoint(t, app, h.srv.URL)
	s := createSession(t, app, app.TestKey, `{"amount":"5.00"}`, "resend")
	pay(t, app, s, "base-sepolia", "0x1", usdc("1"), false)
	sequence(t, app)

	inFlight := startHeldRun(t, app.Dispatcher, h)
	id := endpointDelivery(t, app, endpointID).ID
	if _, err := app.Store.ResendDelivery(t.Context(), app.MerchantID, false, id, "test"); err != nil {
		t.Fatal(err)
	}
	close(h.release)
	if r := <-inFlight; r.err != nil || r.n != 1 {
		t.Fatalf("in-flight run: %d %v", r.n, r.err)
	}
	afterResend := endpointDelivery(t, app, endpointID)
	n, err := app.Dispatcher.RunOnce(t.Context())

	if afterResend.Status != "pending" || afterResend.Attempts != 0 || afterResend.StatusCode != nil {
		t.Fatalf("the in-flight failure overwrote the resend: %+v", afterResend)
	}
	if err != nil || n != 1 {
		t.Fatalf("the resent delivery was not due: %d %v", n, err)
	}
	if d := endpointDelivery(t, app, endpointID); d.Status != "succeeded" || d.Attempts != 1 {
		t.Fatalf("resent delivery: %+v", d)
	}
}

// A shutdown (the worker's context ending) while a delivery is in flight lets the attempt finish
// and records its outcome, instead of cutting the POST and sending the event again after the lease.
func TestWebhookAttemptInFlightFinishesOnShutdown(t *testing.T) {
	app := apptest.New(t, apptest.Options{})
	h := newHeldEndpoint(t)
	endpointID, _ := registerEndpoint(t, app, h.srv.URL)
	s := createSession(t, app, app.TestKey, `{"amount":"5.00"}`, "shutdown")
	pay(t, app, s, "base-sepolia", "0x1", usdc("1"), false)
	sequence(t, app)

	ctx, cancel := context.WithCancel(t.Context())
	if n, err := app.Dispatcher.Start(ctx); err != nil || n != 1 {
		t.Fatalf("start: %d %v", n, err)
	}
	select {
	case <-h.arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("the delivery never reached the endpoint")
	}
	cancel()
	close(h.release)
	app.Dispatcher.Wait()

	d := endpointDelivery(t, app, endpointID)
	if d.Attempts != 1 || d.StatusCode == nil || *d.StatusCode != http.StatusInternalServerError {
		t.Fatalf("the attempt in flight was not recorded: %+v", d)
	}
	if _, err := app.Dispatcher.Start(t.Context()); err != nil {
		t.Fatalf("a background error after the shutdown: %v", err)
	}
}
