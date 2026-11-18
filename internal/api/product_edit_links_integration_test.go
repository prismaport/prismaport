//go:build integration

package api_test

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/prismaport/prismaport/internal/apptest"
)

func errParam(t *testing.T, r apptest.Response) string {
	t.Helper()
	p, _ := r.JSON(t)["error"].(map[string]any)["param"].(string)
	return p
}

func errMessage(t *testing.T, r apptest.Response) string {
	t.Helper()
	m, _ := r.JSON(t)["error"].(map[string]any)["message"].(string)
	return m
}

// productEditLinks lists the ids of the links of product a product edit deactivated, from the
// audit log.
func productEditLinks(t *testing.T, app *apptest.App, product string) []string {
	t.Helper()
	rows, err := app.Store.Pool().Query(t.Context(), `SELECT details->>'link_id' FROM audit_log WHERE merchant_id = $1
		AND action = 'payment_link.updated' AND details->>'reason' = 'product_edit' AND details->>'product_id' = $2
		AND (details->>'active')::boolean = false ORDER BY id`, app.MerchantID, product)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func sessionAmount(t *testing.T, app *apptest.App, id string) string {
	t.Helper()
	var amount string
	if err := app.Store.Pool().QueryRow(t.Context(), `SELECT amount_cents::text FROM sessions WHERE id = $1`, id).Scan(&amount); err != nil {
		t.Fatal(err)
	}
	return amount
}

// A product edit that changes money asks what happens to the active links; nothing else asks.
func TestProductEditAsksAboutActiveLinks(t *testing.T) {
	t.Parallel()
	app := linkApp(t, nil)
	key := app.TestKey
	patch := func(id string, body map[string]any) apptest.Response {
		return app.Do("PATCH", "/v1/products/"+id, key, body)
	}
	prod := createProduct(t, app, key, map[string]any{"name": "Consulting", "price_type": "open", "min_amount": "10.00", "max_amount": "500.00"})
	invoice := apiLink(t, app, key, prod, map[string]any{"amount": "120.00", "name": "Invoice ACME #12"})
	plain := apiLink(t, app, key, prod, nil)
	off := apiLink(t, app, key, prod, nil)
	app.Do("PATCH", "/v1/payment_links/"+off, key, map[string]any{"active": false})

	for i, body := range []map[string]any{
		{"name": "Consulting hours"},
		{"description": "Billed by the hour."},
		{"suggested_amounts": []string{"200.00"}},
		{"success_url": "https://acme.example/thanks"},
		{"metadata": map[string]string{"team": "a"}},
		{"active": false},
		{"active": true},
		// The same values in another spelling are no change.
		{"min_amount": "10", "max_amount": "500.00"},
		{"links": "deactivate", "name": "Consulting"},
	} {
		if r := patch(prod, body); r.Status != http.StatusOK {
			t.Fatalf("edit %d %v: %d %s", i, body, r.Status, r.Body)
		}
	}
	if got := productEditLinks(t, app, prod); len(got) != 0 {
		t.Fatalf("an edit without a money change deactivated %v", got)
	}

	// An invalid value is refused whatever the edit.
	for _, body := range []map[string]any{{"name": "X", "links": "maybe"}, {"links": 5}, {"min_amount": "20.00", "links": "KEEP"}} {
		if r := patch(prod, body); r.Status != http.StatusBadRequest || r.ErrorCode(t) != "invalid_request" || errParam(t, r) != "links" {
			t.Errorf("%v: %d %s", body, r.Status, r.Body)
		}
	}
	if r := app.Do("POST", "/v1/products", key, map[string]any{"name": "X", "price_type": "fixed", "amount": "5.00", "links": "keep"}); r.Status != http.StatusBadRequest {
		t.Errorf("links on create: %d %s", r.Status, r.Body)
	}

	r := patch(prod, map[string]any{"min_amount": "150.00"})
	if r.Status != http.StatusConflict || r.ErrorCode(t) != "links_decision_required" || errParam(t, r) != "links" ||
		errMessage(t, r) != "This product has 2 active links. Choose whether to keep them active or deactivate them." {
		t.Fatalf("a money edit without a decision: %d %s", r.Status, r.Body)
	}
	if got := app.Do("GET", "/v1/products/"+prod, key, nil).JSON(t); got["min_amount"] != "10.00" {
		t.Fatalf("a refused edit was stored: %v", got)
	}

	// keep: the links stay live; the invoice keeps its amount below the new minimum and a session
	// opens at it, while a payer on the plain link is held to the new range.
	if r := patch(prod, map[string]any{"min_amount": "150.00", "links": "keep"}); r.Status != http.StatusOK || r.JSON(t)["min_amount"] != "150.00" {
		t.Fatalf("keep: %d %s", r.Status, r.Body)
	}
	if l := app.Do("GET", "/v1/payment_links/"+invoice, key, nil).JSON(t); l["live"] != true || l["amount"] != "120.00" {
		t.Fatalf("kept invoice link: %v", l)
	}
	if m := publicLink(t, app, invoice); m["price_type"] != "fixed" || m["amount"] != "120.00" {
		t.Fatalf("public kept invoice link: %v", m)
	}
	ses := openLink(app, invoice, "invoice", nil)
	if ses.Status != http.StatusOK || sessionAmount(t, app, ses.JSON(t)["id"].(string)) != "12000" {
		t.Fatalf("a session from a kept link amount outside the range: %d %s", ses.Status, ses.Body)
	}
	if r := openLink(app, plain, "payer", map[string]any{"amount": "100.00"}); r.Status != http.StatusBadRequest || r.ErrorCode(t) != "amount_below_minimum" {
		t.Fatalf("a payer below the new range: %d %s", r.Status, r.Body)
	}
	// Setting an amount still checks the range; editing anything else of the link does not.
	if r := app.Do("PATCH", "/v1/payment_links/"+invoice, key, map[string]any{"amount": "120.00"}); r.Status != http.StatusBadRequest ||
		r.ErrorCode(t) != "amount_below_minimum" {
		t.Fatalf("set an amount below the range: %d %s", r.Status, r.Body)
	}
	if r := app.Do("PATCH", "/v1/payment_links/"+invoice, key, map[string]any{"name": "Invoice ACME #12b"}); r.Status != http.StatusOK {
		t.Fatalf("rename a kept link: %d %s", r.Status, r.Body)
	}

	// deactivate: every active link goes off with the edit, audited; the inactive one is untouched.
	r = patch(prod, map[string]any{"max_amount": "400.00", "links": "deactivate"})
	if r.Status != http.StatusOK || r.JSON(t)["links"].(map[string]any)["live"] != float64(0) {
		t.Fatalf("deactivate: %d %s", r.Status, r.Body)
	}
	for _, id := range []string{invoice, plain, off} {
		if l := app.Do("GET", "/v1/payment_links/"+id, key, nil).JSON(t); l["active"] != false || l["unavailable_reason"] != "inactive" {
			t.Fatalf("link %s after deactivate: %v", id, l)
		}
	}
	want := []string{invoice, plain}
	sort.Strings(want)
	if got := productEditLinks(t, app, prod); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("audited %v, want %v", got, want)
	}
	if r := openLink(app, plain, "after", map[string]any{"amount": "200.00"}); r.Status != http.StatusGone {
		t.Fatalf("a deactivated link opened a session: %d %s", r.Status, r.Body)
	}
	// The session opened before keeps its amount.
	if got := sessionAmount(t, app, ses.JSON(t)["id"].(string)); got != "12000" {
		t.Fatalf("an open session changed: %s", got)
	}
	// Without active links nothing asks; reactivating never re-checks the range.
	if r := patch(prod, map[string]any{"max_amount": "450.00"}); r.Status != http.StatusOK {
		t.Fatalf("a money edit without active links: %d %s", r.Status, r.Body)
	}
	if r := app.Do("PATCH", "/v1/payment_links/"+invoice, key, map[string]any{"active": true}); r.Status != http.StatusOK ||
		r.JSON(t)["live"] != true || r.JSON(t)["amount"] != "120.00" {
		t.Fatalf("reactivate a link outside the range: %d %s", r.Status, r.Body)
	}
	r = patch(prod, map[string]any{"min_amount": "160.00"})
	if r.Status != http.StatusConflict || errMessage(t, r) != "This product has 1 active link. Choose whether to keep it active or deactivate it." {
		t.Fatalf("one active link: %d %s", r.Status, r.Body)
	}

	// The payout is money too; on a fixed price the amount is.
	fixed := createProduct(t, app, key, map[string]any{"name": "Founder", "price_type": "fixed", "amount": "25.00"})
	apiLink(t, app, key, fixed, nil)
	payout := app.Do("GET", "/v1/products/"+fixed, key, nil).JSON(t)["payout"].(map[string]any)
	evm := payout["base-sepolia"].([]any)[0].(map[string]any)["wallet_id"]
	if r := patch(fixed, map[string]any{"amount": "25.00", "payout": map[string]any{"base-sepolia": items(evm, "100")}}); r.Status != http.StatusOK {
		t.Fatalf("the same payout again: %d %s", r.Status, r.Body)
	}
	if r := patch(fixed, map[string]any{"payout": map[string]any{"base-sepolia": []any{}}}); r.Status != http.StatusConflict ||
		r.ErrorCode(t) != "links_decision_required" {
		t.Fatalf("a payout change: %d %s", r.Status, r.Body)
	}
	if r := patch(fixed, map[string]any{"amount": "30.00"}); r.Status != http.StatusConflict {
		t.Fatalf("a fixed amount change: %d %s", r.Status, r.Body)
	}
}

