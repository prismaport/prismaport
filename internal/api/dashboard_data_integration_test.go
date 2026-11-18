//go:build integration

package api_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/destination"
)

func sessionIDs(t *testing.T, c *dashClient, path string) []string {
	t.Helper()
	var out []string
	for _, x := range c.get(path)["data"].([]any) {
		out = append(out, x.(map[string]any)["id"].(string))
	}
	return out
}

func TestDashboardSessions(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	l := dashProductLink(t, c, "test", "l1", map[string]any{"name": "Founder", "price_type": "fixed", "amount": "25.00"})
	link, product := l["id"].(string), l["product_id"].(string)
	var opened []map[string]any
	for i := range 3 {
		app.Clock.Advance(time.Second)
		r := openLink(app, link, fmt.Sprintf("s%d", i), nil)
		if r.Status != http.StatusOK {
			t.Fatalf("open: %d %s", r.Status, r.Body)
		}
		opened = append(opened, r.JSON(t))
	}
	app.Clock.Advance(time.Second)
	api := app.Session(app.TestKey, map[string]any{"amount": "7.00"}, idem("direct")...)
	direct := api.JSON(t)["id"].(string)
	tx := "0xdeadbeef" + fmt.Sprintf("%056x", 42)
	payFull(t, app, opened[1], tx)

	all := sessionIDs(t, c, "/v1/dashboard/sessions?mode=test")
	if len(all) != 4 || all[0] != direct || all[3] != opened[0]["id"] {
		t.Fatalf("newest first: %v", all)
	}
	names := map[string]any{}
	products := map[string]any{}
	for _, x := range c.get("/v1/dashboard/sessions?mode=test")["data"].([]any) {
		m := x.(map[string]any)
		v, ok := m["link_name"]
		_, hasID := m["link_id"]
		if !ok || !hasID {
			t.Fatalf("link_name or link_id missing: %v", m)
		}
		names[m["id"].(string)] = v
		products[m["id"].(string)] = m["product_name"]
	}
	// An API session names no link; a link without a label is listed under its product's name.
	if names[direct] != nil || products[direct] != "Test link" || names[opened[0]["id"].(string)] != "Founder" || products[opened[0]["id"].(string)] != "Founder" {
		t.Fatalf("names: %v %v", names, products)
	}
	if got := sessionIDs(t, c, "/v1/dashboard/sessions?mode=test&status=paid"); len(got) != 1 || got[0] != opened[1]["id"] {
		t.Fatalf("status filter: %v", got)
	}
	if got := sessionIDs(t, c, "/v1/dashboard/sessions?mode=test&link_id="+link); len(got) != 3 {
		t.Fatalf("link filter: %v", got)
	}
	if got := sessionIDs(t, c, "/v1/dashboard/sessions?mode=test&product_id="+product); len(got) != 3 {
		t.Fatalf("product filter: %v", got)
	}
	if got := sessionIDs(t, c, "/v1/dashboard/sessions?mode=test&product_id="+app.Link(app.TestKey)); len(got) != 1 || got[0] != direct {
		t.Fatalf("product filter of API sessions: %v", got)
	}
	if got := sessionIDs(t, c, "/v1/dashboard/sessions?mode=test&product_id="+product+"&link_id="+link+"x"); len(got) != 0 {
		t.Fatalf("filters combine: %v", got)
	}
	if got := sessionIDs(t, c, "/v1/dashboard/sessions?mode=test&q="+strings.ToUpper(tx[2:])); len(got) != 0 {
		t.Fatalf("an EVM hash without 0x is not a prefix of the hash: %v", got)
	}
	if got := sessionIDs(t, c, "/v1/dashboard/sessions?mode=test&q="+strings.ToUpper(tx)); len(got) != 1 || got[0] != opened[1]["id"] {
		t.Fatalf("q by tx hash (any case): %v", got)
	}
	revealed := app.CheckoutReveal(opened[2]["id"].(string), "base-sepolia").JSON(t)
	addr := revealed["pay_to"].([]any)[0].(map[string]any)["address"].(string)
	if got := sessionIDs(t, c, "/v1/dashboard/sessions?mode=test&q="+addr); len(got) != 1 || got[0] != opened[2]["id"] {
		t.Fatalf("q by deposit address: %v", got)
	}
	if got := sessionIDs(t, c, "/v1/dashboard/sessions?mode=test&q="+direct); len(got) != 1 || got[0] != direct {
		t.Fatalf("q by id: %v", got)
	}

	// Partial search: the start of a session id (any case) or of a tx hash, exact amounts.
	search := func(q string) []string {
		return sessionIDs(t, c, "/v1/dashboard/sessions?mode=test&q="+url.QueryEscape(q))
	}
	id1 := opened[1]["id"].(string)
	if got := search(strings.ToLower(id1[:len(id1)-1])); len(got) != 1 || got[0] != id1 {
		t.Fatalf("id prefix: %v", got)
	}
	if got := search(id1[:10]); len(got) == 0 || !slices.Contains(got, id1) {
		t.Fatalf("short id prefix: %v", got)
	} else {
		for _, id := range got {
			if !strings.HasPrefix(id, id1[:10]) {
				t.Fatalf("%s does not start with %s", id, id1[:10])
			}
		}
	}
	if got := search("ses_"); len(got) != 4 {
		t.Fatalf("every session starts with ses_: %v", got)
	}
	if got := search("0xDEADBEEF"); len(got) != 1 || got[0] != id1 {
		t.Fatalf("tx hash prefix: %v", got)
	}
	if got := search("0xdeadbee"); len(got) != 0 {
		t.Fatalf("tx hash prefix below the minimum: %v", got)
	}
	if got := search("0xbeef0000"); len(got) != 0 {
		t.Fatalf("a hash prefix matches from the start only: %v", got)
	}
	if got := search("25"); len(got) != 3 {
		t.Fatalf("amount 25: %v", got)
	}
	if got := search("7.00"); len(got) != 1 || got[0] != direct {
		t.Fatalf("amount 7.00: %v", got)
	}
	if got := search("25.000000"); len(got) != 3 {
		t.Fatalf("token amount 25.000000: %v", got)
	}
	for _, q := range []string{"2.5", "25.001", "%", "ses_%", "_", "0x%", "' OR 1=1 --", "25 OR 1=1"} {
		if got := search(q); len(got) != 0 {
			t.Fatalf("q %q: %v", q, got)
		}
	}
	if live := sessionIDs(t, c, "/v1/dashboard/sessions?mode=live&q=0xdeadbeef"); len(live) != 0 {
		t.Fatalf("search crosses modes: %v", live)
	}

	page := c.get("/v1/dashboard/sessions?mode=test&limit=3")
	next, _ := page["next_cursor"].(string)
	if len(page["data"].([]any)) != 3 || next == "" {
		t.Fatalf("page 1: %v", page)
	}
	page = c.get("/v1/dashboard/sessions?mode=test&limit=3&cursor=" + next)
	if len(page["data"].([]any)) != 1 || page["next_cursor"] != nil {
		t.Fatalf("page 2: %v", page)
	}
	if r := c.do("GET", "/v1/dashboard/sessions?mode=test&status=refunded", nil); r.Status != http.StatusBadRequest {
		t.Fatalf("bad status: %d", r.Status)
	}

	d := c.get("/v1/dashboard/sessions/" + opened[1]["id"].(string) + "?mode=test")
	if d["status"] != "paid" || d["product_id"] != product || d["link_id"] != link || d["link_name"] != "Founder" || d["product_name"] != "Founder" ||
		d["amount_received"] != "25.00" || len(d["payments"].([]any)) != 1 {
		t.Fatalf("detail: %v", d)
	}
	if dd := c.get("/v1/dashboard/sessions/" + direct + "?mode=test"); dd["link_name"] != nil || dd["link_id"] != nil || dd["product_name"] != "Test link" {
		t.Fatalf("an API session's names: %v", dd)
	}
	// The names are the current ones: a label replaces the product name, a product rename shows.
	c.send("PATCH", "/v1/dashboard/links/"+link+"?mode=test", map[string]any{"name": "Founder plan"})
	c.send("PATCH", "/v1/dashboard/products/"+product+"?mode=test", map[string]any{"name": "Founder lifetime"})
	if got := c.get("/v1/dashboard/links/" + link + "/sessions?mode=test")["data"].([]any)[0].(map[string]any); got["link_name"] != "Founder plan" ||
		got["product_name"] != "Founder lifetime" {
		t.Fatalf("renamed link: %v", got)
	}
	if got := c.get("/v1/dashboard/sessions/" + opened[1]["id"].(string) + "?mode=test"); got["link_name"] != "Founder plan" || got["product_name"] != "Founder lifetime" {
		t.Fatalf("renamed link in the detail: %v", got)
	}
	var types []string
	for _, e := range d["events"].([]any) {
		types = append(types, e.(map[string]any)["type"].(string))
	}
	if strings.Join(types, ",") != "session.payment_detected,session.paid" {
		t.Fatalf("events: %v", types)
	}
	links := map[string]string{}
	for _, x := range d["explorer_links"].([]any) {
		m := x.(map[string]any)
		links[m["type"].(string)+"/"+m["chain"].(string)] = m["url"].(string)
	}
	if links["payment/base-sepolia"] != "https://sepolia.basescan.org/tx/"+tx ||
		!strings.HasPrefix(links["address/base-sepolia"], "https://sepolia.basescan.org/address/0x") ||
		links["address/solana-devnet"] != "" { // never revealed (D48): no address to link
		t.Fatalf("explorer links: %v", links)
	}
	if r := c.do("GET", "/v1/dashboard/sessions/ses_01JBNOPE?mode=test", nil); r.Status != http.StatusNotFound || r.ErrorCode(t) != "session_not_found" {
		t.Fatalf("unknown session: %d %s", r.Status, r.Body)
	}

	o := c.get("/v1/dashboard/overview?mode=test")
	totals := o["totals"].([]any)
	if len(totals) != 1 {
		t.Fatalf("totals: %v", totals)
	}
	tot := totals[0].(map[string]any)
	if tot["chain"] != "base-sepolia" || tot["asset"] != "USDC" || tot["received"] != "25.000000" || tot["fees"] != "0.000000" || tot["net"] != "0.000000" {
		t.Fatalf("totals before settlement: %v", tot)
	}
	recent := o["recent_payments"].([]any)
	if len(recent) != 1 || recent[0].(map[string]any)["link_name"] != "Founder plan" || recent[0].(map[string]any)["link_id"] != link ||
		recent[0].(map[string]any)["product_id"] != product || recent[0].(map[string]any)["product_name"] != "Founder lifetime" ||
		recent[0].(map[string]any)["amount"] != "25.000000" ||
		recent[0].(map[string]any)["explorer_url"] != "https://sepolia.basescan.org/tx/"+tx {
		t.Fatalf("recent payments: %v", recent)
	}
	if live := c.get("/v1/dashboard/overview?mode=live"); len(live["totals"].([]any)) != 0 || len(live["recent_payments"].([]any)) != 0 {
		t.Fatalf("live overview shows test money: %v", live)
	}
}

