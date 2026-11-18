package service

import (
	"fmt"
	"testing"

	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/fee"
	"github.com/prismaport/prismaport/internal/store"
)

func TestAddressAttention(t *testing.T) {
	reg := chain.Pilot()
	base, _ := reg.Get("base-sepolia")
	sol, _ := reg.Get("solana-devnet")
	usdc, _ := base.Asset("USDC")
	solUSDC, _ := sol.Asset("USDC")
	w1, w2 := "wal_1", "wal_2"
	evmAddr, evmAddr2 := "0x7a3F5c2b9E81d4A6f0C3e2B7a915D8c4F06Be91c", "0x2546BcD3c84621e976D8185a91A922aE77ECEc30"

	fresh := func() (store.CustomerAddress, store.Product, attentionState) {
		ca := store.CustomerAddress{ID: "caddr_1", Livemode: false, Status: store.AddressActive,
			PayTo:   []store.PayToRecord{{Chain: base.ID, Asset: "USDC", Token: usdc.Token, Fee: base.Fee}},
			Payouts: []store.SessionPayout{{Chain: base.ID, Position: 0, WalletID: &w1, Address: evmAddr, Bps: 10000}}}
		p := store.Product{ID: "prod_1", Active: true, Payout: map[string][]store.PayoutItem{
			base.ID: {{WalletID: w1, Address: evmAddr, Bps: 10000}}}}
		st := attentionState{registry: reg, archived: map[string]string{},
			accepted: []store.ChainAsset{{Chain: base.ID, Asset: "USDC"}}, payable: map[string]chain.Family{base.ID: chain.EVM}}
		return ca, p, st
	}
	reasons := func(list []AddressAttention) string { return fmt.Sprint(list) }

	ca, p, st := fresh()
	if got := addressAttention(ca, &p, st); len(got) != 0 {
		t.Fatalf("up to date address: %v", got)
	}

	// The product now pays another wallet and the old one was archived.
	ca, p, st = fresh()
	p.Payout[base.ID] = []store.PayoutItem{{WalletID: w2, Address: evmAddr2, Bps: 10000}}
	st.archived[w1] = "Old wallet"
	got := addressAttention(ca, &p, st)
	if len(got) != 2 || got[0].Reason != AttentionArchivedWallet || got[0].Wallets[0] != (AttentionWallet{ID: w1, Label: "Old wallet"}) ||
		got[1].Reason != AttentionPayoutChanged || fmt.Sprint(got[1].Chains) != "[base-sepolia]" {
		t.Fatalf("archived wallet and payout: %s", reasons(got))
	}

	// Same wallets, other shares.
	ca, p, st = fresh()
	ca.Payouts = []store.SessionPayout{{Chain: base.ID, Position: 1, WalletID: &w2, Address: evmAddr2, Bps: 3000},
		{Chain: base.ID, Position: 0, WalletID: &w1, Address: evmAddr, Bps: 7000}}
	p.Payout[base.ID] = []store.PayoutItem{{WalletID: w1, Address: evmAddr, Bps: 7000}, {WalletID: w2, Address: evmAddr2, Bps: 3000}}
	if got := addressAttention(ca, &p, st); len(got) != 0 {
		t.Fatalf("same split in another row order: %s", reasons(got))
	}
	p.Payout[base.ID] = []store.PayoutItem{{WalletID: w1, Address: evmAddr, Bps: 5000}, {WalletID: w2, Address: evmAddr2, Bps: 5000}}
	if got := addressAttention(ca, &p, st); len(got) != 1 || got[0].Reason != AttentionPayoutChanged {
		t.Fatalf("shares changed: %s", reasons(got))
	}

	// The fee changed since the address was created.
	ca, p, st = fresh()
	ca.PayTo[0].Fee = fee.Params{FeeBps: 100}
	if got := addressAttention(ca, &p, st); len(got) != 1 || got[0].Reason != AttentionFeeChanged || fmt.Sprint(got[0].Chains) != "[base-sepolia]" {
		t.Fatalf("fee: %s", reasons(got))
	}

	// Solana devnet accepted now, and the product pays there: a rotation would offer it. A chain
	// the product does not pay, or one without a treasury, is not offered.
	ca, p, st = fresh()
	st.accepted = append(st.accepted, store.ChainAsset{Chain: sol.ID, Asset: "USDC"})
	if got := addressAttention(ca, &p, st); len(got) != 0 {
		t.Fatalf("accepted chain the product does not pay: %s", reasons(got))
	}
	p.Payout[sol.ID] = []store.PayoutItem{{WalletID: "wal_sol", Address: "9xQeWvG816bUx9EPjHmaT23yvVM2ZWbrrpZb9PusVFin", Bps: 10000}}
	if got := addressAttention(ca, &p, st); len(got) != 0 {
		t.Fatalf("chain without a treasury: %s", reasons(got))
	}
	st.payable[sol.ID] = chain.Solana
	got = addressAttention(ca, &p, st)
	if len(got) != 1 || got[0].Reason != AttentionNewNetworks || got[0].Assets[0] != (ChainAssetRef{Chain: sol.ID, Asset: "USDC"}) {
		t.Fatalf("new network: %s", reasons(got))
	}

	// Nothing the address offers is accepted any more, and its product was deactivated.
	ca, p, st = fresh()
	st.accepted = nil
	p.Active = false
	got = addressAttention(ca, &p, st)
	if len(got) != 2 || got[0].Reason != AttentionProductInactive || got[1].Reason != AttentionNoNetworks {
		t.Fatalf("inactive product, no networks: %s", reasons(got))
	}

	// A token contract changed in the registry: the entry is not usable and the asset is new again.
	ca, p, st = fresh()
	ca.PayTo = append(ca.PayTo, store.PayToRecord{Chain: sol.ID, Asset: "USDC", Token: "OldMint", Fee: sol.Fee})
	ca.Payouts = append(ca.Payouts, store.SessionPayout{Chain: sol.ID, WalletID: &w2, Address: "x", Bps: 10000})
	p.Payout[sol.ID] = []store.PayoutItem{{WalletID: w2, Address: "x", Bps: 10000}}
	st.accepted = []store.ChainAsset{{Chain: sol.ID, Asset: "USDC"}}
	st.payable[sol.ID] = chain.Solana
	got = addressAttention(ca, &p, st)
	if len(got) != 2 || got[0].Reason != AttentionNewNetworks || got[1].Reason != AttentionNoNetworks || solUSDC.Token == "OldMint" {
		t.Fatalf("token changed: %s", reasons(got))
	}
}
