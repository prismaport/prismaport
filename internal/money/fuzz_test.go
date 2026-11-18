package money

import (
	"errors"
	"math/big"
	"strings"
	"testing"
)

// Native fuzz targets (ADR 0008 E-2). Seeds come from the table tests in money_test.go plus the
// 0 / 2^256-1 bounds; `go test -run=^$ -fuzz=FuzzX ./internal/money` explores from there, and a
// failing input lands in testdata/fuzz/FuzzX to stay a regression seed.

// unitsFrom maps arbitrary bytes to an amount in [0, 2^256-1]: the first 32 bytes, big-endian.
func unitsFrom(b []byte) Units {
	if len(b) > 32 {
		b = b[:32]
	}
	u, err := FromBig(new(big.Int).SetBytes(b))
	if err != nil {
		panic(err) // 32 bytes never exceed 2^256-1
	}
	return u
}

func maxBytes() []byte { return MaxUnits().Big().Bytes() }

// FuzzParseDecimal: whatever the input, ParseDecimal either fails with one of its errors or
// returns an amount in range that formats back to a string parsing to the same amount; a
// canonical input (exactly `decimals` fractional digits) is reproduced byte for byte.
func FuzzParseDecimal(f *testing.F) {
	for _, s := range []struct {
		in  string
		dec uint8
	}{
		{"25", 6}, {"25.00", 2}, {"25.5", 6}, {"0.000001", 6}, {"0", 6}, {"0.0", 2}, {"25.0000001", 6},
		{"25.001", 2}, {"-1", 6}, {"+1", 6}, {"1e6", 6}, {" 1", 6}, {"1 ", 6}, {"", 6}, {".5", 6}, {"5.", 6},
		{"05", 6}, {"1,5", 6}, {"1.2.3", 6}, {"1", 37}, {"1.5", 0}, {"7", 0},
		{MaxUnits().String(), 0}, {"115792089237316195423570985008687907853269984665640564039457.584007913129639935", 18},
		{"115792089237316195423570985008687907853269984665640564039457.584007913129639936", 18},
		{"0.000000000000000001", 18}, {"1.000000000000000000", 18},
	} {
		f.Add(s.in, s.dec)
	}
	f.Fuzz(func(t *testing.T, s string, dec uint8) {
		u, err := ParseDecimal(s, dec)
		if err != nil {
			if !errors.Is(err, ErrSyntax) && !errors.Is(err, ErrPrecision) && !errors.Is(err, ErrRange) && !errors.Is(err, ErrDecimals) {
				t.Fatalf("ParseDecimal(%q, %d): unexpected error class %v", s, dec, err)
			}
			if !u.IsZero() {
				t.Fatalf("ParseDecimal(%q, %d) failed but returned %s", s, dec, u)
			}
			return
		}
		if dec > MaxDecimals {
			t.Fatalf("ParseDecimal(%q, %d) accepted decimals above %d", s, dec, MaxDecimals)
		}
		if u.Big().Sign() < 0 || u.Cmp(MaxUnits()) > 0 {
			t.Fatalf("ParseDecimal(%q, %d) = %s, out of range", s, dec, u)
		}
		out := FormatDecimal(u, dec)
		back, err := ParseDecimal(out, dec)
		if err != nil || !back.Equal(u) {
			t.Fatalf("round trip %q -> %s -> %q -> %s, %v", s, u, out, back, err)
		}
		_, frac, hasPoint := strings.Cut(s, ".")
		if (dec == 0 && !hasPoint) || (dec > 0 && len(frac) == int(dec)) {
			if out != s {
				t.Fatalf("canonical %q (decimals %d) formats as %q", s, dec, out)
			}
		}
	})
}

