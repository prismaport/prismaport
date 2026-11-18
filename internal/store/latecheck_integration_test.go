//go:build integration

package store_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/chainsettle"
	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/payout"
	"github.com/prismaport/prismaport/internal/store"
)

// settle records a settlement of gross for the session's pay_to on chain, split by the session's
// own rule, as the flusher would after a flush.
func settle(t *testing.T, app *apptest.App, sessionID, chainID string, gross money.Units, tx byte) {
	t.Helper()
	ctx := t.Context()
	ses, err := app.Store.LoadSession(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	for _, pt := range ses.PayTo {
		if pt.Chain != chainID || pt.Asset != "USDC" {
			continue
		}
		rule, err := payout.Compute(gross, pt.Decimals, pt.Fee, payout.Bps(pt.Recipients))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := app.Store.RecordSettlement(ctx, sessionID, store.Settlement{Chain: chainID, Asset: "USDC", Gross: gross, Fee: rule.Fee,
			Net: rule.Net, Shares: rule.Shares, TxHash: "0x" + strings.Repeat(string("0123456789abcdef"[tx%16]), 64), SettledAt: app.Clock.Now()}, chainsettle.Verify); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Fatalf("no USDC pay_to on %s", chainID)
}

// lateSession creates a session and moves the clock an hour past its expiry, inside its watch.
func lateSession(t *testing.T, app *apptest.App) string {
	t.Helper()
	r := app.RevealedSession(app.TestKey, `{"amount":"25.00"}`, "Idempotency-Key", "late")
	var v struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(r.Body, &v); err != nil || r.Status != 200 {
		t.Fatalf("create: %d %s", r.Status, r.Body)
	}
	app.Clock.Advance(2 * time.Hour)
	return v.ID
}

func lateQuery(app *apptest.App, chainID string, fam chain.Family) store.ScanQuery {
	return store.ScanQuery{Chain: chainID, Family: fam, Now: app.Clock.Now(), LateFirst: 20 * time.Minute, LateWatch: 23 * time.Hour}
}

// evmLateEntry is the session's USDC entry on base-sepolia, due a late check (checks below leave
// next_check_at alone, so it stays due).
func evmLateEntry(t *testing.T, app *apptest.App) store.WatchEntry {
	t.Helper()
	cands, err := app.Store.LateEntries(t.Context(), lateQuery(app, "base-sepolia", chain.EVM), 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cands {
		if c.Asset == "USDC" {
			return c
		}
	}
	t.Fatalf("no USDC late entry: %+v", cands)
	return store.WatchEntry{}
}

func balance(u *money.Units) string {
	if u == nil {
		return "none"
	}
	return u.String()
}

func checked(b money.Units) store.EVMCheck {
	block := uint64(10)
	return store.EVMCheck{FirstBlock: 1, Block: &block, Balance: &b}
}

// A settlement empties the address, so the next deposit must count as a balance change even
// when it equals the balance seen before; a check that read the row before the settlement must
// not write its stale balance back.
func TestSettlementResetsTheEVMLastBalance(t *testing.T) {
	t.Parallel()
	app := solanaApp(t)
	ctx := t.Context()
	id := lateSession(t, app)
	deposit := money.FromUint64(25_000_000)
	if err := app.Store.MarkChecked(ctx, evmLateEntry(t, app), checked(deposit)); err != nil {
		t.Fatal(err)
	}
	stale := evmLateEntry(t, app)
	if got := balance(stale.LastBalance); got != deposit.String() || stale.CheckedBlock == nil || *stale.CheckedBlock != 10 {
		t.Fatalf("last balance %s, checked block %v", got, stale.CheckedBlock)
	}

	app.Clock.Advance(time.Minute)
	settle(t, app, id, "base-sepolia", deposit, 1)
	afterSettle := balance(evmLateEntry(t, app).LastBalance)
	if err := app.Store.MarkChecked(ctx, stale, checked(deposit)); err != nil {
		t.Fatal(err)
	}
	afterStale := balance(evmLateEntry(t, app).LastBalance)

	if afterSettle != "0" || afterStale != "0" {
		t.Fatalf("last balance after the settlement %s, after a check that started before it %s; want 0 both", afterSettle, afterStale)
	}
	if err := app.Store.MarkChecked(ctx, evmLateEntry(t, app), checked(deposit)); err != nil {
		t.Fatal(err)
	}
	if got := balance(evmLateEntry(t, app).LastBalance); got != deposit.String() {
		t.Fatalf("a check after the settlement stored %s", got)
	}
}

func TestSettlementResetsTheSolanaLastBalance(t *testing.T) {
	t.Parallel()
	app := solanaApp(t)
	ctx := t.Context()
	id := lateSession(t, app)
	// The deposit row is the session's real token account: a settlement resets the row of the
	// account it emptied.
	ses, err := app.Store.LoadSession(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	ata := ""
	for _, p := range ses.PayTo {
		if p.Chain == "solana-devnet" && p.Asset == "USDC" {
			ata = p.TokenAccount()
		}
	}
	if ata == "" {
		t.Fatal("no Solana deposit account")
	}
	rows := []store.SolanaWatch{{Chain: "solana-devnet", Address: ata, Kind: store.SolanaDeposit, SessionID: id, Asset: "USDC"}}
	read := func() store.SolanaWatch {
		t.Helper()
		got, err := app.Store.EnsureSolanaWatch(ctx, rows)
		if err != nil {
			t.Fatal(err)
		}
		return got[ata]
	}
	deposit := money.FromUint64(25_000_000)
	mark := func(prev *time.Time) {
		t.Helper()
		if err := app.Store.MarkSolanaChecked(ctx, "solana-devnet", ata, store.SolanaCheck{SessionID: id, Balance: &deposit, Prev: prev}); err != nil {
			t.Fatal(err)
		}
	}
	mark(read().CheckedAt)
	stale := read()
	if got := balance(stale.LastBalance); got != deposit.String() {
		t.Fatalf("last balance %s", got)
	}

	app.Clock.Advance(time.Minute)
	settle(t, app, id, "solana-devnet", deposit, 2)
	afterSettle := balance(read().LastBalance)
	mark(stale.CheckedAt)
	afterStale := balance(read().LastBalance)

	if afterSettle != "0" || afterStale != "0" {
		t.Fatalf("last balance after the settlement %s, after a check that started before it %s; want 0 both", afterSettle, afterStale)
	}
	mark(read().CheckedAt)
	if got := balance(read().LastBalance); got != deposit.String() {
		t.Fatalf("a check after the settlement stored %s", got)
	}
}

// A "Check again" request is cleared only once every watched deposit entry of the session, on
// every chain, was checked for it; a newer request stays.
func TestRecheckClearedOnceEveryChainChecked(t *testing.T) {
	t.Parallel()
	app := solanaApp(t)
	ctx := t.Context()
	id := lateSession(t, app)
	app.Clock.Advance(48 * time.Hour) // past the watch: only the request makes the entries due
	at := app.Clock.Now().Truncate(time.Microsecond)
	if _, err := app.Store.Pool().Exec(ctx, `UPDATE sessions SET recheck_requested_at = $2 WHERE id = $1`, id, at); err != nil {
		t.Fatal(err)
	}
	evm, err := app.Store.LateEntries(ctx, lateQuery(app, "base-sepolia", chain.EVM), 10)
	if err != nil || len(evm) == 0 {
		t.Fatalf("evm entries %+v %v", evm, err)
	}
	sol, err := app.Store.LateEntries(ctx, lateQuery(app, "solana-devnet", chain.Solana), 10)
	if err != nil || len(sol) != 1 || !sol[0].RecheckDue(nil) {
		t.Fatalf("solana entries %+v %v", sol, err)
	}
	st, err := app.Store.ScanState(ctx, lateQuery(app, "base-sepolia", chain.EVM))
	if err != nil || !st.Recheck || st.LateDue != nil {
		t.Fatalf("state %+v %v", st, err)
	}
	for _, e := range evm {
		if err := app.Store.MarkChecked(ctx, e, store.EVMCheck{FirstBlock: 1, RecheckedFor: e.RecheckRequestedAt}); err != nil {
			t.Fatal(err)
		}
	}
	if ok, err := app.Store.ClearRecheck(ctx, id, at); ok || err != nil {
		t.Fatalf("cleared before the Solana entry was checked: %v", err)
	}
	if st, _ := app.Store.ScanState(ctx, lateQuery(app, "base-sepolia", chain.EVM)); st.Recheck {
		t.Fatal("base-sepolia still has a recheck due")
	}
	const ata = "7UeZhudGACwJb3TSY8hdo4bJTpHiCTFv2wsmLGVP9kQz"
	if _, err := app.Store.EnsureSolanaWatch(ctx, []store.SolanaWatch{{Chain: "solana-devnet", Address: ata, Kind: store.SolanaDeposit,
		SessionID: id, Asset: "USDC"}}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := app.Store.ClearRecheck(ctx, id, at.Add(-time.Second)); ok {
		t.Fatal("cleared a request the check did not read")
	}
	if err := app.Store.MarkSolanaChecked(ctx, "solana-devnet", ata, store.SolanaCheck{SessionID: id, RecheckedFor: &at}); err != nil {
		t.Fatal(err)
	}
	if ok, err := app.Store.ClearRecheck(ctx, id, at); !ok || err != nil {
		t.Fatalf("not cleared: %v", err)
	}
	if st, _ := app.Store.ScanState(ctx, lateQuery(app, "solana-devnet", chain.Solana)); st.Recheck {
		t.Fatal("recheck still due after it was cleared")
	}
}
