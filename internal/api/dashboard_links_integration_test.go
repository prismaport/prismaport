//go:build integration

package api_test

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// dashProduct creates a product with the dashboard API.
func dashProduct(t *testing.T, c *dashClient, mode, key string, body map[string]any) map[string]any {
	t.Helper()
	return c.send("POST", "/v1/dashboard/products?mode="+mode, body, "Idempotency-Key", key)
}

// dashLink creates a payment link with the dashboard API.
func dashLink(t *testing.T, c *dashClient, mode, key string, body map[string]any) map[string]any {
	t.Helper()
	return c.send("POST", "/v1/dashboard/links?mode="+mode, body, "Idempotency-Key", key)
}

// dashProductLink creates a product and a plain link of it, answering the link.
func dashProductLink(t *testing.T, c *dashClient, mode, key string, product map[string]any) map[string]any {
	t.Helper()
	p := dashProduct(t, c, mode, key, product)
	return dashLink(t, c, mode, key, map[string]any{"product_id": p["id"]})
}

func stats(l map[string]any) map[string]any { return l["stats"].(map[string]any) }

// The whole owner journey: sign in, create a product and a link of it, a payer opens it and
// pays, the stats follow, the owner deactivates the link and the public page refuses new payers.
func TestDashboardProductAndLinkFlow(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	p := dashProduct(t, c, "test", "create-1", map[string]any{"name": "Pay Acme", "description": "Anything you like.", "price_type": "open",
		"min_amount": "1.00", "max_amount": "500.00", "suggested_amounts": []string{"5.00", "20.00"}, "metadata": map[string]string{"source": "dashboard"}})
	pid := p["id"].(string)
	if p["price_type"] != "open" || p["description"] != "Anything you like." || p["active"] != true || p["livemode"] != false ||
		p["min_amount"] != "1.00" || p["amount"] != nil || p["currency"] != "USD" || p["success_url"] != nil || len(p["suggested_amounts"].([]any)) != 2 ||
		p["links"].(map[string]any)["total"] != float64(0) || p["updated_at"] == nil || p["payout"].(map[string]any)["base-sepolia"] == nil {
		t.Fatalf("created product: %v", p)
	}
	for _, k := range []string{"url", "expires_at", "max_payments", "kind"} {
		if _, ok := p[k]; ok {
			t.Fatalf("a product has %s: %v", k, p)
		}
	}
	l := dashLink(t, c, "test", "create-1", map[string]any{"product_id": pid, "name": "Newsletter"})
	id := l["id"].(string)
	product := l["product"].(map[string]any)
	if !strings.HasPrefix(id, "plink_") || l["product_id"] != pid || l["name"] != "Newsletter" || l["live"] != true || l["unavailable_reason"] != nil ||
		l["active"] != true || l["livemode"] != false || l["url"] != "https://pay.test/l/"+id || l["amount"] != nil || l["expires_at"] != nil ||
		l["max_payments"] != nil || product["id"] != pid || product["name"] != "Pay Acme" || product["price_type"] != "open" ||
		product["min_amount"] != "1.00" || product["max_amount"] != "500.00" || product["amount"] != nil || product["active"] != true ||
		product["description"] != "Anything you like." || fmt.Sprint(product["suggested_amounts"]) != "[5.00 20.00]" {
		t.Fatalf("created link: %v", l)
	}
	for _, k := range []string{"payout", "kind"} {
		if _, ok := l[k]; ok {
			t.Fatalf("a link has %s: %v", k, l)
		}
	}
	if st := stats(l); st["opened"] != float64(0) || st["paid"] != float64(0) || len(st["received"].([]any)) != 0 ||
		st["last_paid_at"] != nil || st["conversion"] != nil {
		t.Fatalf("new link stats: %v", st)
	}

	pub := publicLink(t, app, id)
	if pub["live"] != true || pub["name"] != "Pay Acme" || pub["description"] != "Anything you like." {
		t.Fatalf("public view: %v", pub)
	}
	paid := openLink(app, id, "payer-1", map[string]any{"amount": "12.50"})
	if paid.Status != http.StatusOK {
		t.Fatalf("public open: %d %s", paid.Status, paid.Body)
	}
	app.Clock.Advance(time.Second)
	pending := openLink(app, id, "payer-2", map[string]any{"amount": "3.00"})
	if pending.Status != http.StatusOK {
		t.Fatalf("second open: %d %s", pending.Status, pending.Body)
	}
	app.Clock.Advance(time.Minute)
	payFull(t, app, paid.JSON(t), "0x"+fmt.Sprintf("%064x", 1))

	l = c.get("/v1/dashboard/links/" + id + "?mode=test")
	st := stats(l)
	recv := st["received"].([]any)
	if st["opened"] != float64(2) || st["paid"] != float64(1) || len(recv) != 1 ||
		recv[0].(map[string]any)["asset"] != "USDC" || recv[0].(map[string]any)["amount"] != "12.500000" ||
		st["last_paid_at"] != app.Clock.Now().Format(time.RFC3339) || st["conversion"] != "0.5000" {
		t.Fatalf("stats after a payment: %v", st)
	}
	sessions := c.get("/v1/dashboard/links/" + id + "/sessions?mode=test")["data"].([]any)
	if len(sessions) != 2 || sessions[0].(map[string]any)["id"] != pending.JSON(t)["id"] || sessions[1].(map[string]any)["status"] != "paid" ||
		sessions[1].(map[string]any)["metadata"].(map[string]any)["source"] != "dashboard" || sessions[1].(map[string]any)["link_name"] != "Newsletter" ||
		sessions[1].(map[string]any)["product_name"] != "Pay Acme" || sessions[1].(map[string]any)["link_id"] != id {
		t.Fatalf("link sessions: %v", sessions)
	}
	// A product sums every session of the product: the link's and the API's.
	api := app.Session(app.TestKey, map[string]any{"product_id": pid, "amount": "8.00"}, idem("api-1")...)
	if api.Status != http.StatusOK {
		t.Fatalf("API session: %d %s", api.Status, api.Body)
	}
	payFull(t, app, api.JSON(t), "0x"+fmt.Sprintf("%064x", 3))
	got := c.get("/v1/dashboard/products/" + pid + "?mode=test")
	if st := stats(got); st["opened"] != float64(3) || st["paid"] != float64(2) || st["received"].([]any)[0].(map[string]any)["amount"] != "20.500000" ||
		got["links"].(map[string]any)["live"] != float64(1) || got["links"].(map[string]any)["total"] != float64(1) {
		t.Fatalf("product stats: %v", got)
	}
	if st := stats(c.get("/v1/dashboard/links/" + id + "?mode=test")); st["opened"] != float64(2) {
		t.Fatalf("an API session counted on the link: %v", st)
	}
	if ps := c.get("/v1/dashboard/products/" + pid + "/sessions?mode=test")["data"].([]any); len(ps) != 3 || ps[0].(map[string]any)["link_id"] != nil ||
		ps[0].(map[string]any)["link_name"] != nil || ps[0].(map[string]any)["product_name"] != "Pay Acme" {
		t.Fatalf("product sessions: %v", ps)
	}
	if pl := c.get("/v1/dashboard/products/" + pid + "/links?mode=test")["data"].([]any); len(pl) != 1 || pl[0].(map[string]any)["id"] != id {
		t.Fatalf("product links: %v", pl)
	}
	for _, path := range []string{"/v1/dashboard/products/prod_nope/links?mode=test", "/v1/dashboard/products/prod_nope/sessions?mode=test",
		"/v1/dashboard/products/prod_nope?mode=test"} {
		if r := c.do("GET", path, nil); r.Status != http.StatusNotFound || r.ErrorCode(t) != "product_not_found" {
			t.Errorf("%s: %d %s", path, r.Status, r.Body)
		}
	}
	for _, path := range []string{"/v1/dashboard/links/plink_nope?mode=test", "/v1/dashboard/links/plink_nope/sessions?mode=test", "/v1/dashboard/links/" + pid + "?mode=test"} {
		if r := c.do("GET", path, nil); r.Status != http.StatusNotFound || r.ErrorCode(t) != "payment_link_not_found" {
			t.Errorf("%s: %d %s", path, r.Status, r.Body)
		}
	}

	l = c.send("PATCH", "/v1/dashboard/links/"+id+"?mode=test", map[string]any{"active": false})
	if l["active"] != false || l["live"] != false || l["unavailable_reason"] != "inactive" {
		t.Fatalf("deactivated: %v", l)
	}
	pub = publicLink(t, app, id)
	if pub["live"] != false || pub["active"] != false || pub["unavailable_reason"] != "inactive" || pub["min_amount"] != nil {
		t.Fatalf("public view of a deactivated link: %v", pub)
	}
	if r := openLink(app, id, "payer-3", map[string]any{"amount": "5.00"}); r.Status != http.StatusGone || r.ErrorCode(t) != "link_unavailable" {
		t.Fatalf("public open after deactivation: %d %s", r.Status, r.Body)
	}
	// The session opened before deactivation still completes.
	payFull(t, app, pending.JSON(t), "0x"+fmt.Sprintf("%064x", 2))
	if st := stats(c.get("/v1/dashboard/links/" + id + "?mode=test")); st["paid"] != float64(2) ||
		st["received"].([]any)[0].(map[string]any)["amount"] != "15.500000" || st["conversion"] != "1.0000" {
		t.Fatalf("stats after the open session paid: %v", st)
	}
	c.send("PATCH", "/v1/dashboard/products/"+pid+"?mode=test", map[string]any{"name": "Pay Acme Inc"})
	if !hasAll(auditActions(t, app, app.MerchantID), "product.created", "payment_link.created", "payment_link.updated", "product.updated") {
		t.Fatalf("audit: %v", auditActions(t, app, app.MerchantID))
	}
	for _, a := range auditActions(t, app, app.MerchantID) {
		if a == "link.created" || a == "link.updated" {
			t.Fatalf("an old audit action: %v", auditActions(t, app, app.MerchantID))
		}
	}
}

