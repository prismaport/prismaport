// Package chain is the registry of supported networks and assets.
package chain

import (
	"fmt"
	"math/big"
	"slices"
	"sort"
	"strings"

	"github.com/prismaport/prismaport/internal/fee"
	"github.com/prismaport/prismaport/internal/money"
)

// Family is a kind of chain whose addresses and transactions work the same way.
type Family string

// The supported chain families.
const (
	EVM    Family = "evm"
	Solana Family = "solana"
)

// Valid reports whether the family is a supported one.
func (f Family) Valid() bool { return f == EVM || f == Solana }

// Asset is a stablecoin on one chain. USDC on Base and USDC on Solana are distinct assets.
type Asset struct {
	Symbol   string
	Token    string
	Decimals uint8
	EIP3009  bool
}

// Chain describes one supported network: its ID, family, mode, assets and fee parameters.
type Chain struct {
	ID       string
	Family   Family
	Livemode bool
	EVMID    uint64
	Assets   []Asset
	Fee      fee.Params
	// Multicall3 is the chain's Multicall3 contract (EVM), which batches the per-tick balance
	// reads (D49). Empty means the canonical 0xcA11bde05977b3631167028862bE2a173976CA11.
	Multicall3 string
}

// Multicall3Canonical is Multicall3's address on Base, Base Sepolia and most EVM chains.
const Multicall3Canonical = "0xcA11bde05977b3631167028862bE2a173976CA11"

// Asset returns the chain's asset with the given symbol, if the chain has it.
func (c Chain) Asset(symbol string) (Asset, bool) {
	for _, a := range c.Assets {
		if a.Symbol == symbol {
			return a, true
		}
	}
	return Asset{}, false
}

// Registry is immutable after construction.
type Registry struct {
	chains map[string]Chain
	// knownTokens are token or mint addresses that must never be a destination, including
	// tokens on chains we do not serve: an EVM destination is shared across every EVM.
	knownTokens map[Family]map[string]struct{}
}

// PilotFee returns the approved pilot fee on every pilot chain (D28): 0.5% plus US$0.10 per
// settlement. A function, so no caller can change it for the others.
func PilotFee() fee.Params { return fee.Params{FeeBps: 50, FixedFeeMicros: 100_000} }

// MaxAssetsPerChain is how many assets one chain may register: a forwarder binds every asset a
// session offers on its chain (D41), and the deployed contracts bind at most 4 tokens
// (TokenList.MAX_TOKENS on EVM, MAX_MINTS on Solana; derive.MaxTokens). More stablecoins on one
// chain need per-asset binding first, a separate decision (D60).
const MaxAssetsPerChain = 4

// NewRegistry validates the chains and builds a registry of them, with their token addresses and
// the extra denylist as known tokens no payout may target.
func NewRegistry(chains []Chain, extraDenylist map[Family][]string) (*Registry, error) {
	r := &Registry{chains: map[string]Chain{}, knownTokens: map[Family]map[string]struct{}{EVM: {}, Solana: {}}}
	for _, c := range chains {
		if !c.Family.Valid() {
			return nil, fmt.Errorf("chain %s: unknown family %q", c.ID, c.Family)
		}
		if _, dup := r.chains[c.ID]; dup {
			return nil, fmt.Errorf("chain %s: duplicate", c.ID)
		}
		if err := c.Fee.Validate(); err != nil {
			return nil, fmt.Errorf("chain %s: %w", c.ID, err)
		}
		if len(c.Assets) > MaxAssetsPerChain {
			return nil, fmt.Errorf("chain %s: %d assets, a forwarder binds at most %d", c.ID, len(c.Assets), MaxAssetsPerChain)
		}
		seen := map[string]bool{}
		for _, a := range c.Assets {
			if seen[a.Symbol] {
				return nil, fmt.Errorf("chain %s: asset %s twice", c.ID, a.Symbol)
			}
			seen[a.Symbol] = true
		}
		for _, a := range c.Assets {
			if a.Decimals < fee.MinTokenDecimals || a.Decimals > CanonicalDecimals {
				return nil, fmt.Errorf("chain %s asset %s: decimals %d outside [%d,%d]", c.ID, a.Symbol, a.Decimals, fee.MinTokenDecimals, CanonicalDecimals)
			}
			r.knownTokens[c.Family][normalize(c.Family, a.Token)] = struct{}{}
		}
		r.chains[c.ID] = c
	}
	for f, list := range extraDenylist {
		if !f.Valid() {
			return nil, fmt.Errorf("denylist: unknown family %q", f)
		}
		for _, a := range list {
			r.knownTokens[f][normalize(f, a)] = struct{}{}
		}
	}
	return r, nil
}

