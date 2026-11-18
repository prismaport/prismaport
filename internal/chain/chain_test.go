package chain

import (
	"fmt"
	"strings"
	"testing"

	"github.com/prismaport/prismaport/internal/evm"
	"github.com/prismaport/prismaport/internal/fee"
	"github.com/prismaport/prismaport/internal/money"
)

func TestPilot(t *testing.T) {
	r := Pilot()
	live := r.ForMode(true)
	if len(live) != 2 || live[0].ID != "base" || live[1].ID != "solana" {
		t.Fatalf("live chains: %+v", live)
	}
	test := r.ForMode(false)
	if len(test) != 2 || test[0].ID != "base-sepolia" || test[1].ID != "solana-devnet" {
		t.Fatalf("test chains: %+v", test)
	}
	for _, c := range append(live, test...) {
		for _, a := range c.Assets {
			if c.Family == EVM {
				addr, err := evm.ParseAddress(a.Token)
				if err != nil || addr.Hex() != a.Token {
					t.Errorf("%s %s token %s not checksummed: %v", c.ID, a.Symbol, a.Token, err)
				}
			}
			if !r.IsKnownToken(c.Family, a.Token) {
				t.Errorf("%s token not denylisted", a.Token)
			}
		}
	}
	for _, tok := range otherEVMTokens {
		if _, err := evm.ParseAddress(tok); err != nil {
			t.Errorf("denylist entry %s: %v", tok, err)
		}
	}
	if !r.IsKnownToken(EVM, "0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48") {
		t.Fatal("denylist must be case-insensitive on EVM")
	}
	if _, ok := r.Get("ethereum"); ok {
		t.Fatal("ethereum must not be served")
	}
}

