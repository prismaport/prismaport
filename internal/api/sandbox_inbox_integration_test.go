//go:build integration

package api_test

import (
	"bytes"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/api"
	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/service"
	"github.com/prismaport/prismaport/internal/webhook"
)

// postInbox sends body to the inbox path of inboxURL on the test server, signed with secrets at
// signedAt (no Signature header when secrets is empty and header is empty).
func postInbox(t *testing.T, app *apptest.App, inboxURL string, body []byte, signedAt time.Time, header string, secrets ...string) apptest.Response {
	t.Helper()
	u, err := url.Parse(inboxURL)
	if err != nil {
		t.Fatal(err)
	}
	req, err := webhook.NewDeliveryRequest(t.Context(), app.Server.URL+u.Path, "evt_x", body, signedAt, secrets...)
	if err != nil {
		t.Fatal(err)
	}
	if len(secrets) == 0 {
		req.Header.Del("Signature")
	}
	if header != "" {
		req.Header.Set("Signature", header)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return apptest.Response{Status: resp.StatusCode, Header: resp.Header, Body: buf.Bytes()}
}

func inboxRequests(t *testing.T, c *dashClient) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, x := range c.get("/v1/dashboard/sandbox/inbox/requests?mode=test&limit=100")["data"].([]any) {
		out = append(out, x.(map[string]any))
	}
	return out
}

