//go:build integration

package api_test

import (
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/checkout"
	"github.com/prismaport/prismaport/internal/service"
	"github.com/prismaport/prismaport/internal/store"
)

func lockApp(t *testing.T, opt apptest.Options) *apptest.App {
	t.Helper()
	opt.Checkout = func(svc *service.Service, st *store.Store) *checkout.Checkout { return checkout.New(svc, st, nil) }
	return apptest.New(t, opt)
}

func publicSession(t *testing.T, app *apptest.App, id, query string) map[string]any {
	t.Helper()
	r := app.Do("GET", "/v1/checkout/sessions/"+id+query, "", nil)
	if r.Status != http.StatusOK {
		t.Fatalf("GET checkout session: %d %s", r.Status, r.Body)
	}
	return r.JSON(t)
}

func seenAt(t *testing.T, app *apptest.App, id string) *time.Time {
	t.Helper()
	at, err := app.Store.CheckoutSeenAt(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

// ?active=1 and the wallet endpoints record the payer's presence at most once per 10 s, and the
// answer is the same with or without it.
func TestCheckoutPresenceThrottle(t *testing.T) {
	t.Parallel()
	app := lockApp(t, apptest.Options{})
	id := newTestSession(t, app, app.TestKey, "25.00")["id"].(string)

	plain := publicSession(t, app, id, "")
	publicSession(t, app, id, "?active=0")
	if at := seenAt(t, app, id); at != nil {
		t.Fatalf("seen without active=1: %v", at)
	}
	active := publicSession(t, app, id, "?active=1")
	if !reflect.DeepEqual(plain, active) {
		t.Fatalf("active=1 changed the answer:\n%v\n%v", plain, active)
	}
	first := app.Clock.Now()
	if at := seenAt(t, app, id); at == nil || !at.Equal(first) {
		t.Fatalf("seen at %v, want %v", at, first)
	}
	app.Clock.Advance(9 * time.Second)
	publicSession(t, app, id, "?active=1")
	if at := seenAt(t, app, id); !at.Equal(first) {
		t.Fatalf("rewritten inside the throttle: %v", at)
	}
	app.Clock.Advance(time.Second)
	publicSession(t, app, id, "?active=1")
	if at := seenAt(t, app, id); !at.Equal(app.Clock.Now()) {
		t.Fatalf("not rewritten after 10 s: %v, now %v", at, app.Clock.Now())
	}

	// A wallet endpoint records presence even when it then refuses (no signed path here).
	app.Clock.Advance(time.Minute)
	r := app.Do("POST", "/v1/checkout/sessions/"+id+"/eip3009/typed_data", "",
		map[string]any{"chain": "base-sepolia", "asset": "USDC", "from": "0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed"})
	if r.Status == http.StatusOK {
		t.Fatalf("typed data without a gateway: %d", r.Status)
	}
	if at := seenAt(t, app, id); !at.Equal(app.Clock.Now()) {
		t.Fatalf("typed_data did not record presence: %v", at)
	}
	app.Clock.Advance(time.Minute)
	r = app.Do("POST", "/v1/checkout/sessions/"+id+"/solana/transaction", "",
		map[string]any{"chain": "solana-devnet", "asset": "USDC", "account": apptest.SolanaDestination})
	if r.Status == http.StatusOK {
		t.Fatalf("solana transaction without a fee payer: %d", r.Status)
	}
	if at := seenAt(t, app, id); !at.Equal(app.Clock.Now()) {
		t.Fatalf("solana/transaction did not record presence: %v", at)
	}
	// An unknown session writes nothing and still answers 404.
	if r := app.Do("GET", "/v1/checkout/sessions/ses_01M5MYPW80FMXFE81YHNB8MPR8?active=1", "", nil); r.Status != http.StatusNotFound {
		t.Fatalf("unknown session: %d", r.Status)
	}
}

func payToKeys(t *testing.T, s map[string]any) [][]string {
	t.Helper()
	var out [][]string
	for _, p := range s["pay_to"].([]any) {
		var keys []string
		for k := range p.(map[string]any) {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		out = append(out, keys)
	}
	return out
}

// Past expires_at the payer's view keeps chain, asset and what was received, never where to pay;
// a paid session keeps its receipt. The page learns the link and whether it still opens sessions.
func TestCheckoutExpiryLock(t *testing.T) {
	t.Parallel()
	warn := 2 * time.Minute
	app := lockApp(t, apptest.Options{CheckoutExpiryWarning: &warn})
	link := app.PaymentLink(app.TestKey, app.Link(app.TestKey), map[string]any{"amount": "25.00"})
	r := app.OpenLink(link, "lock-open", map[string]any{})
	if r.Status != http.StatusOK {
		t.Fatalf("open link: %d %s", r.Status, r.Body)
	}
	opened := r.JSON(t)
	id := opened["id"].(string)
	if opened["link_id"] != link || opened["link_active"] != true || opened["expiry_warning_seconds"] != float64(120) {
		t.Fatalf("open: link %v active %v warning %v", opened["link_id"], opened["link_active"], opened["expiry_warning_seconds"])
	}
	// Nothing to pay to until the payer generates a payment on a network (D48).
	for _, keys := range payToKeys(t, opened) {
		if !slices.Equal(keys, []string{"amount", "asset", "chain", "revealed", "signed_payment"}) {
			t.Fatalf("unrevealed pay_to keys %v", keys)
		}
	}
	for _, c := range []string{"base-sepolia", "solana-devnet"} {
		if r := app.CheckoutReveal(id, c); r.Status != http.StatusOK {
			t.Fatalf("reveal %s: %d %s", c, r.Status, r.Body)
		}
	}
	if sim := app.Do("POST", "/v1/test_helpers/sessions/"+id+"/simulate", app.TestKey,
		map[string]any{"action": "pay_partial", "amount": "10.00", "chain": "base-sepolia", "asset": "USDC"}); sim.Status != http.StatusOK {
		t.Fatalf("pay partial: %d %s", sim.Status, sim.Body)
	}
	open := publicSession(t, app, id, "")
	var solanaKeys []string
	for _, p := range open["pay_to"].([]any) {
		m := p.(map[string]any)
		if m["address"] == nil || m["amount"] == nil {
			t.Fatalf("open session pay_to without deposit details: %v", m)
		}
		if m["chain"] == "solana-devnet" {
			for k := range m {
				solanaKeys = append(solanaKeys, k)
			}
		}
	}
	if !slices.Contains(solanaKeys, "reference") {
		t.Fatalf("open Solana entry: %v", solanaKeys)
	}

	expires, _ := time.Parse(time.RFC3339, open["expires_at"].(string))
	app.Clock.Set(expires.Add(-time.Second))
	if keys := payToKeys(t, publicSession(t, app, id, "")); !slices.Contains(keys[0], "address") {
		t.Fatalf("locked a second early: %v", keys)
	}
	app.Clock.Set(expires)
	locked := publicSession(t, app, id, "?active=1")
	for _, keys := range payToKeys(t, locked) {
		if !slices.Equal(keys, []string{"asset", "chain", "revealed", "signed_payment"}) {
			t.Fatalf("locked pay_to keys %v", keys)
		}
	}
	for _, p := range locked["pay_to"].([]any) {
		if p.(map[string]any)["signed_payment"] != false {
			t.Fatalf("locked entry offers a signed payment: %v", p)
		}
	}
	if locked["amount_received"] != "10.00" || locked["amount_remaining"] != "15.00" || len(locked["payments"].([]any)) != 1 ||
		locked["status"] != "partially_paid" || locked["link_id"] != link || locked["link_active"] != true {
		t.Fatalf("locked session lost what was received: %v", locked)
	}
	if len(locked["pay_to"].([]any)) != len(open["pay_to"].([]any)) {
		t.Fatalf("locked pay_to has %d entries, open had %d", len(locked["pay_to"].([]any)), len(open["pay_to"].([]any)))
	}

	// A link turned off no longer offers a new payment.
	if r := app.Do("PATCH", "/v1/payment_links/"+link, app.TestKey, map[string]any{"active": false}); r.Status != http.StatusOK {
		t.Fatalf("deactivate link: %d %s", r.Status, r.Body)
	}
	if s := publicSession(t, app, id, ""); s["link_active"] != false {
		t.Fatalf("inactive link still active: %v", s["link_active"])
	}

	// An API session has no link.
	app.Clock.Set(expires.Add(time.Hour))
	paid := newTestSession(t, app, app.TestKey, "5.00")["id"].(string)
	if s := publicSession(t, app, paid, ""); s["link_id"] != nil || s["link_active"] != false {
		t.Fatalf("API session link: %v %v", s["link_id"], s["link_active"])
	}
	app.Reveal(app.TestKey, paid)
	// A paid session keeps its receipt after expiry, without deposit details.
	if sim := app.Do("POST", "/v1/test_helpers/sessions/"+paid+"/simulate", app.TestKey,
		map[string]any{"action": "pay_exact", "chain": "base-sepolia", "asset": "USDC"}); sim.Status != http.StatusOK {
		t.Fatalf("pay exact: %d %s", sim.Status, sim.Body)
	}
	before := publicSession(t, app, paid, "")
	if keys := payToKeys(t, before); !slices.Contains(keys[0], "address") {
		t.Fatalf("paid session before expiry lost its address: %v", keys)
	}
	app.Clock.Advance(2 * time.Hour)
	receipt := publicSession(t, app, paid, "")
	if receipt["status"] != "paid" || receipt["amount_received"] != "5.00" || len(receipt["payments"].([]any)) != 1 ||
		receipt["payments"].([]any)[0].(map[string]any)["tx_hash"] == "" {
		t.Fatalf("receipt: %v", receipt)
	}
	for _, keys := range payToKeys(t, receipt) {
		if !slices.Equal(keys, []string{"asset", "chain", "revealed", "signed_payment"}) {
			t.Fatalf("paid session past expiry shows deposit details: %v", keys)
		}
	}
	// The merchant still sees the deposit addresses.
	m := app.Do("GET", "/v1/sessions/"+paid, app.TestKey, nil).JSON(t)
	if m["pay_to"].([]any)[0].(map[string]any)["address"] == nil {
		t.Fatalf("merchant view lost the address: %v", m["pay_to"])
	}
}

// After expiry the Solana Pay transaction request answers 410 session_expired, GET and POST.
func TestSolanaPayGoneAfterExpiry(t *testing.T) {
	t.Parallel()
	app := lockApp(t, apptest.Options{})
	s := newTestSession(t, app, app.TestKey, "25.00")
	id := s["id"].(string)
	path := "/v1/checkout/sessions/" + id + "/solana/pay?chain=solana-devnet&asset=USDC"
	body := map[string]any{"account": apptest.SolanaDestination}
	if r := app.Do("GET", path, "", nil); r.Status == http.StatusGone {
		t.Fatalf("open session answered 410: %s", r.Body)
	}
	expires, _ := time.Parse(time.RFC3339, s["expires_at"].(string))
	app.Clock.Set(expires)
	for _, method := range []string{"GET", "POST"} {
		var b any
		if method == "POST" {
			b = body
		}
		r := app.Do(method, path, "", b)
		if r.Status != http.StatusGone || r.ErrorCode(t) != checkout.CodeSessionExpired {
			t.Fatalf("%s after expiry: %d %s", method, r.Status, r.Body)
		}
		if msg, _ := r.JSON(t)["message"].(string); msg == "" {
			t.Fatalf("%s: no wallet message: %s", method, r.Body)
		}
	}
	// Paid sessions too: the QR belongs to a closed checkout.
	app.Clock.Set(expires.Add(time.Hour))
	paid := newTestSession(t, app, app.TestKey, "5.00")
	pid := paid["id"].(string)
	if sim := app.Do("POST", "/v1/test_helpers/sessions/"+pid+"/simulate", app.TestKey,
		map[string]any{"action": "pay_exact", "chain": "base-sepolia", "asset": "USDC"}); sim.Status != http.StatusOK {
		t.Fatalf("pay exact: %d %s", sim.Status, sim.Body)
	}
	app.Clock.Advance(2 * time.Hour)
	if r := app.Do("GET", "/v1/checkout/sessions/"+pid+"/solana/pay?chain=solana-devnet&asset=USDC", "", nil); r.Status != http.StatusGone {
		t.Fatalf("paid session after expiry: %d %s", r.Status, r.Body)
	}
}

func recheckAudits(t *testing.T, app *apptest.App) int {
	t.Helper()
	n := 0
	for _, a := range auditActions(t, app, app.MerchantID) {
		if a == "session.recheck_requested" {
			n++
		}
	}
	return n
}

// The worker clears recheck_requested_at once its check is done.
func workerDone(t *testing.T, app *apptest.App, id string) {
	t.Helper()
	if _, err := app.Store.Pool().Exec(t.Context(), `UPDATE sessions SET recheck_requested_at = NULL WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
}

// "Check again": sessions:write, only past the watch window, once per pending request, rate
// limited per session and per merchant, audited.
func TestRecheckSession(t *testing.T) {
	t.Parallel()
	app := lockApp(t, apptest.Options{RecheckLimits: service.RecheckLimits{
		Session: service.RecheckLimit{Count: 1, Period: 10 * time.Minute}, Merchant: service.RecheckLimit{Count: 2, Period: time.Hour}}})
	s := app.Reveal(app.TestKey, newTestSession(t, app, app.TestKey, "25.00")["id"].(string)).JSON(t)
	id := s["id"].(string)
	expires, _ := time.Parse(time.RFC3339, s["expires_at"].(string))
	recheck := func(key, id string) apptest.Response {
		t.Helper()
		return app.Do("POST", "/v1/sessions/"+id+"/recheck", key, nil)
	}

	_, readOnly, err := app.Svc.CreateAPIKey(t.Context(), app.MerchantID, false, []string{"sessions:read"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if r := recheck(readOnly, id); r.Status != http.StatusForbidden || r.ErrorCode(t) != service.CodeInsufficientScope {
		t.Fatalf("read-only key: %d %s", r.Status, r.Body)
	}
	for _, at := range []time.Time{expires.Add(-time.Minute), expires, expires.Add(service.DefaultWatchAfterExpiry - time.Second)} {
		app.Clock.Set(at)
		if r := recheck(app.TestKey, id); r.Status != http.StatusConflict || r.ErrorCode(t) != service.CodeSessionStillWatched {
			t.Fatalf("at %s: %d %s", at, r.Status, r.Body)
		}
	}
	if r := recheck(app.TestKey, "ses_01M5MYPW80FMXFE81YHNB8MPR8"); r.Status != http.StatusNotFound {
		t.Fatalf("unknown session: %d %s", r.Status, r.Body)
	}
	app.Clock.Set(expires.Add(service.DefaultWatchAfterExpiry))
	if r := recheck(app.LiveKey, id); r.Status != http.StatusNotFound {
		t.Fatalf("other mode: %d %s", r.Status, r.Body)
	}
	if recheckAudits(t, app) != 0 {
		t.Fatal("refusals were audited")
	}

	r := recheck(app.TestKey, id)
	if r.Status != http.StatusAccepted || r.JSON(t)["id"] != id || r.JSON(t)["object"] != nil && r.JSON(t)["object"] != "session" {
		t.Fatalf("recheck: %d %s", r.Status, r.Body)
	}
	if at, err := app.Store.RecheckRequestedAt(t.Context(), id); err != nil || at == nil || !at.Equal(app.Clock.Now()) {
		t.Fatalf("recheck_requested_at %v %v", at, err)
	}
	if n := recheckAudits(t, app); n != 1 {
		t.Fatalf("%d audit rows", n)
	}
	// Pending: 202 again, no audit row, no token taken.
	for range 3 {
		if r := recheck(app.TestKey, id); r.Status != http.StatusAccepted {
			t.Fatalf("pending recheck: %d %s", r.Status, r.Body)
		}
	}
	if n := recheckAudits(t, app); n != 1 {
		t.Fatalf("pending rechecks audited: %d rows", n)
	}
	// Done by the worker: the session's bucket is empty for 10 minutes.
	workerDone(t, app, id)
	r = recheck(app.TestKey, id)
	if r.Status != http.StatusTooManyRequests || r.ErrorCode(t) != service.CodeRateLimited || r.Header.Get("Retry-After") == "" {
		t.Fatalf("second recheck inside 10 min: %d %s %v", r.Status, r.Body, r.Header)
	}

	// The merchant's bucket (2 an hour here): a second session passes, a third is refused.
	app.Clock.Set(expires)
	var others []string
	for i := range 2 {
		o := app.RevealedSession(app.TestKey, map[string]any{"amount": "5.00"}, "Idempotency-Key", fmt.Sprintf("recheck-other-%d", i))
		if o.Status != http.StatusOK {
			t.Fatalf("session: %d %s", o.Status, o.Body)
		}
		others = append(others, o.JSON(t)["id"].(string))
	}
	app.Clock.Set(expires.Add(48 * time.Hour))
	if r := recheck(app.TestKey, others[0]); r.Status != http.StatusAccepted {
		t.Fatalf("second session: %d %s", r.Status, r.Body)
	}
	r = recheck(app.TestKey, others[1])
	if r.Status != http.StatusTooManyRequests || r.ErrorCode(t) != service.CodeRateLimited {
		t.Fatalf("third recheck within the hour: %d %s", r.Status, r.Body)
	}
	if n := recheckAudits(t, app); n != 2 {
		t.Fatalf("%d audit rows, want 2", n)
	}
}

// The dashboard's "Check again" and the session detail fields its button reads.
func TestDashboardRecheck(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	s := app.Reveal(app.TestKey, newTestSession(t, app, app.TestKey, "25.00")["id"].(string)).JSON(t)
	id := s["id"].(string)
	expires, _ := time.Parse(time.RFC3339, s["expires_at"].(string))
	c := login(t, app, "owner@example.com")

	d := c.get("/v1/dashboard/sessions/" + id + "?mode=test")
	if d["watch_until"] != expires.Add(service.DefaultWatchAfterExpiry).Format(time.RFC3339) || d["recheck_available"] != false || d["recheck_requested_at"] != nil {
		t.Fatalf("detail before: %v %v %v", d["watch_until"], d["recheck_available"], d["recheck_requested_at"])
	}
	r := c.do("POST", "/v1/dashboard/sessions/"+id+"/recheck?mode=test", nil)
	if r.Status != http.StatusConflict || r.ErrorCode(t) != service.CodeSessionStillWatched {
		t.Fatalf("inside the window: %d %s", r.Status, r.Body)
	}
	app.Clock.Set(expires.Add(24 * time.Hour))
	c = login(t, app, "owner@example.com")
	r = c.do("POST", "/v1/dashboard/sessions/"+id+"/recheck?mode=test", map[string]any{})
	if r.Status != http.StatusAccepted {
		t.Fatalf("recheck: %d %s", r.Status, r.Body)
	}
	d = r.JSON(t)
	if d["id"] != id || d["recheck_available"] != true || d["recheck_requested_at"] != app.Clock.Now().Format(time.RFC3339) {
		t.Fatalf("detail after: %v %v %v", d["id"], d["recheck_available"], d["recheck_requested_at"])
	}
	if r := c.do("POST", "/v1/dashboard/sessions/"+id+"/recheck?mode=live", nil); r.Status != http.StatusNotFound {
		t.Fatalf("other mode: %d %s", r.Status, r.Body)
	}
	workerDone(t, app, id)
	r = c.do("POST", "/v1/dashboard/sessions/"+id+"/recheck?mode=test", nil)
	if r.Status != http.StatusTooManyRequests || r.Header.Get("Retry-After") == "" {
		t.Fatalf("again within 10 min: %d %s", r.Status, r.Body)
	}
	if !slices.Contains(auditActions(t, app, app.MerchantID), "session.recheck_requested") {
		t.Fatal("no audit row")
	}
}

// "Check again" on a session nobody generated a payment for (no network revealed, D48) is refused:
// no deposit address was ever shown and no network of it is watched, so the request would never
// be served. The dashboard does not offer it.
func TestRecheckOfAnUnrevealedSessionIsRefused(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	s := newTestSession(t, app, app.TestKey, "25.00")
	id := s["id"].(string)
	expires, _ := time.Parse(time.RFC3339, s["expires_at"].(string))
	app.Clock.Set(expires.Add(24 * time.Hour))
	if r := app.Do("POST", "/v1/sessions/"+id+"/recheck", app.TestKey, nil); r.Status != http.StatusConflict ||
		r.ErrorCode(t) != service.CodeSessionNotRevealed {
		t.Fatalf("merchant API: %d %s", r.Status, r.Body)
	}
	c := login(t, app, "owner@example.com")
	if d := c.get("/v1/dashboard/sessions/" + id + "?mode=test"); d["recheck_available"] != false {
		t.Fatalf("dashboard offers Check again: %v", d["recheck_available"])
	}
	if r := c.do("POST", "/v1/dashboard/sessions/"+id+"/recheck?mode=test", nil); r.Status != http.StatusConflict ||
		r.ErrorCode(t) != service.CodeSessionNotRevealed {
		t.Fatalf("dashboard: %d %s", r.Status, r.Body)
	}
	if at, err := app.Store.RecheckRequestedAt(t.Context(), id); err != nil || at != nil {
		t.Fatalf("a request was stored: %v %v", at, err)
	}
}
