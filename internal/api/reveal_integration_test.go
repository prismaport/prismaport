//go:build integration

package api_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/service"
	"github.com/prismaport/prismaport/internal/session"
	"github.com/prismaport/prismaport/internal/store"
)

func revealAudits(t *testing.T, app *apptest.App) int {
	t.Helper()
	n := 0
	for _, a := range auditActions(t, app, app.MerchantID) {
		if a == "session.revealed" {
			n++
		}
	}
	return n
}

func entry(t *testing.T, s map[string]any, chainID string) map[string]any {
	t.Helper()
	for _, p := range s["pay_to"].([]any) {
		if m := p.(map[string]any); m["chain"] == chainID {
			return m
		}
	}
	t.Fatalf("no %s entry: %v", chainID, s["pay_to"])
	return nil
}

// D48 through the merchant API: sessions:write, audited once per network, idempotent, the first
// reveal starts the pay window and later ones do not move it; the errors of the spec.
func TestMerchantReveal(t *testing.T) {
	t.Parallel()
	app := lockApp(t, apptest.Options{})
	s := newTestSession(t, app, app.TestKey, "25.00")
	id := s["id"].(string)
	reveal := func(key, id string, body any) apptest.Response {
		t.Helper()
		return app.Do("POST", "/v1/sessions/"+id+"/reveal", key, body)
	}
	_, readOnly, err := app.Svc.CreateAPIKey(t.Context(), app.MerchantID, false, []string{"sessions:read"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if r := reveal(readOnly, id, map[string]any{"chain": "base-sepolia"}); r.Status != http.StatusForbidden || r.ErrorCode(t) != service.CodeInsufficientScope {
		t.Fatalf("read-only key: %d %s", r.Status, r.Body)
	}
	for name, body := range map[string]any{"no chain": map[string]any{}, "unknown chain": map[string]any{"chain": "polygon"},
		"a live chain": map[string]any{"chain": "base"}, "a number": map[string]any{"chain": 1}, "an extra field": map[string]any{"chain": "base-sepolia", "asset": "USDC"}} {
		t.Run(name, func(t *testing.T) {
			if r := reveal(app.TestKey, id, body); r.Status != http.StatusBadRequest || r.ErrorCode(t) != "invalid_request" {
				t.Errorf("%s: %d %s", name, r.Status, r.Body)
			}
		})
	}
	if r := reveal(app.TestKey, "ses_01M5MYPW80FMXFE81YHNB8MPR8", map[string]any{"chain": "base-sepolia"}); r.Status != http.StatusNotFound {
		t.Fatalf("unknown session: %d %s", r.Status, r.Body)
	}
	if r := reveal(app.LiveKey, id, map[string]any{"chain": "base-sepolia"}); r.Status != http.StatusNotFound {
		t.Fatalf("other mode: %d %s", r.Status, r.Body)
	}
	if n := revealAudits(t, app); n != 0 {
		t.Fatalf("refusals audited: %d", n)
	}

	app.Clock.Advance(20 * time.Minute)
	first := app.Clock.Now()
	r := reveal(app.TestKey, id, map[string]any{"chain": "base-sepolia"})
	if r.Status != http.StatusOK {
		t.Fatalf("reveal: %d %s", r.Status, r.Body)
	}
	got := r.JSON(t)
	base, sol := entry(t, got, "base-sepolia"), entry(t, got, "solana-devnet")
	if base["revealed"] != true || base["address"] == nil || base["revealed_at"] != first.Format(time.RFC3339) ||
		sol["revealed"] != false || sol["address"] != nil || sol["token_account"] != nil {
		t.Fatalf("pay_to after the reveal: %v", got["pay_to"])
	}
	if got["payment_started_at"] != first.Format(time.RFC3339) || got["expires_at"] != first.Add(30*time.Minute).Format(time.RFC3339) ||
		got["version"] != s["version"] {
		t.Fatalf("after the reveal: started %v expires %v version %v", got["payment_started_at"], got["expires_at"], got["version"])
	}
	if n := revealAudits(t, app); n != 1 {
		t.Fatalf("%d audit rows", n)
	}
	// Again: the same details, no new audit row.
	app.Clock.Advance(time.Minute)
	if again := reveal(app.TestKey, id, map[string]any{"chain": "base-sepolia"}); again.Status != http.StatusOK || string(again.Body) != string(r.Body) {
		t.Fatalf("repeat: %d %s", again.Status, again.Body)
	}
	if n := revealAudits(t, app); n != 1 {
		t.Fatalf("a repeat was audited: %d rows", n)
	}
	// Another network: the first stays, the deadline does not move.
	second := reveal(app.TestKey, id, map[string]any{"chain": "solana-devnet"}).JSON(t)
	if second["expires_at"] != got["expires_at"] || entry(t, second, "base-sepolia")["revealed"] != true ||
		entry(t, second, "solana-devnet")["address"] == nil {
		t.Fatalf("second network: %v", second)
	}
	// Past expires_at: 410, whatever the network.
	app.Clock.Set(first.Add(30 * time.Minute))
	if r := reveal(app.TestKey, id, map[string]any{"chain": "base-sepolia"}); r.Status != http.StatusGone || r.ErrorCode(t) != service.CodeSessionExpired {
		t.Fatalf("after expiry: %d %s", r.Status, r.Body)
	}

	// A paid session takes no new network: 409.
	paid := newTestSession(t, app, app.TestKey, "5.00")["id"].(string)
	app.Reveal(app.TestKey, paid, "base-sepolia")
	if sim := app.Do("POST", "/v1/test_helpers/sessions/"+paid+"/simulate", app.TestKey,
		map[string]any{"action": "pay_exact", "chain": "base-sepolia", "asset": "USDC"}); sim.Status != http.StatusOK {
		t.Fatalf("pay exact: %d %s", sim.Status, sim.Body)
	}
	if r := reveal(app.TestKey, paid, map[string]any{"chain": "solana-devnet"}); r.Status != http.StatusConflict || r.ErrorCode(t) != service.CodeSessionNotPayable {
		t.Fatalf("paid session: %d %s", r.Status, r.Body)
	}
}

// D48 on the hosted checkout: no deposit details before "Generate payment", the reveal shows one
// network, counts as payer presence and starts the pay window; a session nobody reveals expires
// after the selection window.
func TestCheckoutReveal(t *testing.T) {
	t.Parallel()
	app := lockApp(t, apptest.Options{})
	s := newTestSession(t, app, app.TestKey, "25.00")
	id := s["id"].(string)
	created := app.Clock.Now()

	open := publicSession(t, app, id, "")
	if open["payment_started_at"] != nil || open["pay_window_seconds"] != float64(1800) || open["expires_at"] != created.Add(time.Hour).Format(time.RFC3339) {
		t.Fatalf("before the reveal: started %v window %v expires %v", open["payment_started_at"], open["pay_window_seconds"], open["expires_at"])
	}
	for _, p := range open["pay_to"].([]any) {
		m := p.(map[string]any)
		if m["revealed"] != false || m["address"] != nil || m["reference"] != nil || m["token_account"] != nil || m["amount"] != "25.000000" {
			t.Fatalf("unrevealed entry: %v", m)
		}
	}
	if raw := app.Do("GET", "/v1/checkout/sessions/"+id, "", nil); strings.Contains(string(raw.Body), "0x") {
		t.Fatalf("an address leaked before the reveal: %s", raw.Body)
	}
	if r := app.CheckoutReveal(id, "polygon"); r.Status != http.StatusBadRequest || r.ErrorCode(t) != "invalid_request" {
		t.Fatalf("unknown chain: %d %s", r.Status, r.Body)
	}
	if r := app.CheckoutReveal("ses_01M5MYPW80FMXFE81YHNB8MPR8", "base-sepolia"); r.Status != http.StatusNotFound {
		t.Fatalf("unknown session: %d %s", r.Status, r.Body)
	}

	app.Clock.Advance(5 * time.Minute)
	r := app.CheckoutReveal(id, "solana-devnet")
	if r.Status != http.StatusOK {
		t.Fatalf("reveal: %d %s", r.Status, r.Body)
	}
	got := r.JSON(t)
	sol := entry(t, got, "solana-devnet")
	if sol["revealed"] != true || sol["address"] == nil || sol["reference"] == nil || entry(t, got, "base-sepolia")["address"] != nil {
		t.Fatalf("after the reveal: %v", got["pay_to"])
	}
	if got["payment_started_at"] != app.Clock.Now().Format(time.RFC3339) || got["expires_at"] != app.Clock.Now().Add(30*time.Minute).Format(time.RFC3339) {
		t.Fatalf("pay window: %v %v", got["payment_started_at"], got["expires_at"])
	}
	if at := seenAt(t, app, id); !at.Equal(app.Clock.Now()) {
		t.Fatalf("the reveal did not record presence: %v", at)
	}
	ses, err := app.Store.LoadSession(t.Context(), id)
	if err != nil || ses.Revealed("solana-devnet").Source != store.RevealCheckout || revealAudits(t, app) != 0 {
		t.Fatalf("reveal row %+v %v", ses.Reveals, err)
	}
	// The merchant sees the revealed network only.
	m := app.Do("GET", "/v1/sessions/"+id, app.TestKey, nil).JSON(t)
	if entry(t, m, "solana-devnet")["address"] == nil || entry(t, m, "base-sepolia")["address"] != nil {
		t.Fatalf("merchant view: %v", m["pay_to"])
	}

	// Never revealed: expires at the end of the selection window.
	idle := newTestSession(t, app, app.TestKey, "7.00")["id"].(string)
	app.Clock.Advance(time.Hour)
	if n, err := app.Svc.ExpireDue(t.Context(), 100); err != nil || n == 0 {
		t.Fatalf("expire: %d %v", n, err)
	}
	if s := publicSession(t, app, idle, ""); s["status"] != "expired" || s["payment_started_at"] != nil {
		t.Fatalf("unrevealed session after the selection window: %v %v", s["status"], s["payment_started_at"])
	}
	if r := app.CheckoutReveal(idle, "base-sepolia"); r.Status != http.StatusGone || r.ErrorCode(t) != service.CodeSessionExpired {
		t.Fatalf("reveal after expiry: %d %s", r.Status, r.Body)
	}
}

// A link session nobody generates a payment for holds its max_payments slot only until the
// selection window ends.
func TestUnrevealedLinkSessionReleasesItsSlot(t *testing.T) {
	t.Parallel()
	app := lockApp(t, apptest.Options{})
	link := apiLink(t, app, app.TestKey, app.Link(app.TestKey), map[string]any{"amount": "5.00", "max_payments": 1})
	if r := openLink(app, link, "slot-1", nil); r.Status != http.StatusOK {
		t.Fatalf("open: %d %s", r.Status, r.Body)
	}
	if r := openLink(app, link, "slot-2", nil); r.Status != http.StatusGone {
		t.Fatalf("the slot is not held: %d %s", r.Status, r.Body)
	}
	app.Clock.Advance(time.Hour)
	if _, err := app.Svc.ExpireDue(t.Context(), 100); err != nil {
		t.Fatal(err)
	}
	if r := openLink(app, link, "slot-3", nil); r.Status != http.StatusOK {
		t.Fatalf("the slot was not released after the selection window: %d %s", r.Status, r.Body)
	}
}

// The wallet endpoints reveal their network themselves (source wallet, wallet_tx_at) before
// building from the session's expiry.
func TestWalletEndpointsReveal(t *testing.T) {
	t.Parallel()
	e := newSolPay(t, nil)
	id := e.session("25.00")
	e.app.Clock.Advance(10 * time.Minute)
	if r := e.app.Do("GET", payPath(id, solPayChain, "USDC"), "", nil); r.Status != http.StatusOK {
		t.Fatalf("Solana Pay GET: %d %s", r.Status, r.Body)
	}
	ses, err := e.app.Store.LoadSession(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	r := ses.Revealed(solPayChain)
	if r == nil || r.Source != store.RevealWallet || ses.Revealed("base-sepolia") != nil || ses.PaymentStartedAt == nil ||
		!ses.State.ExpiresAt.Equal(e.app.Clock.Now().Add(30*time.Minute)) {
		t.Fatalf("after the Solana Pay GET: reveals %+v started %v expires %s", ses.Reveals, ses.PaymentStartedAt, ses.State.ExpiresAt)
	}
	if at, err := e.app.Store.WalletTxAt(t.Context(), id); err != nil || at == nil || !at.Equal(e.app.Clock.Now()) {
		t.Fatalf("wallet_tx_at %v %v", at, err)
	}
	// solana/transaction on another session reveals it too, and the transaction it builds is
	// valid for the new deadline.
	other := e.session("26.00")
	payer := solKey(t, 43).PublicKey()
	w := e.app.Do("POST", "/v1/checkout/sessions/"+other+"/solana/transaction", "", map[string]string{"chain": solPayChain, "asset": "USDC", "account": payer.String()})
	if w.Status != http.StatusOK {
		t.Fatalf("solana/transaction: %d %s", w.Status, w.Body)
	}
	if ses, err := e.app.Store.LoadSession(t.Context(), other); err != nil || ses.Revealed(solPayChain) == nil {
		t.Fatalf("solana/transaction did not reveal: %+v %v", ses, err)
	}
}

// D47 for "Check again": a session paid exactly is refused while its payment is not final (the
// worker still checks it), then accepted at once; the dashboard's watch_until is the moment it
// left automatic checks.
func TestRecheckOfAPaidSession(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	id := newTestSession(t, app, app.TestKey, "25.00")["id"].(string)
	app.Reveal(app.TestKey, id, "base-sepolia")
	if sim := app.Do("POST", "/v1/test_helpers/sessions/"+id+"/simulate", app.TestKey,
		map[string]any{"action": "pay_exact", "chain": "base-sepolia", "asset": "USDC"}); sim.Status != http.StatusOK {
		t.Fatalf("pay exact: %d %s", sim.Status, sim.Body)
	}
	recheck := func() apptest.Response {
		t.Helper()
		return app.Do("POST", "/v1/sessions/"+id+"/recheck", app.TestKey, nil)
	}
	ses, err := app.Store.LoadSession(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if ses.State.Status != session.Paid || ses.State.Payments[0].Status == session.Finalized {
		t.Fatalf("paid with a pending payment expected: %s %s", ses.State.Status, ses.State.Payments[0].Status)
	}
	if r := recheck(); r.Status != http.StatusConflict || r.ErrorCode(t) != service.CodeSessionStillWatched {
		t.Fatalf("before finality: %d %s", r.Status, r.Body)
	}
	app.Clock.Advance(15 * time.Minute)
	final := app.Clock.Now()
	pid := ses.State.Payments[0].ID
	if _, _, err := app.Store.ApplySessionCommand(t.Context(), id, func(s session.State) (session.State, []session.Event, error) {
		return s.FinalizePayment(pid, final)
	}); err != nil {
		t.Fatal(err)
	}
	c := login(t, app, "owner@example.com")
	d := c.get("/v1/dashboard/sessions/" + id + "?mode=test")
	if d["watch_until"] != final.Format(time.RFC3339) || d["recheck_available"] != true {
		t.Fatalf("dashboard: watch_until %v, recheck_available %v", d["watch_until"], d["recheck_available"])
	}
	app.Clock.Advance(time.Second)
	if r := recheck(); r.Status != http.StatusAccepted {
		t.Fatalf("after finality: %d %s", r.Status, r.Body)
	}
}

// A session on a customer address is revealed on every network from creation (its customer
// already holds the address): the merchant, the public checkout and the address page show its
// deposit details at once, it expires a pay window after it opened (no selection window), the
// reveal endpoints answer it unchanged and write no audit, and the reveal rows say
// customer_address.
func TestCustomerAddressSessionIsRevealedFromCreation(t *testing.T) {
	t.Parallel()
	app := lockApp(t, apptest.Options{})
	a := newAddress(t, app, app.TestKey, map[string]any{"customer_id": "user_77", "product_id": app.Link(app.TestKey)}, http.StatusCreated)
	addr := a["id"].(string)
	created := app.Clock.Now().UTC()
	s := mustOpenOn(t, app, app.TestKey, addr, map[string]any{"amount": "25.00"}, "ca-1")
	id := s["id"].(string)
	if s["payment_started_at"] != created.Format(time.RFC3339) || s["expires_at"] != created.Add(30*time.Minute).Format(time.RFC3339) ||
		s["pay_window_seconds"] != float64(1800) {
		t.Fatalf("customer address session timers %v %v %v", s["payment_started_at"], s["expires_at"], s["pay_window_seconds"])
	}
	want := addressesOf(t, a)
	for _, x := range s["pay_to"].([]any) {
		p := x.(map[string]any)
		if p["revealed"] != true || p["revealed_at"] != created.Format(time.RFC3339) || p["address"] == nil || p["address"] == "" {
			t.Fatalf("pay_to %v", p)
		}
	}
	if got := addressesOf(t, s); len(got) != len(want) {
		t.Fatalf("session addresses %v, customer address %v", got, want)
	}
	var sources []string
	rows, err := app.Store.Pool().Query(t.Context(), `SELECT DISTINCT source FROM session_reveals WHERE session_id = $1`, id)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var src string
		if err := rows.Scan(&src); err != nil {
			t.Fatal(err)
		}
		sources = append(sources, src)
	}
	if rows.Err() != nil || len(sources) != 1 || sources[0] != store.RevealCustomerAddress {
		t.Fatalf("reveal sources %v %v", sources, rows.Err())
	}

	pts, _ := publicSession(t, app, id, "")["pay_to"].([]any)
	if len(pts) == 0 {
		t.Fatal("public session without pay_to")
	}
	for _, x := range pts {
		if p := x.(map[string]any); p["revealed"] != true || p["address"] == nil {
			t.Fatalf("public pay_to %v", p)
		}
	}
	audits := revealAudits(t, app)
	app.Clock.Advance(5 * time.Minute)
	for _, chainID := range []string{"base-sepolia", "solana-devnet"} {
		if r := app.Reveal(app.TestKey, id, chainID); r.Status != http.StatusOK || r.JSON(t)["expires_at"] != s["expires_at"] {
			t.Fatalf("merchant reveal %s: %d %s", chainID, r.Status, r.Body)
		}
		if r := app.CheckoutReveal(id, chainID); r.Status != http.StatusOK || r.JSON(t)["expires_at"] != s["expires_at"] {
			t.Fatalf("checkout reveal %s: %d %s", chainID, r.Status, r.Body)
		}
	}
	if n := revealAudits(t, app); n != audits {
		t.Fatalf("reveals of a session revealed from creation wrote %d audit rows", n-audits)
	}
}