// FuzzFormatRoundTrip: every amount up to 2^256-1 at every scale (0-36 decimals) formats to a
// string that parses back exactly, and FormatMinPlaces never changes the value either.
func FuzzFormatRoundTrip(f *testing.F) {
	for _, s := range []struct {
		units    []byte
		dec, min uint8
	}{
		{big.NewInt(2500).Bytes(), 2, 2}, {nil, 2, 2}, {[]byte{1}, 6, 2}, {big.NewInt(25_000_000).Bytes(), 6, 0},
		{[]byte{5}, 0, 0}, {big.NewInt(10_000_001).Bytes(), 6, 2}, {big.NewInt(10_123_000).Bytes(), 6, 2},
		{nil, 18, 2}, {maxBytes(), 18, 2}, {maxBytes(), 0, 0}, {maxBytes(), 36, 36}, {[]byte{1}, 36, 0},
	} {
		f.Add(s.units, s.dec, s.min)
	}
	f.Fuzz(func(t *testing.T, raw []byte, dec, minPlaces uint8) {
		dec %= MaxDecimals + 1
		minPlaces %= dec + 1
		u := unitsFrom(raw)
		s := FormatDecimal(u, dec)
		back, err := ParseDecimal(s, dec)
		if err != nil || !back.Equal(u) {
			t.Fatalf("FormatDecimal(%s, %d) = %q parses to %s, %v", u, dec, s, back, err)
		}
		if dec > 0 && len(s)-strings.IndexByte(s, '.')-1 != int(dec) {
			t.Fatalf("FormatDecimal(%s, %d) = %q: not exactly %d fractional digits", u, dec, s, dec)
		}
		m := FormatMinPlaces(u, dec, minPlaces)
		back, err = ParseDecimal(m, dec)
		if err != nil || !back.Equal(u) {
			t.Fatalf("FormatMinPlaces(%s, %d, %d) = %q parses to %s, %v", u, dec, minPlaces, m, back, err)
		}
		if i := strings.IndexByte(m, '.'); minPlaces > 0 && (i < 0 || len(m)-i-1 < int(minPlaces)) {
			t.Fatalf("FormatMinPlaces(%s, %d, %d) = %q: fewer than %d places", u, dec, minPlaces, m, minPlaces)
		}
		if p, err := ParseInteger(u.String()); err != nil || !p.Equal(u) {
			t.Fatalf("ParseInteger(%s) = %s, %v", u, p, err)
		}
	})
}

// FuzzArithmetic: Add, Sub, Rescale and MulDivDown stay inside [0, 2^256-1] and agree with
// big.Int, failing (never wrapping) exactly when the true result leaves the range.
func FuzzArithmetic(f *testing.F) {
	f.Add([]byte{1}, []byte{2}, uint8(6), uint8(18), uint64(50), uint64(10_000))
	f.Add(maxBytes(), []byte{1}, uint8(0), uint8(36), uint64(10_000), uint64(10_000))
	f.Add(maxBytes(), maxBytes(), uint8(18), uint8(6), uint64(300), uint64(10_000))
	f.Add([]byte(nil), maxBytes(), uint8(2), uint8(6), uint64(0), uint64(1))
	f.Add(big.NewInt(25_000_000).Bytes(), big.NewInt(100_000).Bytes(), uint8(6), uint8(2), uint64(1<<63), uint64(3))
	f.Fuzz(func(t *testing.T, ra, rb []byte, from, to uint8, num, den uint64) {
		a, b := unitsFrom(ra), unitsFrom(rb)
		max := MaxUnits().Big()
		check := func(op string, got Units, err error, want *big.Int) {
			t.Helper()
			inRange := want.Sign() >= 0 && want.Cmp(max) <= 0
			switch {
			case inRange && (err != nil || got.Big().Cmp(want) != 0):
				t.Fatalf("%s = %s, %v; want %s", op, got, err, want)
			case !inRange && err == nil:
				t.Fatalf("%s = %s, want an error (true result %s is out of range)", op, got, want)
			}
		}
		sum, err := a.Add(b)
		check("Add", sum, err, new(big.Int).Add(a.Big(), b.Big()))
		diff, err := a.Sub(b)
		check("Sub", diff, err, new(big.Int).Sub(a.Big(), b.Big()))
		if den > 0 {
			q, err := MulDivDown(a, num, den)
			p := new(big.Int).Mul(a.Big(), new(big.Int).SetUint64(num))
			check("MulDivDown", q, err, p.Quo(p, new(big.Int).SetUint64(den)))
		}
		from %= MaxDecimals + 1
		to %= MaxDecimals + 1
		r, err := Rescale(a, from, to)
		switch {
		case to >= from:
			check("Rescale up", r, err, new(big.Int).Mul(a.Big(), pow10(to-from)))
		default:
			q, rem := new(big.Int).QuoRem(a.Big(), pow10(from-to), new(big.Int))
			if rem.Sign() != 0 {
				if !errors.Is(err, ErrInexact) {
					t.Fatalf("Rescale(%s, %d, %d) = %s, %v; want ErrInexact", a, from, to, r, err)
				}
				break
			}
			check("Rescale down", r, err, q)
			if up, err := Rescale(r, to, from); err != nil || !up.Equal(a) {
				t.Fatalf("Rescale(%s) back up = %s, %v", r, up, err)
			}
		}
	})
}