// CanonicalDecimals is the common scale used to sum payments across assets with different
// decimals. Scaling up to it is always exact.
const CanonicalDecimals = 18

// ToCanonical converts base units of an asset into the canonical scale.
func ToCanonical(u money.Units, decimals uint8) (money.Units, error) {
	return money.Rescale(u, decimals, CanonicalDecimals)
}

// FromCanonicalUp converts a canonical amount into base units of an asset, rounded up to a
// whole unit: what is due after a deposit in a token of more decimals may not end on one.
func FromCanonicalUp(u money.Units, decimals uint8) (money.Units, error) {
	if decimals > CanonicalDecimals {
		return money.Zero, money.ErrDecimals
	}
	scale := money.Pow10(CanonicalDecimals - decimals)
	q, r := new(big.Int).QuoRem(u.Big(), scale.Big(), new(big.Int))
	if r.Sign() != 0 {
		q.Add(q, big.NewInt(1))
	}
	return money.FromBig(q)
}

func normalize(f Family, addr string) string {
	if f == EVM {
		return strings.ToLower(addr)
	}
	return addr
}

// Get returns the chain with the given ID, if it is registered.
func (r *Registry) Get(id string) (Chain, bool) {
	c, ok := r.chains[id]
	return c, ok
}

// Without returns the registry minus the chains ids names: chains switched off in an environment.
// Their token addresses stay known, so they still never pass as a destination.
func (r *Registry) Without(ids ...string) *Registry {
	// knownTokens is shared: it is read-only once NewRegistry built it.
	out := &Registry{chains: make(map[string]Chain, len(r.chains)), knownTokens: r.knownTokens}
	for id, c := range r.chains {
		if !slices.Contains(ids, id) {
			out.chains[id] = c
		}
	}
	return out
}

