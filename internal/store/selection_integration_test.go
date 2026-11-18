//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/session"
	"github.com/prismaport/prismaport/internal/store"
)

func newSession(t *testing.T, app *apptest.App, amount, idem string) string {
	t.Helper()
	r := app.Session(app.TestKey, `{"amount":"`+amount+`"}`, "Idempotency-Key", idem)
	var v struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(r.Body, &v); err != nil || r.Status != 200 {
		t.Fatalf("create: %d %s", r.Status, r.Body)
	}
	return v.ID
}

func watchedSessions(t *testing.T, app *apptest.App, chainID string, fam chain.Family) map[string]bool {
	t.Helper()
	list := app.Store.WatchSet
	if fam == chain.Solana {
		list = func(ctx context.Context, chainID string, now time.Time) ([]store.WatchEntry, error) {
			return app.Store.SolanaWatchSet(ctx, chainID, now, store.SolanaWalletWindow)
		}
	}
	entries, err := list(t.Context(), chainID, app.Clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, e := range entries {
		out[e.SessionID] = true
	}
	return out
}

func lateSessions(t *testing.T, app *apptest.App, chainID string, fam chain.Family) map[string]bool {
	t.Helper()
	entries, err := app.Store.LateEntries(t.Context(), lateQuery(app, chainID, fam), 100)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, e := range entries {
		out[e.SessionID] = true
	}
	return out
}

// D48: only revealed networks are watched; a chain with nothing revealed stays idle. The first
// reveal starts the pay window, later reveals do not move it.
func TestOnlyRevealedNetworksAreWatched(t *testing.T) {
	t.Parallel()
	app := solanaApp(t)
	ctx := t.Context()
	created := app.Clock.Now()
	id := newSession(t, app, "25.00", "sel")
	ses, err := app.Store.LoadSession(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !ses.State.ExpiresAt.Equal(created.Add(time.Hour)) || ses.PayWindow != 30*time.Minute || ses.PaymentStartedAt != nil || len(ses.Reveals) != 0 {
		t.Fatalf("new session expires %s, pay window %s, started %v, reveals %v", ses.State.ExpiresAt, ses.PayWindow, ses.PaymentStartedAt, ses.Reveals)
	}
	for _, c := range []struct {
		id  string
		fam chain.Family
	}{{"base-sepolia", chain.EVM}, {"solana-devnet", chain.Solana}} {
		t.Run(c.id, func(t *testing.T) {
			if w := watchedSessions(t, app, c.id, c.fam); len(w) != 0 {
				t.Fatalf("%s watches %v before any reveal", c.id, w)
			}
			st, err := app.Store.ScanState(ctx, lateQuery(app, c.id, c.fam))
			if err != nil || st != (store.ChainScanState{}) {
				t.Fatalf("%s state %+v %v: want idle", c.id, st, err)
			}
		})
	}

	app.Clock.Advance(10 * time.Minute)
	revealedAt := app.Clock.Now()
	app.Reveal(app.TestKey, id, "base-sepolia")
	if ses, err = app.Store.LoadSession(ctx, id); err != nil {
		t.Fatal(err)
	}
	if ses.PaymentStartedAt == nil || !ses.PaymentStartedAt.Equal(revealedAt) || !ses.State.ExpiresAt.Equal(revealedAt.Add(30*time.Minute)) {
		t.Fatalf("after the reveal: started %v, expires %s", ses.PaymentStartedAt, ses.State.ExpiresAt)
	}
	if r := ses.Revealed("base-sepolia"); r == nil || r.Source != store.RevealMerchantAPI || !r.RevealedAt.Equal(revealedAt) {
		t.Fatalf("reveal row %+v", r)
	}
	if !watchedSessions(t, app, "base-sepolia", chain.EVM)[id] || len(watchedSessions(t, app, "solana-devnet", chain.Solana)) != 0 {
		t.Fatal("the revealed network is not the only one watched")
	}
	if st, _ := app.Store.ScanState(ctx, lateQuery(app, "base-sepolia", chain.EVM)); !st.Open {
		t.Fatalf("base-sepolia state %+v: want open", st)
	}
	if st, _ := app.Store.ScanState(ctx, lateQuery(app, "solana-devnet", chain.Solana)); st != (store.ChainScanState{}) {
		t.Fatalf("solana-devnet state %+v: want idle", st)
	}
	// The reveal time is where the first rescan of the address starts from (D49).
	if es, err := app.Store.WatchSet(ctx, "base-sepolia", app.Clock.Now()); err != nil || len(es) != 1 || es[0].RevealedAt == nil ||
		!es[0].RevealedAt.Equal(revealedAt) {
		t.Fatalf("watch entries %+v %v: want revealed_at %s", es, err, revealedAt)
	}

	// Another network adds itself without moving the deadline; the first stays watched.
	app.Clock.Advance(5 * time.Minute)
	app.Reveal(app.TestKey, id, "solana-devnet")
	if ses, err = app.Store.LoadSession(ctx, id); err != nil {
		t.Fatal(err)
	}
	if !ses.State.ExpiresAt.Equal(revealedAt.Add(30*time.Minute)) || len(ses.Reveals) != 2 {
		t.Fatalf("second reveal moved expiry to %s, reveals %+v", ses.State.ExpiresAt, ses.Reveals)
	}
	if !watchedSessions(t, app, "base-sepolia", chain.EVM)[id] || !watchedSessions(t, app, "solana-devnet", chain.Solana)[id] {
		t.Fatal("both revealed networks must be watched")
	}
	// A repeated reveal changes nothing.
	again, revealed, err := app.Store.RevealNetwork(ctx, store.RevealInput{SessionID: id, Chain: "base-sepolia", Source: store.RevealCheckout},
		func(*store.Session) error { return nil })
	if err != nil || revealed || len(again.Reveals) != 2 || again.Revealed("base-sepolia").Source != store.RevealMerchantAPI {
		t.Fatalf("repeat: revealed %v, %+v %v", revealed, again.Reveals, err)
	}
	if at, err := app.Store.WalletTxAt(ctx, id); err != nil || at != nil {
		t.Fatalf("wallet_tx_at %v %v without a wallet reveal", at, err)
	}
	if _, _, err := app.Store.RevealNetwork(ctx, store.RevealInput{SessionID: id, Chain: "base-sepolia", Source: store.RevealWallet},
		func(*store.Session) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if at, err := app.Store.WalletTxAt(ctx, id); err != nil || at == nil || !at.Equal(app.Clock.Now()) {
		t.Fatalf("wallet_tx_at %v %v after a wallet reveal", at, err)
	}
}

// Late checks and "Check again" only count revealed networks: an unrevealed one never holds a
// request back.
func TestRecheckIgnoresUnrevealedNetworks(t *testing.T) {
	t.Parallel()
	app := solanaApp(t)
	ctx := t.Context()
	id := newSession(t, app, "25.00", "partial")
	app.Reveal(app.TestKey, id, "base-sepolia")
	app.Clock.Advance(2 * time.Hour)
	if l := lateSessions(t, app, "solana-devnet", chain.Solana); len(l) != 0 {
		t.Fatalf("late checks of an unrevealed network: %v", l)
	}
	if !lateSessions(t, app, "base-sepolia", chain.EVM)[id] {
		t.Fatal("the revealed network gets no late check")
	}
	app.Clock.Advance(48 * time.Hour)
	at := app.Clock.Now().Truncate(time.Microsecond)
	if _, err := app.Store.Pool().Exec(ctx, `UPDATE sessions SET recheck_requested_at = $2 WHERE id = $1`, id, at); err != nil {
		t.Fatal(err)
	}
	evm, err := app.Store.LateEntries(ctx, lateQuery(app, "base-sepolia", chain.EVM), 10)
	if err != nil || len(evm) == 0 {
		t.Fatalf("evm entries %+v %v", evm, err)
	}
	for _, e := range evm {
		if err := app.Store.MarkChecked(ctx, e, store.EVMCheck{FirstBlock: 1, RecheckedFor: e.RecheckRequestedAt}); err != nil {
			t.Fatal(err)
		}
	}
	if ok, err := app.Store.ClearRecheck(ctx, id, at); !ok || err != nil {
		t.Fatalf("an unrevealed network held the request back: %v", err)
	}
}

// pay records a confirmed payment of amount (USDC on chainID) and answers its id.
func pay(t *testing.T, app *apptest.App, id, chainID, amount string, n int) string {
	t.Helper()
	ctx := t.Context()
	ses, err := app.Store.LoadSession(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var addr string
	for _, pt := range ses.PayTo {
		if pt.Chain == chainID && pt.Asset == "USDC" {
			addr = pt.Address
		}
	}
	units, err := money.ParseDecimal(amount, 6)
	if err != nil {
		t.Fatal(err)
	}
	pid := fmt.Sprintf("pay_%s_%d", id, n)
	now := app.Clock.Now()
	for _, cmd := range []store.Command{
		func(s session.State) (session.State, []session.Event, error) {
			return s.DetectPayment(session.NewPayment{ID: pid, Chain: chainID, Asset: "USDC", Address: addr, TxHash: fmt.Sprintf("0x%064d", n),
				Amount: units}, now)
		},
		func(s session.State) (session.State, []session.Event, error) { return s.ConfirmPayment(pid, now) },
	} {
		if _, _, err := app.Store.ApplySessionCommand(ctx, id, cmd); err != nil {
			t.Fatal(err)
		}
	}
	return pid
}

func finalize(t *testing.T, app *apptest.App, id, pid string) {
	t.Helper()
	now := app.Clock.Now()
	if _, _, err := app.Store.ApplySessionCommand(t.Context(), id, func(s session.State) (session.State, []session.Event, error) {
		return s.FinalizePayment(pid, now)
	}); err != nil {
		t.Fatal(err)
	}
}

// D47: a session paid exactly leaves the block scan and late checks once its payments are final;
// overpaid and partially paid sessions keep their late checks; "Check again" still reaches a
// settled session.
func TestSettledSessionLeavesAutomaticChecks(t *testing.T) {
	t.Parallel()
	app := solanaApp(t)
	ctx := t.Context()
	paid := newSession(t, app, "25.00", "paid")
	over := newSession(t, app, "25.00", "over")
	partial := newSession(t, app, "25.00", "partial")
	for _, id := range []string{paid, over, partial} {
		app.Reveal(app.TestKey, id, "base-sepolia")
	}
	p := pay(t, app, paid, "base-sepolia", "25", 1)
	if !watchedSessions(t, app, "base-sepolia", chain.EVM)[paid] {
		t.Fatal("a paid session whose payment is not final left the block scan")
	}
	app.Clock.Advance(time.Minute)
	finalize(t, app, paid, p)
	if watchedSessions(t, app, "base-sepolia", chain.EVM)[paid] {
		t.Fatal("a settled session is still in the block scan")
	}
	for i, id := range []string{over, partial} {
		amount := []string{"26", "10"}[i]
		finalize(t, app, id, pay(t, app, id, "base-sepolia", amount, 10+i))
	}

	app.Clock.Advance(2 * time.Hour)
	late := lateSessions(t, app, "base-sepolia", chain.EVM)
	if late[paid] || !late[over] || !late[partial] {
		t.Fatalf("late checks %v: want overpaid and partially paid only", late)
	}
	// With only the settled session left, nothing is due on the chain.
	if _, err := app.Store.Pool().Exec(ctx, `UPDATE sessions SET status = 'paid' WHERE id = ANY($1)`, []string{over, partial}); err != nil {
		t.Fatal(err)
	}
	if st, err := app.Store.ScanState(ctx, lateQuery(app, "base-sepolia", chain.EVM)); err != nil || st.LateDue != nil || st.Open {
		t.Fatalf("state %+v %v: no late check is due", st, err)
	}
	at := app.Clock.Now().Truncate(time.Microsecond)
	if _, err := app.Store.Pool().Exec(ctx, `UPDATE sessions SET recheck_requested_at = $2 WHERE id = $1`, paid, at); err != nil {
		t.Fatal(err)
	}
	if !lateSessions(t, app, "base-sepolia", chain.EVM)[paid] {
		t.Fatal("Check again does not reach a settled session")
	}
}
