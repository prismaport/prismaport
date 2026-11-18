//go:build integration

package api_test

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/checkout"
	"github.com/prismaport/prismaport/internal/service"
	"github.com/prismaport/prismaport/internal/store"
)

func checkoutApp(t *testing.T) *apptest.App {
	t.Helper()
	return apptest.New(t, apptest.Options{Checkout: func(svc *service.Service, st *store.Store) *checkout.Checkout { return checkout.New(svc, st, nil) }})
}

func publicAddress(t *testing.T, app *apptest.App, id, query string) map[string]any {
	t.Helper()
	r := app.Do("GET", "/v1/checkout/addresses/"+id+query, "", nil)
	if r.Status != http.StatusOK {
		t.Fatalf("public address %s: %d %s", id, r.Status, r.Body)
	}
	return r.JSON(t)
}

func openPublic(t *testing.T, app *apptest.App, id string, body any) apptest.Response {
	t.Helper()
	return app.Do("POST", "/v1/checkout/addresses/"+id+"/sessions", "", body)
}

func mustOpenPublic(t *testing.T, app *apptest.App, id string) map[string]any {
	t.Helper()
	r := openPublic(t, app, id, map[string]any{})
	if r.Status != http.StatusOK {
		t.Fatalf("open on %s: %d %s", id, r.Status, r.Body)
	}
	return r.JSON(t)
}

// An open price: the page shows the fixed address and opens an open-amount session, which the
// public contract presents as "any amount" (null amounts, no signed or wallet path), and the
// button resumes that session until it is paid or expires.
func TestCheckoutAddressOpenAmount(t *testing.T) {
	t.Parallel()
	app := checkoutApp(t)
	a := newAddress(t, app, app.TestKey, map[string]any{"customer_id": "user_8412", "product_id": app.Link(app.TestKey),
		"metadata": map[string]any{"tier": "gold"}}, http.StatusCreated)
	id := a["id"].(string)

	p := publicAddress(t, app, id, "")
	if p["id"] != id || p["status"] != "active" || p["live"] != true || p["unavailable_reason"] != nil || p["replaced_by"] != nil ||
		p["price_type"] != "open" || p["amount"] != nil || p["open_amount"] != true || p["session"] != nil ||
		p["product"].(map[string]any)["name"] != "Test link" {
		t.Fatalf("public address %v", p)
	}
	for _, private := range []string{"customer_id", "metadata", "payout", "product_id"} {
		if _, ok := p[private]; ok {
			t.Fatalf("public address shows %s: %v", private, p)
		}
	}
	if got, want := addressesOf(t, p), addressesOf(t, a); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("public addresses %v, merchant view %v", got, want)
	}

	s := mustOpenPublic(t, app, id)
	sid := s["id"].(string)
	if s["status"] != "open" || s["amount"] != nil || s["open_amount"] != true || s["amount_remaining"] != nil ||
		s["amount_received"] != "0.00" || s["customer_address_id"] != id || s["link_id"] != nil {
		t.Fatalf("open-amount public session %v", s)
	}
	for _, x := range s["pay_to"].([]any) {
		pt := x.(map[string]any)
		if _, ok := pt["amount"]; ok || pt["signed_payment"] != false || pt["connected_wallet"] != nil || pt["address"] == nil {
			t.Fatalf("open-amount pay_to %v", pt)
		}
	}
	if got, want := addressesOf(t, s), addressesOf(t, a); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("session addresses %v, customer address %v", got, want)
	}
	// The merchant's view of the same session: opened on the address, with its metadata.
	m := app.Do("GET", "/v1/sessions/"+sid, app.TestKey, nil).JSON(t)
	if m["customer_address_id"] != id || m["amount"] != nil || m["metadata"].(map[string]any)["tier"] != "gold" {
		t.Fatalf("merchant view %v", m)
	}
	// The session page reads the same shape.
	if v := app.Do("GET", "/v1/checkout/sessions/"+sid, "", nil).JSON(t); v["open_amount"] != true || v["amount"] != nil {
		t.Fatalf("checkout session %v", v)
	}

	// A second click, with or without a body, resumes it.
	for _, body := range []any{nil, map[string]any{}} {
		r := openPublic(t, app, id, body)
		if r.Status != http.StatusOK || r.JSON(t)["id"] != sid {
			t.Fatalf("resume (%v): %d %s", body, r.Status, r.Body)
		}
	}
	if r := openPublic(t, app, id, map[string]any{"amount": "10.00"}); r.Status != http.StatusBadRequest || r.ErrorCode(t) != "invalid_request" {
		t.Fatalf("a body field: %d %s", r.Status, r.Body)
	}

	// The page names the session it resumes, and ?active=1 records presence on it.
	p = publicAddress(t, app, id, "?active=1")
	ses, _ := p["session"].(map[string]any)
	if ses == nil || ses["id"] != sid || ses["status"] != "open" || ses["expires_at"] != s["expires_at"] {
		t.Fatalf("resumable session %v", p["session"])
	}
	var seen *time.Time
	if err := app.Store.Pool().QueryRow(t.Context(), `SELECT checkout_seen_at FROM sessions WHERE id = $1`, sid).Scan(&seen); err != nil || seen == nil {
		t.Fatalf("presence not recorded: %v %v", seen, err)
	}

	// Paid, the session is no longer resumed from the page (D50).
	if r := app.Do("POST", "/v1/test_helpers/sessions/"+sid+"/simulate", app.TestKey, map[string]any{"action": "pay_exact", "amount": "40"}); r.Status != http.StatusOK {
		t.Fatalf("pay: %d %s", r.Status, r.Body)
	}
	if p := publicAddress(t, app, id, ""); p["session"] != nil || p["live"] != true {
		t.Fatalf("after payment %v", p)
	}
	paid := app.Do("GET", "/v1/checkout/sessions/"+sid, "", nil).JSON(t)
	if paid["status"] != "paid" || paid["amount_received"] != "40.00" || paid["amount"] != nil || paid["amount_remaining"] != nil {
		t.Fatalf("paid open-amount session %v", paid)
	}

	// Expired, the next click opens a new session on the same addresses.
	app.Clock.Advance(2 * time.Hour)
	if p := publicAddress(t, app, id, ""); p["session"] != nil {
		t.Fatalf("expired session still resumable: %v", p["session"])
	}
	s2 := mustOpenPublic(t, app, id)
	if s2["id"] == sid || s2["open_amount"] != true || fmt.Sprint(addressesOf(t, s2)) != fmt.Sprint(addressesOf(t, a)) {
		t.Fatalf("second session %v", s2)
	}
}

