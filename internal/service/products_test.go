package service

import (
	"errors"
	"testing"

	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/store"
)

func TestSameMoney(t *testing.T) {
	usd := func(s string) *money.Units {
		t.Helper()
		u, err := money.ParseUSD(s)
		if err != nil {
			t.Fatal(err)
		}
		return &u
	}
	pay := func(wallet string, bps uint16) map[string][]store.PayoutItem {
		return map[string][]store.PayoutItem{"base-sepolia": {{WalletID: wallet, Bps: bps, Label: "Main"}}}
	}
	base := store.Product{MinAmountCents: usd("10.00"), MaxAmountCents: usd("500.00"), Payout: pay("wal_a", 10_000), Name: "A",
		SuggestedCents: []money.Units{*usd("20.00")}, Metadata: map[string]string{"k": "v"}}
	cases := []struct {
		name string
		edit func(p *store.Product)
		same bool
	}{
		{"nothing", func(*store.Product) {}, true},
		{"the same amounts in new pointers", func(p *store.Product) { p.MinAmountCents, p.MaxAmountCents = usd("10"), usd("500.0") }, true},
		{"name, suggested amounts, metadata, active", func(p *store.Product) {
			p.Name, p.SuggestedCents, p.Metadata, p.Active = "B", nil, nil, !p.Active
		}, true},
		{"the same payout with another label", func(p *store.Product) {
			p.Payout = pay("wal_a", 10_000)
			p.Payout["base-sepolia"][0].Label = "Renamed"
		}, true},
		{"an empty network is no network", func(p *store.Product) { p.Payout["solana-devnet"] = nil }, true},
		{"min", func(p *store.Product) { p.MinAmountCents = usd("10.01") }, false},
		{"max cleared", func(p *store.Product) { p.MaxAmountCents = nil }, false},
		{"fixed amount set", func(p *store.Product) { p.AmountCents = usd("10.00") }, false},
		{"another wallet", func(p *store.Product) { p.Payout = pay("wal_b", 10_000) }, false},
		{"another network", func(p *store.Product) {
			p.Payout["solana-devnet"] = []store.PayoutItem{{WalletID: "wal_s", Bps: 10_000}}
		}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			after := base
			after.Payout = store.ClonePayout(base.Payout)
			c.edit(&after)
			if got := sameMoney(base, after); got != c.same {
				t.Errorf("%s: sameMoney = %v", c.name, got)
			}
		})
	}
}

func TestLinksDecisionRequired(t *testing.T) {
	for n, want := range map[int]string{
		1: "This product has 1 active link. Choose whether to keep it active or deactivate it.",
		3: "This product has 3 active links. Choose whether to keep them active or deactivate them.",
	} {
		var e *Error
		if err := at(linksDecisionRequired(n), "links"); !errors.As(err, &e) || e.Kind != KindConflict ||
			e.Code != CodeLinksDecisionRequired || e.Message != want || e.Param != "links" {
			t.Errorf("%d: %+v", n, e)
		}
	}
}
