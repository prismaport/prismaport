//go:build integration

package api_test

import (
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/chainpause"
	"github.com/prismaport/prismaport/internal/checkout"
	"github.com/prismaport/prismaport/internal/service"
	"github.com/prismaport/prismaport/internal/store"
)

func checkoutOf(svc *service.Service, st *store.Store) *checkout.Checkout {
	return checkout.New(svc, st, nil)
}

// payToPairs lists the "chain/asset" of a session's (or an address's) pay_to entries.
func payToPairs(m map[string]any) []string {
	var out []string
	for _, e := range m["pay_to"].([]any) {
		p := e.(map[string]any)
		out = append(out, p["chain"].(string)+"/"+p["asset"].(string))
	}
	return out
}

// payToEntry is the pay_to entry of chain/asset in a session or address answer.
func payToEntry(t *testing.T, m map[string]any, chainID, asset string) map[string]any {
	t.Helper()
	for _, e := range m["pay_to"].([]any) {
		if p := e.(map[string]any); p["chain"] == chainID && p["asset"] == asset {
			return p
		}
	}
	t.Fatalf("no %s/%s in pay_to: %v", chainID, asset, m["pay_to"])
	return nil
}

func methodPairs(product map[string]any) []string {
	var out []string
	for _, e := range product["payment_methods"].([]any) {
		p := e.(map[string]any)
		out = append(out, p["chain"].(string)+"/"+p["asset"].(string))
	}
	return out
}

// network is the GET /v1/checkout/networks entry of chainID.
func network(t *testing.T, app *apptest.App, chainID string) map[string]any {
	t.Helper()
	r := app.Do("GET", "/v1/checkout/networks", "", nil)
	if r.Status != http.StatusOK {
		t.Fatalf("networks: %d %s", r.Status, r.Body)
	}
	for _, n := range r.JSON(t)["data"].([]any) {
		if n := n.(map[string]any); n["id"] == chainID {
			return n
		}
	}
	return nil
}

