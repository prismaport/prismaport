package view

import (
	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/chainpause"
	"github.com/prismaport/prismaport/internal/money"
)

// Network is the public description of one chain, so a payer-side client can switch the wallet
// to the right chain, address the right token and avoid dust it could never settle.
type Network struct {
	ID         string         `json:"id"`
	Family     string         `json:"family"`
	Livemode   bool           `json:"livemode"`
	EVMChainID *uint64        `json:"evm_chain_id"`
	FeeBps     uint16         `json:"fee_bps"`
	FixedFee   string         `json:"fixed_fee"`
	Assets     []NetworkAsset `json:"assets"`
	// Paused says the whole network takes no new payments right now (D64); absent when not.
	Paused bool `json:"paused,omitempty"`
}

// NetworkList is the answer of GET /v1/checkout/networks.
type NetworkList struct {
	Data []Network `json:"data"`
}

// NetworkAsset is one asset on a network as shown in the network registry.
type NetworkAsset struct {
	Symbol   string `json:"symbol"`
	Token    string `json:"token"`
	Decimals uint8  `json:"decimals"`
	EIP3009  bool   `json:"eip3009"`
	// Paused says this asset on this network takes no new payments right now, by itself or with
	// its whole network (D64); absent when not.
	Paused bool `json:"paused,omitempty"`
}

// Networks describes the registry with the pauses in force (GET /v1/checkout/networks). A paused network or asset stays
// listed (an open session may still show its deposit address) and says it is paused.
func Networks(reg *chain.Registry, pauses chainpause.Set) []Network {
	out := []Network{}
	for _, live := range []bool{true, false} {
		for _, c := range reg.ForMode(live) {
			n := Network{ID: c.ID, Family: string(c.Family), Livemode: c.Livemode, FeeBps: c.Fee.FeeBps,
				FixedFee: money.FormatMinPlaces(money.FromUint64(c.Fee.FixedFeeMicros), 6, 2), Assets: []NetworkAsset{},
				Paused: pauses.ChainPaused(c.ID)}
			if c.Family == chain.EVM {
				id := c.EVMID
				n.EVMChainID = &id
			}
			for _, a := range c.Assets {
				n.Assets = append(n.Assets, NetworkAsset{Symbol: a.Symbol, Token: a.Token, Decimals: a.Decimals, EIP3009: a.EIP3009,
					Paused: pauses.Paused(c.ID, a.Symbol)})
			}
			out = append(out, n)
		}
	}
	return out
}