// A fixed price: the page and the session carry the product's amount, as on any session.
func TestCheckoutAddressFixedAmount(t *testing.T) {
	t.Parallel()
	app := checkoutApp(t)
	prod := createProduct(t, app, app.TestKey, map[string]any{"name": "Monthly plan", "price_type": "fixed", "amount": "25.00"})
	id := newAddress(t, app, app.TestKey, map[string]any{"customer_id": "user_1", "product_id": prod}, http.StatusCreated)["id"].(string)

	p := publicAddress(t, app, id, "")
	if p["price_type"] != "fixed" || p["amount"] != "25.00" || p["open_amount"] != false || p["live"] != true {
		t.Fatalf("fixed address %v", p)
	}
	s := mustOpenPublic(t, app, id)
	if s["amount"] != "25.00" || s["open_amount"] != false || s["amount_remaining"] != "25.00" || s["customer_address_id"] != id {
		t.Fatalf("fixed session %v", s)
	}
	for _, x := range s["pay_to"].([]any) {
		if pt := x.(map[string]any); pt["amount"] != "25.000000" || pt["signed_payment"] != false || pt["connected_wallet"] != nil {
			t.Fatalf("fixed pay_to %v", pt)
		}
	}
	// Sessions on one address share its Gateway nonce and Solana reference: no wallet path, even
	// at a fixed price, and the refusal comes before any chain is asked.
	sid := s["id"].(string)
	typed := app.Do("POST", "/v1/checkout/sessions/"+sid+"/eip3009/typed_data", "", map[string]string{"chain": "base-sepolia", "asset": "USDC",
		"from": "0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed"})
	if typed.Status != http.StatusConflict || typed.ErrorCode(t) != "session_not_payable" {
		t.Fatalf("typed data on a customer address session: %d %s", typed.Status, typed.Body)
	}
	wallet := app.Do("POST", "/v1/checkout/sessions/"+sid+"/solana/transaction", "", map[string]string{"chain": "solana-devnet", "asset": "USDC",
		"account": "9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM"})
	if wallet.Status != http.StatusConflict || wallet.ErrorCode(t) != "session_not_payable" {
		t.Fatalf("wallet transaction on a customer address session: %d %s", wallet.Status, wallet.Body)
	}
}

