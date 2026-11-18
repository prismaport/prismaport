//go:build integration

package store_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/ids"
	"github.com/prismaport/prismaport/internal/store"
)

func wallet(t *testing.T, st *store.Store, mid string, live bool, fam chain.Family, label, addr string) store.Wallet {
	t.Helper()
	w, err := st.CreateWallet(t.Context(), store.Wallet{MerchantID: mid, Livemode: live, Family: fam, Label: label, Address: addr,
		Verified: true}, 3, "op")
	if err != nil {
		t.Fatalf("wallet %s: %v", addr, err)
	}
	return w
}

// payTo is a product payout paying one wallet 100% in its family.
func payTo(w store.Wallet) map[string][]store.PayoutItem {
	id := map[chain.Family]string{chain.EVM: "base", chain.Solana: "solana"}[w.Family]
	if !w.Livemode {
		id = map[chain.Family]string{chain.EVM: "base-sepolia", chain.Solana: "solana-devnet"}[w.Family]
	}
	return map[string][]store.PayoutItem{id: {{WalletID: w.ID, Bps: 10_000, Family: w.Family}}}
}

// paidWallet adds a wallet to a merchant (any family, live) and answers a payout paying it.
func paidWallet(t *testing.T, st *store.Store, mid string, live bool) map[string][]store.PayoutItem {
	t.Helper()
	return payTo(wallet(t, st, mid, live, chain.EVM, "Main", "0xPAID"+ids.New("x")[20:]))
}

func fixedProduct(mid string, live bool, payout map[string][]store.PayoutItem) store.Product {
	cents := u("2500")
	return store.Product{ID: ids.New(ids.Product), MerchantID: mid, Livemode: live, Name: "Founder", PriceType: store.PriceFixed,
		AmountCents: &cents, Active: true, Payout: payout}
}

