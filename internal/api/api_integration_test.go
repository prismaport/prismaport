//go:build integration

package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/checkout"
	"github.com/prismaport/prismaport/internal/derive"
	"github.com/prismaport/prismaport/internal/evm"
	"github.com/prismaport/prismaport/internal/service"
	"github.com/prismaport/prismaport/internal/store"
)

func idem(k string) []string { return []string{"Idempotency-Key", k} }

func TestAuth(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	cases := []struct {
		key  string
		hdr  []string
		code int
	}{
		{"", nil, http.StatusUnauthorized},
		{"sk_live_nope", nil, http.StatusUnauthorized},
		{"sk_test_3mJr7AoUXx2Wqd8Tf1S5yE9kBnJr7AoUXx2Wqd8Tf1S5", nil, http.StatusUnauthorized},
	}
	for _, c := range cases {
		t.Run(c.key, func(t *testing.T) {
			r := app.Do("GET", "/v1/products", c.key, nil)
			if r.Status != c.code || r.ErrorCode(t) != "invalid_api_key" {
				t.Errorf("key %q: %d %s", c.key, r.Status, r.Body)
			}
		})
	}
	r := app.Do("GET", "/v1/products", "", nil, "Authorization", "Basic "+app.TestKey)
	if r.Status != http.StatusUnauthorized {
		t.Fatalf("basic auth accepted: %d", r.Status)
	}
	if r := app.Do("GET", "/v1/products", app.TestKey, nil); r.Status != http.StatusOK {
		t.Fatalf("valid key rejected: %d %s", r.Status, r.Body)
	}
	if r := app.Do("GET", "/healthz", "", nil); r.Status != http.StatusNoContent {
		t.Fatalf("healthz: %d", r.Status)
	}
	if r := app.Do("GET", "/v1/nope", app.TestKey, nil); r.Status != http.StatusNotFound || r.ErrorCode(t) != "not_found" {
		t.Fatalf("unknown route: %d %s", r.Status, r.Body)
	}
}

