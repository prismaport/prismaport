// Package view renders API objects. The session shape is the same in GET responses, webhooks
// and the event feed. Amounts are decimal strings computed exactly from base units.
package view

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/payout"
	"github.com/prismaport/prismaport/internal/store"
)

// Renderer turns store records into their API representations.
type Renderer struct {
	CheckoutBaseURL string
	// Chains lists the chain ids of a mode; a product's payout has every one of them as a key.
	// Nil renders only the chains a payout names.
	Chains func(livemode bool) []string
}

func (r Renderer) chains(livemode bool) []string {
	if r.Chains == nil {
		return nil
	}
	return r.Chains(livemode)
}

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func optTS(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := ts(*t)
	return &s
}

func usd(u *money.Units) *string {
	if u == nil {
		return nil
	}
	s := money.FormatUSD(*u)
	return &s
}

func meta(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func (r Renderer) url(kind, id string) string {
	return strings.TrimRight(r.CheckoutBaseURL, "/") + "/" + kind + "/" + id
}

// Product is the merchant API product: the catalog item its payment links sell.
type Product struct {
	ID               string            `json:"id"`
	Name             string            `json:"name"`
	Description      *string           `json:"description,omitempty"`
	PriceType        string            `json:"price_type"`
	Amount           *string           `json:"amount"`
	MinAmount        *string           `json:"min_amount,omitempty"`
	MaxAmount        *string           `json:"max_amount,omitempty"`
	SuggestedAmounts []string          `json:"suggested_amounts,omitempty"`
	Currency         string            `json:"currency"`
	Active           bool              `json:"active"`
	SuccessURL       *string           `json:"success_url,omitempty"`
	Metadata         map[string]string `json:"metadata"`
	Payout           Payout            `json:"payout"`
	Networks         []ProductNetwork  `json:"networks"`
	PaymentMethods   []PaymentMethod   `json:"payment_methods"`
	Links            LinkTotals        `json:"links"`
	CreatedAt        string            `json:"created_at"`
}

// ProductNetwork is the stablecoin choice of one network a product takes (D60): accept all (every
// stablecoin the account accepts there, turned on later included) or some (only Assets). A
// network the product does not take is not listed.
type ProductNetwork struct {
	Chain     string   `json:"chain"`
	ChainName string   `json:"chain_name"`
	Accept    string   `json:"accept"`
	Assets    []string `json:"assets,omitempty"`
}

// NetworksOf renders a product's network choices by chain id.
func NetworksOf(m map[string]store.NetworkChoice) []ProductNetwork {
	out := []ProductNetwork{}
	for _, id := range slices.Sorted(maps.Keys(m)) {
		n := m[id]
		v := ProductNetwork{Chain: id, ChainName: ChainName(id), Accept: string(n.Accept)}
		if n.Accept == store.AcceptSome {
			v.Assets = slices.Clone(n.Assets)
		}
		out = append(out, v)
	}
	return out
}

// PaymentMethod is one (chain, asset) a product's checkouts offer now, with the network name
// payers read ("Base") so a storefront can list a chain it does not know yet.
type PaymentMethod struct {
	Chain     string `json:"chain"`
	ChainName string `json:"chain_name"`
	Asset     string `json:"asset"`
}

var chainNames = map[string]string{
	"base":          "Base",
	"base-sepolia":  "Base Sepolia",
	"solana":        "Solana",
	"solana-devnet": "Solana devnet",
	"arbitrum":      "Arbitrum",
	"polygon":       "Polygon",
	"optimism":      "Optimism",
}

// ChainName is the network name payers read, never the chain id; an unnamed chain gets its id
// with dashes as spaces and the first letter upper case, as the checkout does.
func ChainName(id string) string {
	if n, ok := chainNames[id]; ok {
		return n
	}
	n := strings.ReplaceAll(id, "-", " ")
	if n == "" {
		return n
	}
	return strings.ToUpper(n[:1]) + n[1:]
}

// LinkTotals counts a product's payment links: every one, and those live now.
type LinkTotals struct {
	Live  int `json:"live"`
	Total int `json:"total"`
}

// Payout is where a product's money goes, keyed by chain id, each list in payout order (the
// first item takes the rounding remainder). An empty list means the product does not take that
// network.
type Payout map[string][]PayoutItem

// PayoutItem is one recipient of a product's payout: a wallet, its current label and address, and
// its share as a percentage ("70.00") and in basis points.
type PayoutItem struct {
	WalletID string `json:"wallet_id"`
	Label    string `json:"label"`
	Address  string `json:"address"`
	Share    string `json:"share"`
	Bps      uint16 `json:"bps"`
}

// PayoutOf renders a product's payout with every chain in chains as a key ([] for the networks
// it does not take), plus any other chain it names.
func PayoutOf(m map[string][]store.PayoutItem, chains []string) Payout {
	out := Payout{}
	for _, id := range chains {
		out[id] = []PayoutItem{}
	}
	for id, list := range m {
		items := []PayoutItem{}
		for _, it := range list {
			items = append(items, PayoutItem{WalletID: it.WalletID, Label: it.Label, Address: it.Address, Share: payout.FormatShare(it.Bps), Bps: it.Bps})
		}
		out[id] = items
	}
	return out
}

// URL is the public URL of a payment link or a session's checkout.
func (r Renderer) URL(kind, id string) string { return r.url(kind, id) }

// Product returns the API representation of a product with its link totals.
func (r Renderer) Product(p store.Product, links LinkTotals) Product {
	out := Product{
		ID: p.ID, Name: p.Name, Description: p.Description, PriceType: string(p.PriceType), Amount: usd(p.AmountCents),
		MinAmount: usd(p.MinAmountCents), MaxAmount: usd(p.MaxAmountCents), Currency: "USD", Active: p.Active,
		SuccessURL: p.SuccessURL, Metadata: meta(p.Metadata), CreatedAt: ts(p.CreatedAt),
		Payout: PayoutOf(p.Payout, r.chains(p.Livemode)), Networks: NetworksOf(p.Networks), PaymentMethods: []PaymentMethod{}, Links: links,
	}
	for _, s := range p.SuggestedCents {
		out.SuggestedAmounts = append(out.SuggestedAmounts, money.FormatUSD(s))
	}
	return out
}

// PayTo is one way to pay a session. Until its network is revealed (D48) it has no deposit
// details: no address or token account, revealed false.
type PayTo struct {
	Chain   string `json:"chain"`
	Asset   string `json:"asset"`
	Address string `json:"address,omitempty"`
	// Amount is null on an open-amount session: the payer decides how much to send.
	Amount *string `json:"amount"`
	// TokenAccount is the Solana deposit token account (the PDA's ATA for the mint).
	TokenAccount string `json:"token_account,omitempty"`
	// Revealed says the network's deposit details were shown (to the payer or through the
	// merchant API) and the network is watched; RevealedAt is when.
	Revealed   bool    `json:"revealed"`
	RevealedAt *string `json:"revealed_at,omitempty"`
}

// Payment is the API representation of a payment on a session.
type Payment struct {
	ID          string  `json:"id"`
	Chain       string  `json:"chain"`
	Asset       string  `json:"asset"`
	Amount      string  `json:"amount"`
	TxHash      string  `json:"tx_hash"`
	Status      string  `json:"status"`
	ConfirmedAt *string `json:"confirmed_at"`
	// Simulated is present (true) only on synthetic test-mode payments made by the simulator.
	Simulated bool `json:"simulated,omitempty"`
}

// Settlement is one forwarding of a session's funds: the fee to Prismaport and the net split
// among the recipients. fee + sum(payouts.amount) == gross, in the token's decimals.
type Settlement struct {
	Chain     string             `json:"chain"`
	Asset     string             `json:"asset"`
	Gross     string             `json:"gross"`
	Fee       string             `json:"fee"`
	Net       string             `json:"net"`
	Payouts   []SettlementPayout `json:"payouts"`
	TxHash    string             `json:"tx_hash"`
	SettledAt string             `json:"settled_at"`
	// Simulated is present (true) only on test-mode settlements made by the simulator.
	Simulated bool `json:"simulated,omitempty"`
}

// SettlementPayout is what one recipient received. Recipients whose share rounded to zero
// received nothing and are not listed.
type SettlementPayout struct {
	WalletID *string `json:"wallet_id"`
	Label    *string `json:"label"`
	Address  string  `json:"address"`
	Amount   string  `json:"amount"`
}

// PayoutRecipient is one recipient of a session's payout snapshot: who receives which share of
// the net in that family, fixed when the session opened.
type PayoutRecipient struct {
	WalletID *string `json:"wallet_id"`
	Label    *string `json:"label"`
	Address  string  `json:"address"`
	Share    string  `json:"share"`
	Bps      uint16  `json:"bps"`
}

// Session is the API representation of a checkout session.
type Session struct {
	ID        string  `json:"id"`
	Version   int64   `json:"version"`
	Livemode  bool    `json:"livemode"`
	Status    string  `json:"status"`
	ProductID *string `json:"product_id"`
	// LinkID is the payment link whose page opened the session; null for API sessions.
	LinkID *string `json:"link_id"`
	// CustomerAddressID is the static customer address the session was opened on; null otherwise.
	CustomerAddressID *string `json:"customer_address_id"`
	// Amount is null on an open-amount session (a recharge on a customer address): the payer
	// decides how much to send, and amount_remaining stays "0.00".
	Amount          *string `json:"amount"`
	Currency        string  `json:"currency"`
	AmountReceived  string  `json:"amount_received"`
	AmountRemaining string  `json:"amount_remaining"`
	CheckoutURL     string  `json:"checkout_url"`
	PayTo           []PayTo `json:"pay_to"`
	// Payout is the recipient list per chain the session's addresses and signatures commit
	// to, copied from its product when it opened; a later change to the product does not reach
	// it.
	Payout      map[string][]PayoutRecipient `json:"payout"`
	Payments    []Payment                    `json:"payments"`
	Settlement  *Settlement                  `json:"settlement"`
	Settlements []Settlement                 `json:"settlements,omitempty"`
	Metadata    map[string]string            `json:"metadata"`
	// ClientReferenceID correlates the session with the merchant's own records. Source
	// "merchant": set by the merchant's server through the API, trusted. Source "payer": taken
	// from a payment link URL, which anyone can edit: correlation only, never authorization.
	ClientReferenceID     *string `json:"client_reference_id"`
	ClientReferenceSource *string `json:"client_reference_source"`
	// ExpiresAt is the current deadline: the end of the selection window until a network is
	// revealed, then PaymentStartedAt plus PayWindowSeconds (D48).
	ExpiresAt string `json:"expires_at"`
	// PaymentStartedAt is the first reveal (null until then); PayWindowSeconds is how long the
	// session stays open from it (the request's expires_in).
	PaymentStartedAt *string `json:"payment_started_at"`
	PayWindowSeconds int64   `json:"pay_window_seconds"`
	CreatedAt        string  `json:"created_at"`
	PaidAt           *string `json:"paid_at"`
}

// usdExact renders a canonical-scale amount as USD with at least two places and every non-zero
// digit beyond them, so a sub-cent remainder is never hidden by rounding.
func usdExact(u money.Units) string {
	return money.FormatMinPlaces(u, chain.CanonicalDecimals, money.USDDecimals)
}

// Session returns the API representation of a session.
func (r Renderer) Session(s *store.Session) Session {
	st := s.State
	out := Session{
		ID: s.ID, Version: st.Version, Livemode: s.Livemode, Status: string(st.Status), ProductID: s.ProductID, LinkID: s.LinkID,
		CustomerAddressID: s.CustomerAddressID, Currency: "USD",
		AmountReceived: usdExact(st.Received()), AmountRemaining: usdExact(st.Remaining()),
		CheckoutURL: r.url("s", s.ID), PayTo: []PayTo{}, Payout: map[string][]PayoutRecipient{}, Payments: []Payment{}, Metadata: meta(s.Metadata),
		ClientReferenceID: s.ClientReferenceID, ClientReferenceSource: s.ClientReferenceSource,
		ExpiresAt: ts(st.ExpiresAt), CreatedAt: ts(s.CreatedAt), PaidAt: optTS(st.PaidAt),
		PaymentStartedAt: optTS(s.PaymentStartedAt), PayWindowSeconds: int64(s.PayWindow / time.Second),
	}
	if !st.OpenAmount {
		out.Amount = usd(&st.AmountCents)
	}
	for _, p := range s.PayTo {
		pt := PayTo{Chain: p.Chain, Asset: p.Asset}
		if !st.OpenAmount {
			amount := money.FormatDecimal(p.Amount, p.Decimals)
			pt.Amount = &amount
		}
		if r := s.Revealed(p.Chain); r != nil {
			pt.Address, pt.TokenAccount, pt.Revealed, pt.RevealedAt = p.Address, p.TokenAccount(), true, optTS(&r.RevealedAt)
		}
		out.PayTo = append(out.PayTo, pt)
	}
	out.Payout = payoutView(s.Payouts)
	for _, p := range st.Payments {
		out.Payments = append(out.Payments, Payment{ID: p.ID, Chain: p.Chain, Asset: p.Asset,
			Amount: money.FormatDecimal(p.Amount, p.Decimals), TxHash: p.TxHash, Status: string(p.Status), ConfirmedAt: optTS(p.ConfirmedAt),
			Simulated: p.Simulated})
	}
	for _, x := range s.Settlements {
		st := Settlement{Chain: x.Chain, Asset: x.Asset, Gross: money.FormatDecimal(x.Gross, x.Decimals),
			Fee: money.FormatDecimal(x.Fee, x.Decimals), Net: money.FormatDecimal(x.Net, x.Decimals), Payouts: []SettlementPayout{},
			TxHash: x.TxHash, SettledAt: ts(x.SettledAt), Simulated: x.Simulated}
		for _, p := range x.Payouts {
			st.Payouts = append(st.Payouts, SettlementPayout{WalletID: p.WalletID, Label: p.Label, Address: p.Address,
				Amount: money.FormatDecimal(p.Amount, x.Decimals)})
		}
		out.Settlements = append(out.Settlements, st)
	}
	if n := len(out.Settlements); n > 0 {
		latest := out.Settlements[n-1]
		out.Settlement = &latest
		if n == 1 {
			out.Settlements = nil
		}
	}
	return out
}

// payoutView renders a payout snapshot per chain.
func payoutView(list []store.SessionPayout) map[string][]PayoutRecipient {
	out := map[string][]PayoutRecipient{}
	for _, p := range list {
		out[p.Chain] = append(out[p.Chain], PayoutRecipient{WalletID: p.WalletID, Label: p.Label,
			Address: p.Address, Share: payout.FormatShare(p.Bps), Bps: p.Bps})
	}
	return out
}

// CustomerAddress is a static deposit address of one customer (B11). Sessions are opened on it
// one at a time; it is watched only while one is open (D37).
type CustomerAddress struct {
	ID       string `json:"id"`
	Livemode bool   `json:"livemode"`
	// CustomerID is the merchant's own id for the customer.
	CustomerID string `json:"customer_id"`
	ProductID  string `json:"product_id"`
	// Status is active or rotated; ReplacedBy names the address that replaced a rotated one.
	Status     string         `json:"status"`
	ReplacedBy *string        `json:"replaced_by"`
	PayTo      []AddressPayTo `json:"pay_to"`
	// Payout is the recipient list per chain the addresses commit to, copied from the product
	// when the address was created; only a rotation picks up a later change.
	Payout    map[string][]PayoutRecipient `json:"payout"`
	Metadata  map[string]string            `json:"metadata"`
	URL       string                       `json:"url"`
	CreatedAt string                       `json:"created_at"`
	RotatedAt *string                      `json:"rotated_at"`
}

// AddressPayTo is where the customer pays on one (chain, asset): the same address every time.
type AddressPayTo struct {
	Chain   string `json:"chain"`
	Asset   string `json:"asset"`
	Address string `json:"address"`
	// TokenAccount is the Solana deposit token account (the PDA's ATA for the mint).
	TokenAccount string `json:"token_account,omitempty"`
}

// CustomerAddress returns the API representation of a static customer address.
func (r Renderer) CustomerAddress(ca store.CustomerAddress) CustomerAddress {
	out := CustomerAddress{ID: ca.ID, Livemode: ca.Livemode, CustomerID: ca.CustomerID, ProductID: ca.ProductID, Status: ca.Status,
		ReplacedBy: ca.ReplacedBy, PayTo: []AddressPayTo{}, Payout: payoutView(ca.Payouts), Metadata: meta(ca.Metadata),
		URL: r.url("a", ca.ID), CreatedAt: ts(ca.CreatedAt), RotatedAt: optTS(ca.RotatedAt)}
	for _, p := range ca.PayTo {
		out.PayTo = append(out.PayTo, AddressPayTo{Chain: p.Chain, Asset: p.Asset, Address: p.Address, TokenAccount: p.TokenAccount()})
	}
	return out
}

// Envelope is the webhook and event payload: an event id and type around its data.
type Envelope struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"`
	CreatedAt string    `json:"created_at"`
	Data      EventData `json:"data"`
}

// EventData is the data carried by an event envelope.
type EventData struct {
	Session Session `json:"session"`
}

// EventPayload implements store.Renderer.
func (r Renderer) EventPayload(m store.EventMeta, s *store.Session) ([]byte, error) {
	return json.Marshal(Envelope{ID: m.ID, Type: m.Type, CreatedAt: ts(m.CreatedAt), Data: EventData{Session: r.Session(s)}})
}

// ErrorBody is the JSON body of an API error response.
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail is the code, message and optional field of an API error.
type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// Param names the request field the error is about, such as "payout.base[1].share".
	Param string `json:"param,omitempty"`
}