func TestWalletsProductPayoutsAndAudit(t *testing.T) {
	t.Parallel()
	st := newStore(t)
	ctx := t.Context()
	mid := merchant(t, st)

	first := wallet(t, st, mid, true, chain.EVM, "Main", "0xA")
	if first.UsedByProducts != 0 {
		t.Fatalf("a new wallet is used by %d products", first.UsedByProducts)
	}
	second := wallet(t, st, mid, true, chain.EVM, "Partner", "0xB")
	if _, err := st.CreateWallet(ctx, store.Wallet{MerchantID: mid, Livemode: true, Family: chain.EVM, Label: "Dup", Address: "0xA"}, 3, "op"); !errors.Is(err, store.ErrWalletExists) {
		t.Fatalf("duplicate active address: %v", err)
	}
	third := wallet(t, st, mid, true, chain.EVM, "Third", "0xC")
	if _, err := st.CreateWallet(ctx, store.Wallet{MerchantID: mid, Livemode: true, Family: chain.EVM, Label: "Fourth", Address: "0xD"}, 3, "op"); !errors.Is(err, store.ErrWalletLimit) {
		t.Fatalf("wallet above the limit: %v", err)
	}
	sol := wallet(t, st, mid, true, chain.Solana, "Sol", "So1")
	testMode := wallet(t, st, mid, false, chain.EVM, "Test", "0xA")
	if oldest, err := st.DefaultWallet(ctx, mid, true, chain.EVM); err != nil || oldest.ID != first.ID {
		t.Fatalf("oldest active wallet: %+v %v", oldest, err)
	}
	if _, err := st.DefaultWallet(ctx, mid, false, chain.Solana); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("oldest wallet of an empty family: %v", err)
	}

	// Items are kept in payout order, never sorted, with each wallet's label and address.
	split := map[string][]store.PayoutItem{
		"base":   {{WalletID: second.ID, Bps: 7000, Family: chain.EVM}, {WalletID: first.ID, Bps: 3000, Family: chain.EVM}},
		"solana": {{WalletID: sol.ID, Bps: 10_000, Family: chain.Solana}},
		// Another network of the family pays its own split: the same wallet, other shares.
		"polygon": {{WalletID: first.ID, Bps: 10_000, Family: chain.EVM}},
	}
	p, err := st.CreateProduct(ctx, fixedProduct(mid, true, split), "")
	if err != nil {
		t.Fatal(err)
	}
	got := p.Payout["base"]
	if len(got) != 2 || got[0].WalletID != second.ID || got[0].Label != "Partner" || got[0].Address != "0xB" || got[1].Bps != 3000 ||
		len(p.Payout["solana"]) != 1 || p.Payout["solana"][0].Address != "So1" || len(p.Payout["polygon"]) != 1 || p.Payout["polygon"][0].WalletID != first.ID {
		t.Fatalf("payout roundtrip: %+v", p.Payout)
	}
	if w, _ := st.GetWallet(ctx, mid, true, second.ID); w.UsedByProducts != 1 {
		t.Fatalf("used by %d products", w.UsedByProducts)
	}

	for name, payout := range map[string]map[string][]store.PayoutItem{
		"another family's wallet": {"base": {{WalletID: sol.ID, Bps: 10_000, Family: chain.EVM}}},
		"another mode's wallet":   {"base": {{WalletID: testMode.ID, Bps: 10_000, Family: chain.EVM}}},
		"an unknown wallet":       {"base": {{WalletID: "wal_nope", Bps: 10_000, Family: chain.EVM}}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := st.CreateProduct(ctx, fixedProduct(mid, true, payout), ""); !errors.Is(err, store.ErrWalletUnavailable) {
				t.Errorf("%s: %v", name, err)
			}
		})
	}
	// The database checks the whole product at commit, whatever the caller validated.
	for name, c := range map[string]struct {
		payout map[string][]store.PayoutItem
		want   string
	}{
		"shares below 100%": {map[string][]store.PayoutItem{"base": {{WalletID: first.ID, Bps: 9000, Family: chain.EVM}}}, "10000 bps"},
		"two families on one network": {map[string][]store.PayoutItem{"base": {{WalletID: first.ID, Bps: 5000, Family: chain.EVM},
			{WalletID: sol.ID, Bps: 5000, Family: chain.Solana}}}, "one family"},
		"six wallets": {map[string][]store.PayoutItem{"base": {{WalletID: first.ID, Bps: 5000, Family: chain.EVM}, {WalletID: second.ID, Bps: 1000, Family: chain.EVM},
			{WalletID: third.ID, Bps: 1000, Family: chain.EVM}, {WalletID: first.ID, Bps: 1000, Family: chain.EVM}, {WalletID: second.ID, Bps: 1000, Family: chain.EVM}, {WalletID: third.ID, Bps: 1000, Family: chain.EVM}}}, ""},
		"no network": {nil, "pays no network"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := st.CreateProduct(ctx, fixedProduct(mid, true, c.payout), "")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("%s reached the database: %v", name, err)
			}
		})
	}

	var inUse *store.WalletInUseError
	if _, err := st.ArchiveWallet(ctx, mid, true, second.ID, "op"); !errors.As(err, &inUse) || inUse.Products != 1 || !errors.Is(err, store.ErrWalletInUse) {
		t.Fatalf("archiving a wallet a product pays: %v", err)
	}
	archived, err := st.ArchiveWallet(ctx, mid, true, third.ID, "op")
	if err != nil || archived.ArchivedAt == nil {
		t.Fatalf("archive: %+v %v", archived, err)
	}
	if again, err := st.ArchiveWallet(ctx, mid, true, third.ID, "op"); err != nil || again.ArchivedAt == nil {
		t.Fatalf("archiving twice: %v", err)
	}
	wallet(t, st, mid, true, chain.EVM, "Fourth", "0xC")
	if _, err := st.CreateProduct(ctx, fixedProduct(mid, true, payTo(third)), ""); !errors.Is(err, store.ErrWalletUnavailable) {
		t.Fatalf("a product paying an archived wallet: %v", err)
	}
	list, _ := st.ListWallets(ctx, mid, true, store.WalletFilter{Family: chain.EVM, IncludeArchived: true})
	if len(list) != 4 || list[3].ArchivedAt == nil {
		t.Fatalf("archived wallets list last: %+v", list)
	}

	// A patch that leaves the payout alone writes no payout audit; a change does, with both lists.
	if _, err := st.UpdateProductAs(ctx, mid, true, p.ID, "op", func(p *store.Product) error {
		p.Name = "Renamed"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	upd, err := st.UpdateProductAs(ctx, mid, true, p.ID, "op", func(p *store.Product) error {
		p.Payout = map[string][]store.PayoutItem{"base": {{WalletID: first.ID, Bps: 5000, Family: chain.EVM}, {WalletID: second.ID, Bps: 5000, Family: chain.EVM}}}
		return nil
	})
	if err != nil || len(upd.Payout["base"]) != 2 || upd.Payout["base"][0].Label != "Main" || len(upd.Payout["solana"]) != 0 {
		t.Fatalf("update: %+v %v", upd, err)
	}
	if back, _ := st.GetProduct(ctx, mid, true, p.ID); len(back.Payout["solana"]) != 0 || back.Payout["base"][1].WalletID != second.ID {
		t.Fatalf("read back: %+v", back.Payout)
	}
	if _, err := st.ArchiveWallet(ctx, mid, true, sol.ID, "op"); err != nil {
		t.Fatalf("a wallet no product pays any more: %v", err)
	}
	if _, err := st.UpdateProductAs(ctx, mid, true, p.ID, "op", func(p *store.Product) error {
		p.Payout = nil
		return nil
	}); err == nil {
		t.Fatal("a product left paying nothing")
	}

	other := merchant(t, st)
	if _, err := st.GetWallet(ctx, other, true, first.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("a wallet visible to another merchant")
	}
	if _, err := st.CreateProduct(ctx, fixedProduct(other, true, payTo(first)), ""); !errors.Is(err, store.ErrWalletUnavailable) {
		t.Fatalf("a product paying another merchant's wallet: %v", err)
	}

	log, _ := st.AuditLog(ctx, mid)
	var actions []string
	for _, e := range log {
		actions = append(actions, e.Action)
	}
	want := "merchant.created,wallet.created,wallet.created,wallet.created,wallet.created,wallet.created,wallet.archived,wallet.created," +
		"product.updated,product.payout_changed,product.updated,wallet.archived"
	if strings.Join(actions, ",") != want {
		t.Fatalf("audit\n got %v\nwant %s", strings.Join(actions, ","), want)
	}
	var details struct {
		ProductID string                      `json:"product_id"`
		Previous  map[string][]map[string]any `json:"previous"`
		Payout    map[string][]map[string]any `json:"payout"`
	}
	if err := st.Pool().QueryRow(ctx, `SELECT details FROM audit_log WHERE merchant_id = $1 AND action = 'product.payout_changed'`,
		mid).Scan(&details); err != nil {
		t.Fatal(err)
	}
	item := func(list []map[string]any, i int) string {
		if i >= len(list) {
			return "-"
		}
		return fmt.Sprintf("%v:%v:%v", list[i]["wallet_id"], list[i]["address"], list[i]["bps"])
	}
	if details.ProductID != p.ID || item(details.Previous["base"], 0) != second.ID+":0xB:7000" || item(details.Previous["solana"], 0) != sol.ID+":So1:10000" ||
		item(details.Previous["polygon"], 0) != first.ID+":0xA:10000" || item(details.Payout["base"], 1) != second.ID+":0xB:5000" ||
		len(details.Payout) != 1 {
		t.Fatalf("payout audit %+v", details)
	}
}

// D57: the family's first wallet is its default, a later one leaves it alone, any active wallet
// can be made the default, and archiving the default hands it to the oldest remaining wallet.
func TestWalletDefault(t *testing.T) {
	t.Parallel()
	st := newStore(t)
	ctx := t.Context()
	mid := merchant(t, st)
	first := wallet(t, st, mid, true, chain.EVM, "Main", "0xA")
	second := wallet(t, st, mid, true, chain.EVM, "Partner", "0xB")
	third := wallet(t, st, mid, true, chain.EVM, "Third", "0xC")
	sol := wallet(t, st, mid, true, chain.Solana, "Sol", "So1")
	if !first.Default || second.Default || !sol.Default {
		t.Fatalf("defaults on create: %v %v %v", first.Default, second.Default, sol.Default)
	}
	defaults := func() string {
		t.Helper()
		list, err := st.ListWallets(ctx, mid, true, store.WalletFilter{IncludeArchived: true})
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, w := range list {
			if w.Default {
				out = append(out, w.Label)
			}
		}
		return strings.Join(out, ",")
	}
	if got := defaults(); got != "Main,Sol" {
		t.Fatalf("defaults: %s", got)
	}
	w, err := st.SetDefaultWallet(ctx, mid, true, second.ID, "op")
	if err != nil || !w.Default {
		t.Fatalf("set default: %+v %v", w, err)
	}
	if got := defaults(); got != "Partner,Sol" {
		t.Fatalf("defaults after set: %s", got)
	}
	if d, err := st.DefaultWallet(ctx, mid, true, chain.EVM); err != nil || d.ID != second.ID {
		t.Fatalf("default wallet: %+v %v", d, err)
	}
	if again, err := st.SetDefaultWallet(ctx, mid, true, second.ID, "op"); err != nil || !again.Default {
		t.Fatalf("set the default again: %v", err)
	}
	// Back to the older wallet and forth again: the unique index must hold at every row.
	if _, err := st.SetDefaultWallet(ctx, mid, true, first.ID, "op"); err != nil {
		t.Fatalf("move the default back to the older wallet: %v", err)
	}
	if got := defaults(); got != "Main,Sol" {
		t.Fatalf("defaults after moving back: %s", got)
	}
	if _, err := st.SetDefaultWallet(ctx, mid, true, second.ID, "op"); err != nil {
		t.Fatalf("move the default forward again: %v", err)
	}
	if _, err := st.ArchiveWallet(ctx, mid, true, second.ID, "op"); err != nil {
		t.Fatal(err)
	}
	if got := defaults(); got != "Main,Sol" {
		t.Fatalf("defaults after archiving the default: %s", got)
	}
	if _, err := st.SetDefaultWallet(ctx, mid, true, second.ID, "op"); !errors.Is(err, store.ErrWalletUnavailable) {
		t.Fatalf("an archived default: %v", err)
	}
	if _, err := st.SetDefaultWallet(ctx, merchant(t, st), true, third.ID, "op"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("another merchant's wallet: %v", err)
	}
	if _, err := st.SetDefaultWallet(ctx, mid, false, third.ID, "op"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a wallet of the other mode: %v", err)
	}
	// Archiving that default hands it on again, to the only active EVM wallet left.
	if _, err := st.ArchiveWallet(ctx, mid, true, first.ID, "op"); err != nil {
		t.Fatal(err)
	}
	if d, err := st.DefaultWallet(ctx, mid, true, chain.EVM); err != nil || d.ID != third.ID || !d.Default {
		t.Fatalf("default after archiving: %+v %v", d, err)
	}
}
