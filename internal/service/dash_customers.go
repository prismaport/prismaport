package service

import (
	"context"
	"maps"
	"slices"
	"strings"

	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/store"
	"github.com/prismaport/prismaport/internal/view"
)

// Dashboard customer addresses (B11 phase 3). The
// list, the detail, the sessions and Rotate go through the merchant API's functions; the dashboard
// adds the product, the stats and what a rotation would change (attention).

// Attention reasons: why an active customer address no longer matches what a new address would
// commit to, so the merchant may rotate it.
const (
	// AttentionProductInactive: no session opens on the address, and it cannot be rotated until
	// the product is active again.
	AttentionProductInactive = "product_inactive"
	// AttentionArchivedWallet: the address pays a wallet archived since it was created.
	AttentionArchivedWallet = "archived_wallet"
	// AttentionPayoutChanged: the product pays differently now on these chains.
	AttentionPayoutChanged = "payout_changed"
	// AttentionFeeChanged: the fee on these chains changed since the address was created.
	AttentionFeeChanged = "fee_changed"
	// AttentionNewNetworks: the merchant accepts these (chain, asset) pairs now and the address
	// does not offer them.
	AttentionNewNetworks = "new_networks"
	// AttentionNoNetworks: none of the address's networks is accepted any more; no session opens.
	AttentionNoNetworks = "no_networks"
)

// AddressProduct is the product a customer address sells, as it is now.
type AddressProduct struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	PriceType string `json:"price_type"`
	Active    bool   `json:"active"`
}

// AddressSessionSummary is the address's latest session (the one that owns its watch).
type AddressSessionSummary struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
}

// AddressStats are counted over every session opened on the address, from its sessions and
// payments on every read. Received sums counted (confirmed or finalized) payments.
type AddressStats struct {
	Sessions    int64                  `json:"sessions"`
	Paid        int64                  `json:"paid"`
	Received    []AssetTotal           `json:"received"`
	LastPaidAt  *string                `json:"last_paid_at"`
	LastSession *AddressSessionSummary `json:"last_session"`
}

// AttentionWallet is a wallet named by an attention item.
type AttentionWallet struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// ChainAssetRef names one asset on one chain.
type ChainAssetRef struct {
	Chain string `json:"chain"`
	Asset string `json:"asset"`
}

// AddressAttention is one thing a rotation would change.
type AddressAttention struct {
	Reason  string            `json:"reason"`
	Chains  []string          `json:"chains,omitempty"`
	Wallets []AttentionWallet `json:"wallets,omitempty"`
	Assets  []ChainAssetRef   `json:"assets,omitempty"`
}

// DashCustomerAddress is the merchant API's customer address plus its product, stats and
// attention (empty on a rotated address: it takes no new session anyway).
type DashCustomerAddress struct {
	view.CustomerAddress
	Product   *AddressProduct    `json:"product"`
	Stats     AddressStats       `json:"stats"`
	Attention []AddressAttention `json:"attention"`
}

// DashCustomerAddressList is one page of a customer's addresses and the cursor of the next page, if
// any.
type DashCustomerAddressList struct {
	Data       []DashCustomerAddress `json:"data"`
	NextCursor *string               `json:"next_cursor"`
}

// DashListCustomerAddresses returns one page of the merchant's customer addresses, optionally for
// one customer.
func (s *Service) DashListCustomerAddresses(ctx context.Context, a Auth, customerID, cursor string, limit int) (DashCustomerAddressList, error) {
	if len(cursor) > 64 {
		return DashCustomerAddressList{}, invalid("a filter is too long")
	}
	list, next, err := s.customerAddresses(ctx, a, customerID, cursor, limit)
	if err != nil {
		return DashCustomerAddressList{}, err
	}
	data, err := s.renderAddresses(ctx, a, list)
	if err != nil {
		return DashCustomerAddressList{}, err
	}
	return DashCustomerAddressList{Data: data, NextCursor: next}, nil
}