func TestDashboardAPIKeys(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	k := c.send("POST", "/v1/dashboard/api_keys", map[string]any{"name": "Backend", "scopes": []string{"sessions:read", "sessions:write"}, "mode": "test"})
	secret := k["secret"].(string)
	if !strings.HasPrefix(secret, "sk_test_") || k["name"] != "Backend" || k["livemode"] != false || !strings.HasPrefix(secret, k["prefix"].(string)) {
		t.Fatalf("created key: %v", k)
	}
	if r := app.Do("GET", "/v1/sessions/ses_01JBNOPE", secret, nil); r.Status != http.StatusNotFound {
		t.Fatalf("new key works: %d %s", r.Status, r.Body)
	}
	if r := app.Do("GET", "/v1/products", secret, nil); r.Status != http.StatusForbidden {
		t.Fatalf("scopes enforced: %d %s", r.Status, r.Body)
	}
	list := c.get("/v1/dashboard/api_keys?mode=test")["data"].([]any)
	found := false
	for _, x := range list {
		m := x.(map[string]any)
		if _, leaked := m["secret"]; leaked {
			t.Fatalf("secret listed: %v", m)
		}
		if m["id"] == k["id"] {
			found = true
		}
	}
	if !found || len(list) != 2 {
		t.Fatalf("list: %v", list)
	}
	for _, bad := range []map[string]any{
		{"name": "", "scopes": []string{"sessions:read"}, "mode": "test"},
		{"name": "x", "scopes": []string{}, "mode": "test"},
		{"name": "x", "scopes": []string{"sessions:delete"}, "mode": "test"},
		{"name": "x", "scopes": []string{"sessions:read"}},
		{"name": "x", "scopes": []string{"sessions:read"}, "mode": "prod"},
	} {
		if r := c.do("POST", "/v1/dashboard/api_keys", bad); r.Status != http.StatusBadRequest {
			t.Errorf("%v: %d %s", bad, r.Status, r.Body)
		}
	}
	if r := c.do("POST", "/v1/dashboard/api_keys?mode=live", map[string]any{"name": "x", "scopes": []string{"sessions:read"}, "mode": "test"}); r.Status != http.StatusBadRequest {
		t.Fatalf("mode disagreement: %d %s", r.Status, r.Body)
	}

	// Live keys need a confirmation code: emailed, or the app's once 2FA is on (D58).
	totpSecret := c.enable2FA()
	if r := c.do("POST", "/v1/dashboard/api_keys", map[string]any{"name": "Live", "scopes": []string{"sessions:read"}, "mode": "live", "password": ownerPassword}); r.Status != http.StatusForbidden || r.ErrorCode(t) != "confirmation_required" {
		t.Fatalf("live key without a code: %d %s", r.Status, r.Body)
	}
	if r := c.do("POST", "/v1/dashboard/api_keys", map[string]any{"name": "Test", "scopes": []string{"sessions:read"}, "mode": "test"}); r.Status != http.StatusOK {
		t.Fatalf("test keys need no step-up: %d %s", r.Status, r.Body)
	}
	app.Clock.Advance(30 * time.Second)
	live := c.send("POST", "/v1/dashboard/api_keys", map[string]any{"name": "Live", "scopes": []string{"sessions:read"}, "mode": "live",
		"totp": totpCode(t, app, totpSecret)})
	if !strings.HasPrefix(live["secret"].(string), "sk_live_") {
		t.Fatalf("live key: %v", live)
	}

	rev := c.send("POST", "/v1/dashboard/api_keys/"+k["id"].(string)+"/revoke?mode=test", nil)
	if rev["revoked_at"] == nil {
		t.Fatalf("revoke: %v", rev)
	}
	if r := app.Do("GET", "/v1/sessions/ses_01JBNOPE", secret, nil); r.Status != http.StatusUnauthorized {
		t.Fatalf("revoked key still works: %d", r.Status)
	}
	if r := c.do("POST", "/v1/dashboard/api_keys/"+live["id"].(string)+"/revoke?mode=test", nil); r.Status != http.StatusNotFound {
		t.Fatalf("live key revoked from test mode: %d %s", r.Status, r.Body)
	}
	if !hasAll(auditActions(t, app, app.MerchantID), "api_key.created", "api_key.created", "api_key.created", "api_key.revoked") {
		t.Fatalf("audit: %v", auditActions(t, app, app.MerchantID))
	}
	recs, err := app.Store.AuditRecords(t.Context(), app.MerchantID)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range recs {
		if a.Action == "api_key.created" && strings.Contains(string(a.Details), `"Live"`) && !strings.HasPrefix(a.Actor, "dashboard:usr_") {
			t.Fatalf("dashboard actions name the user: %+v", a)
		}
	}
}