func TestScopesAndRevocation(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	ctx := t.Context()
	k, readOnly, err := app.Svc.CreateAPIKey(ctx, app.MerchantID, false, []string{"products:read"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if r := app.Do("GET", "/v1/products", readOnly, nil); r.Status != http.StatusOK {
		t.Fatalf("read with read scope: %d", r.Status)
	}
	r := app.Do("POST", "/v1/products", readOnly, map[string]any{"name": "x", "price_type": "fixed", "amount": "5.00"})
	if r.Status != http.StatusForbidden || r.ErrorCode(t) != "insufficient_scope" {
		t.Fatalf("write without scope: %d %s", r.Status, r.Body)
	}
	if err := app.Store.RevokeAPIKey(ctx, k.ID, "test"); err != nil {
		t.Fatal(err)
	}
	if r := app.Do("GET", "/v1/products", readOnly, nil); r.Status != http.StatusUnauthorized {
		t.Fatalf("revoked key accepted: %d", r.Status)
	}
}

// liveWallet is the id of the seeded live wallet of a family.
func liveWallet(t *testing.T, app *apptest.App, fam chain.Family) string {
	t.Helper()
	ws, err := app.Store.ListWallets(t.Context(), app.MerchantID, true, store.WalletFilter{Family: fam})
	if err != nil || len(ws) != 1 {
		t.Fatalf("live %s wallets %+v %v", fam, ws, err)
	}
	return ws[0].ID
}

var idPattern = map[string]*regexp.Regexp{
	"prod": regexp.MustCompile(`^prod_[0-9A-HJKMNP-TV-Z]{26}$`),
	"ses":  regexp.MustCompile(`^ses_[0-9A-HJKMNP-TV-Z]{26}$`),
}

// TestProductPaymentMethods: a product lists what its checkouts offer now, so a storefront follows
// the account's accepted assets and the product's payout without a code change.
func TestProductPaymentMethods(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	r := app.Do("POST", "/v1/products", app.LiveKey, map[string]any{"name": "Founder", "price_type": "fixed", "amount": "25.00"})
	if r.Status != http.StatusOK {
		t.Fatalf("create: %d %s", r.Status, r.Body)
	}
	id := r.JSON(t)["id"].(string)
	methods := func(want string) {
		t.Helper()
		g := app.Do("GET", "/v1/products/"+id, app.LiveKey, nil)
		var p struct {
			PaymentMethods json.RawMessage `json:"payment_methods"`
		}
		if err := json.Unmarshal(g.Body, &p); err != nil || g.Status != http.StatusOK {
			t.Fatalf("get: %d %s", g.Status, g.Body)
		}
		if string(p.PaymentMethods) != want {
			t.Fatalf("get payment_methods:\n got %s\nwant %s", p.PaymentMethods, want)
		}
		l := app.Do("GET", "/v1/products", app.LiveKey, nil)
		var list struct {
			Data []struct {
				ID             string          `json:"id"`
				PaymentMethods json.RawMessage `json:"payment_methods"`
			} `json:"data"`
		}
		if err := json.Unmarshal(l.Body, &list); err != nil || len(list.Data) != 1 || string(list.Data[0].PaymentMethods) != want {
			t.Fatalf("list: %d %s", l.Status, l.Body)
		}
	}
	methods(`[{"chain":"base","chain_name":"Base","asset":"USDC"},{"chain":"solana","chain_name":"Solana","asset":"USDC"},{"chain":"solana","chain_name":"Solana","asset":"USDT"}]`)

	ctx := t.Context()
	if err := app.Store.SetAcceptedAssets(ctx, app.MerchantID, true, []store.ChainAsset{{Chain: "base", Asset: "USDC"}, {Chain: "solana", Asset: "USDT"}}, "test"); err != nil {
		t.Fatal(err)
	}
	methods(`[{"chain":"base","chain_name":"Base","asset":"USDC"},{"chain":"solana","chain_name":"Solana","asset":"USDT"}]`)

	if u := app.Do("PATCH", "/v1/products/"+id, app.LiveKey, map[string]any{"payout": map[string]any{"base": []any{}}}); u.Status != http.StatusOK {
		t.Fatalf("drop base: %d %s", u.Status, u.Body)
	}
	methods(`[{"chain":"solana","chain_name":"Solana","asset":"USDT"}]`)

	if err := app.Store.SetAcceptedAssets(ctx, app.MerchantID, true, []store.ChainAsset{{Chain: "base", Asset: "USDC"}}, "test"); err != nil {
		t.Fatal(err)
	}
	methods(`[]`)

	// Test keys see the test networks.
	tp := app.Do("POST", "/v1/products", app.TestKey, map[string]any{"name": "Sandbox", "price_type": "fixed", "amount": "5.00"})
	if !strings.Contains(string(tp.Body), `{"chain":"base-sepolia","chain_name":"Base Sepolia","asset":"USDC"}`) {
		t.Fatalf("test product: %d %s", tp.Status, tp.Body)
	}
}

func TestProductShapeAndValidation(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	r := app.Do("POST", "/v1/products", app.LiveKey, map[string]any{"name": "Founder", "price_type": "fixed", "amount": "25.00"})
	if r.Status != http.StatusOK {
		t.Fatalf("create: %d %s", r.Status, r.Body)
	}
	p := r.JSON(t)
	id := p["id"].(string)
	if !idPattern["prod"].MatchString(id) {
		t.Fatalf("id %s", id)
	}
	want := fmt.Sprintf(`{"id":%q,"name":"Founder","price_type":"fixed","amount":"25.00","currency":"USD","active":true,"metadata":{},`+
		`"payout":{"base":[{"wallet_id":%q,"label":"Main wallet","address":%q,"share":"100.00","bps":10000}],`+
		`"solana":[{"wallet_id":%q,"label":"Main wallet","address":%q,"share":"100.00","bps":10000}]},`+
		`"networks":[{"chain":"base","chain_name":"Base","accept":"all"},{"chain":"solana","chain_name":"Solana","accept":"all"}],`+
		`"payment_methods":[{"chain":"base","chain_name":"Base","asset":"USDC"},{"chain":"solana","chain_name":"Solana","asset":"USDC"},`+
		`{"chain":"solana","chain_name":"Solana","asset":"USDT"}],"links":{"live":0,"total":0},"created_at":"2026-10-23T18:00:00Z"}`,
		id, liveWallet(t, app, chain.EVM), apptest.EVMDestination, liveWallet(t, app, chain.Solana), apptest.SolanaDestination)
	if string(r.Body) != want {
		t.Fatalf("product shape:\n got %s\nwant %s", r.Body, want)
	}

	open := app.Do("POST", "/v1/products", app.LiveKey, map[string]any{"name": "Donate", "price_type": "open", "min_amount": "1.00",
		"max_amount": "100.00", "suggested_amounts": []string{"5.00", "10.00"}, "metadata": map[string]string{"k": "v"}})
	if open.Status != http.StatusOK {
		t.Fatalf("open: %d %s", open.Status, open.Body)
	}
	o := open.JSON(t)
	if o["amount"] != nil || o["min_amount"] != "1.00" || o["max_amount"] != "100.00" {
		t.Fatalf("open shape %s", open.Body)
	}

	bad := []struct {
		body any
		code string
	}{
		{map[string]any{"name": "x", "price_type": "fixed", "amount": "25.001"}, "invalid_request"},
		{map[string]any{"name": "x", "price_type": "fixed", "amount": 25}, "invalid_request"},
		{map[string]any{"name": "x", "price_type": "fixed", "amount": "-1"}, "invalid_request"},
		{map[string]any{"name": "x", "price_type": "fixed", "amount": "0.10"}, "amount_below_minimum"},
		{map[string]any{"name": "x", "price_type": "fixed", "amount": "0.11"}, ""},
		{map[string]any{"name": "x", "price_type": "fixed"}, "invalid_request"},
		{map[string]any{"name": "x", "price_type": "fixed", "amount": "5.00", "min_amount": "1.00"}, "invalid_request"},
		{map[string]any{"name": "x", "price_type": "open"}, "invalid_request"},
		{map[string]any{"name": "x", "price_type": "open", "min_amount": "0.05"}, "amount_below_minimum"},
		{map[string]any{"name": "x", "price_type": "open", "min_amount": "5.00", "max_amount": "4.00"}, "invalid_request"},
		{map[string]any{"name": "x", "price_type": "open", "min_amount": "5.00", "suggested_amounts": []string{"1.00"}}, "invalid_request"},
		{map[string]any{"name": "x", "price_type": "open", "min_amount": "5.00", "suggested_amounts": []string{"5.00", "6.00", "7.00", "8.00"}}, "invalid_request"},
		{map[string]any{"name": "", "price_type": "fixed", "amount": "5.00"}, "invalid_request"},
		{map[string]any{"name": "x", "price_type": "recurring", "amount": "5.00"}, "invalid_request"},
		{map[string]any{"name": "x", "price_type": "fixed", "ammount": "5.00"}, "invalid_request"},
		{map[string]any{"name": "x", "price_type": "fixed", "amount": "5.00", "success_url": "http://insecure.example"}, "invalid_request"},
		{map[string]any{"name": "x", "price_type": "fixed", "amount": "5.00", "metadata": map[string]any{"n": 1}}, "invalid_request"},
		{"[1,2]", "invalid_request"},
	}
	for i, b := range bad {
		t.Run(b.code, func(t *testing.T) {
			r := app.Do("POST", "/v1/products", app.LiveKey, b.body)
			if b.code == "" {
				if r.Status != http.StatusOK {
					t.Errorf("case %d rejected: %s", i, r.Body)
				}
				return
			}
			if r.Status != http.StatusBadRequest || r.ErrorCode(t) != b.code {
				t.Errorf("case %d: %d %s, want %s", i, r.Status, r.Body, b.code)
			}
		})
	}

	if r := app.Do("GET", "/v1/products/"+id, app.LiveKey, nil); r.Status != http.StatusOK || string(r.Body) != want {
		t.Fatalf("get: %d %s", r.Status, r.Body)
	}
	if r := app.Do("GET", "/v1/products/"+id, app.TestKey, nil); r.Status != http.StatusNotFound || r.ErrorCode(t) != "product_not_found" {
		t.Fatalf("test key read a live product: %d %s", r.Status, r.Body)
	}
	app.Clock.Advance(time.Minute)
	patched := app.Do("PATCH", "/v1/products/"+id, app.LiveKey, map[string]any{"name": "Founder v2", "active": false, "amount": "30.00"})
	pm := patched.JSON(t)
	if patched.Status != http.StatusOK || pm["name"] != "Founder v2" || pm["active"] != false || pm["amount"] != "30.00" {
		t.Fatalf("patch: %d %s", patched.Status, patched.Body)
	}
	if r := app.Do("PATCH", "/v1/products/"+id, app.LiveKey, map[string]any{"price_type": "open"}); r.ErrorCode(t) != "invalid_request" {
		t.Fatalf("price_type change accepted: %s", r.Body)
	}
	if r := app.Do("PATCH", "/v1/products/prod_missing", app.LiveKey, map[string]any{"name": "y"}); r.ErrorCode(t) != "product_not_found" {
		t.Fatalf("patch missing: %s", r.Body)
	}
}

func TestProductList(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	var created []string
	for i := 0; i < 5; i++ {
		app.Clock.Advance(time.Millisecond)
		r := app.Do("POST", "/v1/products", app.TestKey, map[string]any{"name": fmt.Sprint("p", i), "price_type": "fixed", "amount": "5.00"})
		created = append(created, r.JSON(t)["id"].(string))
	}
	var seen []string
	after := ""
	for {
		r := app.Do("GET", "/v1/products?limit=2&starting_after="+after, app.TestKey, nil)
		var page struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
			HasMore bool `json:"has_more"`
		}
		if err := json.Unmarshal(r.Body, &page); err != nil {
			t.Fatal(err)
		}
		for _, p := range page.Data {
			seen = append(seen, p.ID)
		}
		if !page.HasMore {
			break
		}
		after = page.Data[len(page.Data)-1].ID
	}
	if len(seen) != 5 || seen[0] != created[4] || seen[4] != created[0] {
		t.Fatalf("pagination: %v vs %v", seen, created)
	}
	if r := app.Do("GET", "/v1/products?limit=0", app.TestKey, nil); r.ErrorCode(t) != "invalid_request" {
		t.Fatal("limit=0 accepted")
	}
}

func createFixed(t *testing.T, app *apptest.App, key, amount string) string {
	t.Helper()
	r := app.Do("POST", "/v1/products", key, map[string]any{"name": "Founder", "price_type": "fixed", "amount": amount,
		"metadata": map[string]string{"plan": "founder"}})
	if r.Status != http.StatusOK {
		t.Fatalf("product: %s", r.Body)
	}
	return r.JSON(t)["id"].(string)
}

func TestSessionShapeLive(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	prod := createFixed(t, app, app.LiveKey, "25.00")
	r := app.Session(app.LiveKey, map[string]any{"product_id": prod, "metadata": map[string]string{"user_id": "f36361bc"}}, idem("k1")...)
	if r.Status != http.StatusOK {
		t.Fatalf("create: %d %s", r.Status, r.Body)
	}
	var s map[string]any
	_ = json.Unmarshal(r.Body, &s)
	id := s["id"].(string)
	if !idPattern["ses"].MatchString(id) {
		t.Fatalf("id %s", id)
	}
	shape := func(payTo, expires, started string) string {
		return fmt.Sprintf(`{"id":%q,"version":1,"livemode":true,"status":"open","product_id":%q,"link_id":null,"customer_address_id":null,"amount":"25.00","currency":"USD",`+
			`"amount_received":"0.00","amount_remaining":"25.00","checkout_url":"https://pay.test/s/%s",`+
			`"pay_to":%s,`+
			`"payout":{"base":[{"wallet_id":%q,"label":"Main wallet","address":%q,"share":"100.00","bps":10000}],`+
			`"solana":[{"wallet_id":%q,"label":"Main wallet","address":%q,"share":"100.00","bps":10000}]},`+
			`"payments":[],"settlement":null,"metadata":{"plan":"founder","user_id":"f36361bc"},`+
			`"client_reference_id":null,"client_reference_source":null,`+
			`"expires_at":%q,"payment_started_at":%s,"pay_window_seconds":1800,"created_at":"2026-10-23T18:00:00Z","paid_at":null}`,
			id, prod, id, payTo, liveWallet(t, app, chain.EVM), apptest.EVMDestination,
			liveWallet(t, app, chain.Solana), apptest.SolanaDestination, expires, started)
	}
	// D48: no deposit details anywhere until a network is revealed; the session waits the
	// selection window (1 h) for it.
	want := shape(`[{"chain":"base","asset":"USDC","amount":"25.000000","revealed":false},`+
		`{"chain":"solana","asset":"USDC","amount":"25.000000","revealed":false},`+
		`{"chain":"solana","asset":"USDT","amount":"25.000000","revealed":false}]`, "2026-10-23T19:00:00Z", "null")
	if string(r.Body) != want {
		t.Fatalf("session shape:\n got %s\nwant %s", r.Body, want)
	}
	g := app.Do("GET", "/v1/sessions/"+id, app.LiveKey, nil)
	if g.Status != http.StatusOK || string(g.Body) != want {
		t.Fatalf("GET differs from create:\n%s", g.Body)
	}

	app.Clock.Advance(10 * time.Minute)
	r = app.Reveal(app.LiveKey, id)
	payTo := r.JSON(t)["pay_to"].([]any)
	if len(payTo) != 3 {
		t.Fatalf("pay_to %v", payTo)
	}
	var evmAddr, solAddr string
	for _, x := range payTo {
		p := x.(map[string]any)
		if p["amount"] != "25.000000" {
			t.Fatalf("pay_to amount %v", p["amount"])
		}
		switch p["chain"] {
		case "base":
			evmAddr = p["address"].(string)
		case "solana":
			if solAddr != "" && solAddr != p["address"] {
				t.Fatal("solana USDC and USDT must share the session address")
			}
			solAddr = p["address"].(string)
		default:
			t.Fatalf("unexpected chain %v", p["chain"])
		}
	}
	// The first reveal starts the 30-minute pay window; the second does not move it.
	want = shape(fmt.Sprintf(`[{"chain":"base","asset":"USDC","address":%q,"amount":"25.000000","revealed":true,"revealed_at":"2026-10-23T18:10:00Z"},`+
		`{"chain":"solana","asset":"USDC","address":%q,"amount":"25.000000","revealed":true,"revealed_at":"2026-10-23T18:10:00Z"},`+
		`{"chain":"solana","asset":"USDT","address":%q,"amount":"25.000000","revealed":true,"revealed_at":"2026-10-23T18:10:00Z"}]`,
		evmAddr, solAddr, solAddr), "2026-10-23T18:40:00Z", `"2026-10-23T18:10:00Z"`)
	if string(r.Body) != want {
		t.Fatalf("revealed shape:\n got %s\nwant %s", r.Body, want)
	}
	if g := app.Do("GET", "/v1/sessions/"+id, app.LiveKey, nil); g.Status != http.StatusOK || string(g.Body) != want {
		t.Fatalf("GET differs from the reveal:\n%s", g.Body)
	}
	if r := app.Do("GET", "/v1/sessions/"+id, app.TestKey, nil); r.ErrorCode(t) != "session_not_found" {
		t.Fatalf("test key read live session: %s", r.Body)
	}
	if r := app.Do("GET", "/v1/sessions/ses_nope", app.LiveKey, nil); r.Status != http.StatusNotFound || r.ErrorCode(t) != "session_not_found" {
		t.Fatalf("missing session: %s", r.Body)
	}
}

func TestSessionIdempotency(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	body := `{"amount":"10.00"}`
	first := app.Session(app.TestKey, body, idem("abc")...)
	if first.Status != http.StatusOK || first.Header.Get("Idempotent-Replayed") != "" {
		t.Fatalf("first: %d %s", first.Status, first.Body)
	}
	app.Clock.Advance(time.Minute)
	again := app.Session(app.TestKey, body, idem("abc")...)
	if again.Status != http.StatusOK || string(again.Body) != string(first.Body) || again.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay differs: %d %s", again.Status, again.Body)
	}
	conflict := app.Session(app.TestKey, `{"amount":"11.00"}`, idem("abc")...)
	if conflict.Status != http.StatusConflict || conflict.ErrorCode(t) != "idempotency_conflict" {
		t.Fatalf("conflict: %d %s", conflict.Status, conflict.Body)
	}
	if r := app.Session(app.TestKey, body); r.ErrorCode(t) != "invalid_request" {
		t.Fatalf("missing key accepted: %s", r.Body)
	}
	// The same key in the other mode is a different scope.
	if r := app.Session(app.LiveKey, body, idem("abc")...); r.Status != http.StatusOK || string(r.Body) == string(first.Body) {
		t.Fatalf("live scope: %d %s", r.Status, r.Body)
	}
	// A failed request stores nothing, so the key can be reused once the request is fixed.
	if r := app.Session(app.TestKey, `{"amount":"0.01"}`, idem("retry")...); r.ErrorCode(t) != "amount_below_minimum" {
		t.Fatalf("below floor: %s", r.Body)
	}
	if r := app.Session(app.TestKey, `{"amount":"1.00"}`, idem("retry")...); r.Status != http.StatusOK {
		t.Fatalf("retry after failure: %s", r.Body)
	}
}