// DashCustomerAddress returns one of the merchant's customer addresses with its sessions and
// totals.
func (s *Service) DashCustomerAddress(ctx context.Context, a Auth, id string) (DashCustomerAddress, error) {
	ca, err := s.customerAddress(ctx, a, id)
	if err != nil {
		return DashCustomerAddress{}, err
	}
	out, err := s.renderAddresses(ctx, a, []store.CustomerAddress{ca})
	if err != nil {
		return DashCustomerAddress{}, err
	}
	return out[0], nil
}

// DashRotateCustomerAddress rotates like POST /v1/customer_addresses/{id}/rotate and answers the
// new address.
func (s *Service) DashRotateCustomerAddress(ctx context.Context, a Auth, id string) (DashCustomerAddress, error) {
	next, err := s.RotateCustomerAddress(ctx, a, id)
	if err != nil {
		return DashCustomerAddress{}, err
	}
	return s.DashCustomerAddress(ctx, a, next.ID)
}

// DashAddressSessions lists the sessions opened on one customer address, newest first, as the
// Payments list shows them.
func (s *Service) DashAddressSessions(ctx context.Context, a Auth, id, cursor string, limit int) (SessionList, error) {
	if _, err := s.customerAddress(ctx, a, id); err != nil {
		return SessionList{}, err
	}
	return s.DashListSessions(ctx, a, store.SessionFilter{CustomerAddressID: id}, cursor, limit)
}

func (s *Service) renderAddresses(ctx context.Context, a Auth, list []store.CustomerAddress) ([]DashCustomerAddress, error) {
	out := make([]DashCustomerAddress, 0, len(list))
	if len(list) == 0 {
		return out, nil
	}
	var addressIDs, productIDs []string
	for _, ca := range list {
		addressIDs = append(addressIDs, ca.ID)
		productIDs = append(productIDs, ca.ProductID)
	}
	counts, err := s.store.AddressCounts(ctx, addressIDs)
	if err != nil {
		return nil, err
	}
	last, err := s.store.LastAddressSessions(ctx, addressIDs)
	if err != nil {
		return nil, err
	}
	products, err := s.store.ProductsByID(ctx, a.MerchantID, a.Livemode, productIDs)
	if err != nil {
		return nil, err
	}
	in, err := s.attentionInputs(ctx, a)
	if err != nil {
		return nil, err
	}
	for _, ca := range list {
		c := counts[ca.ID]
		received, err := ReceivedByAsset(c.Received)
		if err != nil {
			return nil, err
		}
		d := DashCustomerAddress{CustomerAddress: s.view.CustomerAddress(ca), Attention: []AddressAttention{},
			Stats: AddressStats{Sessions: c.Opened, Paid: c.Paid, Received: received, LastPaidAt: optRFC3339(c.LastPaidAt)}}
		if ref, ok := last[ca.ID]; ok {
			d.Stats.LastSession = &AddressSessionSummary{ID: ref.ID, Status: ref.Status, CreatedAt: rfc3339(ref.CreatedAt)}
		}
		var product *store.Product
		if p, ok := products[ca.ProductID]; ok {
			product = &p
			d.Product = &AddressProduct{ID: p.ID, Name: p.Name, PriceType: string(p.PriceType), Active: p.Active}
		}
		if ca.Status == store.AddressActive {
			d.Attention = addressAttention(ca, product, in)
		}
		out = append(out, d)
	}
	return out, nil
}

// attentionState is what an address is compared with: the wallets archived in the mode, the
// assets the merchant accepts and the chains a session can be paid on (with a treasury).
type attentionState struct {
	registry *chain.Registry
	archived map[string]string
	accepted []store.ChainAsset
	payable  map[string]chain.Family
}

func (s *Service) attentionInputs(ctx context.Context, a Auth) (attentionState, error) {
	wallets, err := s.store.ListWallets(ctx, a.MerchantID, a.Livemode, store.WalletFilter{IncludeArchived: true})
	if err != nil {
		return attentionState{}, err
	}
	archived := map[string]string{}
	for _, w := range wallets {
		if w.ArchivedAt != nil {
			archived[w.ID] = w.Label
		}
	}
	accepted, payable, err := s.payableChains(ctx, a)
	if err != nil {
		return attentionState{}, err
	}
	return attentionState{registry: s.cfg.Registry, archived: archived, accepted: accepted, payable: payable}, nil
}