func TestSandboxInbox(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	if got := c.get("/v1/dashboard/sandbox/inbox?mode=test"); got["inbox"] != nil {
		t.Fatalf("inbox before create: %v", got)
	}
	inbox := c.send("POST", "/v1/dashboard/sandbox/inbox?mode=test", nil)["inbox"].(map[string]any)
	inboxURL, secret := inbox["url"].(string), inbox["secret"].(string)
	token := strings.TrimPrefix(inboxURL, apptest.CheckoutBase+service.InboxPath)
	if len(token) < 40 || token == inboxURL || !strings.HasPrefix(secret, "whsec_") || inbox["retention_days"] != float64(7) ||
		inbox["max_requests"] != float64(500) || inbox["max_body_bytes"] != float64(65536) {
		t.Fatalf("inbox: %v", inbox)
	}
	again := c.send("POST", "/v1/dashboard/sandbox/inbox?mode=test", nil)["inbox"].(map[string]any)
	if again["id"] != inbox["id"] || again["secret"] != nil {
		t.Fatalf("create is not idempotent: %v", again)
	}
	eps := c.get("/v1/dashboard/webhook_endpoints?mode=test")["data"].([]any)
	if len(eps) != 1 || eps[0].(map[string]any)["url"] != inboxURL || eps[0].(map[string]any)["id"] != inbox["endpoint_id"] ||
		len(eps[0].(map[string]any)["enabled_events"].([]any)) != 0 {
		t.Fatalf("the inbox is not a test endpoint for every event: %v", eps)
	}

	// A correctly signed request is verified on arrival.
	body := []byte(`{"id":"evt_01JB0000000000000000000001","type":"session.paid","data":{"session":{"id":"ses_1"}}}`)
	r := postInbox(t, app, inboxURL, body, app.Clock.Now(), "", secret)
	if r.Status != http.StatusOK || r.JSON(t)["received"] != true || r.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("receive: %d %s", r.Status, r.Body)
	}
	got := inboxRequests(t, c)
	if len(got) != 1 {
		t.Fatalf("requests: %v", got)
	}
	q := got[0]
	sig := q["signature"].(map[string]any)
	if q["method"] != "POST" || q["body"] != string(body) || q["size"] != float64(len(body)) || q["event_id"] != "evt_01JB0000000000000000000001" ||
		q["event_type"] != "session.paid" || sig["valid"] != true || sig["error"] != nil || sig["skew_seconds"] != float64(0) {
		t.Fatalf("request: %v", q)
	}
	names := map[string]bool{}
	for _, h := range q["headers"].([]any) {
		names[h.(map[string]any)["name"].(string)] = true
	}
	for _, n := range []string{"Signature", "Prismaport-Event-Id", "Content-Type", "User-Agent"} {
		if !names[n] {
			t.Fatalf("header %s not kept: %v", n, q["headers"])
		}
	}

	// Each failure says why.
	cases := []struct {
		name   string
		at     time.Time
		header string
		secret []string
		want   string
		skew   any
	}{
		{"wrong secret", app.Clock.Now(), "", []string{"whsec_other"}, service.SigNoMatch, float64(0)},
		{"no header", app.Clock.Now(), "", nil, service.SigMissing, nil},
		{"malformed", app.Clock.Now(), "v1=abc", nil, service.SigMalformed, nil},
		{"stale", app.Clock.Now().Add(-10 * time.Minute), "", []string{secret}, service.SigTimestamp, float64(600)},
		{"future", app.Clock.Now().Add(6 * time.Minute), "", []string{secret}, service.SigTimestamp, float64(-360)},
	}
	for _, cs := range cases {
		t.Run(cs.name, func(t *testing.T) {
			if r := postInbox(t, app, inboxURL, body, cs.at, cs.header, cs.secret...); r.Status != http.StatusOK {
				t.Fatalf("%s: %d", cs.name, r.Status)
			}
			s := inboxRequests(t, c)[0]["signature"].(map[string]any)
			if s["valid"] != false || s["error"] != cs.want || s["skew_seconds"] != cs.skew {
				t.Errorf("%s: %v", cs.name, s)
			}
		})
	}

	// During a rotation both secrets verify, like the deliveries they check.
	rot := c.send("POST", "/v1/dashboard/webhook_endpoints/"+inbox["endpoint_id"].(string)+"/rotate_secret?mode=test", nil)
	postInbox(t, app, inboxURL, body, app.Clock.Now(), "", secret)
	if s := inboxRequests(t, c)[0]["signature"].(map[string]any); s["valid"] != true {
		t.Fatalf("old secret in the window: %v", s)
	}
	app.Clock.Advance(25 * time.Hour)
	c = login(t, app, "owner@example.com")
	postInbox(t, app, inboxURL, body, app.Clock.Now(), "", secret)
	if s := inboxRequests(t, c)[0]["signature"].(map[string]any); s["error"] != service.SigNoMatch {
		t.Fatalf("old secret after the window: %v", s)
	}
	postInbox(t, app, inboxURL, body, app.Clock.Now(), "", rot["secret"].(string))
	if s := inboxRequests(t, c)[0]["signature"].(map[string]any); s["valid"] != true {
		t.Fatalf("new secret: %v", s)
	}

	// Real deliveries: a test event sent by the dispatcher arrives and verifies.
	if _, err := app.Store.Pool().Exec(t.Context(), `UPDATE webhook_endpoints SET url = $2 WHERE id = $1`, inbox["endpoint_id"],
		app.Server.URL+service.InboxPath+token); err != nil {
		t.Fatal(err)
	}
	res := c.send("POST", "/v1/dashboard/webhook_endpoints/"+inbox["endpoint_id"].(string)+"/test?mode=test", map[string]any{"type": "session.overpaid"})
	if res["response"].(map[string]any)["status"] != float64(200) || !strings.Contains(res["response"].(map[string]any)["body"].(string), `"received":true`) {
		t.Fatalf("test event to the inbox: %v", res)
	}
	first := inboxRequests(t, c)[0]
	if first["event_type"] != "session.overpaid" || first["signature"].(map[string]any)["valid"] != true ||
		first["event_id"] != res["event"].(map[string]any)["id"] {
		t.Fatalf("delivered test event: %v", first)
	}

	// Size limit: a body above 64 KB is refused and not kept.
	n := len(inboxRequests(t, c))
	if r := postInbox(t, app, inboxURL, bytes.Repeat([]byte("a"), service.InboxMaxBodyBytes+1), app.Clock.Now(), "", secret); r.Status != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body: %d %s", r.Status, r.Body)
	}
	if len(inboxRequests(t, c)) != n {
		t.Fatal("an oversized body was kept")
	}
	if r := postInbox(t, app, apptest.CheckoutBase+service.InboxPath+"nope", body, app.Clock.Now(), "", secret); r.Status != http.StatusNotFound {
		t.Fatalf("unknown token: %d", r.Status)
	}
	if r := app.Do("GET", service.InboxPath+token, "", nil); r.Status != http.StatusMethodNotAllowed {
		t.Fatalf("GET on the inbox: %d", r.Status)
	}

	// Paging, newest first.
	page := c.get("/v1/dashboard/sandbox/inbox/requests?mode=test&limit=2")
	if len(page["data"].([]any)) != 2 || page["next_cursor"] == nil {
		t.Fatalf("page: %v", page)
	}
	next := c.get("/v1/dashboard/sandbox/inbox/requests?mode=test&limit=100&cursor=" + page["next_cursor"].(string))
	if len(next["data"].([]any)) != n-2 || next["next_cursor"] != nil {
		t.Fatalf("next page: %d of %d", len(next["data"].([]any)), n)
	}

	// Deleting the inbox closes the URL and its endpoint; a new one has a new token.
	c.send("DELETE", "/v1/dashboard/sandbox/inbox?mode=test", nil)
	if r := postInbox(t, app, inboxURL, body, app.Clock.Now(), "", secret); r.Status != http.StatusNotFound {
		t.Fatalf("deleted inbox still receives: %d", r.Status)
	}
	if got := c.get("/v1/dashboard/sandbox/inbox?mode=test"); got["inbox"] != nil {
		t.Fatalf("inbox after delete: %v", got)
	}
	if eps := c.get("/v1/dashboard/webhook_endpoints?mode=test")["data"].([]any); len(eps) != 0 {
		t.Fatalf("endpoint after delete: %v", eps)
	}
	if r := c.do("GET", "/v1/dashboard/sandbox/inbox/requests?mode=test", nil); r.Status != http.StatusNotFound {
		t.Fatalf("requests without an inbox: %d", r.Status)
	}
	if r := c.do("DELETE", "/v1/dashboard/sandbox/inbox?mode=test", nil); r.Status != http.StatusNotFound {
		t.Fatalf("delete twice: %d", r.Status)
	}
	fresh := c.send("POST", "/v1/dashboard/sandbox/inbox?mode=test", nil)["inbox"].(map[string]any)
	if fresh["url"] == inboxURL || fresh["id"] == inbox["id"] || len(inboxRequests(t, c)) != 0 {
		t.Fatalf("new inbox: %v", fresh)
	}
	// Deleting its endpoint from the endpoint list closes the inbox too.
	c.send("DELETE", "/v1/dashboard/webhook_endpoints/"+fresh["endpoint_id"].(string)+"?mode=test", nil)
	if got := c.get("/v1/dashboard/sandbox/inbox?mode=test"); got["inbox"] != nil {
		t.Fatalf("inbox after its endpoint was deleted: %v", got)
	}
	if !hasAll(auditActions(t, app, app.MerchantID), "sandbox_inbox.created", "sandbox_inbox.deleted", "webhook_endpoint.created") {
		t.Fatal("audit")
	}
}