// ForMode returns the chains of one mode sorted by ID for stable output.
func (r *Registry) ForMode(livemode bool) []Chain {
	var out []Chain
	for _, c := range r.chains {
		if c.Livemode == livemode {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// IsKnownToken reports whether addr is a token, mint or denylisted contract of the family.
func (r *Registry) IsKnownToken(f Family, addr string) bool {
	_, ok := r.knownTokens[f][normalize(f, addr)]
	return ok
}

// otherEVMTokens are widely used token contracts on EVMs we do not serve yet. Pasting one as a
// destination would burn funds on every EVM at once.
var otherEVMTokens = []string{
	"0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48", // USDC Ethereum
	"0xdAC17F958D2ee523a2206206994597C13D831ec7", // USDT Ethereum
	"0xaf88d065e77c8cC2239327C5EDb3A432268e5831", // USDC Arbitrum
	"0xFd086bC7CD5C481DCC9C85ebE478A1C0b69FCbb9", // USDT Arbitrum
	"0x3c499c542cEF5E3811e1192ce70d8cC03d5c3359", // USDC Polygon
	"0xc2132D05D31c914a87C6611C10748AEb04B58e8F", // USDT Polygon
	"0x0b2C639c533813f4Aa9D7837CAf62653d097Ff85", // USDC Optimism
	"0xd9aAEc86B65D86f6A7B5B1b0c42FFA531710b6CA", // USDbC Base
	"0x4200000000000000000000000000000000000006", // WETH Base/Optimism
	"0x8AC76a51cc950d9822D68b83fE1Ad97B32Cd580d", // USDC BNB Chain
	"0x55d398326f99059fF775485246999027B3197955", // USDT BNB Chain
}

// nativeUSDC is Circle's own USDC per EVM chain id. Bridged variants (USDbC on Base) share the
// symbol but have another issuer, so on EVM the pilot accepts these contracts and nothing else.
var nativeUSDC = map[uint64]string{
	8453:  "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913", // Base
	84532: "0x036CbD53842c5426634e7929541eC2318f3dCF7e", // Base Sepolia
}

// solanaPilotMints are the native USDC and USDT mints the pilot accepts on Solana.
var solanaPilotMints = map[string]map[string]string{
	"solana":        {"USDC": "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v", "USDT": "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB"},
	"solana-devnet": {"USDC": "4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU"},
}

// CheckPilotPolicy enforces the approved pilot scope: the pilot fee on every chain; on EVM only
// Base (and its testnet) with native USDC over EIP-3009, so no USDT and no bridged USDC; on
// Solana only the native USDC and USDT mints.
func CheckPilotPolicy(r *Registry) error {
	for _, live := range []bool{true, false} {
		for _, c := range r.ForMode(live) {
			if c.Fee != PilotFee() {
				return fmt.Errorf("chain %s: fee %+v, the pilot fee is %+v", c.ID, c.Fee, PilotFee())
			}
			switch c.Family {
			case EVM:
				usdc, ok := nativeUSDC[c.EVMID]
				if !ok {
					return fmt.Errorf("chain %s (evm id %d) is not a pilot EVM chain", c.ID, c.EVMID)
				}
				if len(c.Assets) != 1 || c.Assets[0].Symbol != "USDC" || !strings.EqualFold(c.Assets[0].Token, usdc) || !c.Assets[0].EIP3009 {
					return fmt.Errorf("chain %s: the pilot accepts only native USDC (%s) over EIP-3009 on EVM", c.ID, usdc)
				}
			case Solana:
				mints, ok := solanaPilotMints[c.ID]
				if !ok {
					return fmt.Errorf("chain %s is not a pilot Solana cluster", c.ID)
				}
				for _, a := range c.Assets {
					if mints[a.Symbol] != a.Token {
						return fmt.Errorf("chain %s: %s %s is not a pilot mint", c.ID, a.Symbol, a.Token)
					}
				}
			}
		}
	}
	return nil
}

// Pilot returns the pilot registry: Base and Solana live, Base Sepolia and Solana devnet test.
func Pilot() *Registry {
	r, err := NewRegistry([]Chain{
		{ID: "base", Family: EVM, Livemode: true, EVMID: 8453, Fee: PilotFee(), Multicall3: Multicall3Canonical, Assets: []Asset{
			{Symbol: "USDC", Token: nativeUSDC[8453], Decimals: 6, EIP3009: true},
		}},
		{ID: "solana", Family: Solana, Livemode: true, Fee: PilotFee(), Assets: []Asset{
			{Symbol: "USDC", Token: solanaPilotMints["solana"]["USDC"], Decimals: 6},
			{Symbol: "USDT", Token: solanaPilotMints["solana"]["USDT"], Decimals: 6},
		}},
		{ID: "base-sepolia", Family: EVM, Livemode: false, EVMID: 84532, Fee: PilotFee(), Multicall3: Multicall3Canonical, Assets: []Asset{
			{Symbol: "USDC", Token: nativeUSDC[84532], Decimals: 6, EIP3009: true},
		}},
		{ID: "solana-devnet", Family: Solana, Livemode: false, Fee: PilotFee(), Assets: []Asset{
			{Symbol: "USDC", Token: solanaPilotMints["solana-devnet"]["USDC"], Decimals: 6},
		}},
	}, map[Family][]string{EVM: otherEVMTokens})
	if err == nil {
		err = CheckPilotPolicy(r)
	}
	if err != nil {
		panic(err)
	}
	return r
}
