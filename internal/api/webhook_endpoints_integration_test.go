//go:build integration

package api_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/apptest"
)

// countingConsumer answers 200 and counts the requests it gets.
func countingConsumer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { n.Add(1) }))
	t.Cleanup(srv.Close)
	return srv, &n
}

// expiredEvent makes one test-mode event (session.expired) and sequences it.
func expiredEvent(t *testing.T, app *apptest.App, tag string) {
	t.Helper()
	r := app.Session(app.TestKey, map[string]any{"amount": "5.00"}, "Idempotency-Key", "d42-"+tag)
	if r.Status != http.StatusOK {
		t.Fatalf("session: %d %s", r.Status, r.Body)
	}
	id := r.JSON(t)["id"].(string)
	if sim := app.Do("POST", "/v1/test_helpers/sessions/"+id+"/simulate", app.TestKey, map[string]any{"action": "expire"}); sim.Status != http.StatusOK {
		t.Fatalf("expire: %d %s", sim.Status, sim.Body)
	}
	if _, err := app.Store.SequenceEvents(t.Context(), 100); err != nil {
		t.Fatal(err)
	}
}

type deliveryRow struct{ id, status, lastError string }

func endpointDeliveries(t *testing.T, app *apptest.App, endpointID string) []deliveryRow {
	t.Helper()
	rows, err := app.Store.Pool().Query(t.Context(), `SELECT id, status, coalesce(last_error, '') FROM webhook_deliveries
		WHERE endpoint_id = $1 AND NOT test ORDER BY id`, endpointID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []deliveryRow
	for rows.Next() {
		var d deliveryRow
		if err := rows.Scan(&d.id, &d.status, &d.lastError); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

func patchEndpoint(t *testing.T, app *apptest.App, key, id string, body any) apptest.Response {
	t.Helper()
	return app.Do("PATCH", "/v1/webhook_endpoints/"+id, key, body)
}

// An edit changes only what it sends, never the secret; a body without a field, a null url or
// status, a bad status, the other mode and a deleted endpoint are refused; each change is audited.
func TestWebhookEndpointEdit(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	consumer, _ := countingConsumer(t)
	r := app.Do("POST", "/v1/webhook_endpoints", app.TestKey, map[string]any{"url": consumer.URL + "/a", "description": "Order fulfilment",
		"enabled_events": []string{"session.paid"}})
	if r.Status != http.StatusOK {
		t.Fatalf("create: %d %s", r.Status, r.Body)
	}
	e := r.JSON(t)
	id := e["id"].(string)
	if e["description"] != "Order fulfilment" || e["status"] != "enabled" || e["disabled_reason"] != nil || e["failing_since"] != nil {
		t.Fatalf("created: %v", e)
	}
	secret := func() []byte {
		t.Helper()
		var b []byte
		if err := app.Store.Pool().QueryRow(t.Context(), `SELECT secret_ciphertext FROM webhook_endpoints WHERE id = $1`, id).Scan(&b); err != nil {
			t.Fatal(err)
		}
		return b
	}
	before := secret()

	for name, body := range map[string]any{
		"empty":             "",
		"no field":          map[string]any{},
		"null url":          map[string]any{"url": nil},
		"null events":       map[string]any{"enabled_events": nil},
		"null status":       map[string]any{"status": nil},
		"bad status":        map[string]any{"status": "paused"},
		"unknown field":     map[string]any{"secret": "whsec_x"},
		"unknown event":     map[string]any{"enabled_events": []string{"session.refunded"}},
		"long text":         map[string]any{"description": strings.Repeat("é", 201)},
		"live rule in test": map[string]any{"url": "http://shop.example/hooks"},
	} {
		t.Run(name, func(t *testing.T) {
			if r := patchEndpoint(t, app, app.TestKey, id, body); r.Status != http.StatusBadRequest || r.ErrorCode(t) != "invalid_request" {
				t.Errorf("%s: %d %s", name, r.Status, r.Body)
			}
		})
	}

	r = patchEndpoint(t, app, app.TestKey, id, map[string]any{"url": consumer.URL + "/b", "enabled_events": []string{}, "description": strings.Repeat("é", 200)})
	if r.Status != http.StatusOK {
		t.Fatalf("edit: %d %s", r.Status, r.Body)
	}
	got := r.JSON(t)
	if got["url"] != consumer.URL+"/b" || len(got["enabled_events"].([]any)) != 0 || got["secret"] != nil || got["created_at"] != e["created_at"] {
		t.Fatalf("edited: %v", got)
	}
	if !bytes.Equal(secret(), before) {
		t.Fatal("an edit changed the secret")
	}
	if r := patchEndpoint(t, app, app.TestKey, id, map[string]any{"description": ""}); r.Status != http.StatusOK || r.JSON(t)["description"] != nil {
		t.Fatalf("clear with empty: %d %s", r.Status, r.Body)
	}
	patchEndpoint(t, app, app.TestKey, id, map[string]any{"description": "x"})
	if r := patchEndpoint(t, app, app.TestKey, id, map[string]any{"description": nil}); r.Status != http.StatusOK || r.JSON(t)["description"] != nil {
		t.Fatalf("clear with null: %d %s", r.Status, r.Body)
	}
	// Sending the stored values (a form that sends the whole object) changes and audits nothing.
	n := len(auditDetails(t, app, "webhook_endpoint.updated"))
	if r := patchEndpoint(t, app, app.TestKey, id, map[string]any{"url": consumer.URL + "/b", "enabled_events": []string{}, "status": "enabled"}); r.Status != http.StatusOK {
		t.Fatalf("same values: %d %s", r.Status, r.Body)
	}
	updates := auditDetails(t, app, "webhook_endpoint.updated")
	if len(updates) != n || len(auditDetails(t, app, "webhook_endpoint.enabled")) != 0 {
		t.Fatalf("a no-op edit was audited: %v", updates)
	}
	if first := updates[0]; fmt.Sprint(first["fields"]) != "[url enabled_events description]" || first["url"] != consumer.URL+"/b" {
		t.Fatalf("audit: %v", first)
	}
	if strings.Contains(fmt.Sprint(updates[1:]), "url:") {
		t.Fatalf("a description edit recorded a url: %v", updates)
	}

	if r := patchEndpoint(t, app, app.LiveKey, id, map[string]any{"description": "x"}); r.Status != http.StatusNotFound {
		t.Fatalf("other mode: %d %s", r.Status, r.Body)
	}
	if r := app.Do("DELETE", "/v1/webhook_endpoints/"+id, app.TestKey, nil); r.Status != http.StatusOK {
		t.Fatalf("delete: %d %s", r.Status, r.Body)
	}
	if r := patchEndpoint(t, app, app.TestKey, id, map[string]any{"status": "enabled"}); r.Status != http.StatusNotFound {
		t.Fatalf("deleted: %d %s", r.Status, r.Body)
	}
	if d := auditDetails(t, app, "webhook_endpoint.deleted"); len(d) != 1 || d[0]["endpoint_id"] != id {
		t.Fatalf("delete audit: %v", d)
	}
}

// A pause discards the events sequenced while it lasts, dead-letters what was pending and
// refuses resends; test events still work; enable starts again from the next event.
func TestWebhookEndpointPauseAndEnable(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	consumer, calls := countingConsumer(t)
	id := c.send("POST", "/v1/dashboard/webhook_endpoints?mode=test", map[string]any{"url": consumer.URL})["id"].(string)
	expiredEvent(t, app, "1")
	pending := endpointDeliveries(t, app, id)
	if len(pending) != 1 || pending[0].status != "pending" {
		t.Fatalf("before the pause: %v", pending)
	}

	paused := c.send("PATCH", "/v1/dashboard/webhook_endpoints/"+id+"?mode=test", map[string]any{"status": "disabled"})
	if paused["status"] != "disabled" || paused["disabled_reason"] != "merchant" || paused["disabled_at"] != app.Clock.Now().Format(time.RFC3339) {
		t.Fatalf("paused: %v", paused)
	}
	if d := endpointDeliveries(t, app, id); d[0].status != "dead" || d[0].lastError != "endpoint paused" {
		t.Fatalf("pending delivery after the pause: %v", d)
	}
	expiredEvent(t, app, "2")
	if d := endpointDeliveries(t, app, id); len(d) != 1 {
		t.Fatalf("an event sequenced while paused got a delivery: %v", d)
	}
	if _, err := app.Dispatcher.RunOnce(t.Context()); err != nil || calls.Load() != 0 {
		t.Fatalf("a paused endpoint got a request: %d %v", calls.Load(), err)
	}
	if r := c.do("POST", "/v1/dashboard/webhook_deliveries/"+pending[0].id+"/resend?mode=test", nil); r.Status != http.StatusConflict ||
		r.ErrorCode(t) != "endpoint_disabled" {
		t.Fatalf("resend while paused: %d %s", r.Status, r.Body)
	}
	if r := app.Do("POST", "/v1/webhook_endpoints/"+id+"/test", app.TestKey, map[string]any{"type": "session.paid"}); r.Status != http.StatusOK ||
		calls.Load() != 1 {
		t.Fatalf("test event on a paused endpoint: %d %s", r.Status, r.Body)
	}
	if list := app.Do("GET", "/v1/webhook_endpoints", app.TestKey, nil).JSON(t)["data"].([]any); len(list) != 1 ||
		list[0].(map[string]any)["status"] != "disabled" {
		t.Fatalf("list: %v", list)
	}
	// Pausing again keeps the first disabled_at.
	app.Clock.Advance(time.Hour)
	if again := c.send("PATCH", "/v1/dashboard/webhook_endpoints/"+id+"?mode=test", map[string]any{"status": "disabled"}); again["disabled_at"] != paused["disabled_at"] {
		t.Fatalf("a second pause moved disabled_at: %v", again)
	}

	enabled := c.send("PATCH", "/v1/dashboard/webhook_endpoints/"+id+"?mode=test", map[string]any{"status": "enabled"})
	if enabled["status"] != "enabled" || enabled["disabled_reason"] != nil || enabled["disabled_at"] != nil || enabled["failing_since"] != nil {
		t.Fatalf("enabled: %v", enabled)
	}
	expiredEvent(t, app, "3")
	if d := endpointDeliveries(t, app, id); len(d) != 2 || d[1].status != "pending" {
		t.Fatalf("after enable: %v", d)
	}
	if _, err := app.Dispatcher.RunOnce(t.Context()); err != nil || calls.Load() != 2 {
		t.Fatalf("after enable the next event goes out: %d %v", calls.Load(), err)
	}
	if r := c.send("POST", "/v1/dashboard/webhook_deliveries/"+pending[0].id+"/resend?mode=test", nil); r["status"] != "pending" {
		t.Fatalf("resend after enable: %v", r)
	}
	if !hasAll(auditActions(t, app, app.MerchantID), "webhook_endpoint.disabled", "webhook_endpoint.enabled") {
		t.Fatalf("audit: %v", auditActions(t, app, app.MerchantID))
	}
	if d := auditDetails(t, app, "webhook_endpoint.disabled"); len(d) != 1 || d[0]["disabled_reason"] != "merchant" || !strings.HasPrefix(d[0]["_actor"].(string), "dashboard:") {
		t.Fatalf("disabled audit: %v", d)
	}
}

// Enable re-checks the stored URL against the rules of now.
func TestWebhookEndpointEnableRechecksTheURL(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	r := app.Do("POST", "/v1/webhook_endpoints", app.LiveKey, map[string]any{"url": "https://shop.example/hooks"})
	if r.Status != http.StatusOK {
		t.Fatalf("create: %d %s", r.Status, r.Body)
	}
	id := r.JSON(t)["id"].(string)
	if r := patchEndpoint(t, app, app.LiveKey, id, map[string]any{"status": "disabled"}); r.Status != http.StatusOK {
		t.Fatalf("pause: %d %s", r.Status, r.Body)
	}
	// A URL stored before today's rules: live over plain http.
	if _, err := app.Store.Pool().Exec(t.Context(), `UPDATE webhook_endpoints SET url = 'http://shop.example/hooks' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if r := patchEndpoint(t, app, app.LiveKey, id, map[string]any{"status": "enabled"}); r.Status != http.StatusBadRequest {
		t.Fatalf("enable with a URL the rules refuse: %d %s", r.Status, r.Body)
	}
	r = patchEndpoint(t, app, app.LiveKey, id, map[string]any{"status": "enabled", "url": "https://shop.example/v2"})
	if r.Status != http.StatusOK || r.JSON(t)["status"] != "enabled" {
		t.Fatalf("enable with a new URL: %d %s", r.Status, r.Body)
	}
}

// The sandbox inbox's endpoint refuses edits and does not count towards the 16 per mode;
// deleted endpoints do not count either.
func TestWebhookEndpointLimitAndInbox(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	inbox := c.send("POST", "/v1/dashboard/sandbox/inbox?mode=test", nil)["inbox"].(map[string]any)
	inboxEndpoint := inbox["endpoint_id"].(string)
	if r := patchEndpoint(t, app, app.TestKey, inboxEndpoint, map[string]any{"status": "disabled"}); r.Status != http.StatusBadRequest {
		t.Fatalf("inbox edit: %d %s", r.Status, r.Body)
	}
	var first string
	for i := range 16 {
		r := app.Do("POST", "/v1/webhook_endpoints", app.TestKey, map[string]any{"url": fmt.Sprintf("https://shop.example/%d", i)})
		if r.Status != http.StatusOK {
			t.Fatalf("endpoint %d: %d %s", i, r.Status, r.Body)
		}
		if i == 0 {
			first = r.JSON(t)["id"].(string)
		}
	}
	patchEndpoint(t, app, app.TestKey, first, map[string]any{"status": "disabled"})
	if r := app.Do("POST", "/v1/webhook_endpoints", app.TestKey, map[string]any{"url": "https://shop.example/17"}); r.Status != http.StatusBadRequest {
		t.Fatalf("17th: %d %s", r.Status, r.Body)
	}
	if r := app.Do("POST", "/v1/webhook_endpoints", app.LiveKey, map[string]any{"url": "https://shop.example/live"}); r.Status != http.StatusOK {
		t.Fatalf("the other mode has its own 16: %d %s", r.Status, r.Body)
	}
	app.Do("DELETE", "/v1/webhook_endpoints/"+first, app.TestKey, nil)
	if r := app.Do("POST", "/v1/webhook_endpoints", app.TestKey, map[string]any{"url": "https://shop.example/17"}); r.Status != http.StatusOK {
		t.Fatalf("after a delete: %d %s", r.Status, r.Body)
	}
}

// Concurrent creates never pass 16.
func TestWebhookEndpointLimitUnderConcurrency(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	ctx := t.Context()
	var ok atomic.Int32
	done := make(chan struct{})
	for i := range 24 {
		go func() {
			defer func() { done <- struct{}{} }()
			body, _ := json.Marshal(map[string]any{"url": fmt.Sprintf("https://shop.example/%d", i)})
			req, _ := http.NewRequestWithContext(ctx, "POST", app.Server.URL+"/v1/webhook_endpoints", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+app.TestKey)
			resp, err := http.DefaultClient.Do(req)
			if err == nil {
				if resp.StatusCode == http.StatusOK {
					ok.Add(1)
				}
				resp.Body.Close()
			}
		}()
	}
	for range 24 {
		<-done
	}
	if ok.Load() != 16 {
		t.Fatalf("%d endpoints created, want 16", ok.Load())
	}
}
