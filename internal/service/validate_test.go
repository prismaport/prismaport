package service

import (
	"errors"
	"strings"
	"testing"

	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/fee"
	"github.com/prismaport/prismaport/internal/money"
)

func TestMinPriceIsTheLargestSmallestSettlingAmount(t *testing.T) {
	pilot := chain.Chain{ID: "base", Fee: chain.PilotFee()}
	cases := []struct {
		chains []chain.Chain
		want   string
	}{
		{nil, "0.01"},
		{[]chain.Chain{pilot}, "0.11"},
		{[]chain.Chain{{ID: "free"}}, "0.01"},
		{[]chain.Chain{pilot, {ID: "max", Fee: fee.Params{FeeBps: fee.MaxFeeBps, FixedFeeMicros: fee.MaxFixedFeeMicros}}}, "5.16"},
	}
	for _, c := range cases {
		t.Run(c.want, func(t *testing.T) {
			got, err := minPrice(c.chains)
			if err != nil || money.FormatUSD(got) != c.want {
				t.Errorf("minPrice(%v)=%s,%v want %s", c.chains, money.FormatUSD(got), err, c.want)
			}
		})
	}
}

func TestCheckFloor(t *testing.T) {
	chains := []chain.Chain{{ID: "base", Fee: chain.PilotFee()}}
	if err := checkFloor("amount", money.FromUint64(11), chains); err != nil {
		t.Fatalf("0.11 refused: %v", err)
	}
	err := checkFloor("amount", money.FromUint64(10), chains)
	var e *Error
	if !errors.As(err, &e) || e.Code != CodeAmountBelowMinimum || !strings.Contains(e.Message, "at least 0.11 USD") {
		t.Fatalf("0.10 accepted or wrong error: %v", err)
	}
}

func TestParseAmountCapsHugeAmounts(t *testing.T) {
	if u, err := parseAmount("amount", "10000000000000.00"); err != nil || money.FormatUSD(u) != "10000000000000.00" {
		t.Fatalf("the cap itself: %s, %v", money.FormatUSD(u), err)
	}
	for _, s := range []string{
		"10000000000000.01",
		"999999999999999999999999999999",
		strings.Repeat("9", 100),
	} {
		_, err := parseAmount("amount", s)
		var e *Error
		if !errors.As(err, &e) || e.Kind != KindInvalid || !strings.Contains(e.Message, "at most 10000000000000.00") {
			t.Errorf("parseAmount(%q) = %v, want a 400 naming the cap", s, err)
		}
	}
}
