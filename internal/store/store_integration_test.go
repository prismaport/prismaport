//go:build integration

package store_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/fee"
	"github.com/prismaport/prismaport/internal/ids"
	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/session"
	"github.com/prismaport/prismaport/internal/store"
	"github.com/prismaport/prismaport/internal/testdb"
	"github.com/prismaport/prismaport/internal/view"
)

var now = time.Date(2026, 10, 23, 18, 0, 0, 123456000, time.UTC)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	return store.New(testdb.Pool(t), view.Renderer{CheckoutBaseURL: "https://pay.test"}).WithClock(func() time.Time { return now })
}

func merchant(t *testing.T, st *store.Store) string {
	t.Helper()
	m, err := st.CreateMerchant(t.Context(), "M", "test")
	if err != nil {
		t.Fatal(err)
	}
	return m.ID
}

func u(s string) money.Units { return money.MustParseInteger(s) }

func TestProductRoundtrip(t *testing.T) {
	t.Parallel()
	st := newStore(t)
	ctx := t.Context()
	mid := merchant(t, st)
	big := u("99999999999999999999999999")
	url := "https://example.com/thanks"
	in := store.Product{ID: ids.New(ids.Product), MerchantID: mid, Livemode: true, Name: "Donate", PriceType: store.PriceOpen,
		MinAmountCents: &big, SuggestedCents: []money.Units{big, big}, SuccessURL: &url,
		Metadata: map[string]string{"k": "v"}, Active: true, Payout: paidWallet(t, st, mid, true)}
	got, err := st.CreateProduct(ctx, in, "")
	if err != nil {
		t.Fatal(err)
	}
	if !got.MinAmountCents.Equal(big) || got.MaxAmountCents != nil || got.AmountCents != nil || len(got.SuggestedCents) != 2 ||
		!got.SuggestedCents[1].Equal(big) || *got.SuccessURL != url || got.Metadata["k"] != "v" || !got.CreatedAt.Equal(now) {
		t.Fatalf("roundtrip: %+v", got)
	}
	upd, err := st.UpdateProduct(ctx, mid, true, got.ID, func(p *store.Product) error {
		p.SuggestedCents = nil
		p.Metadata = nil
		return nil
	})
	if err != nil || upd.SuggestedCents != nil {
		t.Fatalf("update: %+v %v", upd, err)
	}
	again, _ := st.GetProduct(ctx, mid, true, got.ID)
	if again.SuggestedCents != nil || len(again.Metadata) != 0 {
		t.Fatalf("cleared fields came back: %+v", again)
	}
	abort := errors.New("abort")
	if _, err := st.UpdateProduct(ctx, mid, true, got.ID, func(p *store.Product) error { p.Name = "X"; return abort }); !errors.Is(err, abort) {
		t.Fatal(err)
	}
	if p, _ := st.GetProduct(ctx, mid, true, got.ID); p.Name != "Donate" {
		t.Fatal("aborted update persisted")
	}
	if _, err := st.GetProduct(ctx, mid, false, got.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("product visible in the other mode")
	}
}