func TestConcurrentIdempotentCreates(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	var wg sync.WaitGroup
	bodies := make([]string, 12)
	for i := range bodies {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := app.Session(app.TestKey, `{"amount":"10.00"}`, idem("race")...)
			bodies[i] = fmt.Sprint(r.Status, string(r.Body))
		}(i)
	}
	wg.Wait()
	for _, b := range bodies[1:] {
		if b != bodies[0] {
			t.Fatalf("divergent responses:\n%s\n%s", bodies[0], b)
		}
	}
	var n int
	if err := app.Store.Pool().QueryRow(t.Context(), `SELECT count(*) FROM sessions`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("sessions created: %d %v", n, err)
	}
}

func TestSessionValidation(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	fixed := createFixed(t, app, app.TestKey, "25.00")
	open := app.Do("POST", "/v1/products", app.TestKey, map[string]any{"name": "Donate", "price_type": "open", "min_amount": "1.00", "max_amount": "50.00"}).JSON(t)["id"].(string)
	inactive := createFixed(t, app, app.TestKey, "5.00")
	app.Do("PATCH", "/v1/products/"+inactive, app.TestKey, map[string]any{"active": false})

	cases := []struct {
		body   string
		status int
		code   string
	}{
		{`{}`, 400, "invalid_request"},
		{`{"product_id":"prod_missing"}`, 404, "product_not_found"},
		{fmt.Sprintf(`{"product_id":%q}`, inactive), 400, "product_inactive"},
		{fmt.Sprintf(`{"product_id":%q,"amount":"30.00"}`, fixed), 400, "invalid_request"},
		{fmt.Sprintf(`{"product_id":%q}`, open), 400, "invalid_request"},
		{fmt.Sprintf(`{"product_id":%q,"amount":"0.99"}`, open), 400, "amount_below_minimum"},
		{fmt.Sprintf(`{"product_id":%q,"amount":"50.01"}`, open), 400, "amount_above_maximum"},
		{fmt.Sprintf(`{"product_id":%q,"amount":"50.00"}`, open), 200, ""},
		{`{"amount":"0.10"}`, 400, "amount_below_minimum"},
		{`{"amount":"10.00","expires_in":59}`, 400, "invalid_request"},
		{`{"amount":"10.00","expires_in":86401}`, 400, "invalid_request"},
		{`{"amount":"10.00","expires_in":86400}`, 200, ""},
		// 18446744134 s overflows a time.Duration and used to wrap to about a minute.
		{`{"amount":"10.00","expires_in":18446744134}`, 400, "invalid_request"},
		{`{"amount":"10.00","expires_in":-1}`, 400, "invalid_request"},
		{`{"amount":"10.00","expires_in":3600.5}`, 400, "invalid_request"},
		{`{"amount":10}`, 400, "invalid_request"},
		{`{"amount":"10.00","success_url":"javascript:alert(1)"}`, 400, "invalid_request"},
		{`{"amount":"10.00","metadata":{"a":"b"},"extra":1}`, 400, "invalid_request"},
	}
	for i, c := range cases {
		t.Run(c.code, func(t *testing.T) {
			r := app.Session(app.TestKey, c.body, idem(fmt.Sprint("v", i))...)
			if r.Status != c.status || (c.code != "" && r.ErrorCode(t) != c.code) {
				t.Errorf("%s: %d %s", c.body, r.Status, r.Body)
			}
		})
	}
	// expires_in is the pay window: it starts at the first reveal (D48).
	r := app.Session(app.TestKey, `{"amount":"10.00","expires_in":86400}`, idem("exp")...)
	if s := r.JSON(t); s["expires_at"] != "2026-10-23T19:00:00Z" || s["pay_window_seconds"] != float64(86400) || s["payment_started_at"] != nil {
		t.Fatalf("before the reveal: %v", s)
	}
	app.Clock.Advance(5 * time.Minute)
	r = app.Reveal(app.TestKey, r.JSON(t)["id"].(string), "base-sepolia")
	if s := r.JSON(t); s["expires_at"] != "2026-10-24T18:05:00Z" || s["payment_started_at"] != "2026-10-23T18:05:00Z" {
		t.Fatalf("after the reveal: %v", s)
	}
}