func TestRejectsBadConfig(t *testing.T) {
	bad := [][]Chain{
		{{ID: "x", Family: "tron", Fee: PilotFee()}},
		{{ID: "x", Family: EVM, Fee: fee.Params{FeeBps: 301}}},
		{{ID: "x", Family: EVM, Fee: PilotFee(), Assets: []Asset{{Symbol: "X", Token: "0x1", Decimals: 5}}}},
		{{ID: "x", Family: EVM, Fee: PilotFee(), Assets: []Asset{{Symbol: "X", Token: "0x1", Decimals: 19}}}},
		{{ID: "x", Family: EVM, Fee: PilotFee()}, {ID: "x", Family: EVM, Fee: PilotFee()}},
	}
	for i, cs := range bad {
		if _, err := NewRegistry(cs, nil); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
}

// A forwarder binds every asset of its chain and at most MaxAssetsPerChain tokens (D41, D60): the
// registry refuses a fifth asset on one chain, and an asset listed twice.
func TestRegistryAssetsPerChain(t *testing.T) {
	asset := func(i int) Asset {
		return Asset{Symbol: fmt.Sprintf("S%d", i), Token: fmt.Sprintf("0x%d", i), Decimals: 6}
	}
	var four []Asset
	for i := range MaxAssetsPerChain {
		four = append(four, asset(i))
	}
	if _, err := NewRegistry([]Chain{{ID: "x", Family: EVM, Fee: PilotFee(), Assets: four}}, nil); err != nil {
		t.Fatalf("%d assets refused: %v", MaxAssetsPerChain, err)
	}
	five := append(append([]Asset(nil), four...), asset(MaxAssetsPerChain))
	if _, err := NewRegistry([]Chain{{ID: "x", Family: EVM, Fee: PilotFee(), Assets: five}}, nil); err == nil || !strings.Contains(err.Error(), "at most 4") {
		t.Fatalf("%d assets: %v", len(five), err)
	}
	twice := []Asset{asset(0), asset(0)}
	if _, err := NewRegistry([]Chain{{ID: "x", Family: EVM, Fee: PilotFee(), Assets: twice}}, nil); err == nil {
		t.Fatal("an asset listed twice was accepted")
	}
}

func TestToCanonical(t *testing.T) {
	u, err := ToCanonical(money.FromUint64(1), 6)
	if err != nil || u.String() != "1000000000000" {
		t.Fatalf("ToCanonical=%s,%v", u, err)
	}
}

func TestPilotFeeIsTheApprovedOne(t *testing.T) {
	if PilotFee().FeeBps != 50 || PilotFee().FixedFeeMicros != 100_000 {
		t.Fatalf("pilot fee %+v, approved 0.5%% plus US$0.10", PilotFee())
	}
	r := Pilot()
	for _, live := range []bool{true, false} {
		for _, c := range r.ForMode(live) {
			if c.Fee != PilotFee() {
				t.Errorf("%s fee %+v", c.ID, c.Fee)
			}
		}
	}
}

func TestPilotPolicy(t *testing.T) {
	r := Pilot()
	base, _ := r.Get("base")
	if len(base.Assets) != 1 || base.Assets[0].Symbol != "USDC" || !base.Assets[0].EIP3009 {
		t.Fatalf("base assets %+v", base.Assets)
	}
	if _, ok := base.Asset("USDT"); ok {
		t.Fatal("USDT on an EVM chain in the pilot")
	}
	if _, ok := r.Get("arbitrum"); ok {
		t.Fatal("arbitrum must not be served in the pilot")
	}
	sol, _ := r.Get("solana")
	if _, ok := sol.Asset("USDT"); !ok {
		t.Fatal("Solana keeps native USDT")
	}
	if !r.IsKnownToken(EVM, "0xd9aAEc86B65D86f6A7B5B1b0c42FFA531710b6CA") {
		t.Fatal("USDbC must stay denylisted as a destination")
	}

	baseUSDC := Asset{Symbol: "USDC", Token: "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913", Decimals: 6, EIP3009: true}
	solana := Chain{ID: "solana", Family: Solana, Livemode: true, Fee: PilotFee(), Assets: []Asset{
		{Symbol: "USDC", Token: "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v", Decimals: 6},
		{Symbol: "USDT", Token: "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB", Decimals: 6}}}
	cases := map[string][]Chain{
		"usdt on base": {{ID: "base", Family: EVM, Livemode: true, EVMID: 8453, Fee: PilotFee(),
			Assets: []Asset{baseUSDC, {Symbol: "USDT", Token: "0xfde4C96c8593536E31F229EA8f37b2ADa2699bb2", Decimals: 6}}}},
		"bridged usdbc as usdc": {{ID: "base", Family: EVM, Livemode: true, EVMID: 8453, Fee: PilotFee(),
			Assets: []Asset{{Symbol: "USDC", Token: "0xd9aAEc86B65D86f6A7B5B1b0c42FFA531710b6CA", Decimals: 6}}}},
		"usdc without eip3009": {{ID: "base", Family: EVM, Livemode: true, EVMID: 8453, Fee: PilotFee(),
			Assets: []Asset{{Symbol: "USDC", Token: baseUSDC.Token, Decimals: 6}}}},
		"arbitrum": {{ID: "arbitrum", Family: EVM, Livemode: true, EVMID: 42161, Fee: PilotFee(),
			Assets: []Asset{{Symbol: "USDC", Token: "0xaf88d065e77c8cC2239327C5EDb3A432268e5831", Decimals: 6, EIP3009: true}}}},
		"the old d15 fee": {{ID: "base", Family: EVM, Livemode: true, EVMID: 8453, Fee: fee.Params{FeeBps: 100, FixedFeeMicros: 100_000},
			Assets: []Asset{baseUSDC}}},
		"other fixed fee": {{ID: "base", Family: EVM, Livemode: true, EVMID: 8453, Fee: fee.Params{FeeBps: 50, FixedFeeMicros: 50_000},
			Assets: []Asset{baseUSDC}}},
		"no fixed fee": {{ID: "base", Family: EVM, Livemode: true, EVMID: 8453, Fee: fee.Params{FeeBps: 50},
			Assets: []Asset{baseUSDC}}},
		"wrong solana mint": {{ID: "solana", Family: Solana, Livemode: true, Fee: PilotFee(), Assets: []Asset{
			{Symbol: "USDT", Token: "Es9vMFrAgEBUnGv4dMwzBw6rj4Ld3SbUhmQgJjUJdi4B", Decimals: 6}}}},
	}
	for name, cs := range cases {
		t.Run(name, func(t *testing.T) {
			reg, err := NewRegistry(cs, nil)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if err := CheckPilotPolicy(reg); err == nil {
				t.Errorf("%s accepted by the pilot policy", name)
			}
		})
	}
	ok, err := NewRegistry([]Chain{{ID: "base", Family: EVM, Livemode: true, EVMID: 8453, Fee: PilotFee(), Assets: []Asset{baseUSDC}}, solana}, nil)
	if err != nil || CheckPilotPolicy(ok) != nil {
		t.Fatalf("pilot-shaped registry refused: %v %v", err, CheckPilotPolicy(ok))
	}
}

// A chain switched off (D62) leaves the registry it is taken from untouched, and its tokens stay
// known, so they still never pass as a destination.
func TestWithout(t *testing.T) {
	full := Pilot()
	off := full.Without("solana", "solana-devnet")
	if _, ok := off.Get("solana"); ok {
		t.Fatal("solana still served")
	}
	if _, ok := off.Get("base"); !ok {
		t.Fatal("base went off too")
	}
	if _, ok := full.Get("solana"); !ok {
		t.Fatal("the original registry lost solana")
	}
	if !off.IsKnownToken(Solana, "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v") {
		t.Fatal("the USDC mint of an off chain is no longer a known token")
	}
}
