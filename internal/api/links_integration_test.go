//go:build integration

package api_test

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/service"
	"github.com/prismaport/prismaport/internal/store"
)

// payFull pays an opened public session in full on its Base Sepolia address, as the detector
// would after two providers agree, and confirms the payment. The payer generates the payment on
// Base Sepolia first when nobody did (D48).
func payFull(t *testing.T, app *apptest.App, ses map[string]any, tx string) {
	t.Helper()
	loaded, err := app.Store.LoadSession(t.Context(), ses["id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Revealed("base-sepolia") == nil {
		if loaded, err = app.Svc.RevealForPayer(t.Context(), loaded, "base-sepolia", store.RevealCheckout); err != nil {
			t.Fatal(err)
		}
	}
	addresses := map[string]string{}
	for _, p := range loaded.PayTo {
		addresses[p.Chain+"/"+p.Asset] = p.Address
	}
	for _, x := range ses["pay_to"].([]any) {
		p := x.(map[string]any)
		if p["chain"] != "base-sepolia" {
			continue
		}
		amount, err := money.ParseDecimal(p["amount"].(string), 6)
		if err != nil {
			t.Fatal(err)
		}
		id, _, err := app.Svc.RecordPayment(t.Context(), service.Observation{SessionID: ses["id"].(string), Chain: "base-sepolia",
			Asset: "USDC", Address: addresses["base-sepolia/USDC"], TxHash: tx, Amount: amount})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := app.Svc.ConfirmPayment(t.Context(), ses["id"].(string), id); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Fatalf("no base-sepolia pay_to: %v", ses)
}

func publicLink(t *testing.T, app *apptest.App, link string) map[string]any {
	t.Helper()
	r := app.Do("GET", "/v1/checkout/links/"+link, "", nil)
	if r.Status != http.StatusOK {
		t.Fatalf("public link: %d %s", r.Status, r.Body)
	}
	return r.JSON(t)
}

// The merchant API's products have no URL or availability any more: those are a link's.
func TestProductOnTheMerchantAPI(t *testing.T) {
	t.Parallel()
	app := linkApp(t, nil)
	r := app.Do("POST", "/v1/products", app.TestKey, map[string]any{"name": "Founder", "description": "Lifetime access.\nEvery feature.",
		"price_type": "fixed", "amount": "25.00"})
	if r.Status != http.StatusOK {
		t.Fatalf("create: %d %s", r.Status, r.Body)
	}
	m := r.JSON(t)
	for _, k := range []string{"url", "expires_at", "max_payments"} {
		if _, ok := m[k]; ok {
			t.Fatalf("a product renders %s: %s", k, r.Body)
		}
	}
	links := m["links"].(map[string]any)
	if m["description"] != "Lifetime access.\nEvery feature." || links["live"] != float64(0) || links["total"] != float64(0) {
		t.Fatalf("product: %s", r.Body)
	}
	id := m["id"].(string)
	for _, bad := range []map[string]any{{"expires_at": nil}, {"max_payments": 3}} {
		if r := app.Do("PATCH", "/v1/products/"+id, app.TestKey, bad); r.Status != http.StatusBadRequest || r.ErrorCode(t) != "invalid_request" {
			t.Errorf("%v on a product: %d %s", bad, r.Status, r.Body)
		}
	}
	for _, bad := range []any{strings.Repeat("d", 501), "bell\a", 5} {
		if r := app.Do("PATCH", "/v1/products/"+id, app.TestKey, map[string]any{"description": bad}); r.Status != http.StatusBadRequest || r.ErrorCode(t) != "invalid_request" {
			t.Errorf("description %q: %d %s", bad, r.Status, r.Body)
		}
	}
	if r := app.Do("POST", "/v1/products", app.TestKey, map[string]any{"name": "X", "price_type": "fixed", "amount": "5.00", "description": ""}); r.Status != http.StatusBadRequest {
		t.Errorf("an empty description on create: %d %s", r.Status, r.Body)
	}
	if r := app.Do("PATCH", "/v1/products/"+id, app.TestKey, map[string]any{"description": nil}); r.Status != http.StatusOK || r.JSON(t)["description"] != nil {
		t.Fatalf("clear description: %d %s", r.Status, r.Body)
	}
	apiLink(t, app, app.TestKey, id, nil)
	off := apiLink(t, app, app.TestKey, id, nil)
	app.Do("PATCH", "/v1/payment_links/"+off, app.TestKey, map[string]any{"active": false})
	got := app.Do("GET", "/v1/products/"+id, app.TestKey, nil).JSON(t)["links"].(map[string]any)
	if got["live"] != float64(1) || got["total"] != float64(2) {
		t.Fatalf("links: %v", got)
	}
	list := app.Do("GET", "/v1/products", app.TestKey, nil).JSON(t)["data"].([]any)
	if l := list[0].(map[string]any)["links"].(map[string]any); l["total"] != float64(2) {
		t.Fatalf("listed links: %v", l)
	}
}

func TestPaymentLinksAPI(t *testing.T) {
	t.Parallel()
	app := linkApp(t, nil)
	fixed := createProduct(t, app, app.TestKey, map[string]any{"name": "Founder", "price_type": "fixed", "amount": "25.00",
		"metadata": map[string]string{"plan": "founder"}})
	open := createProduct(t, app, app.TestKey, map[string]any{"name": "Consulting", "price_type": "open", "min_amount": "10.00", "max_amount": "500.00"})
	exp := app.Clock.Now().Add(48 * time.Hour).Format(time.RFC3339)
	body := map[string]any{"product_id": open, "name": "Invoice ACME #12", "amount": "120.00", "expires_at": exp, "max_payments": 1,
		"success_url": "https://acme.example/paid", "metadata": map[string]string{"invoice": "12"}}
	r := app.Do("POST", "/v1/payment_links", app.TestKey, body, idem("inv-12")...)
	if r.Status != http.StatusOK {
		t.Fatalf("create: %d %s", r.Status, r.Body)
	}
	l := r.JSON(t)
	id := l["id"].(string)
	stats := l["stats"].(map[string]any)
	if !strings.HasPrefix(id, "plink_") || l["livemode"] != false || l["product_id"] != open || l["name"] != "Invoice ACME #12" ||
		l["amount"] != "120.00" || l["active"] != true || l["expires_at"] != exp || l["max_payments"] != float64(1) ||
		l["success_url"] != "https://acme.example/paid" || l["metadata"].(map[string]any)["invoice"] != "12" ||
		l["url"] != apptest.CheckoutBase+"/l/"+id || l["live"] != true || l["unavailable_reason"] != nil ||
		stats["opened"] != float64(0) || stats["conversion"] != nil || l["created_at"] == nil || l["updated_at"] == nil {
		t.Fatalf("link: %s", r.Body)
	}
	if _, ok := l["payout"]; ok {
		t.Fatalf("a link has no payout: %s", r.Body)
	}
	if again := app.Do("POST", "/v1/payment_links", app.TestKey, body, idem("inv-12")...); again.JSON(t)["id"] != id || again.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay: %d %s", again.Status, again.Body)
	}
	if r := app.Do("POST", "/v1/payment_links", app.TestKey, map[string]any{"product_id": fixed}, idem("inv-12")...); r.Status != http.StatusConflict ||
		r.ErrorCode(t) != "idempotency_conflict" {
		t.Fatalf("same key, other body: %d %s", r.Status, r.Body)
	}
	if r := app.Do("POST", "/v1/payment_links", app.TestKey, map[string]any{"product_id": fixed}); r.Status != http.StatusBadRequest {
		t.Fatalf("create without a key: %d %s", r.Status, r.Body)
	}
	if r := app.Do("GET", "/v1/payment_links/"+id, app.TestKey, nil); r.Status != http.StatusOK || r.JSON(t)["id"] != id {
		t.Fatalf("get: %d %s", r.Status, r.Body)
	}

	// Create refusals, each naming the field.
	seq := 0
	create := func(b map[string]any) apptest.Response {
		seq++
		return app.Do("POST", "/v1/payment_links", app.TestKey, b, idem(fmt.Sprintf("bad-%d", seq))...)
	}
	for _, c := range []struct {
		body        map[string]any
		status      int
		code, param string
	}{
		{map[string]any{}, 400, "invalid_request", "product_id"},
		{map[string]any{"product_id": "prod_nope"}, 404, "product_not_found", "product_id"},
		{map[string]any{"product_id": createProduct(t, app, app.LiveKey, map[string]any{"name": "Live", "price_type": "fixed", "amount": "5.00"})}, 404, "product_not_found", "product_id"},
		{map[string]any{"product_id": fixed, "amount": "25.00"}, 400, "invalid_request", "amount"},
		{map[string]any{"product_id": open, "amount": "9.99"}, 400, "amount_below_minimum", "amount"},
		{map[string]any{"product_id": open, "amount": "500.01"}, 400, "amount_above_maximum", "amount"},
		{map[string]any{"product_id": open, "amount": "12.345"}, 400, "invalid_request", "amount"},
		{map[string]any{"product_id": fixed, "active": false}, 400, "invalid_request", "active"},
		{map[string]any{"product_id": fixed, "name": strings.Repeat("n", 201)}, 400, "invalid_request", "name"},
		{map[string]any{"product_id": fixed, "expires_at": app.Clock.Now().Format(time.RFC3339)}, 400, "invalid_request", "expires_at"},
		{map[string]any{"product_id": fixed, "expires_at": "tomorrow"}, 400, "invalid_request", "expires_at"},
		{map[string]any{"product_id": fixed, "expires_at": app.Clock.Now().Add(11 * 365 * 24 * time.Hour).Format(time.RFC3339)}, 400, "invalid_request", "expires_at"},
		{map[string]any{"product_id": fixed, "max_payments": 0}, 400, "invalid_request", "max_payments"},
		{map[string]any{"product_id": fixed, "max_payments": 1_000_001}, 400, "invalid_request", "max_payments"},
		{map[string]any{"product_id": fixed, "success_url": "javascript:alert(1)"}, 400, "invalid_request", "success_url"},
		{map[string]any{"product_id": fixed, "ammount": "1"}, 400, "invalid_request", ""},
	} {
		t.Run(c.code, func(t *testing.T) {
			r := create(c.body)
			if r.Status != c.status || r.ErrorCode(t) != c.code {
				t.Errorf("%v: %d %s", c.body, r.Status, r.Body)
				return
			}
			if p, _ := r.JSON(t)["error"].(map[string]any)["param"].(string); c.param != "" && p != c.param {
				t.Errorf("%v: param %q", c.body, p)
			}
		})
	}
	// The product's metadata and the link's, merged, must fit the limits.
	many := map[string]string{}
	for i := range 50 {
		many[fmt.Sprintf("k%d", i)] = "v"
	}
	if r := create(map[string]any{"product_id": fixed, "metadata": many}); r.Status != http.StatusBadRequest || r.ErrorCode(t) != "invalid_request" {
		t.Errorf("merged metadata over the limit: %d %s", r.Status, r.Body)
	}
	many["plan"] = "override"
	delete(many, "k0")
	if r := create(map[string]any{"product_id": fixed, "metadata": many}); r.Status != http.StatusOK {
		t.Errorf("a link key over a product key counts once: %d %s", r.Status, r.Body)
	}

	// Patch: every editable field, null clears, product_id is immutable.
	r = app.Do("PATCH", "/v1/payment_links/"+id, app.TestKey, map[string]any{"name": nil, "amount": nil, "expires_at": nil, "max_payments": nil,
		"success_url": nil, "metadata": nil, "active": false})
	l = r.JSON(t)
	if r.Status != http.StatusOK || l["name"] != nil || l["amount"] != nil || l["expires_at"] != nil || l["max_payments"] != nil || l["success_url"] != nil ||
		len(l["metadata"].(map[string]any)) != 0 || l["active"] != false || l["live"] != false || l["unavailable_reason"] != "inactive" {
		t.Fatalf("clear: %d %s", r.Status, r.Body)
	}
	r = app.Do("PATCH", "/v1/payment_links/"+id, app.TestKey, map[string]any{"amount": "200.00", "active": true, "name": "Invoice ACME #13"})
	if r.Status != http.StatusOK || r.JSON(t)["amount"] != "200.00" || r.JSON(t)["live"] != true || r.JSON(t)["name"] != "Invoice ACME #13" {
		t.Fatalf("set: %d %s", r.Status, r.Body)
	}
	if r := app.Do("PATCH", "/v1/payment_links/"+id, app.TestKey, map[string]any{"product_id": fixed}); r.Status != http.StatusBadRequest ||
		r.JSON(t)["error"].(map[string]any)["param"] != "product_id" {
		t.Fatalf("change product: %d %s", r.Status, r.Body)
	}
	if r := app.Do("PATCH", "/v1/payment_links/"+id, app.TestKey, map[string]any{"amount": "600.00"}); r.Status != http.StatusBadRequest ||
		r.ErrorCode(t) != "amount_above_maximum" {
		t.Fatalf("amount above the range: %d %s", r.Status, r.Body)
	}
	for _, missing := range []string{"plink_nope", "prod_nope"} {
		if r := app.Do("PATCH", "/v1/payment_links/"+missing, app.TestKey, map[string]any{"active": true}); r.Status != http.StatusNotFound ||
			r.ErrorCode(t) != "payment_link_not_found" {
			t.Errorf("patch %s: %d %s", missing, r.Status, r.Body)
		}
		if r := app.Do("GET", "/v1/payment_links/"+missing, app.TestKey, nil); r.Status != http.StatusNotFound || r.ErrorCode(t) != "payment_link_not_found" {
			t.Errorf("get %s: %d %s", missing, r.Status, r.Body)
		}
	}
	if r := app.Do("GET", "/v1/payment_links/"+id, app.LiveKey, nil); r.Status != http.StatusNotFound {
		t.Fatalf("a test link from a live key: %d %s", r.Status, r.Body)
	}

	// A range edit asks about the active links; a kept link keeps its amount outside the range.
	second := apiLink(t, app, app.TestKey, open, map[string]any{"amount": "300.00"})
	app.Do("PATCH", "/v1/payment_links/"+second, app.TestKey, map[string]any{"active": false})
	if r := app.Do("PATCH", "/v1/products/"+open, app.TestKey, map[string]any{"max_amount": "150.00"}); r.Status != http.StatusConflict ||
		r.ErrorCode(t) != "links_decision_required" {
		t.Fatalf("range edit: %d %s", r.Status, r.Body)
	}
	if r := app.Do("PATCH", "/v1/products/"+open, app.TestKey, map[string]any{"max_amount": "150.00", "links": "keep"}); r.Status != http.StatusOK {
		t.Fatalf("range edit, links kept: %d %s", r.Status, r.Body)
	}
	if l := app.Do("GET", "/v1/payment_links/"+id, app.TestKey, nil).JSON(t); l["amount"] != "200.00" || l["live"] != true {
		t.Fatalf("a kept link: %v", l)
	}

	// List: newest first, by product and status, paged by cursor.
	page := app.Do("GET", "/v1/payment_links?limit=2", app.TestKey, nil).JSON(t)
	data := page["data"].([]any)
	if len(data) != 2 || page["next_cursor"] == nil || data[0].(map[string]any)["id"] != second {
		t.Fatalf("first page: %v", page)
	}
	rest := app.Do("GET", "/v1/payment_links?limit=100&cursor="+page["next_cursor"].(string), app.TestKey, nil).JSON(t)
	all := app.Do("GET", "/v1/payment_links?status=all&limit=100", app.TestKey, nil).JSON(t)["data"].([]any)
	if len(rest["data"].([]any))+2 != len(all) || rest["next_cursor"] != nil {
		t.Fatalf("rest %v of %d", rest, len(all))
	}
	ofOpen := app.Do("GET", "/v1/payment_links?product_id="+open, app.TestKey, nil).JSON(t)["data"].([]any)
	inactive := app.Do("GET", "/v1/payment_links?product_id="+open+"&status=inactive", app.TestKey, nil).JSON(t)["data"].([]any)
	live := app.Do("GET", "/v1/payment_links?product_id="+open+"&status=live", app.TestKey, nil).JSON(t)["data"].([]any)
	if len(ofOpen) != 2 || len(inactive) != 1 || inactive[0].(map[string]any)["id"] != second || len(live) != 1 || live[0].(map[string]any)["id"] != id {
		t.Fatalf("filters: %d %v %v", len(ofOpen), inactive, live)
	}
	if r := app.Do("GET", "/v1/payment_links?status=paused", app.TestKey, nil); r.Status != http.StatusBadRequest {
		t.Fatalf("bad status: %d %s", r.Status, r.Body)
	}
	if got := app.Do("GET", "/v1/payment_links", app.LiveKey, nil).JSON(t)["data"].([]any); len(got) != 0 {
		t.Fatalf("live key lists test links: %v", got)
	}
}

func TestLinkExpiry(t *testing.T) {
	t.Parallel()
	app := linkApp(t, nil)
	exp := app.Clock.Now().Add(time.Hour)
	prod := createProduct(t, app, app.TestKey, map[string]any{"name": "Early bird", "price_type": "fixed", "amount": "25.00"})
	link := apiLink(t, app, app.TestKey, prod, map[string]any{"expires_at": exp.Format(time.RFC3339)})
	if m := publicLink(t, app, link); m["live"] != true || m["amount"] != "25.00" {
		t.Fatalf("before expiry: %v", m)
	}
	before := openLink(app, link, "before", nil)
	if before.Status != http.StatusOK {
		t.Fatalf("open before expiry: %d %s", before.Status, before.Body)
	}
	// The payer generates the payment before the link expires (D48).
	if r := app.CheckoutReveal(before.JSON(t)["id"].(string), "base-sepolia"); r.Status != http.StatusOK {
		t.Fatalf("reveal: %d %s", r.Status, r.Body)
	}
	app.Clock.Set(exp)
	m := publicLink(t, app, link)
	if m["live"] != false || m["active"] != false || m["unavailable_reason"] != "expired" || m["amount"] != nil {
		t.Fatalf("after expiry: %v", m)
	}
	if r := openLink(app, link, "after", nil); r.Status != http.StatusGone || r.ErrorCode(t) != "link_unavailable" {
		t.Fatalf("open after expiry: %d %s", r.Status, r.Body)
	}
	// The product has no expiry: the merchant API still opens sessions of it.
	if r := app.Session(app.TestKey, map[string]any{"product_id": prod}, idem("api-after")...); r.Status != http.StatusOK {
		t.Fatalf("merchant API session after the link expired: %d %s", r.Status, r.Body)
	}
	if r := openLink(app, link, "before", nil); r.Status != http.StatusOK || r.JSON(t)["id"] != before.JSON(t)["id"] {
		t.Fatalf("replay of a session opened before expiry: %d %s", r.Status, r.Body)
	}
	// The session opened before expiry still completes.
	payFull(t, app, before.JSON(t), "0x"+fmt.Sprintf("%064x", 1))
	if r := app.Do("GET", "/v1/sessions/"+before.JSON(t)["id"].(string), app.TestKey, nil); r.JSON(t)["status"] != "paid" {
		t.Fatalf("session opened before expiry: %s", r.Body)
	}
}

func TestLinkInactiveOnTheMerchantAPIKeepsItsCode(t *testing.T) {
	t.Parallel()
	app := linkApp(t, nil)
	prod := createProduct(t, app, app.TestKey, map[string]any{"name": "Old", "price_type": "fixed", "amount": "9.00", "active": false})
	if r := app.Session(app.TestKey, map[string]any{"product_id": prod}, idem("k")...); r.Status != http.StatusBadRequest || r.ErrorCode(t) != "product_inactive" {
		t.Fatalf("inactive product on the merchant API: %d %s", r.Status, r.Body)
	}
	if r := app.Session(app.LiveKey, map[string]any{"product_id": prod}, idem("k")...); r.Status != http.StatusNotFound || r.ErrorCode(t) != "product_not_found" {
		t.Fatalf("test product from a live key: %d %s", r.Status, r.Body)
	}
	// A link can be made for an inactive product; it stays unavailable until the product is on.
	link := apiLink(t, app, app.TestKey, prod, nil)
	if m := publicLink(t, app, link); m["unavailable_reason"] != "product_inactive" {
		t.Fatalf("link of an inactive product: %v", m)
	}
}

// Payers racing for the last slots of one link: exactly max_payments sessions open from it, the
// rest are refused, nothing can push the paid count over the limit, and neither API sessions nor
// another link of the product take its slots.
func TestLinkMaxPaymentsUnderConcurrency(t *testing.T) {
	t.Parallel()
	app := linkApp(t, nil)
	const limit, racers = 3, 24
	prod := createProduct(t, app, app.TestKey, map[string]any{"name": "Founder", "price_type": "fixed", "amount": "25.00"})
	link := apiLink(t, app, app.TestKey, prod, map[string]any{"max_payments": limit})
	sibling := apiLink(t, app, app.TestKey, prod, map[string]any{"max_payments": limit})
	var wg sync.WaitGroup
	statuses := make([]int, racers)
	codes := make([]string, racers)
	bodies := make([]map[string]any, racers)
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var r apptest.Response
			switch i % 4 {
			case 0:
				r = app.Session(app.TestKey, map[string]any{"product_id": prod}, idem(fmt.Sprintf("api-%d", i))...)
			case 1:
				r = openLink(app, sibling, fmt.Sprintf("sibling-%d", i), nil)
			default:
				r = openLink(app, link, fmt.Sprintf("payer-%d", i), nil)
			}
			statuses[i] = r.Status
			if r.Status == http.StatusOK {
				bodies[i] = r.JSON(t)
			} else {
				codes[i] = r.ErrorCode(t)
			}
		}()
	}
	wg.Wait()
	opened := map[int]int{}
	for i, st := range statuses {
		switch {
		case st == http.StatusOK:
			opened[i%4]++
		case st == http.StatusGone && codes[i] == "link_unavailable" && i%4 >= 1:
		default:
			t.Fatalf("racer %d: %d %s", i, st, codes[i])
		}
	}
	if opened[0] != racers/4 || opened[1] != limit || opened[2]+opened[3] != limit {
		t.Fatalf("opened per kind %v for max_payments %d", opened, limit)
	}
	var n int
	if err := app.Store.Pool().QueryRow(t.Context(), `SELECT count(*) FROM sessions WHERE link_id = $1`, link).Scan(&n); err != nil || n != limit {
		t.Fatalf("%d sessions of the link in the database (%v)", n, err)
	}
	if m := publicLink(t, app, link); m["live"] != false || m["unavailable_reason"] != "reserved" {
		t.Fatalf("all slots held by open sessions: %v", m)
	}
	paid := 0
	for i, b := range bodies {
		if b != nil && i%4 >= 2 {
			paid++
			payFull(t, app, b, "0x"+fmt.Sprintf("%064x", paid))
		}
	}
	if m := publicLink(t, app, link); m["live"] != false || m["unavailable_reason"] != "exhausted" {
		t.Fatalf("all slots paid: %v", m)
	}
	app.Clock.Advance(48 * time.Hour)
	if _, err := app.Svc.ExpireDue(t.Context(), 100); err != nil {
		t.Fatal(err)
	}
	if r := openLink(app, link, "late", nil); r.Status != http.StatusGone {
		t.Fatalf("paid slots are never released: %d %s", r.Status, r.Body)
	}
	if r := openLink(app, sibling, "late", nil); r.Status != http.StatusOK {
		t.Fatalf("the sibling's unpaid slots were released: %d %s", r.Status, r.Body)
	}
}