func TestLiveSessionsFailClosedWithoutDerivation(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{Deriver: derive.ByFamily{chain.EVM: derive.Stub{}, chain.Solana: derive.Stub{}}})
	r := app.Session(app.LiveKey, `{"amount":"10.00"}`, idem("live")...)
	if r.Status != http.StatusServiceUnavailable || r.ErrorCode(t) != "service_unavailable" {
		t.Fatalf("live session with stub deriver: %d %s", r.Status, r.Body)
	}
	if r := app.Session(app.TestKey, `{"amount":"10.00"}`, idem("test")...); r.Status != http.StatusOK {
		t.Fatalf("test session: %d %s", r.Status, r.Body)
	}
}

// TestLiveBaseSessionWithFactory: once a Factory is configured, live Base sessions get the real
// CREATE2 forwarder address (recomputed here from the stored salt), while a live merchant that
// also accepts Solana still fails closed until the Solana program exists.
func TestLiveBaseSessionWithFactory(t *testing.T) {
	t.Parallel()
	factory, _ := evm.ParseAddress("0xEBC1282Dc447787Db5AaC24c0D23Aa7f25b44083")
	d := derive.PerChain{Chains: map[string]derive.AddressDeriver{"base": derive.Forwarders(factory)},
		Fallback: derive.ByFamily{chain.EVM: derive.Stub{}, chain.Solana: derive.Stub{}}}
	app := apptest.New(t, apptest.Options{Deriver: d})
	if r := app.Session(app.LiveKey, `{"amount":"10.00"}`, idem("both")...); r.Status != http.StatusServiceUnavailable {
		t.Fatalf("live session accepting solana: %d %s", r.Status, r.Body)
	}
	ctx := t.Context()
	if err := app.Store.SetAcceptedAssets(ctx, app.MerchantID, true, []store.ChainAsset{{Chain: "base", Asset: "USDC"}}, "test"); err != nil {
		t.Fatal(err)
	}
	r := app.Session(app.LiveKey, `{"amount":"10.00"}`, idem("base")...)
	if r.Status != http.StatusOK {
		t.Fatalf("live base session: %d %s", r.Status, r.Body)
	}
	id := r.JSON(t)["id"].(string)
	ses, err := app.Store.LoadSession(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(ses.PayTo) != 1 || ses.PayTo[0].Chain != "base" {
		t.Fatalf("pay_to %+v", ses.PayTo)
	}
	base, _ := chain.Pilot().Get("base")
	if len(ses.PayTo[0].Recipients) != 1 || ses.PayTo[0].Recipients[0].Address != apptest.EVMDestination || ses.PayTo[0].Recipients[0].Bps != 10_000 {
		t.Fatalf("recipients %+v", ses.PayTo[0].Recipients)
	}
	// The forwarder binds every asset the session offers on the chain (D41).
	var symbols []string
	for _, pt := range ses.PayTo {
		if pt.Chain == "base" {
			symbols = append(symbols, pt.Asset)
		}
	}
	tokens, err := derive.BoundTokens(base, symbols)
	if err != nil {
		t.Fatal(err)
	}
	want, err := derive.Forwarders(factory).Address(base, ses.Salt, derive.Params{Recipients: ses.PayTo[0].Recipients,
		Treasury: ses.PayTo[0].Treasury, Fee: ses.PayTo[0].Fee, Tokens: tokens})
	if err != nil || ses.PayTo[0].Address != want {
		t.Fatalf("address %s, want %s (%v)", ses.PayTo[0].Address, want, err)
	}
}

// TestPublicCheckoutSession: the unauthenticated view carries what a payer needs and nothing a
// merchant keeps private; the signed path fails closed where no Gateway is configured.
func TestPublicCheckoutSession(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{Checkout: func(svc *service.Service, st *store.Store) *checkout.Checkout { return checkout.New(svc, st, nil) }})
	prod := app.Do("POST", "/v1/products", app.TestKey, map[string]any{"name": "Founder", "price_type": "fixed", "amount": "25.00",
		"success_url": "https://example.com/thanks"}).JSON(t)["id"].(string)
	r := app.Session(app.TestKey, map[string]any{"product_id": prod, "metadata": map[string]string{"user_id": "secret-user"}}, idem("p")...)
	id := r.JSON(t)["id"].(string)

	pub := app.Do("GET", "/v1/checkout/sessions/"+id, "", nil)
	if pub.Status != http.StatusOK || pub.Header.Get("Access-Control-Allow-Origin") != "*" || pub.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("public session: %d %v %s", pub.Status, pub.Header, pub.Body)
	}
	for _, leak := range []string{"secret-user", "metadata", app.MerchantID, "settlement\"", "thanks"} {
		if strings.Contains(string(pub.Body), leak) {
			t.Fatalf("public view leaks %q: %s", leak, pub.Body)
		}
	}
	m := pub.JSON(t)
	if m["amount"] != "25.00" || m["status"] != "open" || m["product"].(map[string]any)["name"] != "Founder" || m["amount_remaining"] != "25.00" {
		t.Fatalf("public view: %s", pub.Body)
	}
	payTo := m["pay_to"].([]any)
	if len(payTo) == 0 || payTo[0].(map[string]any)["signed_payment"] != false {
		t.Fatalf("pay_to: %v", payTo)
	}
	typed := app.Do("POST", "/v1/checkout/sessions/"+id+"/eip3009/typed_data", "", map[string]string{"chain": "base-sepolia", "asset": "USDC",
		"from": "0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed"})
	if typed.Status != http.StatusServiceUnavailable || typed.ErrorCode(t) != "service_unavailable" {
		t.Fatalf("typed data without a gateway: %d %s", typed.Status, typed.Body)
	}
	if r := app.Do("POST", "/v1/checkout/sessions/"+id+"/eip3009/typed_data", "", map[string]string{"chain": "base-sepolia"}); r.Status != http.StatusBadRequest {
		t.Fatalf("missing fields: %d %s", r.Status, r.Body)
	}
	if r := app.Do("POST", "/v1/checkout/sessions/"+id+"/eip3009/submit", "", map[string]string{"chain": "base-sepolia", "asset": "USDC",
		"from": "0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed", "value": "1.5", "valid_after": "0", "valid_before": "1", "signature": "0x"}); r.Status != http.StatusBadRequest {
		t.Fatalf("fractional value: %d %s", r.Status, r.Body)
	}
	for _, bad := range []string{"ses_nope", "prod_" + id[4:], strings.Repeat("x", 80)} {
		if r := app.Do("GET", "/v1/checkout/sessions/"+bad, "", nil); r.Status != http.StatusNotFound || r.ErrorCode(t) != "session_not_found" {
			t.Fatalf("%s: %d %s", bad, r.Status, r.Body)
		}
	}
	if r := app.Do("OPTIONS", "/v1/checkout/sessions/"+id+"/eip3009/submit", "", nil); r.Status != http.StatusNoContent ||
		r.Header.Get("Access-Control-Allow-Methods") == "" {
		t.Fatalf("preflight: %d", r.Status)
	}
}

