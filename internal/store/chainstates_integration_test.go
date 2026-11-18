//go:build integration

package store_test

import (
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/store"
)

// ChainStates answers every chain asked for in one query: the scan inputs of each, its latest
// reveal and whether its sender has work (a job due, submitted or with its finality check due).
func TestChainStatesOfEveryChainInOneQuery(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	ctx := t.Context()
	q := store.ChainStatesQuery{Chains: []string{"base", "base-sepolia", "solana-devnet"}, LateFirst: 20 * time.Minute,
		LateWatch: 23 * time.Hour, PayerActive: 30 * time.Second, SolanaWalletWindow: store.SolanaWalletWindow}
	states := func() map[string]store.ChainScanState {
		t.Helper()
		q.Now = app.Clock.Now()
		all, err := app.Store.ChainStates(ctx, q)
		if err != nil || len(all) != 3 {
			t.Fatalf("states %v %v", all, err)
		}
		return all
	}
	for id, st := range states() {
		if st != (store.ChainScanState{}) {
			t.Fatalf("%s before anything: %+v", id, st)
		}
	}
	id := newSession(t, app, "5.00", "states")
	revealedAt := app.Clock.Now()
	app.Reveal(app.TestKey, id, "base-sepolia")
	total := money.FromUint64(5_000_000)
	if _, _, err := app.Store.InsertTxJob(ctx, store.TxJob{Chain: "solana-devnet", Kind: store.JobFlush, Ref: "due", SessionID: id,
		Asset: "USDC", Address: "x", PlannedTotal: &total, To: "y", Calldata: []byte{1}}); err != nil {
		t.Fatal(err)
	}
	all := states()
	if st := all["base-sepolia"]; !st.Open || st.SenderDue || st.LastReveal == nil || !st.LastReveal.Equal(revealedAt) {
		t.Fatalf("base-sepolia: %+v", st)
	}
	if st := all["solana-devnet"]; st.Open || !st.SenderDue || st.LastReveal != nil {
		t.Fatalf("solana-devnet: %+v", st)
	}
	if st := all["base"]; st != (store.ChainScanState{}) {
		t.Fatalf("base: %+v", st)
	}
	// A job waiting for its retry time is no work yet.
	if _, err := app.Store.Pool().Exec(ctx, `UPDATE chain_tx_jobs SET next_attempt_at = $1`, app.Clock.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if st := states()["solana-devnet"]; st.SenderDue {
		t.Fatalf("a job not due yet: %+v", st)
	}
	app.Clock.Advance(time.Minute)
	if st := states()["solana-devnet"]; !st.SenderDue {
		t.Fatalf("a job due: %+v", st)
	}
}

// The credit head only moves forward: a tick whose provider is behind the last reading does not
// take a payment's confirmations back.
func TestCreditHeadOnlyMovesForward(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	ctx := t.Context()
	for _, c := range []struct{ set, want uint64 }{{10, 10}, {7, 10}, {12, 12}} {
		if err := app.Store.SetCreditHead(ctx, "base", c.set); err != nil {
			t.Fatal(err)
		}
		if h, found, err := app.Store.CreditHead(ctx, "base"); err != nil || !found || h != c.want {
			t.Fatalf("after %d: %d %v %v, want %d", c.set, h, found, err, c.want)
		}
	}
}
