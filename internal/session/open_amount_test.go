package session

import (
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/fee"
	"github.com/prismaport/prismaport/internal/money"
)

// The fee of these tests: 1% + US$0.50, so 0.50 USDC alone is dust and 0.51 settles.
var testFee = fee.Params{FeeBps: 100, FixedFeeMicros: 500_000}

func recharge() State {
	return NewOpenAmount([]PayTo{
		{Chain: "base", Asset: "USDC", Address: "0xabc", Decimals: 6, Fee: testFee},
		{Chain: "base", Asset: "USDT", Address: "0xabc", Decimals: 6, Fee: testFee},
	}, t0.Add(time.Hour))
}

func TestOpenAmountStartsOpenWithoutAnAmount(t *testing.T) {
	s := recharge()
	if !s.OpenAmount || s.Status != Open || !s.AmountCents.IsZero() || !s.Remaining().IsZero() {
		t.Fatalf("new open-amount session %+v", s)
	}
}

func TestOpenAmountPaidOnceASumSettles(t *testing.T) {
	s := recharge()
	s = must(t, s, detect("p1", "base", "USDC", "0xabc", "0.50", 6, t0), Open, EventPaymentDetected)
	// Dust alone never pays (D35), and there is no partially_paid.
	s = must(t, s, confirm("p1", t0), Open)
	s = must(t, s, detect("p2", "base", "USDC", "0xabc", "0.01", 6, t0), Open, EventPaymentDetected)
	s = must(t, s, confirm("p2", t0), Paid, EventPaid)
	if s.PaidAt == nil || s.Received().Cmp(mustCanonical("0.51")) != 0 {
		t.Fatalf("paid_at %v received %s", s.PaidAt, s.Received())
	}
	// Further payments are credited without a second session.paid, and never make it overpaid.
	s = must(t, s, detect("p3", "base", "USDC", "0xabc", "100", 6, t0.Add(time.Minute)), Paid, EventPaymentDetected)
	s = must(t, s, confirm("p3", t0.Add(time.Minute)), Paid)
	if s.Received().Cmp(mustCanonical("100.51")) != 0 {
		t.Fatalf("received %s", s.Received())
	}
	// A paid session does not expire.
	if _, _, err := s.Expire(t0.Add(2 * time.Hour)); err == nil {
		t.Fatal("a paid open-amount session expired")
	}
}

func TestOpenAmountSumsAreNotAddedAcrossEntries(t *testing.T) {
	s := recharge()
	s = must(t, s, detect("p1", "base", "USDC", "0xabc", "0.50", 6, t0), Open, EventPaymentDetected)
	s = must(t, s, confirm("p1", t0), Open)
	s = must(t, s, detect("p2", "base", "USDT", "0xabc", "0.50", 6, t0), Open, EventPaymentDetected)
	// 0.50 USDC + 0.50 USDT: each is flushed alone and each is dust.
	s = must(t, s, confirm("p2", t0), Open)
	if s.Received().Cmp(mustCanonical("1.00")) != 0 {
		t.Fatalf("received %s", s.Received())
	}
}

func TestOpenAmountReversalAndExpiry(t *testing.T) {
	s := recharge()
	s = must(t, s, detect("p1", "base", "USDC", "0xabc", "5", 6, t0), Open, EventPaymentDetected)
	s = must(t, s, confirm("p1", t0), Paid, EventPaid)
	s = must(t, s, reverse("p1", t0), Open, EventPaymentReversed)
	if s.PaidAt != nil {
		t.Fatal("paid_at kept after the reversal")
	}
	s = must(t, s, expire(t0.Add(time.Hour)), Expired, EventExpired)
	s = must(t, s, detect("p2", "base", "USDC", "0xabc", "5", 6, t0.Add(2*time.Hour)), Expired, EventPaymentDetected)
	s = must(t, s, confirm("p2", t0.Add(2*time.Hour)), LatePayment, EventLatePayment)
	if !s.Payments[1].Late {
		t.Fatal("payment after expiry not late")
	}
}

func TestOpenAmountExpiresWithDustOnly(t *testing.T) {
	s := recharge()
	s = must(t, s, detect("p1", "base", "USDC", "0xabc", "0.10", 6, t0), Open, EventPaymentDetected)
	s = must(t, s, confirm("p1", t0), Open)
	must(t, s, expire(t0.Add(time.Hour)), Expired, EventExpired)
}

func mustCanonical(usd string) money.Units {
	u, err := money.ParseDecimal(usd, 6)
	if err != nil {
		panic(err)
	}
	c, err := money.Rescale(u, 6, chain.CanonicalDecimals)
	if err != nil {
		panic(err)
	}
	return c
}
