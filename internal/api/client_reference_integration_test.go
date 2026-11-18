//go:build integration

package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func refOf(t *testing.T, ses map[string]any) (any, any) {
	t.Helper()
	id, ok1 := ses["client_reference_id"]
	src, ok2 := ses["client_reference_source"]
	if !ok1 || !ok2 {
		t.Fatalf("session lacks the client reference fields: %v", ses)
	}
	return id, src
}

func TestClientReferenceFromTheMerchantAPI(t *testing.T) {
	t.Parallel()
	app := linkApp(t, nil)
	r := app.Session(app.TestKey, map[string]any{"amount": "25.00", "client_reference_id": "order-42"}, idem("m1")...)
	if r.Status != http.StatusOK {
		t.Fatalf("create: %d %s", r.Status, r.Body)
	}
	ses := r.JSON(t)
	if id, src := refOf(t, ses); id != "order-42" || src != "merchant" {
		t.Fatalf("merchant reference: %v %v", id, src)
	}
	got := app.Do("GET", "/v1/sessions/"+ses["id"].(string), app.TestKey, nil).JSON(t)
	if id, src := refOf(t, got); id != "order-42" || src != "merchant" {
		t.Fatalf("GET: %v %v", id, src)
	}
	prod := createProduct(t, app, app.TestKey, map[string]any{"name": "Founder", "price_type": "fixed", "amount": "25.00"})
	r = app.Session(app.TestKey, map[string]any{"product_id": prod, "client_reference_id": "user:f36361bc"}, idem("m2")...)
	if id, src := refOf(t, r.JSON(t)); id != "user:f36361bc" || src != "merchant" {
		t.Fatalf("with a product: %v %v", id, src)
	}
	if id, src := refOf(t, app.Session(app.TestKey, map[string]any{"amount": "25.00"}, idem("m3")...).JSON(t)); id != nil || src != nil {
		t.Fatalf("without a reference: %v %v", id, src)
	}
	for i, bad := range []any{"", strings.Repeat("a", 129), "order 42", "order/42", "pedido-é", "<script>", "a\nb", 42, true} {
		r := app.Session(app.TestKey, map[string]any{"amount": "25.00", "client_reference_id": bad}, idem(fmt.Sprintf("bad-%d", i))...)
		if r.Status != http.StatusBadRequest || r.ErrorCode(t) != "invalid_request" {
			t.Errorf("%q: %d %s", bad, r.Status, r.Body)
		}
	}
	if r := app.Session(app.TestKey, map[string]any{"amount": "25.00", "client_reference_id": strings.Repeat("A1._:-", 21) + "xy"},
		idem("max")...); r.Status != http.StatusOK {
		t.Fatalf("128 characters: %d %s", r.Status, r.Body)
	}
	// The source is never the caller's to choose.
	r = app.Session(app.TestKey, map[string]any{"amount": "25.00", "client_reference_id": "x", "client_reference_source": "payer"}, idem("src")...)
	if r.Status != http.StatusBadRequest {
		t.Fatalf("source in the body: %d %s", r.Status, r.Body)
	}
}

