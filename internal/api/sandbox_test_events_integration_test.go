//go:build integration

package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/api"
	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/service"
	"github.com/prismaport/prismaport/internal/webhook"
	"github.com/prismaport/prismaport/internal/webhooksig"
)

// receiver is a merchant endpoint that keeps every request it gets.
type receiver struct {
	mu     sync.Mutex
	got    []received
	status int
	reply  string
	srv    *httptest.Server
}

type received struct {
	header http.Header
	body   []byte
}

func newReceiver(t *testing.T, status int, reply string) *receiver {
	t.Helper()
	rc := &receiver{status: status, reply: reply}
	rc.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		rc.mu.Lock()
		rc.got = append(rc.got, received{header: r.Header.Clone(), body: raw})
		status, reply := rc.status, rc.reply
		rc.mu.Unlock()
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(rc.srv.Close)
	return rc
}

func (rc *receiver) all() []received {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return append([]received(nil), rc.got...)
}

func createEndpoint(t *testing.T, app *apptest.App, key, url string) (id, secret string) {
	t.Helper()
	r := app.Do("POST", "/v1/webhook_endpoints", key, map[string]any{"url": url})
	if r.Status != http.StatusOK {
		t.Fatalf("create endpoint: %d %s", r.Status, r.Body)
	}
	m := r.JSON(t)
	return m["id"].(string), m["secret"].(string)
}

func header(t *testing.T, req map[string]any, name string) string {
	t.Helper()
	for _, h := range req["headers"].([]any) {
		if m := h.(map[string]any); m["name"] == name {
			return m["value"].(string)
		}
	}
	t.Fatalf("request has no %s header: %v", name, req)
	return ""
}

