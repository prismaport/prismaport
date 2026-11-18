//go:build integration

package api_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/api"
	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/checkout"
)

// linkApp is an app whose server also mounts the payment-link endpoints. nil options keep the
// limits out of the way of tests about something else.
func linkApp(t *testing.T, opt *api.PublicOptions) *apptest.App {
	t.Helper()
	app := apptest.New(t, apptest.Options{})
	h := api.New(app.Svc, app.Store, nil)
	o := api.PublicOptions{Limits: map[string]api.Limit{api.RouteLinkOpen: {PerMinute: 1000, Burst: 1000}}}
	if opt != nil {
		o = *opt
	}
	if err := h.ConfigurePublic(o); err != nil {
		t.Fatal(err)
	}
	co := checkout.New(app.Svc, app.Store, nil)
	h.MountCheckout(co)
	h.MountCheckoutLinks(co)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	app.Server = srv
	return app
}

func createProduct(t *testing.T, app *apptest.App, key string, body map[string]any) string {
	t.Helper()
	r := app.Do("POST", "/v1/products", key, body)
	if r.Status != http.StatusOK {
		t.Fatalf("create product: %d %s", r.Status, r.Body)
	}
	return r.JSON(t)["id"].(string)
}

var linkKeys atomic.Int64

// apiLink creates a payment link of product with the merchant API; body adds link fields.
func apiLink(t *testing.T, app *apptest.App, key, product string, body map[string]any) string {
	t.Helper()
	m := map[string]any{"product_id": product}
	for k, v := range body {
		m[k] = v
	}
	r := app.Do("POST", "/v1/payment_links", key, m, idem(fmt.Sprintf("create-link-%d", linkKeys.Add(1)))...)
	if r.Status != http.StatusOK {
		t.Fatalf("create link: %d %s", r.Status, r.Body)
	}
	return r.JSON(t)["id"].(string)
}

// productLink creates a product and a plain link of it.
func productLink(t *testing.T, app *apptest.App, key string, product map[string]any) (string, string) {
	t.Helper()
	prod := createProduct(t, app, key, product)
	return prod, apiLink(t, app, key, prod, nil)
}

func openLink(app *apptest.App, link, key string, body any) apptest.Response {
	var hdr []string
	if key != "" {
		hdr = idem(key)
	}
	return app.Do("POST", "/v1/checkout/links/"+link+"/sessions", "", body, hdr...)
}

func sessionSuccessURL(t *testing.T, app *apptest.App, id string) string {
	t.Helper()
	var u *string
	if err := app.Store.Pool().QueryRow(t.Context(), `SELECT success_url FROM sessions WHERE id = $1`, id).Scan(&u); err != nil {
		t.Fatal(err)
	}
	if u == nil {
		return ""
	}
	return *u
}