// A rotated address says so, names its replacement and shows no deposit address; no session
// opens on it, while the one already open runs to its end and is still named.
func TestCheckoutAddressRotated(t *testing.T) {
	t.Parallel()
	app := checkoutApp(t)
	id := newAddress(t, app, app.TestKey, map[string]any{"customer_id": "user_1", "product_id": app.Link(app.TestKey)}, http.StatusCreated)["id"].(string)
	s := mustOpenPublic(t, app, id)
	r := app.Do("POST", "/v1/customer_addresses/"+id+"/rotate", app.TestKey, nil)
	if r.Status != http.StatusOK {
		t.Fatalf("rotate: %d %s", r.Status, r.Body)
	}
	next := r.JSON(t)["id"].(string)

	p := publicAddress(t, app, id, "")
	if p["status"] != "rotated" || p["replaced_by"] != next || p["live"] != false || p["unavailable_reason"] != "rotated" ||
		len(p["pay_to"].([]any)) != 0 {
		t.Fatalf("rotated address %v", p)
	}
	if ses, _ := p["session"].(map[string]any); ses == nil || ses["id"] != s["id"] {
		t.Fatalf("the open session of a rotated address: %v", p["session"])
	}
	r = openPublic(t, app, id, map[string]any{})
	if r.Status != http.StatusConflict || r.ErrorCode(t) != "address_rotated" {
		t.Fatalf("open on a rotated address: %d %s", r.Status, r.Body)
	}
	if _, ok := r.JSON(t)["error"].(map[string]any)["param"]; ok {
		t.Fatalf("a public refusal names a request field: %s", r.Body)
	}
	if n := publicAddress(t, app, next, ""); n["live"] != true || n["status"] != "active" || n["session"] != nil {
		t.Fatalf("replacement %v", n)
	}
	if s2 := mustOpenPublic(t, app, next); s2["customer_address_id"] != next {
		t.Fatalf("session on the replacement %v", s2)
	}
}

// A deactivated product: the page cannot open a session, but the open one is still resumed.
func TestCheckoutAddressProductInactive(t *testing.T) {
	t.Parallel()
	app := checkoutApp(t)
	prod := createProduct(t, app, app.TestKey, map[string]any{"name": "Monthly plan", "price_type": "fixed", "amount": "25.00"})
	id := newAddress(t, app, app.TestKey, map[string]any{"customer_id": "user_1", "product_id": prod}, http.StatusCreated)["id"].(string)
	s := mustOpenPublic(t, app, id)
	if r := app.Do("PATCH", "/v1/products/"+prod, app.TestKey, map[string]any{"active": false}); r.Status != http.StatusOK {
		t.Fatalf("deactivate: %d %s", r.Status, r.Body)
	}
	p := publicAddress(t, app, id, "")
	if p["live"] != false || p["unavailable_reason"] != "product_inactive" || len(p["pay_to"].([]any)) != 0 || p["session"] == nil {
		t.Fatalf("inactive product %v", p)
	}
	if again := mustOpenPublic(t, app, id); again["id"] != s["id"] {
		t.Fatalf("resume under an inactive product: %v", again["id"])
	}
	app.Clock.Advance(2 * time.Hour)
	if r := openPublic(t, app, id, map[string]any{}); r.Status != http.StatusBadRequest || r.ErrorCode(t) != "product_inactive" {
		t.Fatalf("new session under an inactive product: %d %s", r.Status, r.Body)
	}
}

func TestCheckoutAddressNotFound(t *testing.T) {
	t.Parallel()
	app := checkoutApp(t)
	for _, id := range []string{"caddr_01M5N0Q3W8T1V6R9JX2KDHB4FC", "ses_01M5N0Q3W8T1V6R9JX2KDHB4FC", "plink_x"} {
		if r := app.Do("GET", "/v1/checkout/addresses/"+id, "", nil); r.Status != http.StatusNotFound || r.ErrorCode(t) != "customer_address_not_found" {
			t.Fatalf("GET %s: %d %s", id, r.Status, r.Body)
		}
		if r := openPublic(t, app, id, map[string]any{}); r.Status != http.StatusNotFound || r.ErrorCode(t) != "customer_address_not_found" {
			t.Fatalf("POST %s: %d %s", id, r.Status, r.Body)
		}
	}
}

// The public endpoints answer CORS like the other checkout routes.
func TestCheckoutAddressCORS(t *testing.T) {
	t.Parallel()
	app := checkoutApp(t)
	r := app.Do("GET", "/v1/checkout/addresses/caddr_x", "", nil)
	if r.Header.Get("Access-Control-Allow-Origin") != "*" || r.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("headers %v", r.Header)
	}
}
