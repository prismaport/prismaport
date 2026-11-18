package fee

import (
	"errors"
	"math/big"
	"testing"

	"github.com/prismaport/prismaport/internal/money"
	"pgregory.net/rapid"
)

var pilot = Params{FeeBps: 50, FixedFeeMicros: 100_000}

func usdc(s string) money.Units {
	u, err := money.ParseDecimal(s, 6)
	if err != nil {
		panic(err)
	}
	return u
}

func TestComputeExamples(t *testing.T) {
	cases := []struct {
		gross, fee, net string
		dust            bool
	}{
		{"25.000000", "0.225000", "24.775000", false},
		{"5.000000", "0.125000", "4.875000", false},
		{"100.000000", "0.600000", "99.400000", false},
		{"10.010000", "0.150050", "9.859950", false},
		{"10.000199", "0.150000", "9.850199", false},
		{"12.345678", "0.161728", "12.183950", false},
		{"0.110000", "0.100550", "0.009450", false},
		{"0.100503", "0.100502", "0.000001", false},
		{"0.100502", "0", "0", true},
		{"0.100501", "0", "0", true},
		{"0.100000", "0", "0", true},
		{"0.050000", "0", "0", true},
		{"0", "0", "0", true},
	}
	for _, c := range cases {
		t.Run(c.gross, func(t *testing.T) {
			s, err := Compute(usdc(c.gross), 6, pilot)
			if err != nil {
				t.Fatal(err)
			}
			if s.Dust != c.dust || !s.Fee.Equal(usdc(c.fee)) || !s.Net.Equal(usdc(c.net)) {
				t.Errorf("gross %s: got fee=%s net=%s dust=%v", c.gross,
					money.FormatDecimal(s.Fee, 6), money.FormatDecimal(s.Net, 6), s.Dust)
			}
		})
	}
}

func TestEighteenDecimals(t *testing.T) {
	cases := []struct {
		gross, fee string
		dust       bool
	}{
		{"25", "0.225000000000000000", false},
		{"1", "0.105000000000000000", false},
		{"0.100502512562814071", "0.100502512562814070", false},
		{"0.100502512562814070", "0.000000000000000000", true},
	}
	for _, c := range cases {
		t.Run(c.gross, func(t *testing.T) {
			gross, err := money.ParseDecimal(c.gross, 18)
			if err != nil {
				t.Fatal(err)
			}
			s, err := Compute(gross, 18, pilot)
			if err != nil {
				t.Fatal(err)
			}
			if s.Dust != c.dust || money.FormatDecimal(s.Fee, 18) != c.fee {
				t.Errorf("gross %s: fee=%s dust=%v", c.gross, money.FormatDecimal(s.Fee, 18), s.Dust)
			}
		})
	}
}

func TestZeroParts(t *testing.T) {
	s, _ := Compute(usdc("0.100001"), 6, Params{FixedFeeMicros: 100_000})
	if s.Dust || !s.Fee.Equal(usdc("0.1")) || !s.Net.Equal(usdc("0.000001")) {
		t.Errorf("fixed only: fee=%s dust=%v", s.Fee, s.Dust)
	}
	if s, _ := Compute(usdc("0.1"), 6, Params{FixedFeeMicros: 100_000}); !s.Dust {
		t.Error("gross equal to the fixed part settled")
	}
	s, _ = Compute(usdc("25"), 6, Params{FeeBps: 50})
	if s.Dust || !s.Fee.Equal(usdc("0.125")) {
		t.Errorf("percentage only: fee=%s", s.Fee)
	}
	s, _ = Compute(money.FromUint64(1), 6, Params{FeeBps: 50})
	if s.Dust || !s.Fee.IsZero() {
		t.Errorf("one unit, percentage only: fee=%s dust=%v", s.Fee, s.Dust)
	}
}

func TestValidation(t *testing.T) {
	if _, err := Compute(usdc("1"), 6, Params{FeeBps: 301}); !errors.Is(err, ErrFeeBpsCap) {
		t.Fatal("feeBps cap not enforced")
	}
	if _, err := Compute(usdc("1"), 6, Params{FixedFeeMicros: 5_000_001}); !errors.Is(err, ErrFixedFeeCap) {
		t.Fatal("fixed fee cap not enforced")
	}
	if _, err := Compute(usdc("1"), 5, pilot); !errors.Is(err, ErrTokenDecimal) {
		t.Fatal("decimals < 6 accepted")
	}
	if err := (Params{FeeBps: MaxFeeBps, FixedFeeMicros: MaxFixedFeeMicros}).Validate(); err != nil {
		t.Fatal("caps themselves rejected")
	}
}