func TestLinkFixedProduct(t *testing.T) {
	t.Parallel()
	app := linkApp(t, nil)
	prod := createProduct(t, app, app.TestKey, map[string]any{"name": "Founder", "description": "Lifetime access to every feature.",
		"price_type": "fixed", "amount": "25.00", "success_url": "https://example.com/thanks",
		"metadata": map[string]string{"plan": "secret-plan", "tier": "product"}})
	link := apiLink(t, app, app.TestKey, prod, map[string]any{"name": "Instagram bio", "metadata": map[string]string{"tier": "link", "src": "ig"}})

	r := app.Do("GET", "/v1/checkout/links/"+link, "", nil)
	if r.Status != http.StatusOK || r.Header.Get("Cache-Control") != "no-store" || r.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("link view: %d %v %s", r.Status, r.Header, r.Body)
	}
	for _, leak := range []string{"secret-plan", "metadata", app.MerchantID, "thanks", "success_url", "Instagram", "product_id", prod} {
		if strings.Contains(string(r.Body), leak) {
			t.Fatalf("link view leaks %q: %s", leak, r.Body)
		}
	}
	m := r.JSON(t)
	if m["id"] != link || m["name"] != "Founder" || m["description"] != "Lifetime access to every feature." || m["price_type"] != "fixed" ||
		m["amount"] != "25.00" || m["currency"] != "USD" || m["active"] != true || m["live"] != true || m["unavailable_reason"] != nil ||
		m["livemode"] != false || m["min_amount"] != nil {
		t.Fatalf("link view: %s", r.Body)
	}

	first := openLink(app, link, "req-1", map[string]any{})
	if first.Status != http.StatusOK || first.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("open: %d %s", first.Status, first.Body)
	}
	for _, leak := range []string{"secret-plan", "metadata", app.MerchantID, "thanks", "Instagram"} {
		if strings.Contains(string(first.Body), leak) {
			t.Fatalf("opened session leaks %q: %s", leak, first.Body)
		}
	}
	ses := first.JSON(t)
	id := ses["id"].(string)
	product := ses["product"].(map[string]any)
	if !strings.HasPrefix(id, "ses_") || ses["amount"] != "25.00" || ses["status"] != "open" || product["name"] != "Founder" ||
		product["description"] != "Lifetime access to every feature." || len(ses["pay_to"].([]any)) == 0 {
		t.Fatalf("opened session: %s", first.Body)
	}

	again := openLink(app, link, "req-1", map[string]any{})
	if again.Status != http.StatusOK || again.JSON(t)["id"] != id || again.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay: %d %v %s", again.Status, again.Header, again.Body)
	}
	other := openLink(app, link, "req-2", nil)
	if other.Status != http.StatusOK || other.JSON(t)["id"] == id {
		t.Fatalf("second request: %d %s", other.Status, other.Body)
	}

	// The session snapshots the product's metadata with the link's keys over it and the
	// product's return URL (the link sets none), and names the link.
	merchant := app.Do("GET", "/v1/sessions/"+id, app.TestKey, nil).JSON(t)
	md := merchant["metadata"].(map[string]any)
	if merchant["product_id"] != prod || merchant["link_id"] != link || md["plan"] != "secret-plan" || md["tier"] != "link" || md["src"] != "ig" ||
		len(md) != 3 {
		t.Fatalf("merchant view of a link session: %v", merchant)
	}
	if u := sessionSuccessURL(t, app, id); u != "https://example.com/thanks" {
		t.Fatalf("success_url %q", u)
	}

	if r := openLink(app, link, "req-3", map[string]any{"amount": "30.00"}); r.Status != http.StatusBadRequest || r.ErrorCode(t) != "invalid_request" {
		t.Fatalf("amount on a fixed product: %d %s", r.Status, r.Body)
	}
	if r := openLink(app, link, "", nil); r.Status != http.StatusBadRequest || r.ErrorCode(t) != "invalid_request" {
		t.Fatalf("missing key: %d %s", r.Status, r.Body)
	}
	if r := openLink(app, link, strings.Repeat("k", 256), nil); r.Status != http.StatusBadRequest {
		t.Fatalf("long key: %d %s", r.Status, r.Body)
	}
	if r := openLink(app, link, "req-4", map[string]any{"product_id": prod}); r.Status != http.StatusBadRequest || r.ErrorCode(t) != "invalid_request" {
		t.Fatalf("unknown field: %d %s", r.Status, r.Body)
	}
	// A merchant using the same key for its own API call gets its own session, which names no
	// link.
	own := app.Session(app.TestKey, map[string]any{"amount": "7.00"}, idem("req-1")...)
	if own.Status != http.StatusOK || own.JSON(t)["id"] == id || own.JSON(t)["link_id"] != nil {
		t.Fatalf("merchant key namespace: %d %s", own.Status, own.Body)
	}
	// Idempotency keys are per link: the same key on another link of the product opens its own
	// session, with that link's return URL over the product's.
	second := apiLink(t, app, app.TestKey, prod, map[string]any{"success_url": "https://example.com/from-link"})
	elsewhere := openLink(app, second, "req-1", nil)
	if elsewhere.Status != http.StatusOK || elsewhere.JSON(t)["id"] == id {
		t.Fatalf("same key on another link: %d %s", elsewhere.Status, elsewhere.Body)
	}
	if u := sessionSuccessURL(t, app, elsewhere.JSON(t)["id"].(string)); u != "https://example.com/from-link" {
		t.Fatalf("a link's success_url overrides the product's: %q", u)
	}
}