func TestSendTestEvent(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	big := strings.Repeat("x", 3000) + "é"
	rc := newReceiver(t, http.StatusOK, big)
	eid, secret := createEndpoint(t, app, app.TestKey, rc.srv.URL)

	r := app.Do("POST", "/v1/webhook_endpoints/"+eid+"/test", app.TestKey, map[string]any{"type": "session.paid"})
	if r.Status != http.StatusOK {
		t.Fatalf("send test: %d %s", r.Status, r.Body)
	}
	res := r.JSON(t)
	req := res["request"].(map[string]any)
	got := rc.all()
	if len(got) != 1 {
		t.Fatalf("receiver got %d requests", len(got))
	}
	// What the API reports is exactly what was sent, signed like a real delivery.
	if string(got[0].body) != req["body"] || req["method"] != "POST" || req["url"] != rc.srv.URL {
		t.Fatalf("request: %v\nsent: %s", req, got[0].body)
	}
	for _, h := range webhook.DeliveryHeaders {
		if got[0].header.Get(h) != header(t, req, h) {
			t.Fatalf("header %s: sent %q, reported %q", h, got[0].header.Get(h), header(t, req, h))
		}
	}
	if err := webhooksig.Verify(got[0].body, got[0].header.Get("Signature"), secret, 0, app.Clock.Now()); err != nil {
		t.Fatalf("signature: %v", err)
	}
	if got[0].header.Get("User-Agent") != "Prismaport-Webhooks/1" || got[0].header.Get("Content-Type") != "application/json" {
		t.Fatalf("headers: %v", got[0].header)
	}
	var env struct {
		ID   string `json:"id"`
		Type string `json:"type"`
		Data struct {
			Session map[string]any `json:"session"`
		} `json:"data"`
	}
	if err := json.Unmarshal(got[0].body, &env); err != nil {
		t.Fatal(err)
	}
	ses := env.Data.Session
	if env.Type != "session.paid" || !strings.HasPrefix(env.ID, "evt_") || got[0].header.Get("Prismaport-Event-Id") != env.ID ||
		res["event"].(map[string]any)["id"] != env.ID {
		t.Fatalf("envelope: %s", got[0].body)
	}
	if ses["livemode"] != false || ses["status"] != "paid" || ses["amount"] != "25.00" || ses["amount_received"] != "25.00" ||
		!strings.HasPrefix(ses["id"].(string), "ses_") || ses["paid_at"] == nil {
		t.Fatalf("sample session: %v", ses)
	}
	pay := ses["payments"].([]any)[0].(map[string]any)
	if pay["simulated"] != true || pay["status"] != "confirmed" || !strings.HasPrefix(pay["tx_hash"].(string), service.SimulatedHashPrefix) {
		t.Fatalf("sample payment: %v", pay)
	}
	// The sample commits to a 70/30 split per network, as a real session would; it is paid, not
	// settled yet.
	for _, fam := range []string{"base-sepolia", "solana-devnet"} {
		rs := ses["payout"].(map[string]any)[fam].([]any)
		if len(rs) != 2 || rs[0].(map[string]any)["share"] != "70.00" || rs[1].(map[string]any)["bps"] != float64(3000) ||
			!strings.HasPrefix(rs[0].(map[string]any)["wallet_id"].(string), "wal_") {
			t.Fatalf("sample %s payout: %v", fam, rs)
		}
	}
	if ses["settlement"] != nil {
		t.Fatalf("sample settlement: %v", ses["settlement"])
	}
	resp := res["response"].(map[string]any)
	if resp["status"] != float64(200) || resp["body_truncated"] != true || len(resp["body"].(string)) != webhook.TestBodyLimit ||
		resp["latency_ms"].(float64) < 0 || res["error"] != nil {
		t.Fatalf("response: %v", resp)
	}
	d := res["delivery"].(map[string]any)
	if d["test"] != true || d["status"] != "succeeded" || d["event_id"] != env.ID || d["event_type"] != "session.paid" ||
		d["attempts"] != float64(1) || d["last_status_code"] != float64(200) || d["session_id"] != nil {
		t.Fatalf("delivery: %v", d)
	}

	// The sample never enters the feed, and the sequencer has nothing to fan out.
	if n, err := app.Store.SequenceEvents(t.Context(), 100); err != nil || n != 0 {
		t.Fatalf("sequenced %d %v", n, err)
	}
	if feed := app.Do("GET", "/v1/events", app.TestKey, nil).JSON(t); len(feed["data"].([]any)) != 0 {
		t.Fatalf("feed: %v", feed)
	}
	if n, err := app.Dispatcher.RunOnce(t.Context()); err != nil || n != 0 {
		t.Fatalf("dispatcher picked %d %v", n, err)
	}
	if len(rc.all()) != 1 {
		t.Fatal("a test delivery was sent twice")
	}

	// Every event type yields a sample whose session matches it.
	want := map[string]string{"session.payment_detected": "open", "session.partially_paid": "partially_paid", "session.paid": "paid",
		"session.overpaid": "overpaid", "session.expired": "expired", "session.late_payment": "late_payment",
		"session.under_review": "under_review", "payment.reversed": "open"}
	for typ, status := range want {
		t.Run(typ, func(t *testing.T) {
			r := app.Do("POST", "/v1/webhook_endpoints/"+eid+"/test", app.TestKey, map[string]any{"type": typ})
			if r.Status != http.StatusOK {
				t.Fatalf("%s: %d %s", typ, r.Status, r.Body)
			}
			var e struct {
				Type string `json:"type"`
				Data struct {
					Session struct {
						Status   string           `json:"status"`
						Payments []map[string]any `json:"payments"`
					} `json:"session"`
				} `json:"data"`
			}
			if err := json.Unmarshal([]byte(r.JSON(t)["request"].(map[string]any)["body"].(string)), &e); err != nil {
				t.Fatal(err)
			}
			if e.Type != typ || e.Data.Session.Status != status {
				t.Errorf("%s: type %s status %s", typ, e.Type, e.Data.Session.Status)
			}
			if typ == "payment.reversed" && (len(e.Data.Session.Payments) != 1 || e.Data.Session.Payments[0]["status"] != "reversed") {
				t.Errorf("reversed sample: %v", e.Data.Session.Payments)
			}
		})
	}

	// The delivery log lists the tests, marked, and a test is never resent.
	dl := app.Do("GET", "/v1/webhook_endpoints", app.TestKey, nil)
	if dl.Status != http.StatusOK {
		t.Fatal(dl.Status)
	}
	list, more, err := app.Store.ListDeliveries(t.Context(), app.MerchantID, false, eid, "", 100)
	if err != nil || more || len(list) != 9 {
		t.Fatalf("log: %d %v %v", len(list), more, err)
	}
	for _, x := range list {
		if !x.Test || x.EventType == "" || !strings.HasPrefix(x.EventID, "evt_") {
			t.Fatalf("logged test: %+v", x)
		}
	}
	if _, err := app.Store.ResendDelivery(t.Context(), app.MerchantID, false, list[0].ID, "test"); err == nil {
		t.Fatal("a test delivery was resent")
	}
	if !hasAll(auditActions(t, app, app.MerchantID), "webhook_endpoint.test_sent") {
		t.Fatal("no audit entry")
	}
}

