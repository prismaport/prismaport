//go:build integration

package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/prismaport/prismaport/internal/api"
	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/webhooksig"
)

type panelResult struct {
	Action string `json:"action"`
	Events []struct {
		Type           string `json:"type"`
		SessionVersion int64  `json:"session_version"`
	} `json:"events"`
	Settled bool `json:"settled"`
	Session struct {
		ID             string `json:"id"`
		Livemode       bool   `json:"livemode"`
		Status         string `json:"status"`
		AmountReceived string `json:"amount_received"`
		Payments       []struct {
			Status      string  `json:"status"`
			ConfirmedAt *string `json:"confirmed_at"`
		} `json:"payments"`
	} `json:"session"`
}

func panel(t *testing.T, app *apptest.App, id string, body map[string]any) (apptest.Response, panelResult) {
	t.Helper()
	r := app.Do("POST", "/v1/checkout/sessions/"+id+"/test_helpers/simulate", "", body)
	var res panelResult
	if r.Status == http.StatusOK {
		if err := json.Unmarshal(r.Body, &res); err != nil {
			t.Fatal(err)
		}
	}
	return r, res
}

func eventTypes(res panelResult) string {
	var out []string
	for _, e := range res.Events {
		out = append(out, e.Type)
	}
	return strings.Join(out, ",")
}

// The hosted checkout's Test mode panel pays, expires and settles a test session through the
// simulator, and the merchant's webhooks, feed and dashboard see the real event sequence.
func TestCheckoutTestPanel(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	rc := newReceiver(t, http.StatusOK, "ok")
	_, secret := createEndpoint(t, app, app.TestKey, rc.srv.URL)
	id := newTestSession(t, app, app.TestKey, "25.00")["id"].(string)

	r, res := panel(t, app, id, map[string]any{"action": "pay_partial", "amount": "10.00", "chain": "base-sepolia", "asset": "USDC"})
	if r.Status != http.StatusOK || eventTypes(res) != "session.payment_detected,session.partially_paid" || res.Session.Status != "partially_paid" ||
		res.Session.AmountReceived != "10.00" || r.Header.Get("Access-Control-Allow-Origin") != "*" || r.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("pay part: %d %s", r.Status, r.Body)
	}
	// The answer is the payer's view: nothing the merchant keeps private. A merchant without a
	// logo is not named either.
	if !strings.Contains(string(r.Body), `"merchant":null`) {
		t.Fatalf("the panel answer names a merchant with no logo: %s", r.Body)
	}
	// `"mer_` is a merchant id as a value ("customer_address_id" contains mer_ too).
	for _, private := range []string{"merchant_id", `"mer_`, "metadata", "settlement", "payout", "evt_", "client_reference"} {
		if strings.Contains(string(r.Body), private) {
			t.Fatalf("the panel answer shows %q: %s", private, r.Body)
		}
	}
	if _, res = panel(t, app, id, map[string]any{"action": "pay_exact"}); eventTypes(res) != "session.payment_detected,session.paid" || res.Session.Status != "paid" || res.Settled {
		t.Fatalf("pay the rest: %+v", res)
	}
	for _, p := range res.Session.Payments {
		if p.Status != "confirmed" || p.ConfirmedAt == nil {
			t.Fatalf("a confirmed payment tells the payer when: %+v", res.Session.Payments)
		}
	}
	if r, _ := panel(t, app, id, map[string]any{"action": "pay_exact"}); r.Status != http.StatusConflict || r.ErrorCode(t) != "invalid_state" {
		t.Fatalf("pay a paid session: %d %s", r.Status, r.Body)
	}
	if _, res = panel(t, app, id, map[string]any{"action": "settle"}); !res.Settled || len(res.Events) != 0 || res.Session.Status != "paid" {
		t.Fatalf("settle: %+v", res)
	}
	if r, _ := panel(t, app, id, map[string]any{"action": "settle"}); r.Status != http.StatusConflict {
		t.Fatalf("settle twice: %d %s", r.Status, r.Body)
	}

	// The merchant sees the same thing a real payment produces: dashboard, feed and webhooks.
	c := login(t, app, "owner@example.com")
	detail := c.get("/v1/dashboard/sessions/" + id + "?mode=test")
	st, ok := detail["settlement"].(map[string]any)
	if detail["status"] != "paid" || !ok || st["simulated"] != true || st["fee"] != "0.225000" || len(st["payouts"].([]any)) == 0 {
		t.Fatalf("dashboard detail: %v", detail)
	}
	if _, err := app.Store.SequenceEvents(t.Context(), 100); err != nil {
		t.Fatal(err)
	}
	for {
		n, err := app.Dispatcher.RunOnce(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}
	feed := app.Do("GET", "/v1/events?limit=100", app.TestKey, nil).JSON(t)["data"].([]any)
	var feedTypes []string
	for _, e := range feed {
		feedTypes = append(feedTypes, e.(map[string]any)["type"].(string))
	}
	want := "session.payment_detected,session.partially_paid,session.payment_detected,session.paid"
	if strings.Join(feedTypes, ",") != want {
		t.Fatalf("feed: %v", feedTypes)
	}
	got := rc.all()
	if len(got) != 4 {
		t.Fatalf("webhooks delivered: %d", len(got))
	}
	for _, d := range got {
		if err := webhooksig.Verify(d.body, d.header.Get(webhooksig.HeaderName), secret, 0, app.Clock.Now()); err != nil {
			t.Fatalf("delivery signature: %v", err)
		}
	}
	if !hasAll(auditActions(t, app, app.MerchantID), "session.simulated", "session.simulated", "session.simulated") {
		t.Fatalf("audit: %v", auditActions(t, app, app.MerchantID))
	}

	// Overpay and expiry on fresh sessions.
	if _, res = panel(t, app, newTestSession(t, app, app.TestKey, "5.00")["id"].(string), map[string]any{"action": "overpay"}); res.Session.Status != "overpaid" {
		t.Fatalf("overpay: %+v", res)
	}
	if _, res = panel(t, app, newTestSession(t, app, app.TestKey, "5.00")["id"].(string), map[string]any{"action": "expire"}); eventTypes(res) != "session.expired" ||
		res.Session.Status != "expired" {
		t.Fatalf("expire: %+v", res)
	}
}

