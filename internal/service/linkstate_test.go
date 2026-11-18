package service

import (
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/store"
	"pgregory.net/rapid"
)

func ptr[T any](v T) *T { return &v }

func TestLiveness(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name          string
		l             store.PaymentLink
		productActive bool
		c             store.LinkCounts
		live          bool
		reason        string
	}{
		{"active, no limits", store.PaymentLink{Active: true}, true, store.LinkCounts{Opened: 9, Paid: 9, Held: 9}, true, ""},
		{"inactive", store.PaymentLink{Active: false}, true, store.LinkCounts{}, false, ReasonInactive},
		{"inactive product", store.PaymentLink{Active: true}, false, store.LinkCounts{}, false, ReasonProductInactive},
		{"inactive product wins over everything", store.PaymentLink{Active: false, ExpiresAt: ptr(now.Add(-time.Hour)), MaxPayments: ptr(1)}, false,
			store.LinkCounts{Paid: 1, Held: 1}, false, ReasonProductInactive},
		{"inactive wins over expired", store.PaymentLink{Active: false, ExpiresAt: ptr(now.Add(-time.Hour))}, true, store.LinkCounts{}, false, ReasonInactive},
		{"expires later", store.PaymentLink{Active: true, ExpiresAt: ptr(now.Add(time.Second))}, true, store.LinkCounts{}, true, ""},
		{"expires exactly now", store.PaymentLink{Active: true, ExpiresAt: ptr(now)}, true, store.LinkCounts{}, false, ReasonExpired},
		{"expired wins over exhausted", store.PaymentLink{Active: true, ExpiresAt: ptr(now.Add(-time.Minute)), MaxPayments: ptr(1)}, true,
			store.LinkCounts{Paid: 1, Held: 1}, false, ReasonExpired},
		{"free slot", store.PaymentLink{Active: true, MaxPayments: ptr(3)}, true, store.LinkCounts{Opened: 5, Paid: 1, Held: 2}, true, ""},
		{"paid reaches the limit", store.PaymentLink{Active: true, MaxPayments: ptr(2)}, true, store.LinkCounts{Paid: 2, Held: 2}, false, ReasonExhausted},
		{"paid above the limit (late payments)", store.PaymentLink{Active: true, MaxPayments: ptr(2)}, true, store.LinkCounts{Paid: 3, Held: 3}, false, ReasonExhausted},
		{"open sessions hold the last slots", store.PaymentLink{Active: true, MaxPayments: ptr(2)}, true, store.LinkCounts{Paid: 1, Held: 2}, false, ReasonReserved},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			live, reason := Liveness(c.l, c.productActive, c.c, now)
			if live != c.live || reason != c.reason {
				t.Errorf("%s: got (%v, %q), want (%v, %q)", c.name, live, reason, c.live, c.reason)
			}
		})
	}
}

// A live link always has a free slot, so opening one more session never exceeds max_payments.
func TestLivenessNeverOversubscribes(t *testing.T) {
	now := time.Now()
	rapid.Check(t, func(t *rapid.T) {
		limit := rapid.IntRange(1, 50).Draw(t, "limit")
		paid := rapid.Int64Range(0, 60).Draw(t, "paid")
		held := paid + rapid.Int64Range(0, 60).Draw(t, "open")
		live, reason := Liveness(store.PaymentLink{Active: true, MaxPayments: &limit}, true, store.LinkCounts{Paid: paid, Held: held}, now)
		if live && held+1 > int64(limit) {
			t.Fatalf("live with %d held of %d", held, limit)
		}
		if !live && reason == ReasonExhausted && paid < int64(limit) {
			t.Fatalf("exhausted with %d paid of %d", paid, limit)
		}
	})
}

func TestConversion(t *testing.T) {
	if Conversion(0, 0) != nil {
		t.Fatal("conversion of nothing opened must be null")
	}
	for _, c := range []struct {
		paid, opened int64
		want         string
	}{{0, 4, "0.0000"}, {1, 2, "0.5000"}, {1, 3, "0.3333"}, {2, 3, "0.6666"}, {3, 3, "1.0000"}, {1, 10000, "0.0001"}, {1, 10001, "0.0000"}} {
		t.Run(c.want, func(t *testing.T) {
			if got := *Conversion(c.paid, c.opened); got != c.want {
				t.Errorf("%d/%d: %s, want %s", c.paid, c.opened, got, c.want)
			}
		})
	}
}

func TestReceivedByAsset(t *testing.T) {
	got, err := ReceivedByAsset([]store.AssetAmount{
		{Asset: "USDT", Decimals: 6, Amount: money.FromUint64(1)},
		{Asset: "USDC", Decimals: 6, Amount: money.FromUint64(25_000_000)},
		{Asset: "USDC", Decimals: 6, Amount: money.FromUint64(10_000_001)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != (AssetTotal{"USDC", "35.000001"}) || got[1] != (AssetTotal{"USDT", "0.000001"}) {
		t.Fatalf("%v", got)
	}
	mixed, err := ReceivedByAsset([]store.AssetAmount{
		{Asset: "USDC", Decimals: 6, Amount: money.FromUint64(1_500_000)},
		{Asset: "USDC", Decimals: 18, Amount: money.MustParseInteger("2000000000000000001")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if mixed[0].Amount != "3.500000000000000001" {
		t.Fatalf("mixed decimals: %v", mixed)
	}
	empty, err := ReceivedByAsset(nil)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("empty: %v %v", empty, err)
	}
}
