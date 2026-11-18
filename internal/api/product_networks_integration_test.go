//go:build integration

package api_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/store"
)

// The live pilot registry: base takes USDC, solana USDC and USDT. apptest accepts all three.
const (
	methodBaseUSDC   = `{"chain":"base","chain_name":"Base","asset":"USDC"}`
	methodSolanaUSDC = `{"chain":"solana","chain_name":"Solana","asset":"USDC"}`
	methodSolanaUSDT = `{"chain":"solana","chain_name":"Solana","asset":"USDT"}`
)

func methodsJSON(ms ...string) string { return "[" + strings.Join(ms, ",") + "]" }

// productState reads a product through the merchant API: its networks and payment_methods as
// JSON, and the networks its payout pays.
func productState(t *testing.T, app *apptest.App, key, id string) (networks, methods string, paid []string) {
	t.Helper()
	r := app.Do("GET", "/v1/products/"+id, key, nil)
	var p struct {
		Networks       json.RawMessage            `json:"networks"`
		PaymentMethods json.RawMessage            `json:"payment_methods"`
		Payout         map[string]json.RawMessage `json:"payout"`
	}
	if r.Status != http.StatusOK || json.Unmarshal(r.Body, &p) != nil {
		t.Fatalf("get %s: %d %s", id, r.Status, r.Body)
	}
	for c, items := range p.Payout {
		if string(items) != "[]" {
			paid = append(paid, c)
		}
	}
	slices.Sort(paid)
	return string(p.Networks), string(p.PaymentMethods), paid
}

func patchProduct(t *testing.T, app *apptest.App, key, id string, body map[string]any) apptest.Response {
	t.Helper()
	return app.Do("PATCH", "/v1/products/"+id, key, body)
}

func mustPatch(t *testing.T, app *apptest.App, key, id string, body map[string]any) {
	t.Helper()
	if r := patchProduct(t, app, key, id, body); r.Status != http.StatusOK {
		t.Fatalf("patch %v: %d %s", body, r.Status, r.Body)
	}
}

func acceptLive(t *testing.T, app *apptest.App, pairs ...string) {
	t.Helper()
	var assets []store.ChainAsset
	for _, p := range pairs {
		c, a, _ := strings.Cut(p, "/")
		assets = append(assets, store.ChainAsset{Chain: c, Asset: a})
	}
	if err := app.Store.SetAcceptedAssets(t.Context(), app.MerchantID, true, assets, "test"); err != nil {
		t.Fatal(err)
	}
}

// errorParam answers an error response's status, code and param.
func errorParam(t *testing.T, r apptest.Response) (int, string, string) {
	t.Helper()
	var body struct {
		Error struct {
			Code  string `json:"code"`
			Param string `json:"param"`
		} `json:"error"`
	}
	if err := json.Unmarshal(r.Body, &body); err != nil {
		t.Fatalf("error body %s: %v", r.Body, err)
	}
	return r.Status, body.Error.Code, body.Error.Param
}