// An opened but unpaid session holds its slot until it expires, then releases it.
func TestLinkSlotReleasedOnExpiry(t *testing.T) {
	t.Parallel()
	app := linkApp(t, nil)
	prod := createProduct(t, app, app.TestKey, map[string]any{"name": "One seat", "price_type": "fixed", "amount": "25.00"})
	link := apiLink(t, app, app.TestKey, prod, map[string]any{"max_payments": 1})
	first := openLink(app, link, "first", nil)
	if first.Status != http.StatusOK {
		t.Fatalf("open: %d %s", first.Status, first.Body)
	}
	if r := openLink(app, link, "second", nil); r.Status != http.StatusGone || r.ErrorCode(t) != "link_unavailable" {
		t.Fatalf("second open while the first is unpaid: %d %s", r.Status, r.Body)
	}
	// Not released before the sweeper has expired it, even past expires_at.
	app.Clock.Advance(2 * time.Hour)
	if r := openLink(app, link, "second", nil); r.Status != http.StatusGone {
		t.Fatalf("slot released before the session expired: %d %s", r.Status, r.Body)
	}
	if n, err := app.Svc.ExpireDue(t.Context(), 100); err != nil || n != 1 {
		t.Fatalf("expire: %d %v", n, err)
	}
	if m := publicLink(t, app, link); m["live"] != true {
		t.Fatalf("after expiry the slot is free: %v", m)
	}
	second := openLink(app, link, "second", nil)
	if second.Status != http.StatusOK {
		t.Fatalf("open after release: %d %s", second.Status, second.Body)
	}
	payFull(t, app, second.JSON(t), "0x"+fmt.Sprintf("%064x", 7))
	if m := publicLink(t, app, link); m["unavailable_reason"] != "exhausted" {
		t.Fatalf("after the paid slot: %v", m)
	}
	// Raising the limit reopens the link.
	if r := app.Do("PATCH", "/v1/payment_links/"+link, app.TestKey, map[string]any{"max_payments": 2}); r.Status != http.StatusOK {
		t.Fatalf("raise: %d %s", r.Status, r.Body)
	}
	if m := publicLink(t, app, link); m["live"] != true {
		t.Fatalf("after raising the limit: %v", m)
	}
}

