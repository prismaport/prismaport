//go:build integration

package api_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/checkout"
	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/service"
	"github.com/prismaport/prismaport/internal/store"
)

// The payer's view counts a detected payment's confirmations up to the head the detector last
// credited on, so the checkout can show progress, and drops the count once the payment is confirmed.
func TestCheckoutConfirmations(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{Checkout: func(svc *service.Service, st *store.Store) *checkout.Checkout {
		reg, _ := svc.Registry().Get("base-sepolia")
		return checkout.New(svc, st, map[string]checkout.Chain{"base-sepolia": {Chain: reg, Confirmations: 10}})
	}})
	r := app.Session(app.TestKey, map[string]any{"amount": "25.00"}, "Idempotency-Key", "confirmations")
	if r.Status != http.StatusOK {
		t.Fatalf("session: %d %s", r.Status, r.Body)
	}
	id := r.JSON(t)["id"].(string)
	ses := app.Reveal(app.TestKey, id, "base-sepolia").JSON(t)
	var p map[string]any
	for _, pt := range ses["pay_to"].([]any) {
		if m := pt.(map[string]any); m["chain"] == "base-sepolia" && m["asset"] == "USDC" {
			p = m
		}
	}
	if p == nil {
		t.Fatalf("no base-sepolia USDC option: %v", ses["pay_to"])
	}
	amount, err := money.ParseDecimal(p["amount"].(string), 6)
	if err != nil {
		t.Fatal(err)
	}
	pid, _, err := app.Svc.RecordPayment(t.Context(), service.Observation{SessionID: id, Chain: "base-sepolia", Asset: "USDC",
		Address: p["address"].(string), TxHash: fmt.Sprintf("0x%064x", 1), Amount: amount})
	if err != nil {
		t.Fatal(err)
	}
	payment := func() map[string]any {
		t.Helper()
		r := app.Do("GET", "/v1/checkout/sessions/"+id, "", nil)
		if r.Status != http.StatusOK {
			t.Fatalf("checkout view: %d %s", r.Status, r.Body)
		}
		return r.JSON(t)["payments"].([]any)[0].(map[string]any)
	}
	hash := func(b byte) string { return fmt.Sprintf("0x%064x", b) }

	// Not tracked on chain yet: nothing to count.
	if got := payment(); got["status"] != "detected" || got["confirmations"] != nil || got["confirmations_required"] != nil {
		t.Fatalf("an untracked payment: %v", got)
	}
	if err := app.Store.UpsertChainPayment(t.Context(), store.ChainPayment{PaymentID: pid, SessionID: id, Chain: "base-sepolia",
		Asset: "USDC", Kind: "transfer", BlockNumber: 100, BlockHash: hash(1)}); err != nil {
		t.Fatal(err)
	}
	if got := payment(); got["confirmations"] != nil {
		t.Fatalf("a chain never credited on has no head to count from: %v", got)
	}
	for _, c := range []struct {
		head uint64
		want float64
	}{{105, 6}, {140, 10}} {
		if err := app.Store.SetCreditHead(t.Context(), "base-sepolia", c.head); err != nil {
			t.Fatal(err)
		}
		if got := payment(); got["confirmations"] != c.want || got["confirmations_required"] != float64(10) {
			t.Fatalf("head %d: %v", c.head, got)
		}
	}
	if _, err := app.Svc.ConfirmPayment(t.Context(), id, pid); err != nil {
		t.Fatal(err)
	}
	if got := payment(); got["status"] != "confirmed" || got["confirmations"] != nil {
		t.Fatalf("a confirmed payment: %v", got)
	}
}
