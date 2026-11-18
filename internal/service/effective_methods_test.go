package service

import (
	"fmt"
	"testing"

	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/store"
)

// D60: what a product offers is the registry ∩ the account's accepted assets ∩ the product's
// choice, on payable chains it pays.
func TestEffectiveMethods(t *testing.T) {
	reg := chain.Pilot()
	pays := []store.PayoutItem{{WalletID: "wal_1", Bps: 10_000}}
	everything := []store.ChainAsset{{Chain: "base", Asset: "USDC"}, {Chain: "solana", Asset: "USDC"}, {Chain: "solana", Asset: "USDT"},
		{Chain: "base-sepolia", Asset: "USDC"}, {Chain: "solana", Asset: "DAI"}, {Chain: "ethereum", Asset: "USDC"}}
	payable := map[string]chain.Family{"base": chain.EVM, "solana": chain.Solana}
	product := func(networks map[string]store.NetworkChoice) *store.Product {
		p := &store.Product{Payout: map[string][]store.PayoutItem{}, Networks: networks}
		for id := range networks {
			p.Payout[id] = pays
		}
		return p
	}
	all, some := store.NetworkChoice{Accept: store.AcceptAll}, func(a ...string) store.NetworkChoice {
		return store.NetworkChoice{Accept: store.AcceptSome, Assets: a}
	}
	cases := []struct {
		name     string
		accepted []store.ChainAsset
		payable  map[string]chain.Family
		p        *store.Product
		want     string
	}{
		{"all everywhere: every registered, accepted pair of the mode", everything, payable,
			product(map[string]store.NetworkChoice{"base": all, "solana": all}), "[{base USDC} {solana USDC} {solana USDT}]"},
		{"some narrows one chain", everything, payable,
			product(map[string]store.NetworkChoice{"base": all, "solana": some("USDT")}), "[{base USDC} {solana USDT}]"},
		{"a listed asset the account has off is not offered", []store.ChainAsset{{Chain: "base", Asset: "USDC"}, {Chain: "solana", Asset: "USDC"}}, payable,
			product(map[string]store.NetworkChoice{"base": all, "solana": some("USDT")}), "[{base USDC}]"},
		{"a network the product does not take", everything, payable,
			product(map[string]store.NetworkChoice{"solana": all}), "[{solana USDC} {solana USDT}]"},
		{"a chain without a treasury", everything, map[string]chain.Family{"solana": chain.Solana},
			product(map[string]store.NetworkChoice{"base": all, "solana": some("USDC")}), "[{solana USDC}]"},
		{"a payout without a choice takes all", everything, payable,
			&store.Product{Payout: map[string][]store.PayoutItem{"solana": pays}}, "[{solana USDC} {solana USDT}]"},
		{"nothing left", []store.ChainAsset{{Chain: "solana", Asset: "USDC"}}, payable,
			product(map[string]store.NetworkChoice{"solana": some("USDT")}), "[]"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := fmt.Sprint(effectiveMethods(reg, true, c.accepted, c.payable, c.p)); got != c.want {
				t.Errorf("%s: got %s, want %s", c.name, got, c.want)
			}
		})
	}
}

func TestAddressOffers(t *testing.T) {
	p := &store.Product{Networks: map[string]store.NetworkChoice{"solana": {Accept: store.AcceptSome, Assets: []string{"USDT"}},
		"base": {Accept: store.AcceptAll}}}
	for _, c := range []struct {
		chain, asset string
		want         bool
	}{{"solana", "USDT", true}, {"solana", "USDC", false}, {"base", "USDC", true}, {"other", "USDC", true}} {
		t.Run(c.chain, func(t *testing.T) {
			if got := addressOffers(p, c.chain, c.asset); got != c.want {
				t.Errorf("%s/%s: %v", c.chain, c.asset, got)
			}
		})
	}
	if !addressOffers(nil, "solana", "USDC") {
		t.Error("no product")
	}
}
