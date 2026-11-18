package money

import (
	"errors"
	"math/big"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

func TestParseDecimal(t *testing.T) {
	cases := []struct {
		in   string
		dec  uint8
		want string
		err  error
	}{
		{"25", 6, "25000000", nil},
		{"25.00", 2, "2500", nil},
		{"25.5", 6, "25500000", nil},
		{"0.000001", 6, "1", nil},
		{"0", 6, "0", nil},
		{"0.0", 2, "0", nil},
		{"25.0000001", 6, "", ErrPrecision},
		{"25.001", 2, "", ErrPrecision},
		{"-1", 6, "", ErrSyntax},
		{"+1", 6, "", ErrSyntax},
		{"1e6", 6, "", ErrSyntax},
		{" 1", 6, "", ErrSyntax},
		{"1 ", 6, "", ErrSyntax},
		{"", 6, "", ErrSyntax},
		{".5", 6, "", ErrSyntax},
		{"5.", 6, "", ErrSyntax},
		{"05", 6, "", ErrSyntax},
		{"1,5", 6, "", ErrSyntax},
		{"1.2.3", 6, "", ErrSyntax},
		{"1", 37, "", ErrDecimals},
		{"1.5", 0, "", ErrPrecision},
		{"7", 0, "7", nil},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := ParseDecimal(c.in, c.dec)
			if c.err != nil {
				if !errors.Is(err, c.err) {
					t.Errorf("ParseDecimal(%q,%d) err=%v, want %v", c.in, c.dec, err, c.err)
				}
				return
			}
			if err != nil || got.String() != c.want {
				t.Errorf("ParseDecimal(%q,%d)=%s,%v want %s", c.in, c.dec, got, err, c.want)
			}
		})
	}
}

func TestRangeLimits(t *testing.T) {
	max := MaxUnits()
	if max.String() != "115792089237316195423570985008687907853269984665640564039457584007913129639935" {
		t.Fatalf("max=%s", max)
	}
	if _, err := max.Add(FromUint64(1)); !errors.Is(err, ErrRange) {
		t.Fatalf("overflow not detected: %v", err)
	}
	if _, err := ParseInteger(new(big.Int).Add(max.Big(), big.NewInt(1)).String()); !errors.Is(err, ErrRange) {
		t.Fatalf("2^256 accepted: %v", err)
	}
	if _, err := FromUint64(1).Sub(FromUint64(2)); !errors.Is(err, ErrNegative) {
		t.Fatalf("negative not detected: %v", err)
	}
	if _, err := FromBig(big.NewInt(-1)); !errors.Is(err, ErrNegative) {
		t.Fatal("negative big accepted")
	}
	for _, bad := range []string{"", "-1", "01", "1.0", "abc"} {
		if _, err := ParseInteger(bad); err == nil {
			t.Errorf("ParseInteger(%q) accepted", bad)
		}
	}
}

func TestFormat(t *testing.T) {
	cases := []struct {
		units string
		dec   uint8
		want  string
	}{
		{"2500", 2, "25.00"},
		{"0", 2, "0.00"},
		{"1", 6, "0.000001"},
		{"25000000", 6, "25.000000"},
		{"5", 0, "5"},
	}
	for _, c := range cases {
		t.Run(c.units, func(t *testing.T) {
			if got := FormatDecimal(MustParseInteger(c.units), c.dec); got != c.want {
				t.Errorf("FormatDecimal(%s,%d)=%s want %s", c.units, c.dec, got, c.want)
			}
		})
	}
	min := []struct {
		units    string
		dec, min uint8
		want     string
	}{
		{"10000000", 6, 2, "10.00"},
		{"10000001", 6, 2, "10.000001"},
		{"10100000", 6, 2, "10.10"},
		{"10120000", 6, 2, "10.12"},
		{"10123000", 6, 2, "10.123"},
		{"0", 18, 2, "0.00"},
		{"10000000", 6, 0, "10"},
		{"2500", 2, 2, "25.00"},
	}
	for _, c := range min {
		t.Run(c.units, func(t *testing.T) {
			if got := FormatMinPlaces(MustParseInteger(c.units), c.dec, c.min); got != c.want {
				t.Errorf("FormatMinPlaces(%s,%d,%d)=%s want %s", c.units, c.dec, c.min, got, c.want)
			}
		})
	}
}