// D64: pausing a chain takes it out of everything new, after the refresh interval, while a
// session already open keeps its deposit address there, says it is paused and is still
// credited; pausing every chain refuses new sessions with chain_unavailable; resuming brings
// the chain back.
func TestChainPause(t *testing.T) {
	t.Parallel()
	// Arrange: a session revealed on Base Sepolia and one not revealed yet, before the pause.
	app, _ := dashAppWith(t, apptest.Options{}, nil)
	ctx := t.Context()
	c := login(t, app, "owner@example.com")
	open := newTestSession(t, app, app.TestKey, "25.00")["id"].(string)
	if r := app.CheckoutReveal(open, "base-sepolia"); r.Status != http.StatusOK {
		t.Fatalf("reveal: %d %s", r.Status, r.Body)
	}
	unrevealed := newTestSession(t, app, app.TestKey, "25.00")["id"].(string)
	if pauses := c.get("/v1/dashboard/me")["chain_pauses"].([]any); len(pauses) != 0 {
		t.Fatalf("pauses before any: %v", pauses)
	}

	// Act: pause Base Sepolia.
	if changed, err := app.Svc.PauseChain(ctx, "base-sepolia", "", "cli:ops"); err != nil || !changed {
		t.Fatalf("pause: %v %v", changed, err)
	}
	if changed, err := app.Svc.PauseChain(ctx, "base-sepolia", "", "cli:ops"); err != nil || changed {
		t.Fatalf("pause again: %v %v", changed, err)
	}

	// Assert: the processes serve their copy until the refresh interval has passed.
	if got := payToPairs(newTestSession(t, app, app.TestKey, "25.00")); !slices.Contains(got, "base-sepolia/USDC") {
		t.Fatalf("paused before the refresh interval: %v", got)
	}
	app.Clock.Advance(chainpause.DefaultRefresh)
	fresh := newTestSession(t, app, app.TestKey, "25.00")
	if got := payToPairs(fresh); !slices.Equal(got, []string{"solana-devnet/USDC"}) {
		t.Fatalf("a new session offers %v", got)
	}
	product := app.Do("GET", "/v1/products/"+app.Link(app.TestKey), app.TestKey, nil).JSON(t)
	if got := methodPairs(product); !slices.Equal(got, []string{"solana-devnet/USDC"}) {
		t.Fatalf("payment_methods %v", got)
	}
	n := network(t, app, "base-sepolia")
	if n["paused"] != true || n["assets"].([]any)[0].(map[string]any)["paused"] != true {
		t.Fatalf("networks: %v", n)
	}
	if n := network(t, app, "solana-devnet"); n["paused"] != nil {
		t.Fatalf("solana-devnet listed as paused: %v", n)
	}
	// The open session keeps its address, says the network is paused and offers no wallet path.
	ses := app.Do("GET", "/v1/checkout/sessions/"+open, "", nil).JSON(t)
	pt := payToEntry(t, ses, "base-sepolia", "USDC")
	if pt["paused"] != true || pt["revealed"] != true || pt["address"] == "" || pt["signed_payment"] != false {
		t.Fatalf("open session on the paused chain: %v", pt)
	}
	if pt := payToEntry(t, ses, "solana-devnet", "USDC"); pt["paused"] != nil {
		t.Fatalf("solana-devnet entry paused: %v", pt)
	}
	// A repeat reveal of the network already revealed answers; a first one is refused.
	if r := app.CheckoutReveal(open, "base-sepolia"); r.Status != http.StatusOK {
		t.Fatalf("repeat reveal: %d %s", r.Status, r.Body)
	}
	if r := app.CheckoutReveal(unrevealed, "base-sepolia"); r.Status != http.StatusConflict || r.ErrorCode(t) != service.CodeChainUnavailable {
		t.Fatalf("first reveal on a paused chain: %d %s", r.Status, r.Body)
	}
	if r := app.Do("POST", "/v1/sessions/"+unrevealed+"/reveal", app.TestKey, map[string]any{"chain": "base-sepolia"}); r.Status != http.StatusConflict ||
		r.ErrorCode(t) != service.CodeChainUnavailable {
		t.Fatalf("merchant reveal on a paused chain: %d %s", r.Status, r.Body)
	}
	if r := app.CheckoutReveal(unrevealed, "solana-devnet"); r.Status != http.StatusOK {
		t.Fatalf("reveal on another network: %d %s", r.Status, r.Body)
	}
	if err := app.Svc.CheckPayable(ctx, "base-sepolia", "USDC"); service.AsError(err).Code != service.CodeChainUnavailable {
		t.Fatalf("a wallet payment on the paused chain: %v", err)
	}
	// Money in flight lands: a payment on the paused chain still credits the open session.
	if r, res := panel(t, app, open, map[string]any{"action": "pay_exact", "chain": "base-sepolia", "asset": "USDC"}); r.Status != http.StatusOK ||
		res.Session.Status != "paid" {
		t.Fatalf("payment on the paused chain: %d %s", r.Status, r.Body)
	}
	// The dashboard of an affected merchant shows the pause.
	pauses := c.get("/v1/dashboard/me")["chain_pauses"].([]any)
	if len(pauses) != 1 {
		t.Fatalf("chain_pauses %v", pauses)
	}
	if p := pauses[0].(map[string]any); p["chain"] != "base-sepolia" || p["chain_name"] != "Base Sepolia" || p["asset"] != nil || p["livemode"] != false ||
		p["paused_at"] == "" || p["paused_by"] != nil {
		t.Fatalf("chain_pauses[0] %v", p)
	}

	// Every network paused: new sessions are refused as chain_unavailable, not as unconfigured.
	if _, err := app.Svc.PauseChain(ctx, "solana-devnet", "USDC", "cli:ops"); err != nil {
		t.Fatal(err)
	}
	app.Clock.Advance(chainpause.DefaultRefresh)
	r := app.Session(app.TestKey, map[string]any{"amount": "25.00"}, "Idempotency-Key", "all-paused")
	if r.Status != http.StatusConflict || r.ErrorCode(t) != service.CodeChainUnavailable {
		t.Fatalf("session with every network paused: %d %s", r.Status, r.Body)
	}
	if got := methodPairs(app.Do("GET", "/v1/products/"+app.Link(app.TestKey), app.TestKey, nil).JSON(t)); len(got) != 0 {
		t.Fatalf("payment_methods with every network paused: %v", got)
	}

	// Resume: the chain is offered again, the refused key works now, the strip is gone.
	for _, p := range [][2]string{{"base-sepolia", ""}, {"solana-devnet", "USDC"}} {
		if changed, err := app.Svc.ResumeChain(ctx, p[0], p[1], "cli:ops"); err != nil || !changed {
			t.Fatalf("resume %v: %v %v", p, changed, err)
		}
	}
	if changed, err := app.Svc.ResumeChain(ctx, "base-sepolia", "", "cli:ops"); err != nil || changed {
		t.Fatalf("resume again: %v %v", changed, err)
	}
	app.Clock.Advance(chainpause.DefaultRefresh)
	r = app.Session(app.TestKey, map[string]any{"amount": "25.00"}, "Idempotency-Key", "all-paused")
	if r.Status != http.StatusOK || !slices.Equal(payToPairs(r.JSON(t)), []string{"base-sepolia/USDC", "solana-devnet/USDC"}) {
		t.Fatalf("after the resume: %d %s", r.Status, r.Body)
	}
	if pauses := c.get("/v1/dashboard/me")["chain_pauses"].([]any); len(pauses) != 0 {
		t.Fatalf("pauses after the resume: %v", pauses)
	}
	var audited int
	if err := app.Store.Pool().QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE merchant_id IS NULL AND actor = 'cli:ops'
		AND action IN ('chain.paused', 'chain.resumed')`).Scan(&audited); err != nil || audited != 4 {
		t.Fatalf("audit rows %d %v", audited, err)
	}
}

// D64: pausing one asset leaves the chain's other assets offered.
func TestAssetPause(t *testing.T) {
	t.Parallel()
	// Arrange
	refresh := time.Duration(0)
	app := apptest.New(t, apptest.Options{PauseRefresh: &refresh, Checkout: checkoutOf})
	ctx := t.Context()

	// Act
	if _, err := app.Svc.PauseChain(ctx, "solana", "USDT", "cli:ops"); err != nil {
		t.Fatal(err)
	}

	// Assert
	product := app.Do("GET", "/v1/products/"+app.Link(app.LiveKey), app.LiveKey, nil).JSON(t)
	if got := methodPairs(product); !slices.Equal(got, []string{"base/USDC", "solana/USDC"}) {
		t.Fatalf("payment_methods %v", got)
	}
	r := app.Session(app.LiveKey, map[string]any{"amount": "25.00"}, "Idempotency-Key", "asset-paused")
	if r.Status != http.StatusOK || !slices.Equal(payToPairs(r.JSON(t)), []string{"base/USDC", "solana/USDC"}) {
		t.Fatalf("session: %d %s", r.Status, r.Body)
	}
	n := network(t, app, "solana")
	assets := n["assets"].([]any)
	if n["paused"] != nil || assets[0].(map[string]any)["paused"] != nil || assets[1].(map[string]any)["paused"] != true {
		t.Fatalf("networks: %v", n)
	}
	if err := app.Svc.CheckPayable(ctx, "solana", "USDC"); err != nil {
		t.Fatalf("the other asset: %v", err)
	}
	if err := app.Svc.CheckPayable(ctx, "solana", "USDT"); service.AsError(err).Code != service.CodeChainUnavailable {
		t.Fatalf("the paused asset: %v", err)
	}
	for _, bad := range [][2]string{{"solana", "DAI"}, {"sol", ""}} {
		if _, err := app.Svc.PauseChain(ctx, bad[0], bad[1], "cli:ops"); service.AsError(err).Code != service.CodeInvalidRequest {
			t.Fatalf("pause %v: %v", bad, err)
		}
	}
}

// D64 over D62: a chain switched off in the environment can be neither paused nor resumed, and a
// pause left in the database never brings it back.
func TestPauseOffChain(t *testing.T) {
	t.Parallel()
	// Arrange: a pause of solana-devnet left from when it was served.
	refresh := time.Duration(0)
	app := apptest.New(t, apptest.Options{Registry: chain.Pilot().Without("solana", "solana-devnet"), PauseRefresh: &refresh, Checkout: checkoutOf})
	ctx := t.Context()
	if _, err := app.Store.PauseChain(ctx, "solana-devnet", "", "cli:ops"); err != nil {
		t.Fatal(err)
	}

	// Act
	_, pauseErr := app.Svc.PauseChain(ctx, "solana", "", "cli:ops")
	_, resumeErr := app.Svc.ResumeChain(ctx, "solana-devnet", "", "cli:ops")

	// Assert
	for _, err := range []error{pauseErr, resumeErr} {
		if service.AsError(err).Code != service.CodeInvalidRequest {
			t.Fatalf("an off chain named: %v", err)
		}
	}
	statuses, stale, err := app.Svc.ChainStatuses(ctx)
	if err != nil || len(stale) != 1 || stale[0].Chain != "solana-devnet" {
		t.Fatalf("stale pauses: %v %v", stale, err)
	}
	for _, s := range statuses {
		if s.Chain != "base" && s.Chain != "base-sepolia" || s.Pause != nil {
			t.Fatalf("status %+v", s)
		}
	}
	if network(t, app, "solana-devnet") != nil {
		t.Fatal("an off chain listed")
	}
	if got := payToPairs(newTestSession(t, app, app.TestKey, "25.00")); !slices.Equal(got, []string{"base-sepolia/USDC"}) {
		t.Fatalf("session: %v", got)
	}
}

// D64 and customer addresses: an existing address keeps the paused network, says so and keeps
// it in the sessions opened on it (they are what watches it); a new address leaves it out, and
// none is issued with every network paused.
func TestChainPauseCustomerAddress(t *testing.T) {
	t.Parallel()
	// Arrange
	refresh := time.Duration(0)
	app := apptest.New(t, apptest.Options{PauseRefresh: &refresh, Checkout: checkoutOf})
	ctx := t.Context()
	product := app.Link(app.TestKey)
	before := newAddress(t, app, app.TestKey, map[string]any{"customer_id": "before", "product_id": product}, http.StatusCreated)["id"].(string)

	// Act
	if _, err := app.Svc.PauseChain(ctx, "base-sepolia", "", "cli:ops"); err != nil {
		t.Fatal(err)
	}

	// Assert
	page := publicAddress(t, app, before, "")
	if pt := payToEntry(t, page, "base-sepolia", "USDC"); pt["paused"] != true || pt["address"] == "" {
		t.Fatalf("address page: %v", pt)
	}
	ses := mustOpenPublic(t, app, before)
	if got := payToPairs(ses); !slices.Equal(got, []string{"base-sepolia/USDC", "solana-devnet/USDC"}) {
		t.Fatalf("a session on the address offers %v", got)
	}
	if pt := payToEntry(t, ses, "base-sepolia", "USDC"); pt["paused"] != true || pt["address"] == "" {
		t.Fatalf("the paused entry of a session on the address: %v", pt)
	}
	after := newAddress(t, app, app.TestKey, map[string]any{"customer_id": "after", "product_id": product}, http.StatusCreated)
	if got := payToPairs(after); !slices.Equal(got, []string{"solana-devnet/USDC"}) {
		t.Fatalf("a new address offers %v", got)
	}
	if _, err := app.Svc.PauseChain(ctx, "solana-devnet", "", "cli:ops"); err != nil {
		t.Fatal(err)
	}
	r := app.Do("POST", "/v1/customer_addresses", app.TestKey, map[string]any{"customer_id": "none", "product_id": product})
	if r.Status != http.StatusConflict || r.ErrorCode(t) != service.CodeChainUnavailable {
		t.Fatalf("an address with every network paused: %d %s", r.Status, r.Body)
	}
}