func TestClientReferenceFromAPaymentLink(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	rc := newReceiver(t, http.StatusOK, "ok")
	createEndpoint(t, app, app.TestKey, rc.srv.URL)
	_, prod := productLink(t, app, app.TestKey, map[string]any{"name": "Founder", "price_type": "fixed", "amount": "25.00"})
	linkID := prod

	r := openLink(app, prod, "payer-1", map[string]any{"client_reference_id": "cart:7"})
	if r.Status != http.StatusOK {
		t.Fatalf("open: %d %s", r.Status, r.Body)
	}
	// The payer-facing session never shows it back: it is the merchant's correlation value.
	if strings.Contains(string(r.Body), "client_reference") || strings.Contains(string(r.Body), "cart:7") {
		t.Fatalf("public session shows the reference: %s", r.Body)
	}
	pub := r.JSON(t)
	sesID := pub["id"].(string)
	if g := app.Do("GET", "/v1/checkout/sessions/"+sesID, "", nil); strings.Contains(string(g.Body), "cart:7") {
		t.Fatalf("public GET shows the reference: %s", g.Body)
	}
	ses := app.Do("GET", "/v1/sessions/"+sesID, app.TestKey, nil).JSON(t)
	if id, src := refOf(t, ses); id != "cart:7" || src != "payer" {
		t.Fatalf("payer reference: %v %v", id, src)
	}
	// Idempotency covers the reference: same key, same body replays; another reference conflicts.
	if again := openLink(app, prod, "payer-1", map[string]any{"client_reference_id": "cart:7"}); again.Header.Get("Idempotent-Replayed") != "true" ||
		again.JSON(t)["id"] != sesID {
		t.Fatalf("replay: %d %s", again.Status, again.Body)
	}
	if other := openLink(app, prod, "payer-1", map[string]any{"client_reference_id": "cart:8"}); other.Status != http.StatusConflict {
		t.Fatalf("another reference with the same key: %d %s", other.Status, other.Body)
	}
	for i, body := range []map[string]any{
		{"client_reference_id": "cart 7"},
		{"client_reference_id": strings.Repeat("x", 129)},
		{"client_reference_id": ""},
		{"client_reference_id": "cart:7", "client_reference_source": "merchant"},
	} {
		if r := openLink(app, prod, fmt.Sprintf("bad-%d", i), body); r.Status != http.StatusBadRequest || r.ErrorCode(t) != "invalid_request" {
			t.Errorf("%v: %d %s", body, r.Status, r.Body)
		}
	}
	plain := openLink(app, prod, "payer-2", map[string]any{}).JSON(t)
	if id, src := refOf(t, app.Do("GET", "/v1/sessions/"+plain["id"].(string), app.TestKey, nil).JSON(t)); id != nil || src != nil {
		t.Fatalf("a link opened without a reference: %v %v", id, src)
	}

	// Webhooks and the feed carry it with its source.
	merchant := app.Session(app.TestKey, map[string]any{"amount": "25.00", "client_reference_id": "order-9"}, idem("srv")...).JSON(t)
	payFull(t, app, ses, "0x"+strings.Repeat("ab", 32))
	payFull(t, app, merchant, "0x"+strings.Repeat("cd", 32))
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
	// Each also says the link it was opened from, or none for an API session.
	want := map[string][3]string{sesID: {"cart:7", "payer", linkID}, merchant["id"].(string): {"order-9", "merchant", ""}}
	type envelope struct {
		Type string `json:"type"`
		Data struct {
			Session struct {
				ID     string  `json:"id"`
				Ref    *string `json:"client_reference_id"`
				Source *string `json:"client_reference_source"`
				LinkID *string `json:"link_id"`
			} `json:"session"`
		} `json:"data"`
	}
	check := func(where string, raw []byte) bool {
		t.Helper()
		var e envelope
		if err := json.Unmarshal(raw, &e); err != nil {
			t.Fatalf("%s: %v", where, err)
		}
		w, ok := want[e.Data.Session.ID]
		if !ok {
			if e.Data.Session.Ref != nil {
				t.Errorf("%s: %s has a reference", where, e.Data.Session.ID)
			}
			return false
		}
		if e.Data.Session.Ref == nil || *e.Data.Session.Ref != w[0] || e.Data.Session.Source == nil || *e.Data.Session.Source != w[1] {
			t.Errorf("%s %s %s: %v %v", where, e.Type, e.Data.Session.ID, e.Data.Session.Ref, e.Data.Session.Source)
		}
		if got := e.Data.Session.LinkID; (w[2] == "" && got != nil) || (w[2] != "" && (got == nil || *got != w[2])) ||
			!strings.Contains(string(raw), `"link_id":`) {
			t.Errorf("%s %s %s: link_id %v", where, e.Type, e.Data.Session.ID, got)
		}
		return true
	}
	seen := 0
	for _, d := range rc.all() {
		if check("webhook", d.body) {
			seen++
		}
	}
	if seen < 4 {
		t.Fatalf("%d webhooks about the two sessions", seen)
	}
	seen = 0
	for _, raw := range app.Do("GET", "/v1/events?after=0&limit=200", app.TestKey, nil).JSON(t)["data"].([]any) {
		b, _ := json.Marshal(raw)
		if check("feed", b) {
			seen++
		}
	}
	if seen < 4 {
		t.Fatalf("%d feed events about the two sessions", seen)
	}

	// Dashboard: an exact filter, and the detail.
	if got := sessionIDs(t, c, "/v1/dashboard/sessions?mode=test&client_reference_id=cart:7"); len(got) != 1 || got[0] != sesID {
		t.Fatalf("filter: %v", got)
	}
	if got := sessionIDs(t, c, "/v1/dashboard/sessions?mode=test&client_reference_id=order-9"); len(got) != 1 || got[0] != merchant["id"] {
		t.Fatalf("filter merchant reference: %v", got)
	}
	if got := sessionIDs(t, c, "/v1/dashboard/sessions?mode=test&client_reference_id=cart"); len(got) != 0 {
		t.Fatalf("filter is exact: %v", got)
	}
	if got := sessionIDs(t, c, "/v1/dashboard/sessions?mode=live&client_reference_id=cart:7"); len(got) != 0 {
		t.Fatalf("filter crosses modes: %v", got)
	}
	if r := c.do("GET", "/v1/dashboard/sessions?mode=test&client_reference_id=cart%207", nil); r.Status != http.StatusBadRequest {
		t.Fatalf("bad filter: %d %s", r.Status, r.Body)
	}
	d := c.get("/v1/dashboard/sessions/" + sesID + "?mode=test")
	if id, src := refOf(t, d); id != "cart:7" || src != "payer" {
		t.Fatalf("detail: %v %v", id, src)
	}
	// Another merchant never finds it.
	secondMerchant(t, app, "other@example.com")
	other := login(t, app, "other@example.com")
	if got := sessionIDs(t, other, "/v1/dashboard/sessions?mode=test&client_reference_id=cart:7"); len(got) != 0 {
		t.Fatalf("another merchant: %v", got)
	}
}