func TestRescale(t *testing.T) {
	u, err := USDToToken(MustParseInteger("2500"), 6)
	if err != nil || u.String() != "25000000" {
		t.Fatalf("USDToToken=%s,%v", u, err)
	}
	u, err = USDToToken(MustParseInteger("2500"), 18)
	if err != nil || u.String() != "25"+strings.Repeat("0", 18) {
		t.Fatalf("USDToToken 18=%s,%v", u, err)
	}
	if _, err := USDToToken(FromUint64(1), 1); !errors.Is(err, ErrDecimals) {
		t.Fatal("decimals below cents accepted")
	}
	if _, err := Rescale(FromUint64(1001), 3, 2); !errors.Is(err, ErrInexact) {
		t.Fatal("inexact downscale accepted")
	}
	if got, _ := Rescale(FromUint64(1000), 3, 2); got.String() != "100" {
		t.Fatalf("downscale=%s", got)
	}
}

func genUnits(t *rapid.T, label string) Units {
	b := rapid.SliceOfN(rapid.Byte(), 0, 32).Draw(t, label)
	u, err := FromBig(new(big.Int).SetBytes(b))
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestPropertyFormatParseRoundtrip(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		u := genUnits(t, "u")
		dec := rapid.Uint8Range(0, MaxDecimals).Draw(t, "dec")
		s := FormatDecimal(u, dec)
		back, err := ParseDecimal(s, dec)
		if err != nil {
			t.Fatalf("parse(%q): %v", s, err)
		}
		if !back.Equal(u) {
			t.Fatalf("roundtrip %s -> %q -> %s", u, s, back)
		}
		minPlaces := rapid.Uint8Range(0, dec).Draw(t, "min")
		trimmed := FormatMinPlaces(u, dec, minPlaces)
		back, err = ParseDecimal(trimmed, dec)
		if err != nil || !back.Equal(u) {
			t.Fatalf("min-places roundtrip %s -> %q -> %s (%v)", u, trimmed, back, err)
		}
		ib, err := ParseInteger(u.String())
		if err != nil || !ib.Equal(u) {
			t.Fatalf("integer roundtrip %s: %v", u, err)
		}
	})
}

func TestPropertyRescaleExact(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		cents := FromUint64(rapid.Uint64Range(0, 1<<53).Draw(t, "cents"))
		dec := rapid.Uint8Range(2, MaxDecimals).Draw(t, "dec")
		tok, err := USDToToken(cents, dec)
		if err != nil {
			t.Fatal(err)
		}
		back, err := Rescale(tok, dec, USDDecimals)
		if err != nil || !back.Equal(cents) {
			t.Fatalf("rescale roundtrip %s -> %s -> %s (%v)", cents, tok, back, err)
		}
		if FormatUSD(cents)+strings.Repeat("0", int(dec)-2) != FormatDecimal(tok, dec) {
			t.Fatalf("decimal strings disagree: %s vs %s", FormatUSD(cents), FormatDecimal(tok, dec))
		}
	})
}

func TestPropertyArithmetic(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		a := genUnits(t, "a")
		b := genUnits(t, "b")
		sum, err := a.Add(b)
		if err != nil {
			if new(big.Int).Add(a.Big(), b.Big()).Cmp(maxUnits) <= 0 {
				t.Fatalf("spurious overflow %s+%s", a, b)
			}
			return
		}
		d, err := sum.Sub(b)
		if err != nil || !d.Equal(a) {
			t.Fatalf("(a+b)-b != a: %s %s", a, b)
		}
		if a.Cmp(b) < 0 {
			if _, err := a.Sub(b); !errors.Is(err, ErrNegative) {
				t.Fatalf("a<b but a-b ok")
			}
		}
	})
}

func TestPropertyRejectsTooManyDigits(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		dec := rapid.Uint8Range(0, 18).Draw(t, "dec")
		extra := rapid.IntRange(1, 5).Draw(t, "extra")
		frac := rapid.StringMatching("[0-9]{"+itoa(int(dec)+extra)+"}").Draw(t, "frac")
		if _, err := ParseDecimal("1."+frac, dec); !errors.Is(err, ErrPrecision) {
			t.Fatalf("accepted %q with %d decimals: %v", "1."+frac, dec, err)
		}
	})
}

func itoa(n int) string { return big.NewInt(int64(n)).String() }