func TestDashboardProductAndLinkIdempotency(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	body := map[string]any{"name": "Founder", "price_type": "fixed", "amount": "25.00"}
	first := c.do("POST", "/v1/dashboard/products?mode=test", body, "Idempotency-Key", "k1")
	again := c.do("POST", "/v1/dashboard/products?mode=test", body, "Idempotency-Key", "k1")
	if first.Status != http.StatusOK || again.Status != http.StatusOK || again.JSON(t)["id"] != first.JSON(t)["id"] ||
		again.Header.Get("Idempotent-Replayed") != "true" || first.Header.Get("Idempotent-Replayed") != "" {
		t.Fatalf("replay: %d %s / %d %s", first.Status, first.Body, again.Status, again.Body)
	}
	if r := c.do("POST", "/v1/dashboard/products?mode=test", map[string]any{"name": "Founder", "price_type": "fixed", "amount": "26.00"},
		"Idempotency-Key", "k1"); r.Status != http.StatusConflict || r.ErrorCode(t) != "idempotency_conflict" {
		t.Fatalf("same key, other body: %d %s", r.Status, r.Body)
	}
	if r := c.do("POST", "/v1/dashboard/products?mode=test", body); r.Status != http.StatusBadRequest {
		t.Fatalf("missing key: %d %s", r.Status, r.Body)
	}
	pid := first.JSON(t)["id"].(string)
	// The same key creates a link too: products and links have their own keys.
	link := c.do("POST", "/v1/dashboard/links?mode=test", map[string]any{"product_id": pid}, "Idempotency-Key", "k1")
	linkAgain := c.do("POST", "/v1/dashboard/links?mode=test", map[string]any{"product_id": pid}, "Idempotency-Key", "k1")
	if link.Status != http.StatusOK || linkAgain.JSON(t)["id"] != link.JSON(t)["id"] || linkAgain.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("link replay: %d %s / %s", link.Status, link.Body, linkAgain.Body)
	}
	// Keys are per mode: the same key in live mode creates a live product.
	live := c.send("POST", "/v1/dashboard/products?mode=live", body, "Idempotency-Key", "k1")
	if live["id"] == pid || live["livemode"] != true {
		t.Fatalf("live with the same key: %v", live)
	}
	// Keys are apart from the merchant API's: an API session with key k1 is not a replay.
	if r := app.Session(app.TestKey, map[string]any{"amount": "7.00"}, idem("k1")...); r.Status != http.StatusOK {
		t.Fatalf("API key namespace: %d %s", r.Status, r.Body)
	}

	var wg sync.WaitGroup
	products := make([]string, 8)
	links := make([]string, 8)
	for i := range products {
		wg.Add(2)
		go func() {
			defer wg.Done()
			r := c.do("POST", "/v1/dashboard/products?mode=test", map[string]any{"name": "Race", "price_type": "fixed", "amount": "9.00"}, "Idempotency-Key", "race")
			if r.Status == http.StatusOK {
				products[i] = r.JSON(t)["id"].(string)
			}
		}()
		go func() {
			defer wg.Done()
			r := c.do("POST", "/v1/dashboard/links?mode=test", map[string]any{"product_id": pid, "name": "Race"}, "Idempotency-Key", "race")
			if r.Status == http.StatusOK {
				links[i] = r.JSON(t)["id"].(string)
			}
		}()
	}
	wg.Wait()
	for i := range products {
		if products[i] == "" || products[i] != products[0] || links[i] == "" || links[i] != links[0] {
			t.Fatalf("concurrent creates with one key: %v %v", products, links)
		}
	}
	var n, m int
	if err := app.Store.Pool().QueryRow(t.Context(), `SELECT (SELECT count(*) FROM products WHERE name = 'Race'),
		(SELECT count(*) FROM payment_links WHERE name = 'Race')`).Scan(&n, &m); err != nil || n != 1 || m != 1 {
		t.Fatalf("%d products and %d links for one key (%v)", n, m, err)
	}
}

