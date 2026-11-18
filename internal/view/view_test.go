package view

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/session"
	"github.com/prismaport/prismaport/internal/store"
)

var t0 = time.Date(2026, 10, 23, 18, 0, 0, 0, time.UTC)

func fixture() *store.Session {
	pt := []store.PayToRecord{{Chain: "base", Asset: "USDC", Decimals: 6, Address: "0xabc", Amount: money.FromUint64(25_000_000)}}
	st := session.New(money.FromUint64(2500), store.PayToState(pt), t0.Add(time.Hour))
	st, _, _ = st.DetectPayment(session.NewPayment{ID: "pay_1", Chain: "base", Asset: "USDC", Address: "0xabc", TxHash: "0xt", Amount: money.FromUint64(10_000_001)}, t0)
	st, _, _ = st.ConfirmPayment("pay_1", t0.Add(time.Minute))
	return &store.Session{ID: "ses_1", MerchantID: "mer_1", CreatedAt: t0, PayTo: pt, State: st}
}

func TestSessionRendering(t *testing.T) {
	r := Renderer{CheckoutBaseURL: "https://pay.x/"}
	v := r.Session(fixture())
	if v.AmountReceived != "10.000001" || v.AmountRemaining != "14.999999" {
		t.Fatalf("sub-cent amounts rounded: %s / %s", v.AmountReceived, v.AmountRemaining)
	}
	if v.CheckoutURL != "https://pay.x/s/ses_1" || v.Status != "partially_paid" || v.Version != 3 {
		t.Fatalf("%+v", v)
	}
	if v.Payments[0].Amount != "10.000001" || *v.Payments[0].ConfirmedAt != "2026-10-23T18:01:00Z" {
		t.Fatalf("payment %+v", v.Payments[0])
	}
	b, _ := json.Marshal(v)
	if !strings.Contains(string(b), `"settlement":null`) || strings.Contains(string(b), `"settlements"`) || !strings.Contains(string(b), `"metadata":{}`) {
		t.Fatalf("json %s", b)
	}
	// An API session names no link; a session opened from a link's page names it.
	if !strings.Contains(string(b), `"product_id":null,"link_id":null,`) {
		t.Fatalf("link_id missing: %s", b)
	}
	s := fixture()
	s.ProductID, s.LinkID = str("prod_1"), str("plink_1")
	if b, _ := json.Marshal(r.Session(s)); !strings.Contains(string(b), `"product_id":"prod_1","link_id":"plink_1",`) {
		t.Fatalf("link session %s", b)
	}
}

func TestSettlementRendering(t *testing.T) {
	r := Renderer{}
	s := fixture()
	s.Settlements = []store.Settlement{
		{Chain: "base", Asset: "USDC", Decimals: 6, Gross: money.FromUint64(10_010_000), Fee: money.FromUint64(100_100), Net: money.FromUint64(9_909_900), TxHash: "0x1", SettledAt: t0},
	}
	v := r.Session(s)
	if v.Settlement == nil || v.Settlement.Fee != "0.100100" || v.Settlement.Net != "9.909900" || v.Settlements != nil {
		t.Fatalf("single: %+v", v)
	}
	s.Settlements = append(s.Settlements, store.Settlement{Chain: "base", Asset: "USDC", Decimals: 6, Gross: money.FromUint64(1_000_000),
		Fee: money.FromUint64(100_000), Net: money.FromUint64(900_000), TxHash: "0x2", SettledAt: t0.Add(time.Hour)})
	v = r.Session(s)
	if v.Settlement.TxHash != "0x2" || len(v.Settlements) != 2 {
		t.Fatalf("several: %+v", v)
	}
}

func TestEnvelope(t *testing.T) {
	b, err := Renderer{}.EventPayload(store.EventMeta{ID: "evt_1", Type: "session.paid", CreatedAt: t0}, fixture())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(b), `{"id":"evt_1","type":"session.paid","created_at":"2026-10-23T18:00:00Z","data":{"session":{"id":"ses_1"`) {
		t.Fatalf("envelope %s", b)
	}
}