// A product's metadata may never push a link that stays active over the limits once merged, and
// turning a link on checks its merged metadata again.
func TestProductMetadataProtectsActiveLinks(t *testing.T) {
	t.Parallel()
	app := linkApp(t, nil)
	key := app.TestKey
	keys := func(prefix string, n int) map[string]string {
		m := map[string]string{}
		for i := range n {
			m[fmt.Sprintf("%s%d", prefix, i)] = "v"
		}
		return m
	}
	prod := createProduct(t, app, key, map[string]any{"name": "Seats", "price_type": "fixed", "amount": "25.00", "metadata": keys("p", 10)})
	active := apiLink(t, app, key, prod, map[string]any{"metadata": keys("a", 30)})
	sleeping := apiLink(t, app, key, prod, map[string]any{"metadata": keys("b", 35)})
	app.Do("PATCH", "/v1/payment_links/"+sleeping, key, map[string]any{"active": false})

	// 20 + 30 fits the active link; the inactive one (55 merged) does not count.
	if r := app.Do("PATCH", "/v1/products/"+prod, key, map[string]any{"metadata": keys("p", 20)}); r.Status != http.StatusOK {
		t.Fatalf("metadata within the limits: %d %s", r.Status, r.Body)
	}
	r := app.Do("PATCH", "/v1/products/"+prod, key, map[string]any{"metadata": keys("p", 21)})
	if r.Status != http.StatusBadRequest || r.ErrorCode(t) != "invalid_request" || errParam(t, r) != "metadata" || !strings.Contains(errMessage(t, r), active) {
		t.Fatalf("metadata over the limits for an active link: %d %s", r.Status, r.Body)
	}
	if r := app.Do("PATCH", "/v1/payment_links/"+sleeping, key, map[string]any{"active": true}); r.Status != http.StatusBadRequest ||
		errParam(t, r) != "metadata" {
		t.Fatalf("reactivate a link over the limits: %d %s", r.Status, r.Body)
	}
	if l := app.Do("GET", "/v1/payment_links/"+sleeping, key, nil).JSON(t); l["active"] != false {
		t.Fatalf("a refused reactivation turned the link on: %v", l)
	}
	// Deactivating or renaming a link is never blocked by its merged metadata.
	if r := app.Do("PATCH", "/v1/payment_links/"+sleeping, key, map[string]any{"name": "Old"}); r.Status != http.StatusOK {
		t.Fatalf("rename an inactive link over the limits: %d %s", r.Status, r.Body)
	}
	if r := app.Do("PATCH", "/v1/payment_links/"+active, key, map[string]any{"active": false}); r.Status != http.StatusOK {
		t.Fatalf("deactivate: %d %s", r.Status, r.Body)
	}
	if r := app.Do("PATCH", "/v1/products/"+prod, key, map[string]any{"metadata": keys("p", 25)}); r.Status != http.StatusOK {
		t.Fatalf("metadata with no active link left: %d %s", r.Status, r.Body)
	}
	// Links deactivated by the same edit no longer count.
	app.Do("PATCH", "/v1/products/"+prod, key, map[string]any{"metadata": keys("p", 10)})
	app.Do("PATCH", "/v1/payment_links/"+active, key, map[string]any{"active": true})
	if r := app.Do("PATCH", "/v1/products/"+prod, key, map[string]any{"metadata": keys("p", 25), "amount": "30.00", "links": "deactivate"}); r.Status != http.StatusOK {
		t.Fatalf("metadata with every active link deactivated: %d %s", r.Status, r.Body)
	}
	if r := app.Do("PATCH", "/v1/products/"+prod, key, map[string]any{"metadata": keys("p", 10)}); r.Status != http.StatusOK {
		t.Fatal(r.Body)
	}
	if r := app.Do("PATCH", "/v1/payment_links/"+active, key, map[string]any{"active": true}); r.Status != http.StatusOK {
		t.Fatalf("reactivate within the limits: %d %s", r.Status, r.Body)
	}
	if r := app.Do("PATCH", "/v1/products/"+prod, key, map[string]any{"metadata": keys("p", 25), "amount": "35.00", "links": "keep"}); r.Status != http.StatusBadRequest ||
		errParam(t, r) != "metadata" {
		t.Fatalf("metadata over the limits with the links kept: %d %s", r.Status, r.Body)
	}
	if got := app.Do("GET", "/v1/products/"+prod, key, nil).JSON(t); got["amount"] != "30.00" {
		t.Fatalf("a refused edit changed the amount: %v", got)
	}
}

