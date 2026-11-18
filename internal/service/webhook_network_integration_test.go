//go:build integration

package service_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/webhook"
)

func queueOneDelivery(t *testing.T, app *apptest.App, url, idemKey string) string {
	t.Helper()
	endpointID, _ := registerEndpoint(t, app, url)
	s := createSession(t, app, app.TestKey, `{"amount":"5.00"}`, idemKey)
	pay(t, app, s, "base-sepolia", "0x1", usdc("1"), false)
	sequence(t, app)
	return endpointID
}

// With private networks refused (production), a delivery to a loopback endpoint is stopped when
// the connection is dialed: the endpoint gets nothing and the attempt fails with the rule.
func TestWebhookDialRefusesLoopbackEndpoint(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	c := newConsumer(t, http.StatusOK)
	endpointID := queueOneDelivery(t, app, c.srv.URL, "dial")
	strict := webhook.NewDispatcher(app.Svc, app.Store, webhook.Config{Timeout: 2 * time.Second}, nil)

	n, err := strict.RunOnce(t.Context())

	if err != nil || n != 1 {
		t.Fatalf("dispatch: %d %v", n, err)
	}
	if got := len(c.deliveries()); got != 0 {
		t.Fatalf("the loopback endpoint received %d requests", got)
	}
	d := endpointDelivery(t, app, endpointID)
	if d.Status != "pending" || d.Attempts != 1 || d.StatusCode != nil || d.LastError == nil ||
		*d.LastError != "refused address: the endpoint resolves to a non-public address" {
		t.Fatalf("delivery: %+v (error %v)", d, deref(d.LastError))
	}
}

// A redirect is a failed attempt and is never followed, so it cannot carry a delivery past the
// URL rules.
func TestWebhookRedirectIsNotFollowed(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	target := newConsumer(t, http.StatusOK)
	redirector := httptest.NewServer(http.RedirectHandler(target.srv.URL+"/internal", http.StatusFound))
	t.Cleanup(redirector.Close)
	endpointID := queueOneDelivery(t, app, redirector.URL, "redirect")

	n, err := app.Dispatcher.RunOnce(t.Context())

	if err != nil || n != 1 {
		t.Fatalf("dispatch: %d %v", n, err)
	}
	if got := len(target.deliveries()); got != 0 {
		t.Fatalf("the redirect was followed: the target received %d requests", got)
	}
	d := endpointDelivery(t, app, endpointID)
	if d.Status != "pending" || d.Attempts != 1 || d.StatusCode == nil || *d.StatusCode != http.StatusFound {
		t.Fatalf("delivery: %+v (error %v)", d, deref(d.LastError))
	}
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}
