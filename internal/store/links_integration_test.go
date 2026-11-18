//go:build integration

package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/ids"
	"github.com/prismaport/prismaport/internal/session"
	"github.com/prismaport/prismaport/internal/store"
)

func openProduct(t *testing.T, st *store.Store, mid, min, max string) store.Product {
	t.Helper()
	lo := u(min)
	p := store.Product{ID: ids.New(ids.Product), MerchantID: mid, Name: "Donate", PriceType: store.PriceOpen, MinAmountCents: &lo,
		Active: true, Payout: paidWallet(t, st, mid, false)}
	if max != "" {
		hi := u(max)
		p.MaxAmountCents = &hi
	}
	got, err := st.CreateProduct(t.Context(), p, "")
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func createLink(t *testing.T, st *store.Store, l store.PaymentLink) store.PaymentLink {
	t.Helper()
	ctx := t.Context()
	err := pgx.BeginFunc(ctx, st.Pool(), func(tx pgx.Tx) error {
		if _, err := st.LockProductShared(ctx, tx, l.MerchantID, l.Livemode, l.ProductID); err != nil {
			return err
		}
		return st.CreateLinkTx(ctx, tx, l, "test")
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.GetLink(ctx, l.MerchantID, l.Livemode, l.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// linkSession stores an open session of product, opened from link when it is not empty.
func linkSession(t *testing.T, st *store.Store, mid, productID, linkID string, status session.Status) {
	t.Helper()
	ctx := t.Context()
	payTo := []store.PayToRecord{{Chain: "base-sepolia", Asset: "USDC", Family: chain.EVM, Token: "0xT", Decimals: 6, Address: "0x" + ids.New("a")[2:],
		Amount: u("1000000"), Treasury: "0xR", BoundTokens: []string{"0xT"}}}
	ses := &store.Session{ID: ids.New(ids.Session), MerchantID: mid, PayWindow: 30 * time.Minute, ProductID: &productID, CreatedAt: now, UpdatedAt: now, PayTo: payTo,
		Payouts: []store.SessionPayout{{Chain: "base-sepolia", Family: chain.EVM, Address: "0xD", Bps: 10_000}}}
	copy(ses.Salt[:], ids.New("salt"))
	if linkID != "" {
		ses.LinkID = &linkID
	}
	ses.State = session.New(u("100"), store.PayToState(payTo), now.Add(time.Hour))
	ses.State.Status = status
	if err := pgx.BeginFunc(ctx, st.Pool(), func(tx pgx.Tx) error { return st.InsertSession(ctx, tx, ses) }); err != nil {
		t.Fatal(err)
	}
}

func TestPaymentLinkRoundtrip(t *testing.T) {
	t.Parallel()
	st := newStore(t)
	ctx := t.Context()
	mid := merchant(t, st)
	p := openProduct(t, st, mid, "500", "10000")
	exp := now.Add(36*time.Hour + 789*time.Microsecond)
	limit := 250
	amount := u("2500")
	label, url := "Invoice ACME #12", "https://shop.example/thanks"
	got := createLink(t, st, store.PaymentLink{ID: ids.New(ids.PaymentLink), MerchantID: mid, ProductID: p.ID, Name: &label, AmountCents: &amount,
		Active: true, ExpiresAt: &exp, MaxPayments: &limit, SuccessURL: &url, Metadata: map[string]string{"invoice": "12"}})
	if *got.Name != label || !got.AmountCents.Equal(amount) || !got.ExpiresAt.Equal(exp) || *got.MaxPayments != limit || *got.SuccessURL != url ||
		got.Metadata["invoice"] != "12" || !got.CreatedAt.Equal(now) || got.ProductID != p.ID {
		t.Fatalf("roundtrip: %+v", got)
	}
	upd, err := st.UpdateLinkAs(ctx, mid, false, got.ID, "test", func(l *store.PaymentLink, prod store.Product) error {
		if prod.ID != p.ID {
			t.Errorf("fn got product %s", prod.ID)
		}
		l.Name, l.AmountCents, l.ExpiresAt, l.MaxPayments, l.SuccessURL, l.Metadata, l.Active = nil, nil, nil, nil, nil, nil, false
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	again, err := st.GetLink(ctx, mid, false, got.ID)
	if err != nil || again.Name != nil || again.AmountCents != nil || again.ExpiresAt != nil || again.MaxPayments != nil || again.SuccessURL != nil ||
		len(again.Metadata) != 0 || again.Active || upd.Active {
		t.Fatalf("cleared: %+v %v", again, err)
	}
	abort := errors.New("abort")
	if _, err := st.UpdateLinkAs(ctx, mid, false, got.ID, "test", func(l *store.PaymentLink, _ store.Product) error { l.Active = true; return abort }); !errors.Is(err, abort) {
		t.Fatal(err)
	}
	if l, _ := st.GetLink(ctx, mid, false, got.ID); l.Active {
		t.Fatal("aborted update persisted")
	}
	if _, err := st.GetLink(ctx, mid, true, got.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("link visible in the other mode")
	}
	if l, err := st.LoadLink(ctx, got.ID); err != nil || l.MerchantID != mid {
		t.Fatalf("public load: %+v %v", l, err)
	}
	if names, err := st.LinkNames(ctx, mid, false, []string{got.ID}); err != nil || names[got.ID] != "Donate" {
		t.Fatalf("an unlabelled link is named after its product: %v %v", names, err)
	}
	log, _ := st.AuditLog(ctx, mid)
	if n := len(log); n < 2 || log[n-2].Action != "payment_link.created" || log[n-1].Action != "payment_link.updated" {
		t.Fatalf("audit %+v", log)
	}
}

// Link counts are a link's own sessions; product counts every session of the product.
func TestLinkAndProductCounts(t *testing.T) {
	t.Parallel()
	st := newStore(t)
	ctx := t.Context()
	mid := merchant(t, st)
	p := openProduct(t, st, mid, "100", "")
	a := createLink(t, st, store.PaymentLink{ID: ids.New(ids.PaymentLink), MerchantID: mid, ProductID: p.ID, Active: true})
	b := createLink(t, st, store.PaymentLink{ID: ids.New(ids.PaymentLink), MerchantID: mid, ProductID: p.ID, Active: true})
	linkSession(t, st, mid, p.ID, a.ID, session.Open)
	linkSession(t, st, mid, p.ID, a.ID, session.Paid)
	linkSession(t, st, mid, p.ID, b.ID, session.Expired)
	linkSession(t, st, mid, p.ID, b.ID, session.PartiallyPaid)
	linkSession(t, st, mid, p.ID, "", session.Open)
	links, err := st.LinkCounts(ctx, []string{a.ID, b.ID, "plink_unknown"})
	if err != nil {
		t.Fatal(err)
	}
	if c := links[a.ID]; c.Opened != 2 || c.Paid != 1 || c.Held != 2 || c.Open != 1 {
		t.Fatalf("link a %+v", c)
	}
	if c := links[b.ID]; c.Opened != 2 || c.Held != 1 || c.Open != 1 {
		t.Fatalf("link b %+v", c)
	}
	if _, ok := links["plink_unknown"]; !ok {
		t.Fatal("every requested id gets an entry")
	}
	products, err := st.ProductCounts(ctx, []string{p.ID})
	if err != nil {
		t.Fatal(err)
	}
	if c := products[p.ID]; c.Opened != 5 || c.Paid != 1 || c.Open != 3 {
		t.Fatalf("product %+v", c)
	}
	// A session must name a link of its own product.
	other := openProduct(t, st, mid, "100", "")
	err = pgx.BeginFunc(ctx, st.Pool(), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE sessions SET link_id = $1 WHERE product_id = $2`, a.ID, other.ID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO sessions (id, merchant_id, livemode, product_id, link_id, amount_cents, status, version, salt, expires_at)
			VALUES ($1, $2, false, $3, $4, 100, 'open', 1, $5, now() + interval '1 hour')`, ids.New(ids.Session), mid, other.ID, a.ID, make([]byte, 32))
		return err
	})
	if err == nil {
		t.Fatal("a session named a link of another product")
	}
}

// EditProductAs shows fn the product's active links only and, when fn says so, deactivates them
// with the edit, each audited with its reason; any failure leaves the product and its links as
// they were.
func TestEditProductDeactivatesLinks(t *testing.T) {
	t.Parallel()
	st := newStore(t)
	ctx := t.Context()
	mid := merchant(t, st)
	p := openProduct(t, st, mid, "500", "10000")
	set := u("600")
	a := createLink(t, st, store.PaymentLink{ID: ids.New(ids.PaymentLink), MerchantID: mid, ProductID: p.ID, AmountCents: &set, Active: true})
	b := createLink(t, st, store.PaymentLink{ID: ids.New(ids.PaymentLink), MerchantID: mid, ProductID: p.ID, Active: true})
	off := createLink(t, st, store.PaymentLink{ID: ids.New(ids.PaymentLink), MerchantID: mid, ProductID: p.ID, Active: false})
	other := openProduct(t, st, mid, "500", "")
	elsewhere := createLink(t, st, store.PaymentLink{ID: ids.New(ids.PaymentLink), MerchantID: mid, ProductID: other.ID, Active: true})
	activeOf := func() map[string]bool {
		t.Helper()
		out := map[string]bool{}
		for _, id := range []string{a.ID, b.ID, off.ID, elsewhere.ID} {
			l, err := st.GetLink(ctx, mid, false, id)
			if err != nil {
				t.Fatal(err)
			}
			out[id] = l.Active
		}
		return out
	}
	reasons := func() int {
		t.Helper()
		var n int
		if err := st.Pool().QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE merchant_id = $1 AND action = 'payment_link.updated'
			AND details->>'reason' = 'product_edit'`, mid).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// A failure after the links were turned off (a payout to an unknown wallet) rolls back all.
	_, err := st.EditProductAs(ctx, mid, false, p.ID, "test", func(p *store.Product, active []store.PaymentLink) (bool, error) {
		if len(active) != 2 || active[0].ID != min(a.ID, b.ID) {
			t.Errorf("active links %+v", active)
		}
		lo := u("700")
		p.MinAmountCents = &lo
		p.Payout = map[string][]store.PayoutItem{"base-sepolia": {{WalletID: "wal_nope", Bps: 10_000, Family: chain.EVM}}}
		return true, nil
	})
	if err == nil {
		t.Fatal("a payout to an unknown wallet was stored")
	}
	if got := activeOf(); !got[a.ID] || !got[b.ID] || got[off.ID] || !got[elsewhere.ID] || reasons() != 0 {
		t.Fatalf("after a failed edit: %v, %d audits", got, reasons())
	}
	if got, _ := st.GetProduct(ctx, mid, false, p.ID); !got.MinAmountCents.Equal(u("500")) {
		t.Fatalf("a failed edit stored the range: %+v", got)
	}

	abort := errors.New("abort")
	if _, err := st.EditProductAs(ctx, mid, false, p.ID, "test", func(*store.Product, []store.PaymentLink) (bool, error) { return true, abort }); !errors.Is(err, abort) {
		t.Fatal(err)
	}
	// Keeping them changes no link: a stored amount may lie outside the new range.
	if _, err := st.EditProductAs(ctx, mid, false, p.ID, "test", func(p *store.Product, _ []store.PaymentLink) (bool, error) {
		lo := u("700")
		p.MinAmountCents = &lo
		return false, nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := activeOf(); !got[a.ID] || !got[b.ID] || reasons() != 0 {
		t.Fatalf("after keep: %v", got)
	}
	if l, _ := st.GetLink(ctx, mid, false, a.ID); !l.AmountCents.Equal(set) {
		t.Fatalf("a kept link lost its amount: %+v", l)
	}

	upd, err := st.EditProductAs(ctx, mid, false, p.ID, "test", func(p *store.Product, _ []store.PaymentLink) (bool, error) {
		hi := u("9000")
		p.MaxAmountCents = &hi
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := activeOf(); got[a.ID] || got[b.ID] || got[off.ID] || !got[elsewhere.ID] || reasons() != 2 {
		t.Fatalf("after deactivate: %v, %d audits", got, reasons())
	}
	if l, _ := st.GetLink(ctx, mid, false, a.ID); !l.UpdatedAt.Equal(upd.UpdatedAt) {
		t.Fatalf("link updated_at %v, product %v", l.UpdatedAt, upd.UpdatedAt)
	}
	// With no active link left, fn sees none and nothing more is audited.
	if _, err := st.EditProductAs(ctx, mid, false, p.ID, "test", func(_ *store.Product, active []store.PaymentLink) (bool, error) {
		if len(active) != 0 {
			t.Errorf("active links %+v", active)
		}
		return true, nil
	}); err != nil || reasons() != 2 {
		t.Fatalf("%v, %d audits", err, reasons())
	}
}

func TestProductDescriptionRoundtrip(t *testing.T) {
	t.Parallel()
	st := newStore(t)
	ctx := t.Context()
	mid := merchant(t, st)
	d := "Lifetime access.\nEvery feature."
	in := fixedProduct(mid, false, paidWallet(t, st, mid, false))
	in.Description = &d
	got, err := st.CreateProduct(ctx, in, "test")
	if err != nil || got.Description == nil || *got.Description != d {
		t.Fatalf("roundtrip %+v %v", got, err)
	}
	upd, err := st.UpdateProductAs(ctx, mid, false, got.ID, "test", func(p *store.Product) error { p.Description = nil; return nil })
	if err != nil || upd.Description != nil {
		t.Fatalf("clear %+v %v", upd, err)
	}
	log, _ := st.AuditLog(ctx, mid)
	if n := len(log); log[n-2].Action != "product.created" || log[n-1].Action != "product.updated" {
		t.Fatalf("audit %+v", log)
	}
}