// The dashboard asks the same way, deactivates the same way and shows how many checkouts are
// still open at the old price.
func TestDashboardProductEditLinks(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	p := dashProduct(t, c, "test", "prod", map[string]any{"name": "Seats", "price_type": "fixed", "amount": "25.00"})
	pid := p["id"].(string)
	if st := p["stats"].(map[string]any); st["open"] != float64(0) {
		t.Fatalf("stats of a new product: %v", st)
	}
	a := dashLink(t, c, "test", "a", map[string]any{"product_id": pid})["id"].(string)
	b := dashLink(t, c, "test", "b", map[string]any{"product_id": pid})["id"].(string)
	var opened []map[string]any
	for i, l := range []string{a, a, b} {
		r := openLink(app, l, fmt.Sprintf("open-%d", i), nil)
		if r.Status != http.StatusOK {
			t.Fatalf("open: %d %s", r.Status, r.Body)
		}
		opened = append(opened, r.JSON(t))
	}
	payFull(t, app, opened[0], "0x"+fmt.Sprintf("%064x", 9))
	if r := app.Session(app.TestKey, map[string]any{"product_id": pid}, idem("api")...); r.Status != http.StatusOK {
		t.Fatalf("api session: %d %s", r.Status, r.Body)
	}
	path := "/v1/dashboard/products/" + pid + "?mode=test"
	if st := c.get(path)["stats"].(map[string]any); st["open"] != float64(3) || st["opened"] != float64(4) || st["paid"] != float64(1) {
		t.Fatalf("stats: %v", st)
	}
	if r := c.do("PATCH", path, map[string]any{"amount": "30.00"}); r.Status != http.StatusConflict || r.ErrorCode(t) != "links_decision_required" ||
		errParam(t, r) != "links" || errMessage(t, r) != "This product has 2 active links. Choose whether to keep them active or deactivate them." {
		t.Fatalf("no decision: %d %s", r.Status, r.Body)
	}
	if r := c.do("PATCH", path, map[string]any{"amount": "30.00", "links": "pause"}); r.Status != http.StatusBadRequest || errParam(t, r) != "links" {
		t.Fatalf("bad decision: %d %s", r.Status, r.Body)
	}
	if got := c.send("PATCH", path, map[string]any{"amount": "30.00", "links": "keep"}); got["amount"] != "30.00" || got["links"].(map[string]any)["live"] != float64(2) {
		t.Fatalf("keep: %v", got)
	}
	got := c.send("PATCH", path, map[string]any{"amount": "35.00", "links": "deactivate"})
	if got["amount"] != "35.00" || got["links"].(map[string]any)["live"] != float64(0) || got["stats"].(map[string]any)["open"] != float64(3) {
		t.Fatalf("deactivate: %v", got)
	}
	for _, l := range c.get("/v1/dashboard/products/" + pid + "/links?mode=test&status=all")["data"].([]any) {
		if m := l.(map[string]any); m["active"] != false {
			t.Fatalf("a link stayed active: %v", m)
		}
	}
	want := []string{a, b}
	sort.Strings(want)
	if got := productEditLinks(t, app, pid); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("audited %v, want %v", got, want)
	}
	// Sessions already open keep the old price.
	if got := sessionAmount(t, app, opened[2]["id"].(string)); got != "2500" {
		t.Fatalf("an open session changed: %s", got)
	}
}