func TestLinkOpenProduct(t *testing.T) {
	t.Parallel()
	app := linkApp(t, nil)
	prod, link := productLink(t, app, app.TestKey, map[string]any{"name": "Coffee", "price_type": "open", "min_amount": "5.00",
		"max_amount": "100.00", "suggested_amounts": []string{"5.00", "10.00", "25.00"}})
	m := app.Do("GET", "/v1/checkout/links/"+link, "", nil).JSON(t)
	if m["price_type"] != "open" || m["amount"] != nil || m["min_amount"] != "5.00" || m["max_amount"] != "100.00" || m["description"] != nil ||
		len(m["suggested_amounts"].([]any)) != 3 || m["suggested_amounts"].([]any)[1] != "10.00" {
		t.Fatalf("open link view: %v", m)
	}
	cases := []struct {
		body any
		code string
	}{
		{map[string]any{}, "invalid_request"},
		{map[string]any{"amount": "4.99"}, "amount_below_minimum"},
		{map[string]any{"amount": "100.01"}, "amount_above_maximum"},
		{map[string]any{"amount": "5.001"}, "invalid_request"},
		{map[string]any{"amount": "-5"}, "invalid_request"},
		{map[string]any{"amount": 12.5}, "invalid_request"},
		{"not json", "invalid_request"},
	}
	for i, c := range cases {
		t.Run(c.code, func(t *testing.T) {
			r := openLink(app, link, fmt.Sprintf("bad-%d", i), c.body)
			if r.Status != http.StatusBadRequest || r.ErrorCode(t) != c.code {
				t.Errorf("%v: %d %s", c.body, r.Status, r.Body)
			}
		})
	}
	r := openLink(app, link, "donor-1", map[string]any{"amount": "12.50"})
	if r.Status != http.StatusOK || r.JSON(t)["amount"] != "12.50" || r.JSON(t)["amount_remaining"] != "12.50" {
		t.Fatalf("open amount: %d %s", r.Status, r.Body)
	}
	payTo := r.JSON(t)["pay_to"].([]any)[0].(map[string]any)
	if payTo["amount"] != "12.500000" {
		t.Fatalf("pay_to amount: %v", payTo)
	}
	if r := openLink(app, link, "donor-1", map[string]any{"amount": "13.00"}); r.Status != http.StatusConflict || r.ErrorCode(t) != "idempotency_conflict" {
		t.Fatalf("same key, other amount: %d %s", r.Status, r.Body)
	}
	if r := openLink(app, link, "donor-2", map[string]any{"amount": "5.00"}); r.Status != http.StatusOK {
		t.Fatalf("minimum is inclusive: %d %s", r.Status, r.Body)
	}
	if r := openLink(app, link, "donor-3", map[string]any{"amount": "100.00"}); r.Status != http.StatusOK {
		t.Fatalf("maximum is inclusive: %d %s", r.Status, r.Body)
	}

	// A link that sets the amount answers a fixed price of that amount; the payer cannot
	// change it.
	quote := apiLink(t, app, app.TestKey, prod, map[string]any{"amount": "42.10", "name": "Quote ACME"})
	m = app.Do("GET", "/v1/checkout/links/"+quote, "", nil).JSON(t)
	if m["price_type"] != "fixed" || m["amount"] != "42.10" || m["min_amount"] != nil || m["max_amount"] != nil || len(m["suggested_amounts"].([]any)) != 0 ||
		m["name"] != "Coffee" {
		t.Fatalf("a link with an amount: %v", m)
	}
	if r := openLink(app, quote, "q-1", map[string]any{"amount": "42.10"}); r.Status != http.StatusBadRequest || r.ErrorCode(t) != "invalid_request" {
		t.Fatalf("payer amount on a link that sets one: %d %s", r.Status, r.Body)
	}
	r = openLink(app, quote, "q-2", nil)
	if r.Status != http.StatusOK || r.JSON(t)["amount"] != "42.10" || r.JSON(t)["pay_to"].([]any)[0].(map[string]any)["amount"] != "42.100000" {
		t.Fatalf("session of a link with an amount: %d %s", r.Status, r.Body)
	}
	// The merchant API keeps its own amount on the same product.
	if r := app.Session(app.TestKey, map[string]any{"product_id": prod, "amount": "99.00"}, idem("cart")...); r.Status != http.StatusOK ||
		r.JSON(t)["amount"] != "99.00" || r.JSON(t)["link_id"] != nil {
		t.Fatalf("API session: %d %s", r.Status, r.Body)
	}
}