// Live sessions never take a simulated step, and the panel offers only its own actions.
func TestCheckoutTestPanelRefusals(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	live := app.Session(app.LiveKey, map[string]any{"amount": "25.00"}, "Idempotency-Key", "live-panel")
	if live.Status != http.StatusOK {
		t.Fatalf("live session: %d %s", live.Status, live.Body)
	}
	liveID := live.JSON(t)["id"].(string)
	for _, action := range []string{"pay_exact", "pay_partial", "overpay", "expire", "settle", "under_review"} {
		if r, _ := panel(t, app, liveID, map[string]any{"action": action}); r.Status != http.StatusForbidden || r.ErrorCode(t) != "test_mode_only" {
			t.Errorf("live %s: %d %s", action, r.Status, r.Body)
		}
	}
	after := app.Do("GET", "/v1/sessions/"+liveID, app.LiveKey, nil).JSON(t)
	if after["status"] != "open" || after["version"] != float64(1) || len(after["payments"].([]any)) != 0 {
		t.Fatalf("live session changed: %v", after)
	}
	id := newTestSession(t, app, app.TestKey, "25.00")["id"].(string)
	for _, action := range []string{"under_review", "reverse_last_payment", "late_payment", "", "refund"} {
		if r, _ := panel(t, app, id, map[string]any{"action": action}); r.Status != http.StatusBadRequest {
			t.Errorf("action %q: %d %s", action, r.Status, r.Body)
		}
	}
	if r, _ := panel(t, app, id, map[string]any{"action": "pay_exact", "merchant_id": "mer_x"}); r.Status != http.StatusBadRequest {
		t.Fatalf("unknown field: %d", r.Status)
	}
	for _, missing := range []string{"ses_01JB000000000000000000NOPE", "nope", "prod_01JB000000000000000000NOPE"} {
		if r, _ := panel(t, app, missing, map[string]any{"action": "pay_exact"}); r.Status != http.StatusNotFound {
			t.Errorf("%s: %d %s", missing, r.Status, r.Body)
		}
	}
	if r, _ := panel(t, app, id, map[string]any{"action": "pay_partial", "amount": "30.00"}); r.Status != http.StatusBadRequest {
		t.Fatalf("a part above what is due: %d %s", r.Status, r.Body)
	}
	if after := app.Do("GET", "/v1/sessions/"+id, app.TestKey, nil).JSON(t); after["version"] != float64(1) {
		t.Fatalf("refused steps changed the test session: %v", after)
	}
}

func TestCheckoutTestPanelRateLimit(t *testing.T) {
	t.Parallel()
	limits := api.PublicOptions{Limits: map[string]api.Limit{}}
	for route := range api.DefaultLimits() {
		limits.Limits[route] = api.Limit{PerMinute: 10_000, Burst: 10_000}
	}
	limits.Limits[api.RouteCheckoutTestPanel] = api.Limit{PerMinute: 1, Burst: 2}
	app, _ := dashApp(t, &limits)
	id := newTestSession(t, app, app.TestKey, "25.00")["id"].(string)
	panel(t, app, id, map[string]any{"action": "pay_partial"})
	panel(t, app, id, map[string]any{"action": "pay_partial"})
	if r, _ := panel(t, app, id, map[string]any{"action": "pay_exact"}); r.Status != http.StatusTooManyRequests || r.Header.Get("Retry-After") == "" {
		t.Fatalf("third step: %d %s", r.Status, r.Body)
	}
}