// D60: a product takes all, some or none of the stablecoins of each network; what it offers is the
// registry ∩ the account's accepted assets ∩ that choice, computed on every read.
func TestProductNetworksBehaviour(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	key := app.LiveKey

	// Created without networks: every taken network takes all, so it offers what it did before D60.
	all := newProduct(t, app, key, nil)["id"].(string)
	nets, methods, _ := productState(t, app, key, all)
	if nets != `[{"chain":"base","chain_name":"Base","accept":"all"},{"chain":"solana","chain_name":"Solana","accept":"all"}]` ||
		methods != methodsJSON(methodBaseUSDC, methodSolanaUSDC, methodSolanaUSDT) {
		t.Fatalf("default product: %s %s", nets, methods)
	}

	// Narrowing to some on solana, with an active link: only the stablecoins move, so no links decision.
	some := newProduct(t, app, key, nil)["id"].(string)
	app.PaymentLink(key, some, nil)
	mustPatch(t, app, key, some, map[string]any{"networks": map[string]any{"solana": map[string]any{"accept": "some", "assets": []string{"USDT"}}}})
	nets, methods, paid := productState(t, app, key, some)
	if nets != `[{"chain":"base","chain_name":"Base","accept":"all"},{"chain":"solana","chain_name":"Solana","accept":"some","assets":["USDT"]}]` ||
		methods != methodsJSON(methodBaseUSDC, methodSolanaUSDT) || strings.Join(paid, ",") != "base,solana" {
		t.Fatalf("some USDT: %s %s %v", nets, methods, paid)
	}

	// The account turns USDT off: the some product keeps USDT stored but offers nothing on solana;
	// the all product loses USDT too. Turning it back on restores both.
	acceptLive(t, app, "base/USDC", "solana/USDC")
	if nets2, methods, _ := productState(t, app, key, some); nets2 != nets || methods != methodsJSON(methodBaseUSDC) {
		t.Fatalf("USDT off, some product: %s %s", nets2, methods)
	}
	if _, methods, _ := productState(t, app, key, all); methods != methodsJSON(methodBaseUSDC, methodSolanaUSDC) {
		t.Fatalf("USDT off, all product: %s", methods)
	}
	acceptLive(t, app, "base/USDC", "solana/USDC", "solana/USDT")
	if _, methods, _ := productState(t, app, key, some); methods != methodsJSON(methodBaseUSDC, methodSolanaUSDT) {
		t.Fatalf("USDT on again, some product: %s", methods)
	}

	// all is dynamic, some is not: a stablecoin turned on later joins all, never some.
	acceptLive(t, app, "base/USDC", "solana/USDT")
	usdc := newProduct(t, app, key, nil)["id"].(string)
	mustPatch(t, app, key, usdc, map[string]any{"networks": map[string]any{"solana": map[string]any{"accept": "some", "assets": []string{"USDT"}}}})
	acceptLive(t, app, "base/USDC", "solana/USDC", "solana/USDT")
	if _, methods, _ := productState(t, app, key, all); methods != methodsJSON(methodBaseUSDC, methodSolanaUSDC, methodSolanaUSDT) {
		t.Fatalf("all product after USDC on: %s", methods)
	}
	if _, methods, _ := productState(t, app, key, usdc); methods != methodsJSON(methodBaseUSDC, methodSolanaUSDT) {
		t.Fatalf("some product after USDC on: %s", methods)
	}

	// A session binds and offers exactly the effective set.
	r := app.Session(key, map[string]any{"product_id": some}, idem("net-s1")...)
	if r.Status != http.StatusOK {
		t.Fatalf("session: %d %s", r.Status, r.Body)
	}
	ses, err := app.Store.GetSession(t.Context(), app.MerchantID, true, r.JSON(t)["id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	var offered []string
	usdt, _ := mustChain(t, app, "solana").Asset("USDT")
	for _, p := range ses.PayTo {
		offered = append(offered, p.Chain+"/"+p.Asset)
		if p.Chain == "solana" && (len(p.BoundTokens) != 1 || p.BoundTokens[0] != usdt.Token) {
			t.Fatalf("solana bound tokens %v, want only USDT", p.BoundTokens)
		}
	}
	if strings.Join(offered, ",") != "base/USDC,solana/USDT" {
		t.Fatalf("session offers %v", offered)
	}

	// none drops the network and its payout; the legacy payout [] does the same.
	mustPatch(t, app, key, some, map[string]any{"networks": map[string]any{"base": map[string]any{"accept": "none"}}, "links": "keep"})
	nets, methods, paid = productState(t, app, key, some)
	if nets != `[{"chain":"solana","chain_name":"Solana","accept":"some","assets":["USDT"]}]` || methods != methodsJSON(methodSolanaUSDT) ||
		strings.Join(paid, ",") != "solana" {
		t.Fatalf("base none: %s %s %v", nets, methods, paid)
	}
	mustPatch(t, app, key, all, map[string]any{"payout": map[string]any{"solana": []any{}}})
	if nets, methods, paid := productState(t, app, key, all); nets != `[{"chain":"base","chain_name":"Base","accept":"all"}]` ||
		methods != methodsJSON(methodBaseUSDC) || strings.Join(paid, ",") != "base" {
		t.Fatalf("legacy payout []: %s %s %v", nets, methods, paid)
	}

	// The some list's only stablecoin turned off: nothing is left, so no session opens (as with no
	// accepted asset at all).
	acceptLive(t, app, "base/USDC", "solana/USDC")
	if _, methods, _ := productState(t, app, key, some); methods != "[]" {
		t.Fatalf("nothing left: %s", methods)
	}
	if status, _, msg := errorOf(t, app.Session(key, map[string]any{"product_id": some}, idem("net-s2")...)); status != 400 ||
		!strings.Contains(msg, "no payment methods") {
		t.Fatalf("session with nothing left: %d %s", status, msg)
	}

	// Switching a network back from none to all without a payout pays the family's default wallet.
	mustPatch(t, app, key, some, map[string]any{"networks": map[string]any{"base": map[string]any{"accept": "all"}}, "links": "keep"})
	if nets, methods, paid := productState(t, app, key, some); !strings.Contains(nets, `{"chain":"base","chain_name":"Base","accept":"all"}`) ||
		methods != methodsJSON(methodBaseUSDC) || strings.Join(paid, ",") != "base,solana" {
		t.Fatalf("base back to all: %s %s %v", nets, methods, paid)
	}

	// The audit tells a stablecoin choice apart from a payout change.
	if acts := strings.Join(auditActions(t, app, app.MerchantID), ","); !strings.Contains(acts, "product.networks_changed") {
		t.Fatalf("audit: %s", acts)
	}
}

func mustChain(t *testing.T, app *apptest.App, id string) chain.Chain {
	t.Helper()
	c, ok := app.Registry.Get(id)
	if !ok {
		t.Fatalf("no chain %s", id)
	}
	return c
}

func TestProductNetworksValidation(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	key := app.LiveKey
	id := newProduct(t, app, key, nil)["id"].(string)
	base := liveWallet(t, app, chain.EVM)
	cases := []struct {
		name  string
		body  map[string]any
		code  string
		param string
	}{
		{"unknown network", map[string]any{"networks": map[string]any{"ethereum": map[string]any{"accept": "all"}}}, "unknown_chain", "networks.ethereum"},
		{"network of the other mode", map[string]any{"networks": map[string]any{"base-sepolia": map[string]any{"accept": "all"}}}, "unknown_chain", "networks.base-sepolia"},
		{"unknown accept", map[string]any{"networks": map[string]any{"base": map[string]any{"accept": "maybe"}}}, "invalid_request", "networks.base.accept"},
		{"missing accept", map[string]any{"networks": map[string]any{"base": map[string]any{}}}, "invalid_request", "networks.base.accept"},
		{"unknown field", map[string]any{"networks": map[string]any{"base": map[string]any{"accept": "all", "x": 1}}}, "invalid_request", "networks.base.x"},
		{"not an object", map[string]any{"networks": map[string]any{"base": "all"}}, "invalid_request", "networks.base"},
		{"networks null", map[string]any{"networks": nil}, "invalid_request", "networks"},
		{"some without assets", map[string]any{"networks": map[string]any{"solana": map[string]any{"accept": "some"}}}, "invalid_request", "networks.solana.assets"},
		{"some with no asset", map[string]any{"networks": map[string]any{"solana": map[string]any{"accept": "some", "assets": []string{}}}}, "invalid_request", "networks.solana.assets"},
		{"assets with all", map[string]any{"networks": map[string]any{"solana": map[string]any{"accept": "all", "assets": []string{"USDC"}}}}, "invalid_request", "networks.solana.assets"},
		{"assets with none", map[string]any{"networks": map[string]any{"solana": map[string]any{"accept": "none", "assets": []string{"USDC"}}}}, "invalid_request", "networks.solana.assets"},
		{"asset twice", map[string]any{"networks": map[string]any{"solana": map[string]any{"accept": "some", "assets": []string{"USDT", "USDT"}}}}, "invalid_request", "networks.solana.assets"},
		{"asset not offered there", map[string]any{"networks": map[string]any{"base": map[string]any{"accept": "some", "assets": []string{"USDT"}}}}, "invalid_request", "networks.base.assets"},
		{"assets not a list", map[string]any{"networks": map[string]any{"base": map[string]any{"accept": "some", "assets": "USDC"}}}, "invalid_request", "networks.base.assets"},
		{"none with a payout", map[string]any{"networks": map[string]any{"base": map[string]any{"accept": "none"}},
			"payout": map[string]any{"base": items(base, "100.00")}}, "invalid_request", "payout.base"},
		{"all with payout []", map[string]any{"networks": map[string]any{"base": map[string]any{"accept": "all"}},
			"payout": map[string]any{"base": []any{}}}, "invalid_request", "networks.base.accept"},
		{"nothing left", map[string]any{"networks": map[string]any{"base": map[string]any{"accept": "none"}, "solana": map[string]any{"accept": "none"}}},
			"invalid_request", "payout"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, create := range []bool{true, false} {
				var r apptest.Response
				if create {
					body := map[string]any{"name": "x", "price_type": "fixed", "amount": "5.00"}
					for k, v := range c.body {
						body[k] = v
					}
					r = app.Do("POST", "/v1/products", key, body)
				} else {
					r = patchProduct(t, app, key, id, c.body)
				}
				if status, code, param := errorParam(t, r); status != http.StatusBadRequest || code != c.code || param != c.param {
					t.Errorf("%s (create %v): %d %s %s: %s", c.name, create, status, code, param, r.Body)
				}
			}
		})
	}
	// A stablecoin the account has off in Settings may be chosen: it is stored, offered once on.
	acceptLive(t, app, "base/USDC", "solana/USDC")
	r := app.Do("POST", "/v1/products", key, map[string]any{"name": "x", "price_type": "fixed", "amount": "5.00",
		"networks": map[string]any{"solana": map[string]any{"accept": "some", "assets": []string{"USDT", "USDC"}}, "base": map[string]any{"accept": "none"}}})
	if r.Status != http.StatusOK {
		t.Fatalf("an asset off in Settings: %d %s", r.Status, r.Body)
	}
	created := r.JSON(t)["id"].(string)
	// Stored in the registry's order, whatever the request's.
	if nets, methods, paid := productState(t, app, key, created); nets != `[{"chain":"solana","chain_name":"Solana","accept":"some","assets":["USDC","USDT"]}]` ||
		methods != methodsJSON(methodSolanaUSDC) || strings.Join(paid, ",") != "solana" {
		t.Fatalf("created with an asset off: %s %s %v", nets, methods, paid)
	}
	// assets null is absent.
	mustPatch(t, app, key, created, map[string]any{"networks": map[string]any{"solana": map[string]any{"accept": "all", "assets": nil}}})

	// Taking a network back without a payout needs a wallet of its family.
	only := app.Do("POST", "/v1/products", key, map[string]any{"name": "Base only", "price_type": "fixed", "amount": "5.00",
		"networks": map[string]any{"solana": map[string]any{"accept": "none"}}}).JSON(t)["id"].(string)
	mustPatch(t, app, key, id, map[string]any{"networks": map[string]any{"solana": map[string]any{"accept": "none"}}})
	mustPatch(t, app, key, created, map[string]any{"networks": map[string]any{"solana": map[string]any{"accept": "none"}, "base": map[string]any{"accept": "all"}}})
	app.ArchiveWallet(key, liveWallet(t, app, chain.Solana))
	r = patchProduct(t, app, key, only, map[string]any{"networks": map[string]any{"solana": map[string]any{"accept": "all"}}})
	if status, code, param := errorParam(t, r); status != 400 || code != "invalid_request" || param != "networks.solana" ||
		!strings.Contains(string(r.Body), "Add a Solana wallet") {
		t.Fatalf("no solana wallet: %d %s", r.Status, r.Body)
	}
	r = app.Do("POST", "/v1/products", key, map[string]any{"name": "x", "price_type": "fixed", "amount": "5.00",
		"networks": map[string]any{"solana": map[string]any{"accept": "all"}}})
	if status, _, param := errorParam(t, r); status != 400 || param != "networks.solana" {
		t.Fatalf("create taking a network without a wallet: %d %s", r.Status, r.Body)
	}
}

// Settings lists, per active product, the networks it takes and how, so the page can count the
// products a stablecoin serves. The dashboard product carries networks and payment_methods too.
func TestDashboardProductNetworks(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	key := app.LiveKey
	a := newProduct(t, app, key, nil)["id"].(string)
	mustPatch(t, app, key, a, map[string]any{"networks": map[string]any{"solana": map[string]any{"accept": "some", "assets": []string{"USDT"}}}})
	b := newProduct(t, app, key, nil)["id"].(string)
	mustPatch(t, app, key, b, map[string]any{"active": false})

	st := c.get("/v1/dashboard/settings?mode=live")
	got, _ := json.Marshal(st["product_networks"])
	want := `[{"accept":"all","chain":"base","product_id":"` + a + `","product_name":"Founder"},` +
		`{"accept":"some","assets":["USDT"],"chain":"solana","product_id":"` + a + `","product_name":"Founder"}]`
	if string(got) != want {
		t.Fatalf("settings product_networks:\n got %s\nwant %s", got, want)
	}
	if got := c.get("/v1/dashboard/settings?mode=test")["product_networks"]; got == nil || len(got.([]any)) != 0 {
		t.Fatalf("test mode product_networks: %v", got)
	}

	p := c.get("/v1/dashboard/products/" + a + "?mode=live")
	nets, _ := json.Marshal(p["networks"])
	methods, _ := json.Marshal(p["payment_methods"])
	if string(nets) != `[{"accept":"all","chain":"base","chain_name":"Base"},{"accept":"some","assets":["USDT"],"chain":"solana","chain_name":"Solana"}]` ||
		string(methods) != `[{"asset":"USDC","chain":"base","chain_name":"Base"},{"asset":"USDT","chain":"solana","chain_name":"Solana"}]` {
		t.Fatalf("dashboard product: %s %s", nets, methods)
	}
	// The dashboard's product routes take networks like the API's.
	upd := c.send("PATCH", "/v1/dashboard/products/"+a+"?mode=live", map[string]any{"networks": map[string]any{"solana": map[string]any{"accept": "all"}}})
	if nets, _ := json.Marshal(upd["networks"]); !strings.Contains(string(nets), `{"accept":"all","chain":"solana","chain_name":"Solana"}`) {
		t.Fatalf("dashboard patch: %s", nets)
	}
}

// A customer address keeps the tokens it bound; a product that narrows a network to some is
// treated like the account turning the other stablecoins off: sessions on the address stop
// offering them, and rotating gives an address bound to the narrowed set.
func TestCustomerAddressFollowsProductNetworks(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	key := app.LiveKey
	prod := newProduct(t, app, key, nil)["id"].(string)
	addr := newAddress(t, app, key, map[string]any{"customer_id": "user_1", "product_id": prod}, http.StatusCreated)
	id := addr["id"].(string)
	if got := strings.Join(sortedKeys(addressesOf(t, addr)), ","); got != "base/USDC,solana/USDC,solana/USDT" {
		t.Fatalf("address offers %s", got)
	}

	mustPatch(t, app, key, prod, map[string]any{"networks": map[string]any{"solana": map[string]any{"accept": "some", "assets": []string{"USDT"}}}})
	s := mustOpenOn(t, app, key, id, nil, "addr-net-1")
	if got := strings.Join(sortedKeys(addressesOf(t, s)), ","); got != "base/USDC,solana/USDT" {
		t.Fatalf("session on the address offers %s", got)
	}
	ses, err := app.Store.GetSession(t.Context(), app.MerchantID, true, s["id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ses.PayTo {
		if p.Chain == "solana" && len(p.BoundTokens) != 2 {
			t.Fatalf("the address's bound tokens changed: %v", p.BoundTokens)
		}
	}
	rederive(t, app, ses)

	// The account keeps only solana USDC on, which the product's some list leaves out: nothing of
	// the address is offered any more, and the address says so.
	acceptLive(t, app, "solana/USDC")
	d := c.get("/v1/dashboard/customer_addresses/" + id + "?mode=live")
	if got := strings.Join(attentionReasons(d), ","); !strings.Contains(got, "no_networks") {
		t.Fatalf("attention %s: %v", got, d["attention"])
	}
	// The product takes all of solana again: the address offers its solana USDC again.
	mustPatch(t, app, key, prod, map[string]any{"networks": map[string]any{"solana": map[string]any{"accept": "all"}}})
	d = c.get("/v1/dashboard/customer_addresses/" + id + "?mode=live")
	if got := strings.Join(attentionReasons(d), ","); strings.Contains(got, "no_networks") || strings.Contains(got, "new_networks") {
		t.Fatalf("attention after all again %s: %v", got, d["attention"])
	}
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