func TestLinkInactiveAndUnknown(t *testing.T) {
	t.Parallel()
	app := linkApp(t, nil)
	prod, link := productLink(t, app, app.TestKey, map[string]any{"name": "Old plan", "price_type": "fixed", "amount": "9.00"})
	open := openLink(app, link, "before", nil)
	if open.Status != http.StatusOK {
		t.Fatalf("open: %d %s", open.Status, open.Body)
	}
	view := func() map[string]any { return app.Do("GET", "/v1/checkout/links/"+link, "", nil).JSON(t) }

	// Deactivating the product stops the link, whatever the link says; reactivating it brings it
	// back.
	if r := app.Do("PATCH", "/v1/products/"+prod, app.TestKey, map[string]any{"active": false}); r.Status != http.StatusOK {
		t.Fatalf("deactivate product: %d %s", r.Status, r.Body)
	}
	if m := view(); m["active"] != false || m["live"] != false || m["unavailable_reason"] != "product_inactive" || m["amount"] != nil || m["name"] != "Old plan" {
		t.Fatalf("inactive product view: %v", m)
	}
	r := openLink(app, link, "after", nil)
	if r.Status != http.StatusGone || r.ErrorCode(t) != "link_unavailable" || r.JSON(t)["error"].(map[string]any)["message"] != "This link is no longer available." {
		t.Fatalf("open with an inactive product: %d %s", r.Status, r.Body)
	}
	if r := app.Do("PATCH", "/v1/payment_links/"+link, app.TestKey, map[string]any{"active": false}); r.Status != http.StatusOK {
		t.Fatalf("deactivate link: %d %s", r.Status, r.Body)
	}
	if m := view(); m["unavailable_reason"] != "product_inactive" {
		t.Fatalf("an inactive product comes first: %v", m)
	}
	if r := app.Do("PATCH", "/v1/products/"+prod, app.TestKey, map[string]any{"active": true}); r.Status != http.StatusOK {
		t.Fatalf("reactivate product: %d %s", r.Status, r.Body)
	}
	if m := view(); m["unavailable_reason"] != "inactive" {
		t.Fatalf("inactive link view: %v", m)
	}
	if r := openLink(app, link, "after", nil); r.Status != http.StatusGone || r.ErrorCode(t) != "link_unavailable" {
		t.Fatalf("open inactive link: %d %s", r.Status, r.Body)
	}
	if r := app.Do("PATCH", "/v1/payment_links/"+link, app.TestKey, map[string]any{"active": true}); r.Status != http.StatusOK {
		t.Fatalf("reactivate link: %d %s", r.Status, r.Body)
	}
	if m := view(); m["live"] != true {
		t.Fatalf("live again: %v", m)
	}
	// A replay of a session opened while live still answers with that session.
	if r := openLink(app, link, "before", nil); r.Status != http.StatusOK || r.JSON(t)["id"] != open.JSON(t)["id"] {
		t.Fatalf("replay: %d %s", r.Status, r.Body)
	}
	// Product ids, the links of the old model, are not links.
	for _, bad := range []string{prod, "plink_01JBNOPE", "ses_" + link[6:], strings.Repeat("p", 80), "plink_" + strings.Repeat("A", 70)} {
		if r := app.Do("GET", "/v1/checkout/links/"+bad, "", nil); r.Status != http.StatusNotFound || r.ErrorCode(t) != "payment_link_not_found" {
			t.Errorf("get %s: %d %s", bad, r.Status, r.Body)
		}
		if r := openLink(app, bad, "k", nil); r.Status != http.StatusNotFound || r.ErrorCode(t) != "payment_link_not_found" {
			t.Errorf("open %s: %d %s", bad, r.Status, r.Body)
		}
	}
	if r := app.Do("GET", "/v1/checkout/products/"+prod, "", nil); r.Status != http.StatusNotFound {
		t.Errorf("the products endpoint is still served: %d", r.Status)
	}
	if r := app.Do("POST", "/v1/checkout/products/"+prod+"/sessions", "", nil, idem("x")...); r.Status != http.StatusNotFound {
		t.Errorf("the products sessions endpoint is still served: %d", r.Status)
	}
}

func TestLinkLiveProduct(t *testing.T) {
	t.Parallel()
	app := linkApp(t, nil)
	_, link := productLink(t, app, app.LiveKey, map[string]any{"name": "Founder", "price_type": "fixed", "amount": "25.00"})
	r := openLink(app, link, "live-1", nil)
	if r.Status != http.StatusOK || r.JSON(t)["livemode"] != true {
		t.Fatalf("live open: %d %s", r.Status, r.Body)
	}
	chains := map[string]bool{}
	for _, p := range r.JSON(t)["pay_to"].([]any) {
		chains[p.(map[string]any)["chain"].(string)] = true
	}
	if !chains["base"] || !chains["solana"] || chains["base-sepolia"] {
		t.Fatalf("live pay_to chains: %v", chains)
	}
}