func TestDashboardProductValidation(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	for i, bad := range []map[string]any{
		{"price_type": "fixed", "amount": "25.00"},
		{"name": "x", "amount": "25.00"},
		{"name": "x", "price_type": "donation", "amount": "25.00"},
		{"name": "x", "price_type": "open", "amount": "25.00"},
		{"name": "x", "price_type": "fixed", "min_amount": "1.00"},
		{"name": "x", "kind": "fixed", "amount": "25.00"},
		{"name": "x", "price_type": "fixed", "amount": "25.00", "active": false},
		{"name": "x", "price_type": "fixed", "amount": "0.10"},
		{"name": "x", "price_type": "fixed", "amount": "25.001"},
		{"name": "x", "price_type": "open", "min_amount": "5.00", "suggested_amounts": []string{"1.00"}},
		{"name": "x", "price_type": "open", "min_amount": "5.00", "suggested_amounts": []string{"5.00", "6.00", "7.00", "8.00"}},
		{"name": "x", "price_type": "fixed", "amount": "25.00", "expires_at": app.Clock.Now().Add(time.Hour).Format(time.RFC3339)},
		{"name": "x", "price_type": "fixed", "amount": "25.00", "max_payments": 3},
		{"name": "x", "price_type": "fixed", "amount": "25.00", "success_url": "javascript:alert(1)"},
		{"name": "x", "price_type": "fixed", "amount": "25.00", "description": strings.Repeat("d", 501)},
		{"name": strings.Repeat("n", 201), "price_type": "fixed", "amount": "25.00"},
	} {
		r := c.do("POST", "/v1/dashboard/products?mode=test", bad, "Idempotency-Key", fmt.Sprintf("bad-%d", i))
		if r.Status != http.StatusBadRequest {
			t.Errorf("%v: %d %s", bad, r.Status, r.Body)
		}
	}
	p := dashProduct(t, c, "test", "ok", map[string]any{"name": "Founder", "price_type": "fixed", "amount": "25.00"})
	id := p["id"].(string)
	if r := c.do("PATCH", "/v1/dashboard/products/"+id+"?mode=test", map[string]any{"price_type": "open"}); r.Status != http.StatusBadRequest {
		t.Fatalf("price_type change: %d %s", r.Status, r.Body)
	}
	p = c.send("PATCH", "/v1/dashboard/products/"+id+"?mode=test", map[string]any{"amount": "30.00", "name": "Founder plus",
		"description": "Plus.", "success_url": "https://example.com/thanks"})
	if p["amount"] != "30.00" || p["name"] != "Founder plus" || p["description"] != "Plus." || p["success_url"] != "https://example.com/thanks" {
		t.Fatalf("patch: %v", p)
	}
	p = c.send("PATCH", "/v1/dashboard/products/"+id+"?mode=test", map[string]any{"description": nil, "success_url": nil})
	if p["description"] != nil || p["success_url"] != nil {
		t.Fatalf("clear: %v", p)
	}
	if r := c.do("PATCH", "/v1/dashboard/products/prod_01JBNOPE?mode=test", map[string]any{"active": false}); r.Status != http.StatusNotFound {
		t.Fatalf("unknown product: %d %s", r.Status, r.Body)
	}
}