func TestProductRendering(t *testing.T) {
	min, max := money.FromUint64(100), money.FromUint64(5000)
	modes := func(live bool) []string {
		if live {
			return []string{"base", "solana"}
		}
		return []string{"base-sepolia", "solana-devnet"}
	}
	p := Renderer{CheckoutBaseURL: "https://pay.x", Chains: modes}.Product(store.Product{ID: "prod_1", Name: "Donate", PriceType: store.PriceOpen,
		MinAmountCents: &min, MaxAmountCents: &max, SuggestedCents: []money.Units{money.FromUint64(500)}, Active: true, CreatedAt: t0},
		LinkTotals{Live: 1, Total: 3})
	b, _ := json.Marshal(p)
	want := `{"id":"prod_1","name":"Donate","price_type":"open","amount":null,"min_amount":"1.00","max_amount":"50.00","suggested_amounts":["5.00"],"currency":"USD","active":true,"metadata":{},"payout":{"base-sepolia":[],"solana-devnet":[]},"networks":[],"payment_methods":[],"links":{"live":1,"total":3},"created_at":"2026-10-23T18:00:00Z"}`
	if string(b) != want {
		t.Fatalf("got  %s\nwant %s", b, want)
	}
	// Networks lists each taken network by chain id: all without assets, some with its list (D60).
	p = Renderer{Chains: modes}.Product(store.Product{ID: "prod_2", Name: "Two", PriceType: store.PriceFixed, AmountCents: &max, CreatedAt: t0,
		Networks: map[string]store.NetworkChoice{"solana-devnet": {Accept: store.AcceptSome, Assets: []string{"USDC"}}, "base-sepolia": {Accept: store.AcceptAll}}},
		LinkTotals{})
	if b, _ := json.Marshal(p.Networks); string(b) != `[{"chain":"base-sepolia","chain_name":"Base Sepolia","accept":"all"},{"chain":"solana-devnet","chain_name":"Solana devnet","accept":"some","assets":["USDC"]}]` {
		t.Fatalf("networks %s", b)
	}
	// A product has no URL of its own; its description shows when it has one.
	p = Renderer{Chains: modes}.Product(store.Product{ID: "prod_3", Name: "Lifetime", Description: str("Every feature, forever."),
		PriceType: store.PriceFixed, AmountCents: &max, CreatedAt: t0}, LinkTotals{})
	if b, _ := json.Marshal(p); !strings.Contains(string(b), `"name":"Lifetime","description":"Every feature, forever.","price_type":"fixed"`) ||
		strings.Contains(string(b), `"url"`) {
		t.Fatalf("product %s", b)
	}
	// Every chain of the product's mode is a key; a chain the product names outside the registry
	// (one removed later) still shows.
	p = Renderer{Chains: modes}.Product(store.Product{ID: "prod_2", Livemode: true, CreatedAt: t0, Payout: map[string][]store.PayoutItem{
		"solana": {{WalletID: "wal_a", Bps: 6667, Label: "Main", Address: "SoA"}, {WalletID: "wal_b", Bps: 3333, Label: "Partner", Address: "SoB"}},
		"old":    {{WalletID: "wal_c", Bps: 10_000, Label: "Old", Address: "0xC"}}}}, LinkTotals{})
	if b, _ := json.Marshal(p.Payout); string(b) != `{"base":[],"old":[{"wallet_id":"wal_c","label":"Old","address":"0xC","share":"100.00","bps":10000}],`+
		`"solana":[{"wallet_id":"wal_a","label":"Main","address":"SoA","share":"66.67","bps":6667},`+
		`{"wallet_id":"wal_b","label":"Partner","address":"SoB","share":"33.33","bps":3333}]}` {
		t.Fatalf("payout %s", b)
	}
}

func str(s string) *string { return &s }

// The session carries its payout snapshot per chain and each settlement its payouts, with
// fee + sum(payouts) == gross in the token's decimals.
func TestPayoutRendering(t *testing.T) {
	s := fixture()
	s.Payouts = []store.SessionPayout{
		{Chain: "base", Family: chain.EVM, Position: 0, WalletID: str("wal_a"), Label: str("Main"), Address: "0xA", Bps: 7000},
		{Chain: "base", Family: chain.EVM, Position: 1, WalletID: str("wal_b"), Label: str("Partner"), Address: "0xB", Bps: 3000},
	}
	s.Settlements = []store.Settlement{{Chain: "base", Asset: "USDC", Decimals: 6, Gross: money.FromUint64(25_000_000),
		Fee: money.FromUint64(250_000), Net: money.FromUint64(24_750_000), TxHash: "0x1", SettledAt: t0, Simulated: true,
		Payouts: []store.SettlementPayout{
			{Position: 0, WalletID: str("wal_a"), Label: str("Main"), Address: "0xA", Amount: money.FromUint64(17_325_000)},
			{Position: 1, WalletID: str("wal_b"), Label: str("Partner"), Address: "0xB", Amount: money.FromUint64(7_425_000)},
		}}}
	v := Renderer{}.Session(s)
	b, _ := json.Marshal(v.Payout)
	if string(b) != `{"base":[{"wallet_id":"wal_a","label":"Main","address":"0xA","share":"70.00","bps":7000},{"wallet_id":"wal_b","label":"Partner","address":"0xB","share":"30.00","bps":3000}]}` {
		t.Fatalf("payout %s", b)
	}
	b, _ = json.Marshal(v.Settlement)
	if string(b) != `{"chain":"base","asset":"USDC","gross":"25.000000","fee":"0.250000","net":"24.750000","payouts":[{"wallet_id":"wal_a","label":"Main","address":"0xA","amount":"17.325000"},{"wallet_id":"wal_b","label":"Partner","address":"0xB","amount":"7.425000"}],"tx_hash":"0x1","settled_at":"2026-10-23T18:00:00Z","simulated":true}` {
		t.Fatalf("settlement %s", b)
	}
	s.Settlements[0].Simulated = false
	if b, _ := json.Marshal(Renderer{}.Session(s).Settlement); strings.Contains(string(b), "simulated") {
		t.Fatalf("a real settlement says simulated: %s", b)
	}
	if b, _ := json.Marshal(Renderer{}.Session(fixture())); !strings.Contains(string(b), `"payout":{}`) {
		t.Fatalf("a session without a snapshot must render an empty payout object: %s", b)
	}
}

func TestChainName(t *testing.T) {
	for id, want := range map[string]string{"base": "Base", "solana-devnet": "Solana devnet", "arbitrum-sepolia": "Arbitrum sepolia", "": ""} {
		t.Run(id, func(t *testing.T) {
			if got := ChainName(id); got != want {
				t.Errorf("ChainName(%q) = %q, want %q", id, got, want)
			}
		})
	}
}