func TestLinkConcurrentOpens(t *testing.T) {
	t.Parallel()
	app := linkApp(t, nil)
	_, link := productLink(t, app, app.TestKey, map[string]any{"name": "Founder", "price_type": "fixed", "amount": "25.00"})
	var wg sync.WaitGroup
	ids := make([]string, 8)
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := openLink(app, link, "same", nil)
			if r.Status == http.StatusOK {
				ids[i] = r.JSON(t)["id"].(string)
			}
		}()
	}
	wg.Wait()
	for _, id := range ids {
		if id == "" || id != ids[0] {
			t.Fatalf("concurrent opens with one key: %v", ids)
		}
	}
	var n int
	if err := app.Store.Pool().QueryRow(t.Context(), `SELECT count(*) FROM sessions WHERE link_id = $1`, link).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("%d sessions for one key", n)
	}
}

func TestLinkRateLimit(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	h := api.New(app.Svc, app.Store, nil)
	if err := h.ConfigurePublic(api.PublicOptions{Limits: map[string]api.Limit{api.RouteLinkOpen: {PerMinute: 2, Burst: 2}},
		Now: app.Clock.Now, ClientIPHeader: "X-Forwarded-For"}); err != nil {
		t.Fatal(err)
	}
	co := checkout.New(app.Svc, app.Store, nil)
	h.MountCheckout(co)
	h.MountCheckoutLinks(co)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	app.Server = srv
	_, link := productLink(t, app, app.TestKey, map[string]any{"name": "Founder", "price_type": "fixed", "amount": "25.00"})
	from := func(ip, key string) apptest.Response {
		return app.Do("POST", "/v1/checkout/links/"+link+"/sessions", "", nil, "Idempotency-Key", key, "X-Forwarded-For", ip+", 10.0.0.1")
	}
	for i, key := range []string{"a", "b"} {
		if r := from("203.0.113.7", key); r.Status != http.StatusOK {
			t.Fatalf("request %d: %d %s", i, r.Status, r.Body)
		}
	}
	r := from("203.0.113.7", "c")
	if r.Status != http.StatusTooManyRequests || r.ErrorCode(t) != "rate_limited" || r.Header.Get("Retry-After") != "30" {
		t.Fatalf("over the limit: %d %v %s", r.Status, r.Header, r.Body)
	}
	if r := from("198.51.100.4", "d"); r.Status != http.StatusOK {
		t.Fatalf("another client: %d %s", r.Status, r.Body)
	}
	app.Clock.Advance(30 * time.Second)
	if r := from("203.0.113.7", "c"); r.Status != http.StatusOK {
		t.Fatalf("after refill: %d %s", r.Status, r.Body)
	}
	if r := from("203.0.113.7", "e"); r.Status != http.StatusTooManyRequests {
		t.Fatalf("refill is one token: %d %s", r.Status, r.Body)
	}
}

func TestCheckoutNetworks(t *testing.T) {
	t.Parallel()
	app := linkApp(t, nil)
	r := app.Do("GET", "/v1/checkout/networks", "", nil)
	if r.Status != http.StatusOK || r.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("networks: %d %s", r.Status, r.Body)
	}
	byID := map[string]map[string]any{}
	for _, n := range r.JSON(t)["data"].([]any) {
		byID[n.(map[string]any)["id"].(string)] = n.(map[string]any)
	}
	base := byID["base"]
	if base == nil || base["evm_chain_id"] != float64(8453) || base["fixed_fee"] != "0.10" || base["fee_bps"] != float64(50) || base["family"] != "evm" {
		t.Fatalf("base: %v", base)
	}
	usdc := base["assets"].([]any)[0].(map[string]any)
	if usdc["symbol"] != "USDC" || usdc["token"] != "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913" || usdc["decimals"] != float64(6) || usdc["eip3009"] != true {
		t.Fatalf("base USDC: %v", usdc)
	}
	if sol := byID["solana"]; sol == nil || sol["evm_chain_id"] != nil || len(sol["assets"].([]any)) != 2 {
		t.Fatalf("solana: %v", sol)
	}
	if byID["base-sepolia"]["livemode"] != false {
		t.Fatalf("test chains: %v", byID["base-sepolia"])
	}
}