func TestSettlesUSD(t *testing.T) {
	cases := map[string]bool{"0.10": false, "0.11": true, "0.09": false, "25.00": true}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			c, _ := money.ParseUSD(in)
			got, err := SettlesUSD(c, pilot)
			if err != nil || got != want {
				t.Errorf("SettlesUSD(%s)=%v,%v want %v", in, got, err, want)
			}
		})
	}
}

func TestMinSettlingCents(t *testing.T) {
	cases := []struct {
		p    Params
		want uint64
	}{
		{pilot, 11},
		{Params{FeeBps: 100, FixedFeeMicros: 100_000}, 11},
		{Params{FeeBps: 0, FixedFeeMicros: 100_000}, 11},
		{Params{FeeBps: 50, FixedFeeMicros: 99_499}, 10},
		{Params{FeeBps: 50, FixedFeeMicros: 99_500}, 11},
		{Params{FeeBps: 300, FixedFeeMicros: 5_000_000}, 516},
		{Params{}, 1},
	}
	for _, c := range cases {
		got, err := MinSettlingCents(c.p)
		if err != nil || !got.Equal(money.FromUint64(c.want)) {
			t.Errorf("MinSettlingCents(%+v)=%s,%v want %d", c.p, got, err, c.want)
		}
	}
	if _, err := MinSettlingCents(Params{FeeBps: 301}); !errors.Is(err, ErrFeeBpsCap) {
		t.Fatal("feeBps cap not enforced")
	}
}

func genParams(t *rapid.T) Params {
	return Params{
		FeeBps:         rapid.Uint16Range(0, MaxFeeBps).Draw(t, "bps"),
		FixedFeeMicros: rapid.Uint64Range(0, MaxFixedFeeMicros).Draw(t, "fixedFee"),
	}
}

func TestPropertyInvariants(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		p := genParams(t)
		dec := rapid.Uint8Range(6, 30).Draw(t, "dec")
		raw := rapid.SliceOfN(rapid.Byte(), 0, 31).Draw(t, "gross")
		gross, _ := money.FromBig(new(big.Int).SetBytes(raw))
		s, err := Compute(gross, dec, p)
		if err != nil {
			t.Fatal(err)
		}
		fixedPart := new(big.Int).Mul(new(big.Int).SetUint64(p.FixedFeeMicros), new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(dec-6)), nil))
		pct := new(big.Int).Mul(gross.Big(), big.NewInt(int64(p.FeeBps)))
		pct.Quo(pct, big.NewInt(10_000))
		expected := new(big.Int).Add(fixedPart, pct)
		if s.Dust {
			if expected.Cmp(gross.Big()) < 0 {
				t.Fatalf("dust although fee %s < gross %s", expected, gross)
			}
			if !s.Fee.IsZero() || !s.Net.IsZero() {
				t.Fatal("dust split moved money")
			}
			return
		}
		sum, _ := s.Fee.Add(s.Net)
		if !sum.Equal(gross) {
			t.Fatalf("fee+net != gross: %s+%s != %s", s.Fee, s.Net, gross)
		}
		if s.Fee.Big().Cmp(expected) != 0 {
			t.Fatalf("fee %s, expected %s", s.Fee, expected)
		}
		capU, _ := Cap(gross, dec)
		if s.Fee.Cmp(capU) > 0 {
			t.Fatalf("fee %s above cap %s", s.Fee, capU)
		}
		if s.Net.IsZero() {
			t.Fatal("settlement with zero net")
		}
	})
}

func TestPropertyFloorMatchesDust(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		p := genParams(t)
		cents := money.FromUint64(rapid.Uint64Range(0, 1_000_000).Draw(t, "cents"))
		dec := rapid.Uint8Range(6, 18).Draw(t, "dec")
		gross, _ := money.USDToToken(cents, dec)
		s, _ := Compute(gross, dec, p)
		ok, _ := SettlesUSD(cents, p)
		if ok == s.Dust {
			t.Fatalf("amount %s: settles=%v but dust=%v", cents, ok, s.Dust)
		}
		least, _ := MinSettlingCents(p)
		if ok != (cents.Cmp(least) >= 0) {
			t.Fatalf("amount %s: settles=%v but the smallest settling amount is %s", cents, ok, least)
		}
	})
}