func TestDashboardLinkValidation(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	fixed := dashProduct(t, c, "test", "fixed", map[string]any{"name": "Founder", "price_type": "fixed", "amount": "25.00"})["id"]
	open := dashProduct(t, c, "test", "open", map[string]any{"name": "Quote", "price_type": "open", "min_amount": "10.00", "max_amount": "100.00"})["id"]
	for i, bad := range []map[string]any{
		{},
		{"product_id": "prod_nope"},
		{"product_id": fixed, "amount": "25.00"},
		{"product_id": open, "amount": "9.00"},
		{"product_id": open, "amount": "101.00"},
		{"product_id": fixed, "active": false},
		{"product_id": fixed, "expires_at": app.Clock.Now().Add(-time.Hour).Format(time.RFC3339)},
		{"product_id": fixed, "max_payments": 0},
		{"product_id": fixed, "success_url": "javascript:alert(1)"},
		{"product_id": fixed, "name": strings.Repeat("n", 201)},
		{"product_id": fixed, "payout": map[string]any{}},
		{"product_id": fixed, "kind": "fixed"},
	} {
		r := c.do("POST", "/v1/dashboard/links?mode=test", bad, "Idempotency-Key", fmt.Sprintf("bad-%d", i))
		if r.Status != http.StatusBadRequest && r.Status != http.StatusNotFound {
			t.Errorf("%v: %d %s", bad, r.Status, r.Body)
		}
	}
	l := dashLink(t, c, "test", "ok", map[string]any{"product_id": open, "amount": "50.00"})
	id := l["id"].(string)
	exp := app.Clock.Now().Add(24 * time.Hour).Format(time.RFC3339)
	l = c.send("PATCH", "/v1/dashboard/links/"+id+"?mode=test", map[string]any{"amount": "60.00", "name": "Quote ACME",
		"expires_at": exp, "max_payments": 10, "success_url": "https://example.com/thanks", "metadata": map[string]string{"quote": "7"}})
	if l["amount"] != "60.00" || l["name"] != "Quote ACME" || l["expires_at"] != exp || l["max_payments"] != float64(10) ||
		l["success_url"] != "https://example.com/thanks" || l["metadata"].(map[string]any)["quote"] != "7" {
		t.Fatalf("patch: %v", l)
	}
	l = c.send("PATCH", "/v1/dashboard/links/"+id+"?mode=test", map[string]any{"expires_at": nil, "max_payments": nil, "success_url": nil,
		"name": nil, "amount": nil})
	if l["expires_at"] != nil || l["max_payments"] != nil || l["success_url"] != nil || l["name"] != nil || l["amount"] != nil {
		t.Fatalf("clear: %v", l)
	}
	// An empty label is no label.
	if l = c.send("PATCH", "/v1/dashboard/links/"+id+"?mode=test", map[string]any{"name": ""}); l["name"] != nil {
		t.Fatalf("empty label: %v", l)
	}
	if r := c.do("PATCH", "/v1/dashboard/links/"+id+"?mode=test", map[string]any{"product_id": fixed}); r.Status != http.StatusBadRequest {
		t.Fatalf("product change: %d %s", r.Status, r.Body)
	}
	if r := c.do("PATCH", "/v1/dashboard/links/plink_01JBNOPE?mode=test", map[string]any{"active": false}); r.Status != http.StatusNotFound ||
		r.ErrorCode(t) != "payment_link_not_found" {
		t.Fatalf("unknown link: %d %s", r.Status, r.Body)
	}
	// A range edit asks about the active links on the dashboard too.
	c.send("PATCH", "/v1/dashboard/links/"+id+"?mode=test", map[string]any{"amount": "80.00"})
	if r := c.do("PATCH", "/v1/dashboard/products/"+open.(string)+"?mode=test", map[string]any{"max_amount": "70.00"}); r.Status != http.StatusConflict ||
		r.ErrorCode(t) != "links_decision_required" {
		t.Fatalf("range edit: %d %s", r.Status, r.Body)
	}
}