func TestDashboardWebhooks(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	consumer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }))
	t.Cleanup(consumer.Close)
	e := c.send("POST", "/v1/dashboard/webhook_endpoints?mode=test", map[string]any{"url": consumer.URL, "enabled_events": []string{"session.paid"}})
	eid := e["id"].(string)
	if !strings.HasPrefix(e["secret"].(string), "whsec_") || e["livemode"] != false {
		t.Fatalf("endpoint: %v", e)
	}
	if list := c.get("/v1/dashboard/webhook_endpoints?mode=test")["data"].([]any); len(list) != 1 || list[0].(map[string]any)["secret"] != nil {
		t.Fatalf("list: %v", list)
	}
	if list := c.get("/v1/dashboard/webhook_endpoints?mode=live")["data"].([]any); len(list) != 0 {
		t.Fatalf("live list: %v", list)
	}
	rot := c.send("POST", "/v1/dashboard/webhook_endpoints/"+eid+"/rotate_secret?mode=test", nil)
	if rot["secret"] == e["secret"] {
		t.Fatal("rotate kept the secret")
	}

	link := dashProductLink(t, c, "test", "l", map[string]any{"name": "Founder", "price_type": "fixed", "amount": "25.00"})["id"].(string)
	ses := openLink(app, link, "s", nil).JSON(t)
	payFull(t, app, ses, "0x"+fmt.Sprintf("%064x", 5))
	if _, err := app.Store.SequenceEvents(t.Context(), 100); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Dispatcher.RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	dl := c.get("/v1/dashboard/webhook_endpoints/" + eid + "/deliveries?mode=test")["data"].([]any)
	if len(dl) != 1 {
		t.Fatalf("deliveries: %v", dl)
	}
	d := dl[0].(map[string]any)
	if d["event_type"] != "session.paid" || d["session_id"] != ses["id"] || d["attempts"] != float64(1) || d["last_status_code"] != float64(500) ||
		d["status"] != "pending" {
		t.Fatalf("delivery: %v", d)
	}
	if _, err := app.Store.Pool().Exec(t.Context(), `UPDATE webhook_deliveries SET status = 'dead', attempts = 12`); err != nil {
		t.Fatal(err)
	}
	re := c.send("POST", "/v1/dashboard/webhook_deliveries/"+d["id"].(string)+"/resend?mode=test", nil)
	if re["status"] != "pending" || re["attempts"] != float64(0) || re["next_attempt_at"] != app.Clock.Now().Format(time.RFC3339) {
		t.Fatalf("resend: %v", re)
	}
	if r := c.do("POST", "/v1/dashboard/webhook_deliveries/"+d["id"].(string)+"/resend?mode=live", nil); r.Status != http.StatusNotFound {
		t.Fatalf("resend from the other mode: %d %s", r.Status, r.Body)
	}
	c.send("DELETE", "/v1/dashboard/webhook_endpoints/"+eid+"?mode=test", nil)
	if r := c.do("POST", "/v1/dashboard/webhook_deliveries/"+d["id"].(string)+"/resend?mode=test", nil); r.Status != http.StatusConflict {
		t.Fatalf("resend to a deleted endpoint: %d %s", r.Status, r.Body)
	}
	// A deleted endpoint's log stays readable.
	if dl := c.get("/v1/dashboard/webhook_endpoints/" + eid + "/deliveries?mode=test")["data"].([]any); len(dl) != 1 {
		t.Fatalf("log after delete: %v", dl)
	}
	if r := c.do("POST", "/v1/dashboard/webhook_endpoints?mode=test", map[string]any{"url": consumer.URL, "enabled_events": []string{"session.refunded"}}); r.Status != http.StatusBadRequest {
		t.Fatalf("unknown event: %d %s", r.Status, r.Body)
	}
	if !hasAll(auditActions(t, app, app.MerchantID), "webhook_endpoint.created", "webhook_endpoint.secret_rotated", "webhook_delivery.resent", "webhook_endpoint.deleted") {
		t.Fatalf("audit: %v", auditActions(t, app, app.MerchantID))
	}
}