// Stats are computed from sessions and payments on every read; under concurrent opens and
// payments they equal a direct count of the source of truth.
func TestLinkCountsMatchTheSourceUnderConcurrency(t *testing.T) {
	t.Parallel()
	app := linkApp(t, nil)
	_, link := productLink(t, app, app.TestKey, map[string]any{"name": "Tips", "price_type": "open", "min_amount": "1.00"})
	const n = 16
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := openLink(app, link, fmt.Sprintf("tip-%d", i), map[string]any{"amount": fmt.Sprintf("%d.%02d", 1+i, i)})
			if r.Status != http.StatusOK {
				t.Errorf("open %d: %d %s", i, r.Status, r.Body)
				return
			}
			if i%2 == 0 {
				payFull(t, app, r.JSON(t), "0x"+fmt.Sprintf("%064x", 100+i))
			}
		}()
	}
	wg.Wait()
	counts, err := app.Store.LinkCounts(t.Context(), []string{link})
	if err != nil {
		t.Fatal(err)
	}
	c := counts[link]
	var opened, paid int64
	var received string
	err = app.Store.Pool().QueryRow(t.Context(), `SELECT count(*), count(*) FILTER (WHERE status = 'paid'),
		(SELECT coalesce(sum(p.amount), 0)::text FROM payments p JOIN sessions s ON s.id = p.session_id
			WHERE s.link_id = $1 AND p.status IN ('confirmed', 'finalized'))
		FROM sessions WHERE link_id = $1`, link).Scan(&opened, &paid, &received)
	if err != nil {
		t.Fatal(err)
	}
	if c.Opened != opened || c.Paid != paid || opened != n || paid != n/2 {
		t.Fatalf("counts %+v, source opened=%d paid=%d", c, opened, paid)
	}
	// Even tips: amount (1+i).(i) for i = 0, 2, ..., 14.
	want := money.Zero
	for i := 0; i < n; i += 2 {
		u, _ := money.ParseDecimal(fmt.Sprintf("%d.%02d", 1+i, i), 6)
		want, _ = want.Add(u)
	}
	if len(c.Received) != 1 || !c.Received[0].Amount.Equal(want) || c.Received[0].Amount.String() != received {
		t.Fatalf("received %+v, source %s, want %s", c.Received, received, want)
	}
	if got := app.Do("GET", "/v1/payment_links/"+link, app.TestKey, nil).JSON(t)["stats"].(map[string]any); got["opened"] != float64(n) ||
		got["paid"] != float64(n/2) || got["conversion"] != "0.5000" {
		t.Fatalf("API stats %v", got)
	}
}