// Even if a merchant's accepted assets name EVM USDT, bridged USDC or Arbitrum, the pilot
// registry only ever produces native USDC on Base and USDC/USDT on Solana.
func TestPilotPayToIsNativeUSDCOnEVM(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	extra := []store.ChainAsset{{Chain: "base", Asset: "USDC"}, {Chain: "base", Asset: "USDT"}, {Chain: "base", Asset: "USDbC"},
		{Chain: "arbitrum", Asset: "USDC"}, {Chain: "arbitrum", Asset: "USDT"}, {Chain: "solana", Asset: "USDC"}, {Chain: "solana", Asset: "USDT"}}
	if err := app.Store.SetAcceptedAssets(t.Context(), app.MerchantID, true, extra, "test"); err != nil {
		t.Fatal(err)
	}
	r := app.Session(app.LiveKey, `{"amount":"25.00"}`, idem("pilot")...)
	if r.Status != http.StatusOK {
		t.Fatalf("create: %d %s", r.Status, r.Body)
	}
	var s struct {
		PayTo []struct{ Chain, Asset string } `json:"pay_to"`
	}
	_ = json.Unmarshal(r.Body, &s)
	var got []string
	for _, p := range s.PayTo {
		got = append(got, p.Chain+":"+p.Asset)
	}
	if strings.Join(got, ",") != "base:USDC,solana:USDC,solana:USDT" {
		t.Fatalf("pay_to %v", got)
	}
}