func TestDashboardLinkListOrderAndFilters(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	fixed := dashProduct(t, c, "test", "fixed", map[string]any{"name": "Founder", "price_type": "fixed", "amount": "10.00"})["id"].(string)
	open := dashProduct(t, c, "test", "open", map[string]any{"name": "Donation", "price_type": "open", "min_amount": "1.00"})["id"].(string)
	mk := func(label, product string, extra map[string]any) string {
		// Ids order by creation time; a step keeps "newest" unambiguous.
		app.Clock.Advance(time.Second)
		body := map[string]any{"product_id": product, "name": label}
		for k, v := range extra {
			body[k] = v
		}
		return dashLink(t, c, "test", label, body)["id"].(string)
	}
	pay := func(id, tx string) {
		t.Helper()
		r := openLink(app, id, "p-"+tx, map[string]any{})
		if r.Status != http.StatusOK {
			r = openLink(app, id, "p-"+tx, map[string]any{"amount": "10.00"})
		}
		if r.Status != http.StatusOK {
			t.Fatalf("open %s: %d %s", id, r.Status, r.Body)
		}
		payFull(t, app, r.JSON(t), "0x"+tx)
	}
	never := mk("Never paid", fixed, nil)
	early := mk("Paid early", open, nil)
	late := mk("Paid late", fixed, nil)
	off := mk("Switched off", fixed, nil)
	full := mk("Sold out", fixed, map[string]any{"max_payments": 1})
	soon := mk("Ends soon", open, map[string]any{"expires_at": app.Clock.Now().Add(2 * time.Hour).Format(time.RFC3339)})
	pay(early, fmt.Sprintf("%064x", 1))
	app.Clock.Advance(time.Minute)
	pay(full, fmt.Sprintf("%064x", 2))
	app.Clock.Advance(time.Minute)
	pay(late, fmt.Sprintf("%064x", 3))
	c.send("PATCH", "/v1/dashboard/links/"+off+"?mode=test", map[string]any{"active": false})

	ids := func(path string) []string {
		var out []string
		for _, x := range c.get(path)["data"].([]any) {
			out = append(out, x.(map[string]any)["id"].(string))
		}
		return out
	}
	// Live first (by last payment, never-paid last, then newest), then the rest the same way.
	want := []string{late, early, soon, never, full, off}
	if got := ids("/v1/dashboard/links?mode=test"); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("order:\n got %v\nwant %v", got, want)
	}
	if got := ids("/v1/dashboard/links?mode=test&status=live"); strings.Join(got, ",") != strings.Join(want[:4], ",") {
		t.Fatalf("live: %v", got)
	}
	if got := ids("/v1/dashboard/links?mode=test&status=inactive"); strings.Join(got, ",") != full+","+off {
		t.Fatalf("inactive: %v", got)
	}
	if got := ids("/v1/dashboard/links?mode=test&product_id=" + open); strings.Join(got, ",") != early+","+soon {
		t.Fatalf("by product: %v", got)
	}
	if got := ids("/v1/dashboard/links?mode=test&q=PAID"); strings.Join(got, ",") != late+","+early+","+never {
		t.Fatalf("q by label: %v", got)
	}
	if got := ids("/v1/dashboard/links?mode=test&q=donat"); strings.Join(got, ",") != early+","+soon {
		t.Fatalf("q by product name: %v", got)
	}
	if got := ids("/v1/dashboard/links?mode=test&q=" + off); len(got) != 1 || got[0] != off {
		t.Fatalf("q by id: %v", got)
	}
	if got := ids("/v1/dashboard/links?mode=test&q=" + open); len(got) != 2 {
		t.Fatalf("q by product id: %v", got)
	}
	var walked []string
	cursor := ""
	for range 10 {
		page := c.get("/v1/dashboard/links?mode=test&limit=4&cursor=" + cursor)
		for _, x := range page["data"].([]any) {
			walked = append(walked, x.(map[string]any)["id"].(string))
		}
		next, _ := page["next_cursor"].(string)
		if next == "" {
			break
		}
		cursor = next
	}
	if strings.Join(walked, ",") != strings.Join(want, ",") {
		t.Fatalf("paginated walk: %v", walked)
	}
	for _, bad := range []string{"status=gone", "cursor=%21%21", "limit=0", "limit=101"} {
		if r := c.do("GET", "/v1/dashboard/links?mode=test&"+bad, nil); r.Status != http.StatusBadRequest {
			t.Errorf("%s: %d %s", bad, r.Status, r.Body)
		}
	}
	if l := c.get("/v1/dashboard/links/" + full + "?mode=test"); l["unavailable_reason"] != "exhausted" {
		t.Fatalf("sold out: %v", l)
	}

	attention := func() map[string]string {
		out := map[string]string{}
		for _, x := range c.get("/v1/dashboard/overview?mode=test")["links_attention"].([]any) {
			m := x.(map[string]any)
			out[m["id"].(string)] = m["reason"].(string)
		}
		return out
	}
	if a := attention(); a[full] != "exhausted" || a[soon] != "expiring" || len(a) != 2 {
		t.Fatalf("links needing attention: %v", a)
	}
	// Deactivating a product flags only its links with open sessions.
	c.send("PATCH", "/v1/dashboard/links/"+off+"?mode=test", map[string]any{"active": true})
	if r := openLink(app, off, "held", nil); r.Status != http.StatusOK {
		t.Fatalf("open: %d %s", r.Status, r.Body)
	}
	c.send("PATCH", "/v1/dashboard/products/"+fixed+"?mode=test", map[string]any{"active": false})
	if a := attention(); a[off] != "product_inactive" || a[soon] != "expiring" || len(a) != 2 {
		t.Fatalf("after deactivating a product: %v", a)
	}
	if l := c.get("/v1/dashboard/links/" + never + "?mode=test"); l["unavailable_reason"] != "product_inactive" || l["product"].(map[string]any)["active"] != false {
		t.Fatalf("a link of an inactive product: %v", l)
	}
}