func TestDashboardSettings(t *testing.T) {
	t.Parallel()
	app, _ := dashAppWith(t, apptest.Options{SolanaInspector: missingATA{}}, nil)
	c := login(t, app, "owner@example.com")
	st := c.get("/v1/dashboard/settings?mode=test")
	if len(st) != 3 || st["destinations"] != nil || st["default_splits"] != nil || len(st["product_networks"].([]any)) != 0 {
		t.Fatalf("settings carry only accepted_assets, fee and product_networks: %v", st)
	}
	assets := map[string]bool{}
	for _, x := range st["accepted_assets"].([]any) {
		m := x.(map[string]any)
		assets[m["chain"].(string)+"/"+m["asset"].(string)] = m["enabled"].(bool)
	}
	if len(assets) != 2 || !assets["base-sepolia/USDC"] || !assets["solana-devnet/USDC"] {
		t.Fatalf("test assets: %v", assets)
	}
	live := map[string]bool{}
	for _, x := range c.get("/v1/dashboard/settings?mode=live")["accepted_assets"].([]any) {
		m := x.(map[string]any)
		live[m["chain"].(string)+"/"+m["asset"].(string)] = m["enabled"].(bool)
	}
	if len(live) != 3 || !live["base/USDC"] || !live["solana/USDC"] || !live["solana/USDT"] {
		t.Fatalf("live assets: %v", live)
	}
	if fee := st["fee"].(map[string]any); fee["fee_bps"] != float64(50) || fee["fixed_fee"] != "0.10" {
		t.Fatalf("fee: %v", fee)
	}

	// Adding a wallet changes what no link pays: a link made before keeps paying its wallet.
	newEVM := "0x1111111111111111111111111111111111111111"
	add := func(family, addr string, body map[string]any) apptest.Response {
		if body == nil {
			body = map[string]any{}
		}
		body["family"], body["address"] = family, addr
		return c.do("POST", "/v1/dashboard/wallets?mode=test", body)
	}
	link := app.Link(app.TestKey)
	// A Live wallet takes a confirmation code; Test mode asks for none (D58).
	addLive := func(addr string, body map[string]any) apptest.Response {
		body["family"], body["address"] = "evm", addr
		return c.do("POST", "/v1/dashboard/wallets?mode=live", body)
	}
	if r := addLive(newEVM, map[string]any{"password": ownerPassword}); r.Status != http.StatusForbidden || r.ErrorCode(t) != "confirmation_required" {
		t.Fatalf("no code: %d %s", r.Status, r.Body)
	}
	if r := addLive(newEVM, map[string]any{"email_code": "000000"}); r.Status != http.StatusForbidden || r.ErrorCode(t) != "invalid_code" {
		t.Fatalf("wrong code: %d %s", r.Status, r.Body)
	}
	for _, bad := range []string{"0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913", "0x0000000000000000000000000000000000000000",
		"0x5AAEB6053F3E94C9B9A09F33669435E7EF1BEAEd", apptest.EVMTreasury, "not an address"} {
		if r := add("evm", bad, nil); r.Status != http.StatusBadRequest || r.ErrorCode(t) != "invalid_request" {
			t.Errorf("%s: %d %s", bad, r.Status, r.Body)
		}
	}
	// Live Solana wallets must hold a token account for every accepted mint; in Test mode such a
	// wallet is kept, not checked yet.
	if r := c.do("POST", "/v1/dashboard/wallets?mode=live", map[string]any{"family": "solana", "address": "DEb5yphxEaPc5BN118svVN4R3GFu9jKs31Gcv5yekjZx"}); r.Status != http.StatusBadRequest || !strings.Contains(string(r.Body), "token account") {
		t.Fatalf("missing ATA in live mode: %d %s", r.Status, r.Body)
	}
	if r := add("solana", "DEb5yphxEaPc5BN118svVN4R3GFu9jKs31Gcv5yekjZx", nil); r.Status != http.StatusOK ||
		r.JSON(t)["verified"] != false {
		t.Fatalf("missing ATA in test mode: %d %s", r.Status, r.Body)
	}
	if d := add("evm", newEVM, nil); d.Status != http.StatusOK || d.JSON(t)["verified"] != true {
		t.Fatalf("add: %d %s", d.Status, d.Body)
	}
	ses := app.Session(app.TestKey, map[string]any{"product_id": link, "amount": "5.00"}, idem("after-change")...)
	if ses.Status != http.StatusOK {
		t.Fatalf("session: %d %s", ses.Status, ses.Body)
	}
	stored, err := app.Store.LoadSession(t.Context(), ses.JSON(t)["id"].(string))
	if err != nil || len(stored.PayTo[0].Recipients) != 1 || stored.PayTo[0].Recipients[0].Address != apptest.EVMDestination {
		t.Fatalf("a link changed with a new wallet: %+v %v", stored, err)
	}

	// With 2FA on, a refused address does not spend the code; a valid Live wallet needs one.
	secret := c.enable2FA()
	app.Clock.Advance(30 * time.Second)
	code := totpCode(t, app, secret)
	if r := addLive("0x0000000000000000000000000000000000000000", map[string]any{"totp": code}); r.Status != http.StatusBadRequest {
		t.Fatalf("bad address with 2FA: %d %s", r.Status, r.Body)
	}
	if r := addLive("0x2222222222222222222222222222222222222222", map[string]any{}); r.Status != http.StatusForbidden || r.ErrorCode(t) != "confirmation_required" {
		t.Fatalf("2FA on, no code: %d %s", r.Status, r.Body)
	}
	if r := addLive("0x2222222222222222222222222222222222222222", map[string]any{"totp": code}); r.Status != http.StatusOK {
		t.Fatalf("2FA wallet: %d %s", r.Status, r.Body)
	}

	a := c.send("PUT", "/v1/dashboard/accepted_assets?mode=test", map[string]any{"items": []map[string]any{{"chain": "solana-devnet", "asset": "USDC", "enabled": false}}})
	got := map[string]bool{}
	for _, x := range a["accepted_assets"].([]any) {
		m := x.(map[string]any)
		got[m["chain"].(string)+"/"+m["asset"].(string)] = m["enabled"].(bool)
	}
	if !got["base-sepolia/USDC"] || got["solana-devnet/USDC"] {
		t.Fatalf("after disabling Solana: %v", got)
	}
	r := app.Session(app.TestKey, map[string]any{"amount": "5.00"}, idem("after-assets")...)
	if pt := r.JSON(t)["pay_to"].([]any); len(pt) != 1 || pt[0].(map[string]any)["chain"] != "base-sepolia" {
		t.Fatalf("pay_to after disabling Solana: %v", pt)
	}
	for _, bad := range []any{
		[]map[string]any{{"chain": "base-sepolia", "asset": "USDC", "enabled": false}},
		[]map[string]any{{"chain": "base-sepolia", "asset": "USDT", "enabled": true}},
		[]map[string]any{{"chain": "base", "asset": "USDC", "enabled": true}},
		[]map[string]any{{"chain": "arbitrum", "asset": "USDC", "enabled": true}},
		[]map[string]any{{"chain": "base-sepolia", "asset": "USDC", "enabled": true}, {"chain": "base-sepolia", "asset": "USDC", "enabled": true}},
		[]map[string]any{{"chain": "base-sepolia", "asset": "USDC"}},
		[]map[string]any{},
	} {
		if r := c.do("PUT", "/v1/dashboard/accepted_assets?mode=test", map[string]any{"items": bad}); r.Status != http.StatusBadRequest {
			t.Errorf("%v: %d %s", bad, r.Status, r.Body)
		}
	}
	if !hasAll(auditActions(t, app, app.MerchantID), "wallet.created", "accepted_assets.set", "user.step_up_failed") {
		t.Fatalf("audit: %v", auditActions(t, app, app.MerchantID))
	}
}

// missingATA reports every Solana wallet but the seeded one without a USDT token account.
type missingATA struct{}

func (missingATA) Inspect(context.Context, bool, string) (destination.SolanaAccountKind, error) {
	return destination.SolanaWallet, nil
}

func (missingATA) MissingTokenAccounts(_ context.Context, _ bool, w string) ([]destination.MissingTokenAccount, error) {
	if w == apptest.SolanaDestination {
		return nil, nil
	}
	return []destination.MissingTokenAccount{{Chain: "solana", Asset: "USDT", Mint: "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB", Account: "x"}}, nil
}
