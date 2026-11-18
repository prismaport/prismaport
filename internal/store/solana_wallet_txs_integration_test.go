//go:build integration

package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/store"
)

// A session has one wallet transaction in flight: a new one replaces it only once it is both
// older than the window and past its last valid height, and every column reads back exactly.
func TestWalletTxReservation(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	ctx := t.Context()
	r := app.Session(app.TestKey, `{"amount":"25.00"}`, "Idempotency-Key", "wallet-tx")
	id := r.JSON(t)["id"].(string)
	const ttl = 2 * time.Minute
	if _, err := app.Store.GetWalletTx(ctx, id); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("before any: %v", err)
	}
	big := money.MustParseInteger("115792089237316195423570985008687907853269984665640564039457584007913129639935")
	first := store.WalletTx{SessionID: id, Chain: "solana-devnet", Asset: "USDC", Payer: "payerA", Gross: big, Transaction: "AQID",
		LastValidHeight: 1_000}
	if ok, err := app.Store.ReserveWalletTx(ctx, first, 850, ttl); err != nil || !ok {
		t.Fatalf("first: %v %v", ok, err)
	}
	got, err := app.Store.GetWalletTx(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	first.IssuedAt = app.Clock.Now()
	if !got.Gross.Equal(big) || got.Chain != first.Chain || got.Asset != first.Asset || got.Payer != first.Payer ||
		got.Transaction != first.Transaction || got.LastValidHeight != first.LastValidHeight || !got.IssuedAt.Equal(first.IssuedAt) {
		t.Fatalf("roundtrip %+v, want %+v", got, first)
	}
	second := first
	second.Payer, second.Gross, second.Transaction, second.LastValidHeight = "payerB", money.MustParseInteger("15000000"), "BAUG", 1_300
	for _, c := range []struct {
		name    string
		advance time.Duration
		height  uint64
		ok      bool
	}{
		{"young and valid", 0, 900, false},
		{"old but still valid by height", ttl, 1_000, false},
		{"past its height but young", ttl - time.Second, 1_001, false},
		{"old and past its height", ttl, 1_001, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			app.Clock.Set(first.IssuedAt.Add(c.advance))
			ok, err := app.Store.ReserveWalletTx(ctx, second, c.height, ttl)
			if err != nil || ok != c.ok {
				t.Fatalf("%s: %v %v, want %v", c.name, ok, err, c.ok)
			}
			w, _ := app.Store.GetWalletTx(ctx, id)
			if want := map[bool]string{false: "payerA", true: "payerB"}[c.ok]; w.Payer != want {
				t.Fatalf("%s: recorded %s", c.name, w.Payer)
			}
			if out := first.Outstanding(app.Clock.Now(), c.height, ttl); out == c.ok {
				t.Fatalf("%s: Outstanding %v disagrees with the reservation", c.name, out)
			}
		})
	}
}