func TestSendTestEventSignsLikeARotatedDelivery(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	rc := newReceiver(t, http.StatusNoContent, "")
	eid, oldSecret := createEndpoint(t, app, app.TestKey, rc.srv.URL)
	rot := app.Do("POST", "/v1/webhook_endpoints/"+eid+"/rotate_secret", app.TestKey, nil).JSON(t)
	newSecret := rot["secret"].(string)
	if r := app.Do("POST", "/v1/webhook_endpoints/"+eid+"/test", app.TestKey, map[string]any{"type": "session.expired"}); r.Status != http.StatusOK {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
	got := rc.all()[0]
	sig := got.header.Get("Signature")
	if strings.Count(sig, "v1=") != 2 {
		t.Fatalf("rotation window: %s", sig)
	}
	for _, s := range []string{newSecret, oldSecret} {
		if err := webhooksig.Verify(got.body, sig, s, 0, app.Clock.Now()); err != nil {
			t.Fatalf("secret %s: %v", s[:10], err)
		}
	}
	// After the window only the current secret signs, as for real deliveries.
	app.Clock.Advance(25 * time.Hour)
	app.Do("POST", "/v1/webhook_endpoints/"+eid+"/test", app.TestKey, map[string]any{"type": "session.expired"})
	got = rc.all()[1]
	if sig := got.header.Get("Signature"); strings.Count(sig, "v1=") != 1 || webhooksig.Verify(got.body, sig, newSecret, 0, app.Clock.Now()) != nil {
		t.Fatalf("after the window: %s", sig)
	}
}

func TestSendTestEventFailures(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	failing := newReceiver(t, http.StatusInternalServerError, `{"error":"boom"}`)
	eid, _ := createEndpoint(t, app, app.TestKey, failing.srv.URL)
	res := app.Do("POST", "/v1/webhook_endpoints/"+eid+"/test", app.TestKey, map[string]any{"type": "session.paid"}).JSON(t)
	d := res["delivery"].(map[string]any)
	resp := res["response"].(map[string]any)
	if resp["status"] != float64(500) || resp["body"] != `{"error":"boom"}` || resp["body_truncated"] != false ||
		d["status"] != "dead" || d["last_status_code"] != float64(500) || !strings.Contains(d["last_error"].(string), "500") {
		t.Fatalf("failing endpoint: %v", res)
	}

	gone := httptest.NewServer(http.NotFoundHandler())
	url := gone.URL
	gone.Close()
	gid, _ := createEndpoint(t, app, app.TestKey, url)
	res = app.Do("POST", "/v1/webhook_endpoints/"+gid+"/test", app.TestKey, map[string]any{"type": "session.paid"}).JSON(t)
	if res["response"] != nil || res["error"] == nil || res["delivery"].(map[string]any)["status"] != "dead" ||
		res["delivery"].(map[string]any)["last_status_code"] != nil {
		t.Fatalf("unreachable endpoint: %v", res)
	}

	// A production dispatcher refuses private addresses for tests exactly as for deliveries.
	strict := webhook.NewDispatcher(app.Svc, app.Store, webhook.Config{Timeout: 2 * time.Second}, nil)
	a := service.Auth{MerchantID: app.MerchantID, Scopes: []string{"webhooks:write"}, KeyID: "k"}
	tr, err := strict.SendTest(t.Context(), a, eid, "session.paid")
	if err != nil || tr.Error == nil || !strings.Contains(*tr.Error, "non-public") || tr.Response != nil {
		t.Fatalf("private address: %+v %v", tr, err)
	}

	cases := []struct {
		name, path, key string
		body            any
		status          int
		code            string
	}{
		{"missing type", "/v1/webhook_endpoints/" + eid + "/test", app.TestKey, map[string]any{}, 400, "invalid_request"},
		{"unknown type", "/v1/webhook_endpoints/" + eid + "/test", app.TestKey, map[string]any{"type": "session.refunded"}, 400, "invalid_request"},
		{"unknown field", "/v1/webhook_endpoints/" + eid + "/test", app.TestKey, map[string]any{"type": "session.paid", "x": 1}, 400, "invalid_request"},
		{"unknown endpoint", "/v1/webhook_endpoints/we_01JB0000000000000000000000/test", app.TestKey, map[string]any{"type": "session.paid"}, 404, "not_found"},
		{"live key", "/v1/webhook_endpoints/" + eid + "/test", app.LiveKey, map[string]any{"type": "session.paid"}, 403, "test_mode_only"},
		{"no key", "/v1/webhook_endpoints/" + eid + "/test", "", map[string]any{"type": "session.paid"}, 401, "invalid_api_key"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := app.Do("POST", c.path, c.key, c.body)
			if r.Status != c.status || r.ErrorCode(t) != c.code {
				t.Errorf("%s: %d %s", c.name, r.Status, r.Body)
			}
		})
	}
	// A live endpoint can never be tested, whatever the key.
	liveID, _ := createEndpoint(t, app, app.LiveKey, "https://example.com/hooks")
	if r := app.Do("POST", "/v1/webhook_endpoints/"+liveID+"/test", app.LiveKey, map[string]any{"type": "session.paid"}); r.Status != 403 || r.ErrorCode(t) != "test_mode_only" {
		t.Fatalf("live endpoint: %d %s", r.Status, r.Body)
	}
	if r := app.Do("POST", "/v1/webhook_endpoints/"+liveID+"/test", app.TestKey, map[string]any{"type": "session.paid"}); r.Status != 404 {
		t.Fatalf("live endpoint with a test key: %d %s", r.Status, r.Body)
	}
	// Scope: webhooks:write is required.
	_, readOnly, err := app.Svc.CreateAPIKey(t.Context(), app.MerchantID, false, []string{"webhooks:read"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if r := app.Do("POST", "/v1/webhook_endpoints/"+eid+"/test", readOnly, map[string]any{"type": "session.paid"}); r.Status != 403 || r.ErrorCode(t) != "insufficient_scope" {
		t.Fatalf("scope: %d %s", r.Status, r.Body)
	}
	// A deleted endpoint is gone for tests too.
	app.Do("DELETE", "/v1/webhook_endpoints/"+eid, app.TestKey, nil)
	if r := app.Do("POST", "/v1/webhook_endpoints/"+eid+"/test", app.TestKey, map[string]any{"type": "session.paid"}); r.Status != 404 {
		t.Fatalf("deleted endpoint: %d %s", r.Status, r.Body)
	}
}

func TestDashboardSendTestEvent(t *testing.T) {
	t.Parallel()
	limits := api.PublicOptions{Limits: map[string]api.Limit{api.RouteWebhookTest: {PerMinute: 1, Burst: 2}}}
	app, _ := dashApp(t, &limits)
	c := login(t, app, "owner@example.com")
	rc := newReceiver(t, http.StatusOK, "ok")
	e := c.send("POST", "/v1/dashboard/webhook_endpoints?mode=test", map[string]any{"url": rc.srv.URL})
	eid := e["id"].(string)
	res := c.send("POST", "/v1/dashboard/webhook_endpoints/"+eid+"/test?mode=test", map[string]any{"type": "session.partially_paid"})
	if res["delivery"].(map[string]any)["test"] != true || res["response"].(map[string]any)["body"] != "ok" {
		t.Fatalf("dashboard test: %v", res)
	}
	if err := webhooksig.Verify(rc.all()[0].body, rc.all()[0].header.Get("Signature"), e["secret"].(string), 0, app.Clock.Now()); err != nil {
		t.Fatal(err)
	}
	dl := c.get("/v1/dashboard/webhook_endpoints/" + eid + "/deliveries?mode=test")["data"].([]any)
	if len(dl) != 1 || dl[0].(map[string]any)["test"] != true || dl[0].(map[string]any)["event_type"] != "session.partially_paid" {
		t.Fatalf("log: %v", dl)
	}
	if r := c.do("POST", "/v1/dashboard/webhook_deliveries/"+dl[0].(map[string]any)["id"].(string)+"/resend?mode=test", nil); r.Status != http.StatusConflict {
		t.Fatalf("resend of a test: %d %s", r.Status, r.Body)
	}
	if r := c.do("POST", "/v1/dashboard/webhook_endpoints/"+eid+"/test?mode=live", map[string]any{"type": "session.paid"}); r.Status != 403 || r.ErrorCode(t) != "test_mode_only" {
		t.Fatalf("live mode: %d %s", r.Status, r.Body)
	}
	// The rate limit is per merchant: the burst of 2 is spent by the second send.
	c.send("POST", "/v1/dashboard/webhook_endpoints/"+eid+"/test?mode=test", map[string]any{"type": "session.paid"})
	r := c.do("POST", "/v1/dashboard/webhook_endpoints/"+eid+"/test?mode=test", map[string]any{"type": "session.paid"})
	if r.Status != http.StatusTooManyRequests || r.Header.Get("Retry-After") == "" {
		t.Fatalf("rate limit: %d %s", r.Status, r.Body)
	}
	// Another merchant cannot test this endpoint.
	secondMerchant(t, app, "other@example.com")
	intruder := login(t, app, "other@example.com")
	if r := intruder.do("POST", "/v1/dashboard/webhook_endpoints/"+eid+"/test?mode=test", map[string]any{"type": "session.paid"}); r.Status != http.StatusNotFound {
		t.Fatalf("intruder: %d %s", r.Status, r.Body)
	}
	if len(rc.all()) != 2 {
		t.Fatalf("receiver got %d", len(rc.all()))
	}
}