func TestSandboxInboxLimits(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, &api.PublicOptions{Limits: map[string]api.Limit{api.RouteSandboxInbox: {PerMinute: 1, Burst: 2}}})
	c := login(t, app, "owner@example.com")
	inbox := c.send("POST", "/v1/dashboard/sandbox/inbox?mode=test", nil)["inbox"].(map[string]any)
	body := []byte(`{"id":"evt_1"}`)
	for range 2 {
		if r := postInbox(t, app, inbox["url"].(string), body, app.Clock.Now(), "", inbox["secret"].(string)); r.Status != http.StatusOK {
			t.Fatalf("within the burst: %d", r.Status)
		}
	}
	r := postInbox(t, app, inbox["url"].(string), body, app.Clock.Now(), "", inbox["secret"].(string))
	if r.Status != http.StatusTooManyRequests || r.Header.Get("Retry-After") == "" {
		t.Fatalf("rate limit: %d %s", r.Status, r.Body)
	}

	// Cap: the newest 500 are kept.
	i, err := app.Svc.LookupSandboxInbox(t.Context(), strings.TrimPrefix(inbox["url"].(string), apptest.CheckoutBase+service.InboxPath))
	if err != nil {
		t.Fatal(err)
	}
	var last string
	for k := range service.InboxMaxRequests + 5 {
		app.Clock.Advance(time.Millisecond)
		id, err := app.Svc.ReceiveSandboxRequest(t.Context(), i, "POST", http.Header{"X-N": {string(rune('a' + k%26))}}, body)
		if err != nil {
			t.Fatal(err)
		}
		last = id
	}
	var count int
	if err := app.Store.Pool().QueryRow(t.Context(), `SELECT count(*) FROM sandbox_requests`).Scan(&count); err != nil || count != service.InboxMaxRequests {
		t.Fatalf("kept %d %v", count, err)
	}
	if got := inboxRequests(t, c); got[0]["id"] != last {
		t.Fatalf("newest is not first: %v", got[0]["id"])
	}

	// Retention: after 7 days the list is empty and the purge deletes them.
	app.Clock.Advance(service.InboxRetention + time.Second)
	c = login(t, app, "owner@example.com")
	if got := inboxRequests(t, c); len(got) != 0 {
		t.Fatalf("expired requests shown: %d", len(got))
	}
	purged := 0
	for {
		n, err := app.Svc.PurgeSandbox(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
		purged += n
	}
	if purged != service.InboxMaxRequests {
		t.Fatalf("purged %d", purged)
	}

	// Live mode is refused on every inbox route; another merchant never sees this inbox.
	for _, p := range []struct{ method, path string }{{"GET", "/v1/dashboard/sandbox/inbox"}, {"POST", "/v1/dashboard/sandbox/inbox"},
		{"DELETE", "/v1/dashboard/sandbox/inbox"}, {"GET", "/v1/dashboard/sandbox/inbox/requests"}} {
		t.Run(p.path, func(t *testing.T) {
			if r := c.do(p.method, p.path+"?mode=live", nil); r.Status != http.StatusForbidden || r.ErrorCode(t) != "test_mode_only" {
				t.Errorf("%s %s live: %d %s", p.method, p.path, r.Status, r.Body)
			}
		})
	}
	secondMerchant(t, app, "other@example.com")
	intruder := login(t, app, "other@example.com")
	if got := intruder.get("/v1/dashboard/sandbox/inbox?mode=test"); got["inbox"] != nil {
		t.Fatalf("intruder sees an inbox: %v", got)
	}
	if r := intruder.do("GET", "/v1/dashboard/sandbox/inbox/requests?mode=test", nil); r.Status != http.StatusNotFound {
		t.Fatalf("intruder requests: %d %s", r.Status, r.Body)
	}
	if r := intruder.do("DELETE", "/v1/dashboard/sandbox/inbox?mode=test", nil); r.Status != http.StatusNotFound {
		t.Fatalf("intruder delete: %d", r.Status)
	}
	if got := c.get("/v1/dashboard/sandbox/inbox?mode=test"); got["inbox"] == nil {
		t.Fatal("the owner lost the inbox")
	}
}

// Concurrent creates leave exactly one inbox and one endpoint.
func TestSandboxInboxConcurrentCreate(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	a := service.Auth{MerchantID: app.MerchantID, KeyID: "dashboard", Actor: "test"}
	var wg sync.WaitGroup
	ids := make([]string, 8)
	errs := make([]error, 8)
	for k := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := app.Svc.CreateSandboxInbox(t.Context(), a)
			ids[k], errs[k] = v.ID, err
		}()
	}
	wg.Wait()
	for k := range ids {
		if errs[k] != nil || ids[k] != ids[0] {
			t.Fatalf("racer %d: %s %v (first %s)", k, ids[k], errs[k], ids[0])
		}
	}
	var inboxes, endpoints int
	if err := app.Store.Pool().QueryRow(t.Context(), `SELECT (SELECT count(*) FROM sandbox_inboxes), (SELECT count(*) FROM webhook_endpoints)`).
		Scan(&inboxes, &endpoints); err != nil || inboxes != 1 || endpoints != 1 {
		t.Fatalf("%d inboxes, %d endpoints, %v", inboxes, endpoints, err)
	}
}