func TestSessionRoundtripExactAmounts(t *testing.T) {
	t.Parallel()
	st := newStore(t)
	ctx := t.Context()
	mid := merchant(t, st)
	cents := u("123456789012345678901234567")
	amount18, _ := money.USDToToken(cents, 18)
	payTo := []store.PayToRecord{{Chain: "bsc", Asset: "USDT", Family: chain.EVM, Token: "0xT", Decimals: 18, Address: "0xA", Amount: amount18,
		Treasury: "0xR", Fee: fee.Params{FeeBps: 300, FixedFeeMicros: 5_000_000}, BoundTokens: []string{"0xS", "0xT"}}}
	payouts := []store.SessionPayout{{Chain: "bsc", Family: chain.EVM, Position: 0, Address: "0xD", Bps: 6000},
		{Chain: "bsc", Family: chain.EVM, Position: 1, Address: "0xE", Bps: 4000}}
	var salt [32]byte
	salt[0], salt[31] = 1, 2
	pid := (*string)(nil)
	ses := &store.Session{ID: ids.New(ids.Session), MerchantID: mid, PayWindow: 30 * time.Minute, Livemode: false, ProductID: pid, Metadata: map[string]string{"a": "b"},
		Salt: salt, CreatedAt: now, UpdatedAt: now, PayTo: payTo, Payouts: payouts}
	ses.State = session.New(cents, store.PayToState(payTo), now.Add(time.Hour))
	err := pgx.BeginFunc(ctx, st.Pool(), func(tx pgx.Tx) error { return st.InsertSession(ctx, tx, ses) })
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.GetSession(ctx, mid, false, ses.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.State.AmountCents.Equal(cents) || !got.PayTo[0].Amount.Equal(amount18) || got.PayTo[0].Fee.FeeBps != 300 ||
		got.Salt != salt || got.State.Status != session.Open || got.State.Version != 1 || !got.State.ExpiresAt.Equal(now.Add(time.Hour)) ||
		got.PayWindow != 30*time.Minute || got.PaymentStartedAt != nil || len(got.Reveals) != 0 {
		t.Fatalf("roundtrip: %+v", got)
	}
	if rs := got.PayTo[0].Recipients; got.PayTo[0].Family != chain.EVM || len(rs) != 2 || rs[0].Address != "0xD" || rs[0].Bps != 6000 ||
		rs[1].Address != "0xE" || len(got.Payouts) != 2 {
		t.Fatalf("payout snapshot roundtrip: %+v %+v", got.PayTo[0], got.Payouts)
	}
	if bt := got.PayTo[0].BoundTokens; len(bt) != 2 || bt[0] != "0xS" || bt[1] != "0xT" {
		t.Fatalf("bound tokens roundtrip: %v", bt)
	}
	unsplit := &store.Session{ID: ids.New(ids.Session), MerchantID: mid, PayWindow: 30 * time.Minute, Salt: [32]byte{9}, CreatedAt: now, UpdatedAt: now, PayTo: payTo}
	unsplit.State = session.New(cents, store.PayToState(payTo), now.Add(time.Hour))
	if err := pgx.BeginFunc(ctx, st.Pool(), func(tx pgx.Tx) error { return st.InsertSession(ctx, tx, unsplit) }); err == nil {
		t.Fatal("a session without a payout snapshot was stored")
	}
	payment := u("999999999999999999999999999999")
	_, evs, err := st.ApplySessionCommand(ctx, ses.ID, func(s session.State) (session.State, []session.Event, error) {
		return s.DetectPayment(session.NewPayment{ID: "pay_1", Chain: "bsc", Asset: "USDT", Address: "0xA", TxHash: "0xh", LogIndex: 7, Amount: payment}, now)
	})
	if err != nil || len(evs) != 1 {
		t.Fatalf("detect: %v %v", evs, err)
	}
	reloaded, _ := st.GetSession(ctx, mid, false, ses.ID)
	p := reloaded.State.Payments[0]
	if !p.Amount.Equal(payment) || p.LogIndex != 7 || p.Decimals != 18 || p.Status != session.Detected || reloaded.State.Version != 2 {
		t.Fatalf("payment roundtrip: %+v", p)
	}
	if !strings.Contains(string(evs[0].Payload), `"amount":"999999999999.999999999999999999"`) {
		t.Fatalf("payload amount not exact: %s", evs[0].Payload)
	}
	if _, err := st.GetSession(ctx, "mer_other", false, ses.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("session visible to another merchant")
	}
	// A failing command leaves no trace.
	_, _, err = st.ApplySessionCommand(ctx, ses.ID, func(s session.State) (session.State, []session.Event, error) {
		return s.ConfirmPayment("pay_missing", now)
	})
	if !errors.Is(err, session.ErrUnknownPayment) {
		t.Fatal(err)
	}
	if again, _ := st.GetSession(ctx, mid, false, ses.ID); again.State.Version != 2 {
		t.Fatal("failed command changed the version")
	}
	if ids, _ := st.SessionsByAddress(ctx, "bsc", "0xA"); len(ids) != 1 || ids[0] != ses.ID {
		t.Fatalf("address lookup %v", ids)
	}
}

// A stored amount the session's value methods cannot compute with (a payment or the session
// amount past the canonical scale) is refused as an error when the session is read, never a
// panic, and a session with such an amount is never written.
func TestStoredAmountOutOfRange(t *testing.T) {
	t.Parallel()
	st := newStore(t)
	ctx := t.Context()
	mid := merchant(t, st)
	cents := u("2500")
	amount6, _ := money.USDToToken(cents, 6)
	payTo := []store.PayToRecord{{Chain: "base", Asset: "USDC", Family: chain.EVM, Token: "0xT", Decimals: 6, Address: "0xA", Amount: amount6,
		Treasury: "0xR", Fee: fee.Params{FeeBps: 100}, BoundTokens: []string{"0xT"}}}
	payouts := []store.SessionPayout{{Chain: "base", Family: chain.EVM, Position: 0, Address: "0xD", Bps: 10000}}
	insert := func(cents money.Units) (*store.Session, error) {
		ses := &store.Session{ID: ids.New(ids.Session), MerchantID: mid, PayWindow: 30 * time.Minute, Salt: [32]byte{7}, CreatedAt: now,
			UpdatedAt: now, PayTo: payTo, Payouts: payouts}
		ses.State = session.New(cents, store.PayToState(payTo), now.Add(time.Hour))
		return ses, pgx.BeginFunc(ctx, st.Pool(), func(tx pgx.Tx) error { return st.InsertSession(ctx, tx, ses) })
	}
	if _, err := insert(money.MaxUnits()); !errors.Is(err, session.ErrAmountOutOfRange) {
		t.Fatalf("an amount past the canonical scale was stored: %v", err)
	}
	ses, err := insert(cents)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.ApplySessionCommand(ctx, ses.ID, func(s session.State) (session.State, []session.Event, error) {
		return s.DetectPayment(session.NewPayment{ID: "pay_1", Chain: "base", Asset: "USDC", Address: "0xA", TxHash: "0xh", Amount: amount6}, now)
	}); err != nil {
		t.Fatal(err)
	}
	read := func(what string) {
		t.Helper()
		if _, err := st.GetSession(ctx, mid, false, ses.ID); !errors.Is(err, session.ErrAmountOutOfRange) {
			t.Fatalf("%s: %v", what, err)
		}
		if _, _, err := st.ApplySessionCommand(ctx, ses.ID, func(s session.State) (session.State, []session.Event, error) {
			return s.ConfirmPayment("pay_1", now)
		}); !errors.Is(err, session.ErrAmountOutOfRange) {
			t.Fatalf("%s, confirmed: %v", what, err)
		}
	}
	max := money.MaxUnits().String()
	if _, err := st.Pool().Exec(ctx, `UPDATE payments SET amount = $1::numeric WHERE session_id = $2`, max, ses.ID); err != nil {
		t.Fatal(err)
	}
	read("a payment past the canonical scale")
	if _, err := st.Pool().Exec(ctx, `UPDATE payments SET amount = $1::numeric WHERE session_id = $2`, amount6.String(), ses.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool().Exec(ctx, `UPDATE sessions SET amount_cents = $1::numeric WHERE id = $2`, max, ses.ID); err != nil {
		t.Fatal(err)
	}
	read("a session amount past the canonical scale")
}

func TestAcceptedAssetsKeysAndAudit(t *testing.T) {
	t.Parallel()
	st := newStore(t)
	ctx := t.Context()
	mid := merchant(t, st)
	if err := st.SetAcceptedAssets(ctx, mid, true, []store.ChainAsset{{"base", "USDC"}, {"solana", "USDT"}}, "op"); err != nil {
		t.Fatal(err)
	}
	if a, _ := st.AcceptedAssets(ctx, mid, true); len(a) != 2 || a[1].Asset != "USDT" {
		t.Fatalf("assets %v", a)
	}
	var hash [32]byte
	hash[0] = 1
	k, err := st.InsertAPIKey(ctx, mid, true, hash, "sk_live_abcd", []string{"sessions:read"}, "op")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := st.AuthenticateAPIKey(ctx, hash); err != nil || got.ID != k.ID || !got.Livemode {
		t.Fatalf("auth %v %v", got, err)
	}
	if err := st.RevokeAPIKey(ctx, k.ID, "op"); err != nil {
		t.Fatal(err)
	}
	if err := st.RevokeAPIKey(ctx, k.ID, "op"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("double revoke")
	}
	if _, err := st.AuthenticateAPIKey(ctx, hash); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("revoked key authenticates")
	}
	log, _ := st.AuditLog(ctx, mid)
	var actions []string
	for _, e := range log {
		actions = append(actions, e.Action)
	}
	want := "merchant.created,accepted_assets.set,api_key.created,api_key.revoked"
	if strings.Join(actions, ",") != want {
		t.Fatalf("audit %v", actions)
	}
}