// A product edit that deactivates its links never deadlocks with link edits and sessions opened
// from those links at the same time: every writer locks the product before a link.
func TestProductDeactivateRacesLinkWrites(t *testing.T) {
	t.Parallel()
	app := linkApp(t, nil)
	key := app.TestKey
	prod := createProduct(t, app, key, map[string]any{"name": "Seats", "price_type": "fixed", "amount": "25.00"})
	links := []string{apiLink(t, app, key, prod, nil), apiLink(t, app, key, prod, nil), apiLink(t, app, key, prod, nil)}
	// Few API calls per round: the merchant API key is rate limited.
	for round := range 8 {
		if _, err := app.Store.Pool().Exec(t.Context(), `UPDATE payment_links SET active = true WHERE product_id = $1`, prod); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var mu sync.Mutex
		var bad []string
		record := func(what string, r apptest.Response) {
			if r.Status >= 500 {
				mu.Lock()
				bad = append(bad, fmt.Sprintf("%s: %d %s", what, r.Status, r.Body))
				mu.Unlock()
			}
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := app.Do("PATCH", "/v1/products/"+prod, key, map[string]any{"amount": fmt.Sprintf("%d.00", 26+round), "links": "deactivate"})
			if r.Status != http.StatusOK {
				mu.Lock()
				bad = append(bad, fmt.Sprintf("product edit: %d %s", r.Status, r.Body))
				mu.Unlock()
			}
		}()
		for i, l := range links {
			wg.Add(2)
			go func() {
				defer wg.Done()
				record("open", openLink(app, l, fmt.Sprintf("race-%d-%d", round, i), nil))
			}()
			go func() {
				defer wg.Done()
				record("link edit", app.Do("PATCH", "/v1/payment_links/"+l, key, map[string]any{"name": fmt.Sprintf("Seat %d", round)}))
			}()
		}
		wg.Wait()
		if len(bad) > 0 {
			t.Fatalf("round %d: %v", round, bad)
		}
		var active int
		if err := app.Store.Pool().QueryRow(t.Context(), `SELECT count(*) FROM payment_links WHERE product_id = $1 AND active`, prod).Scan(&active); err != nil || active != 0 {
			t.Fatalf("round %d: %d links still active (%v)", round, active, err)
		}
	}
}