// addressAttention lists what rotating an active address would change: a new address takes the
// product's payout and the fee now, and every (chain, asset) the product offers now
// (effectiveMethods, through newAddress).
// It reads only stored rows and the registry: no blacklist check, no derivation.
func addressAttention(ca store.CustomerAddress, p *store.Product, st attentionState) []AddressAttention {
	out := []AddressAttention{}
	if p == nil || !p.Active {
		out = append(out, AddressAttention{Reason: AttentionProductInactive})
	}

	var wallets []AttentionWallet
	seen := map[string]bool{}
	for _, x := range ca.Payouts {
		if x.WalletID == nil || seen[*x.WalletID] {
			continue
		}
		if label, ok := st.archived[*x.WalletID]; ok {
			seen[*x.WalletID] = true
			wallets = append(wallets, AttentionWallet{ID: *x.WalletID, Label: label})
		}
	}
	if len(wallets) > 0 {
		out = append(out, AddressAttention{Reason: AttentionArchivedWallet, Wallets: wallets})
	}

	if p != nil {
		byChain := map[string][]store.SessionPayout{}
		for _, x := range ca.Payouts {
			byChain[x.Chain] = append(byChain[x.Chain], x)
		}
		var changed []string
		for _, id := range slices.Sorted(maps.Keys(byChain)) {
			if !samePayout(byChain[id], p.Payout[id]) {
				changed = append(changed, id)
			}
		}
		if len(changed) > 0 {
			out = append(out, AddressAttention{Reason: AttentionPayoutChanged, Chains: changed})
		}
	}

	fees := map[string]bool{}
	usable := false
	has := map[[2]string]bool{}
	accepted := map[[2]string]bool{}
	for _, x := range st.accepted {
		accepted[[2]string{x.Chain, x.Asset}] = true
	}
	for _, x := range ca.PayTo {
		c, ok := st.registry.Get(x.Chain)
		if !ok {
			continue
		}
		if c.Fee != x.Fee {
			fees[x.Chain] = true
		}
		asset, ok := c.Asset(x.Asset)
		if !ok || !strings.EqualFold(asset.Token, x.Token) {
			continue
		}
		has[[2]string{x.Chain, x.Asset}] = true
		if c.Livemode == ca.Livemode && accepted[[2]string{x.Chain, x.Asset}] && addressOffers(p, x.Chain, x.Asset) {
			usable = true
		}
	}
	if len(fees) > 0 {
		out = append(out, AddressAttention{Reason: AttentionFeeChanged, Chains: slices.Sorted(maps.Keys(fees))})
	}

	if p != nil {
		// What a new address would offer (effectiveMethods) that this one does not.
		var added []ChainAssetRef
		for _, x := range effectiveMethods(st.registry, ca.Livemode, st.accepted, st.payable, p) {
			if has[[2]string{x.Chain, x.Asset}] {
				continue
			}
			added = append(added, ChainAssetRef{Chain: x.Chain, Asset: x.Asset})
		}
		if len(added) > 0 {
			out = append(out, AddressAttention{Reason: AttentionNewNetworks, Assets: added})
		}
	}
	if !usable {
		out = append(out, AddressAttention{Reason: AttentionNoNetworks})
	}
	return out
}

// samePayout reports whether an address's recipients on one chain (in position order) are the
// product's items there now: same wallets, addresses and shares, in the same order.
func samePayout(addr []store.SessionPayout, items []store.PayoutItem) bool {
	if len(addr) != len(items) {
		return false
	}
	slices.SortFunc(addr, func(x, y store.SessionPayout) int { return x.Position - y.Position })
	for i, x := range addr {
		it := items[i]
		if x.WalletID == nil || *x.WalletID != it.WalletID || x.Bps != it.Bps || !strings.EqualFold(x.Address, it.Address) {
			return false
		}
	}
	return true
}
