//go:build integration

package api_test

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/chain"
)

func attentionReasons(ca map[string]any) []string {
	var out []string
	for _, x := range ca["attention"].([]any) {
		out = append(out, x.(map[string]any)["reason"].(string))
	}
	return out
}

// The dashboard's Customers pages: list, detail, sessions and Rotate of customer addresses, with
// the product, stats and what a rotation would change.
func TestDashboardCustomerAddresses(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	key := app.TestKey
	evmTest := ""
	for _, ch := range app.Registry.ForMode(false) {
		if ch.Family == chain.EVM {
			evmTest = ch.ID
		}
	}
	w1 := walletID(t, app, key, apptest.EVMDestination)
	prod := newProduct(t, app, key, nil)["id"].(string)
	id := newAddress(t, app, key, map[string]any{"customer_id": "user_1", "product_id": prod}, http.StatusCreated)["id"].(string)
	newAddress(t, app, key, map[string]any{"customer_id": "user_2", "product_id": prod}, http.StatusCreated)

	list := c.get("/v1/dashboard/customer_addresses?mode=test")
	data := list["data"].([]any)
	if len(data) != 2 || list["next_cursor"] != nil {
		t.Fatalf("list %v", list)
	}
	var first map[string]any
	for _, x := range data {
		if x.(map[string]any)["id"] == id {
			first = x.(map[string]any)
		}
	}
	if first["id"] != id || first["customer_id"] != "user_1" || first["status"] != "active" ||
		first["product"].(map[string]any)["name"] != "Founder" || len(attentionReasons(first)) != 0 ||
		first["stats"].(map[string]any)["sessions"].(float64) != 0 || first["stats"].(map[string]any)["last_session"] != nil {
		t.Fatalf("fresh address %v", first)
	}
	if got := c.get("/v1/dashboard/customer_addresses?mode=test&customer_id=user_1")["data"].([]any); len(got) != 1 {
		t.Fatalf("filtered by customer %v", got)
	}
	if got := c.get("/v1/dashboard/customer_addresses?mode=test&limit=1"); len(got["data"].([]any)) != 1 || got["next_cursor"] == nil {
		t.Fatalf("page of 1 %v", got)
	}
	if r := c.do("GET", "/v1/dashboard/customer_addresses?mode=test&customer_id=bad%20id", nil); r.Status != http.StatusBadRequest {
		t.Fatalf("bad customer_id: %d %s", r.Status, r.Body)
	}
	if got := c.get("/v1/dashboard/customer_addresses?mode=live")["data"].([]any); len(got) != 0 {
		t.Fatalf("live list %v", got)
	}

	// A paid session, then a second one: the first is superseded.
	s1 := mustOpenOn(t, app, key, id, nil, "dash-ca-1")["id"].(string)
	simulate(t, app, s1, map[string]any{"action": "pay_exact"})
	app.Clock.Advance(time.Minute)
	s2 := mustOpenOn(t, app, key, id, nil, "dash-ca-2")["id"].(string)
	d := c.get("/v1/dashboard/customer_addresses/" + id + "?mode=test")
	st := d["stats"].(map[string]any)
	if st["sessions"].(float64) != 2 || st["paid"].(float64) != 1 || fmt.Sprint(st["received"]) != "[map[amount:25.000000 asset:USDC]]" ||
		st["last_paid_at"] == nil || st["last_session"].(map[string]any)["id"] != s2 || st["last_session"].(map[string]any)["status"] != "open" {
		t.Fatalf("stats %v", st)
	}
	if d["url"] != apptest.CheckoutBase+"/a/"+id {
		t.Fatalf("url %v", d["url"])
	}
	ses := c.get("/v1/dashboard/customer_addresses/" + id + "/sessions?mode=test")["data"].([]any)
	if len(ses) != 2 || ses[0].(map[string]any)["id"] != s2 || ses[1].(map[string]any)["id"] != s1 ||
		ses[0].(map[string]any)["product_name"] != "Founder" || ses[0].(map[string]any)["customer_address_id"] != id {
		t.Fatalf("address sessions %v", ses)
	}
	if got := c.get("/v1/dashboard/sessions?mode=test&customer_address_id=" + id)["data"].([]any); len(got) != 2 {
		t.Fatalf("payments filtered by address %v", got)
	}
	old := c.get("/v1/dashboard/sessions/" + s1 + "?mode=test")
	if old["superseded_by"] != s2 || old["recheck_available"] != false || fmt.Sprint(old["settled_with"]) != "[]" {
		t.Fatalf("superseded session %v %v %v", old["superseded_by"], old["recheck_available"], old["settled_with"])
	}
	if cur := c.get("/v1/dashboard/sessions/" + s2 + "?mode=test"); cur["superseded_by"] != nil {
		t.Fatalf("current session superseded_by %v", cur["superseded_by"])
	}
	// A flush of s2 that found the forwarder emptied by s1's flush (outcome covered, as the
	// senders write it): s2's page names the session and transaction that settled it.
	if _, err := app.Store.Pool().Exec(t.Context(), `INSERT INTO chain_tx_jobs (id, chain, kind, ref, session_id, asset, address,
		planned_total, to_address, calldata, sender, details, status, outcome, next_attempt_at, created_at, updated_at)
		VALUES ('job_covered', $1, 'flush', 'covered-ref', $2, 'USDC', '0xabc', 1, '0xabc', '\x00'::bytea, '0xsender',
		jsonb_build_object('covered_by_tx', '0xfeed', 'covered_by_session', $3::text), 'done', 'covered', now(), now(), now())`,
		evmTest, s2, s1); err != nil {
		t.Fatal(err)
	}
	if cur := c.get("/v1/dashboard/sessions/" + s2 + "?mode=test"); fmt.Sprint(cur["settled_with"]) !=
		fmt.Sprintf("[map[asset:USDC chain:%s session_id:%s tx_hash:0xfeed]]", evmTest, s1) {
		t.Fatalf("settled_with %v", cur["settled_with"])
	}

	// The product pays another wallet now and the old one is archived.
	w2 := app.Wallet(key, chain.EVM, "Second", "0x2546BcD3c84621e976D8185a91A922aE77ECEc30").ID
	if r := app.Do("PATCH", "/v1/products/"+prod, key, map[string]any{"payout": map[string]any{evmTest: items(w2, "100")}}); r.Status != http.StatusOK {
		t.Fatalf("payout change: %d %s", r.Status, r.Body)
	}
	app.ArchiveWallet(key, w1)
	d = c.get("/v1/dashboard/customer_addresses/" + id + "?mode=test")
	if got := fmt.Sprint(attentionReasons(d)); got != "[archived_wallet payout_changed]" {
		t.Fatalf("attention %s: %v", got, d["attention"])
	}
	if w := d["attention"].([]any)[0].(map[string]any)["wallets"].([]any)[0].(map[string]any); w["id"] != w1 {
		t.Fatalf("archived wallet %v", w)
	}

	// Rotate: CSRF like every change, then the new address, up to date.
	if r := newClient(t, app).do("POST", "/v1/dashboard/customer_addresses/"+id+"/rotate?mode=test", nil); r.Status != http.StatusUnauthorized {
		t.Fatalf("rotate signed out: %d %s", r.Status, r.Body)
	}
	app.Clock.Advance(2 * time.Hour)
	b := c.send("POST", "/v1/dashboard/customer_addresses/"+id+"/rotate?mode=test", nil)
	bid := b["id"].(string)
	if bid == id || b["status"] != "active" || b["customer_id"] != "user_1" || len(attentionReasons(b)) != 0 ||
		b["stats"].(map[string]any)["sessions"].(float64) != 0 {
		t.Fatalf("rotated to %v", b)
	}
	d = c.get("/v1/dashboard/customer_addresses/" + id + "?mode=test")
	if d["status"] != "rotated" || d["replaced_by"] != bid || len(attentionReasons(d)) != 0 {
		t.Fatalf("old address %v", d)
	}
	if r := c.do("POST", "/v1/dashboard/customer_addresses/"+id+"/rotate?mode=test", nil); r.Status != http.StatusConflict || r.ErrorCode(t) != "address_rotated" {
		t.Fatalf("rotate twice: %d %s", r.Status, r.Body)
	}
	if r := c.do("GET", "/v1/dashboard/customer_addresses/caddr_01JBNOPE?mode=test", nil); r.Status != http.StatusNotFound || r.ErrorCode(t) != "customer_address_not_found" {
		t.Fatalf("unknown: %d %s", r.Status, r.Body)
	}
	if r := c.do("GET", "/v1/dashboard/customer_addresses/"+id+"/sessions?mode=live", nil); r.Status != http.StatusNotFound {
		t.Fatalf("other mode: %d %s", r.Status, r.Body)
	}

	// Another merchant sees none of it.
	secondMerchant(t, app, "other@example.com")
	other := login(t, app, "other@example.com")
	if r := other.do("GET", "/v1/dashboard/customer_addresses/"+id+"?mode=test", nil); r.Status != http.StatusNotFound {
		t.Fatalf("other merchant detail: %d %s", r.Status, r.Body)
	}
	if r := other.do("POST", "/v1/dashboard/customer_addresses/"+bid+"/rotate?mode=test", nil); r.Status != http.StatusNotFound {
		t.Fatalf("other merchant rotate: %d %s", r.Status, r.Body)
	}
	if got := other.get("/v1/dashboard/customer_addresses?mode=test")["data"].([]any); len(got) != 0 {
		t.Fatalf("other merchant list %v", got)
	}
}