func TestDashboardProductList(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	var made []string
	for i, name := range []string{"Alpha", "Beta", "Gamma", "Delta", "Epsilon"} {
		app.Clock.Advance(time.Second)
		made = append(made, dashProduct(t, c, "test", name, map[string]any{"name": name, "price_type": "fixed", "amount": fmt.Sprintf("%d.00", 10+i)})["id"].(string))
	}
	c.send("PATCH", "/v1/dashboard/products/"+made[3]+"?mode=test", map[string]any{"active": false})
	// A payment moves the oldest product up: last payment comes before newest.
	paidLink := dashLink(t, c, "test", "paid-alpha", map[string]any{"product_id": made[0]})["id"].(string)
	opened := openLink(app, paidLink, "alpha-1", nil)
	if opened.Status != http.StatusOK {
		t.Fatalf("open: %d %s", opened.Status, opened.Body)
	}
	payFull(t, app, opened.JSON(t), "0x"+fmt.Sprintf("%064x", 77))
	ids := func(path string) []string {
		var out []string
		for _, x := range c.get(path)["data"].([]any) {
			out = append(out, x.(map[string]any)["id"].(string))
		}
		return out
	}
	// Active first, then by last payment (never paid last), then newest.
	want := []string{made[0], made[4], made[2], made[1], made[3]}
	if got := ids("/v1/dashboard/products?mode=test"); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("order:\n got %v\nwant %v", got, want)
	}
	if got := ids("/v1/dashboard/products?mode=test&status=inactive"); len(got) != 1 || got[0] != made[3] {
		t.Fatalf("inactive: %v", got)
	}
	if got := ids("/v1/dashboard/products?mode=test&status=active"); len(got) != 4 {
		t.Fatalf("active: %v", got)
	}
	if got := ids("/v1/dashboard/products?mode=test&q=LTA"); strings.Join(got, ",") != made[3] {
		t.Fatalf("q: %v", got)
	}
	if got := ids("/v1/dashboard/products?mode=test&q=" + made[1]); strings.Join(got, ",") != made[1] {
		t.Fatalf("q by id: %v", got)
	}
	var walked []string
	cursor := ""
	for range 10 {
		page := c.get("/v1/dashboard/products?mode=test&limit=2&cursor=" + cursor)
		for _, x := range page["data"].([]any) {
			walked = append(walked, x.(map[string]any)["id"].(string))
		}
		next, _ := page["next_cursor"].(string)
		if next == "" {
			break
		}
		cursor = next
	}
	if strings.Join(walked, ",") != strings.Join(want, ",") {
		t.Fatalf("paginated walk: %v", walked)
	}
	for _, bad := range []string{"status=live", "cursor=%21%21", "limit=0"} {
		if r := c.do("GET", "/v1/dashboard/products?mode=test&"+bad, nil); r.Status != http.StatusBadRequest {
			t.Errorf("%s: %d %s", bad, r.Status, r.Body)
		}
	}
	if got := ids("/v1/dashboard/products?mode=live"); len(got) != 0 {
		t.Fatalf("live products: %v", got)
	}
}
