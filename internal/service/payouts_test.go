package service

import (
	"errors"
	"testing"

	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/store"
)

func TestPayoutItemsShape(t *testing.T) {
	base := chain.Chain{ID: "base", Family: chain.EVM}
	in := func(pairs ...string) []PayoutItemInput {
		var out []PayoutItemInput
		for i := 0; i+1 < len(pairs); i += 2 {
			out = append(out, PayoutItemInput{WalletID: pairs[i], Share: pairs[i+1]})
		}
		return out
	}
	got, err := payoutItems(base, in("wal_b", "33.34", "wal_a", "66.66"))
	if err != nil || len(got) != 2 || got[0].WalletID != "wal_b" || got[0].Bps != 3334 || got[1].Bps != 6666 || got[1].Family != chain.EVM {
		t.Fatalf("order kept, shares in bps, family set: %+v %v", got, err)
	}
	if got, err := payoutItems(base, nil); err != nil || got != nil {
		t.Fatalf("an empty list takes no payments: %+v %v", got, err)
	}
	for name, c := range map[string]struct {
		in    []PayoutItemInput
		msg   string
		param string
	}{
		"90%":         {in("wal_a", "60", "wal_b", "30"), "Shares must add up to 100%. Now 90%.", "payout.base"},
		"100.5%":      {in("wal_a", "70.5", "wal_b", "30"), "Shares must add up to 100%. Now 100.5%.", "payout.base"},
		"six":         {in("a", "10", "b", "10", "c", "10", "d", "10", "e", "10", "f", "50"), "A split pays 1 to 5 wallets.", "payout.base"},
		"a repeat":    {in("wal_a", "50", "wal_a", "50"), "Each wallet can appear only once in a split.", "payout.base[1].wallet_id"},
		"no wallet":   {in("", "100"), "payout.base[0].wallet_id is required", "payout.base[0].wallet_id"},
		"zero":        {in("wal_a", "100", "wal_b", "0"), "", "payout.base[1].share"},
		"3 decimals":  {in("wal_a", "99.999", "wal_b", "0.001"), "", "payout.base[0].share"},
		"not a share": {in("wal_a", "all"), "", "payout.base[0].share"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := payoutItems(base, c.in)
			var e *Error
			if !errors.As(err, &e) || e.Code != CodeInvalidRequest || (c.msg != "" && e.Message != c.msg) || e.Param != c.param {
				t.Errorf("%s: %+v", name, err)
			}
		})
	}
}

func TestSamePayout(t *testing.T) {
	a := map[string][]store.PayoutItem{"base": {{WalletID: "w1", Bps: 7000}, {WalletID: "w2", Bps: 3000}}}
	b := store.ClonePayout(a)
	if !store.SamePayout(a, b) {
		t.Fatal("a clone differs")
	}
	b["base"][0].Bps = 6000
	if store.SamePayout(a, b) || a["base"][0].Bps != 7000 {
		t.Fatal("a change to the clone reached the original or went unseen")
	}
	if store.SamePayout(a, map[string][]store.PayoutItem{"base": {{WalletID: "w2", Bps: 3000}, {WalletID: "w1", Bps: 7000}}}) {
		t.Fatal("the order is part of a payout")
	}
	if store.SamePayout(a, map[string][]store.PayoutItem{"base": a["base"], "polygon": {{WalletID: "w1", Bps: 10000}}}) {
		t.Fatal("another network paid is a change")
	}
	if !store.SamePayout(map[string][]store.PayoutItem{"base": nil}, nil) {
		t.Fatal("an empty network is no network")
	}
	if got := store.PayoutChains(map[string][]store.PayoutItem{"solana": nil, "base": nil}); len(got) != 2 || got[0] != "base" {
		t.Fatalf("chains sorted: %v", got)
	}
}
