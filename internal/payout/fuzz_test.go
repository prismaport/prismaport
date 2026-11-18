package payout

import (
	"encoding/binary"
	"errors"
	"math/big"
	"testing"

	"github.com/prismaport/prismaport/internal/fee"
	"github.com/prismaport/prismaport/internal/money"
)

// Native fuzz targets (ADR 0008 E-2) for the split rule. The seed corpus is every case of
// contracts/test/vectors/splits.json (the vectors the Solidity and Rust implementations check too)
// plus the table cases of TestCheckBps. Run one with
// `go test -run=^$ -fuzz=FuzzShares ./internal/payout`.

func amountFrom(b []byte) money.Units {
	if len(b) > 32 {
		b = b[:32]
	}
	u, err := money.FromBig(new(big.Int).SetBytes(b))
	if err != nil {
		panic(err)
	}
	return u
}

// encodeBps packs shares as little-endian uint16 pairs, the fuzzer's encoding of a split.
func encodeBps(bps []uint16) []byte {
	out := make([]byte, 0, 2*len(bps))
	for _, b := range bps {
		out = binary.LittleEndian.AppendUint16(out, b)
	}
	return out
}

// decodeBps reads up to MaxRecipients+1 shares (one more than allowed, so the count check is
// reached too).
func decodeBps(raw []byte) []uint16 {
	var out []uint16
	for i := 0; i+1 < len(raw) && len(out) <= MaxRecipients; i += 2 {
		out = append(out, binary.LittleEndian.Uint16(raw[i:]))
	}
	return out
}

// validBps turns any shares into a valid split of the same length (1 to 5): every share above
// zero, summing to exactly BpsTotal, each one steered by the fuzzed value.
func validBps(raw []uint16) []uint16 {
	n := min(max(len(raw), 1), MaxRecipients)
	out := make([]uint16, n)
	left := BpsTotal
	for i := 0; i < n-1; i++ {
		hi := left - (n - 1 - i)
		v := 1
		if i < len(raw) {
			v = 1 + int(raw[i])%hi
		}
		out[i] = uint16(v)
		left -= v
	}
	out[n-1] = uint16(left)
	return out
}

// bpsOracle is CheckBps written out: 1 to 5 entries, none zero, summing to exactly 10_000.
func bpsOracle(bps []uint16) error {
	if len(bps) == 0 || len(bps) > MaxRecipients {
		return ErrCount
	}
	sum := 0
	for _, b := range bps {
		if b == 0 {
			return ErrZeroShare
		}
		sum += int(b)
	}
	if sum != BpsTotal {
		return ErrSum
	}
	return nil
}

// checkShares asserts the split rule on one result: shares add up to exactly net (nothing
// created or lost), every share after the first is the floor of its exact part, and the first
// carries at most n-1 units of rounding remainder.
func checkShares(t *testing.T, net money.Units, bps []uint16, shares []money.Units) {
	t.Helper()
	if len(shares) != len(bps) {
		t.Fatalf("%d shares for %d recipients", len(shares), len(bps))
	}
	total := new(big.Int)
	for i, s := range shares {
		if s.Big().Sign() < 0 {
			t.Fatalf("share %d negative: %s", i, s)
		}
		total.Add(total, s.Big())
		exact := new(big.Int).Mul(net.Big(), big.NewInt(int64(bps[i])))
		exact.Quo(exact, big.NewInt(BpsTotal))
		if i > 0 && s.Big().Cmp(exact) != 0 {
			t.Fatalf("share %d = %s, floor(net*bps/10_000) is %s", i, s, exact)
		}
		if i == 0 {
			extra := new(big.Int).Sub(s.Big(), exact)
			if extra.Sign() < 0 || extra.Cmp(big.NewInt(int64(len(bps)-1))) > 0 {
				t.Fatalf("first share %s carries %s units over its floor %s (n=%d)", s, extra, exact, len(bps))
			}
		}
	}
	if total.Cmp(net.Big()) != 0 {
		t.Fatalf("shares add up to %s, net is %s", total, net)
	}
}

func FuzzShares(f *testing.F) {
	v := loadVectors(f)
	for _, c := range v.Splits {
		f.Add(money.MustParseInteger(c.Net).Big().Bytes(), encodeBps(c.Bps), false)
	}
	for _, c := range v.InvalidBps {
		raw := make([]uint16, len(c.Bps))
		for i, b := range c.Bps {
			raw[i] = uint16(b) // 65_535 and 10_001 stay as they are; nothing in the file is wider
		}
		f.Add([]byte{100}, encodeBps(raw), false)
	}
	f.Add(money.MaxUnits().Big().Bytes(), encodeBps([]uint16{1, 9_996, 1, 1, 1}), false)
	f.Add(money.MaxUnits().Big().Bytes(), encodeBps([]uint16{65_535, 65_535, 65_535, 65_535, 65_535}), true)
	f.Fuzz(func(t *testing.T, rawNet, rawBps []byte, normalize bool) {
		net := amountFrom(rawNet)
		bps := decodeBps(rawBps)
		if normalize {
			bps = validBps(bps)
		}
		want := bpsOracle(bps)
		if err := CheckBps(bps); !errors.Is(err, want) || (want == nil) != (err == nil) {
			t.Fatalf("CheckBps(%v) = %v, want %v", bps, err, want)
		}
		shares, err := Shares(net, bps)
		if want != nil {
			if err == nil || shares != nil {
				t.Fatalf("Shares(%s, %v) = %v, %v; want the %v refusal", net, bps, shares, err, want)
			}
			return
		}
		if err != nil {
			t.Fatalf("Shares(%s, %v): %v", net, bps, err)
		}
		checkShares(t, net, bps, shares)
	})
}

// FuzzSettlement: the fee rule followed by the split never pays out more than the gross: fee +
// every share == gross exactly when it settles, nothing moves on dust, the result matches
// fee.Compute, and Verify accepts exactly that answer and refuses it moved by one unit.
func FuzzSettlement(f *testing.F) {
	v := loadVectors(f)
	for _, c := range v.Settlements {
		f.Add(money.MustParseInteger(c.Gross).Big().Bytes(), c.Decimals, c.FeeBps, c.FixedFeeMicros, encodeBps(c.Bps))
	}
	f.Add(money.MaxUnits().Big().Bytes(), uint8(18), uint16(fee.MaxFeeBps), uint64(fee.MaxFixedFeeMicros), encodeBps([]uint16{1, 9_996, 1, 1, 1}))
	f.Add(money.MaxUnits().Big().Bytes(), uint8(6), uint16(0), uint64(0), encodeBps([]uint16{2_000, 2_000, 2_000, 2_000, 2_000}))
	f.Fuzz(func(t *testing.T, rawGross []byte, dec uint8, feeBps uint16, fixed uint64, rawBps []byte) {
		gross := amountFrom(rawGross)
		dec = fee.MinTokenDecimals + dec%13 // 6..18
		p := fee.Params{FeeBps: feeBps % (fee.MaxFeeBps + 1), FixedFeeMicros: fixed % (fee.MaxFixedFeeMicros + 1)}
		bps := validBps(decodeBps(rawBps))
		s, err := Compute(gross, dec, p, bps)
		if err != nil {
			t.Fatalf("Compute(%s, %d, %+v, %v): %v", gross, dec, p, bps, err)
		}
		fs, err := fee.Compute(gross, dec, p)
		if err != nil {
			t.Fatal(err)
		}
		if s.Dust != fs.Dust || !s.Fee.Equal(fs.Fee) || !s.Net.Equal(fs.Net) || !s.Gross.Equal(gross) {
			t.Fatalf("settlement %+v disagrees with fee.Compute %+v", s, fs)
		}
		if s.Dust {
			if len(s.Shares) != 0 || !s.Fee.IsZero() || !s.Net.IsZero() {
				t.Fatalf("dust settlement moved money: %+v", s)
			}
			if Verify(gross, dec, p, bps, money.Zero, nil) == nil {
				t.Fatal("dust verified")
			}
			return
		}
		checkShares(t, s.Net, bps, s.Shares)
		paid := new(big.Int).Set(s.Fee.Big())
		for _, x := range s.Shares {
			paid.Add(paid, x.Big())
		}
		if paid.Cmp(gross.Big()) != 0 {
			t.Fatalf("fee + shares = %s, gross %s: the gateway would pay out %s", paid, gross, new(big.Int).Sub(paid, gross.Big()))
		}
		if err := Verify(gross, dec, p, bps, s.Fee, s.Shares); err != nil {
			t.Fatalf("Verify refuses the rule's own answer: %v", err)
		}
		one := money.FromUint64(1)
		if up, err := s.Fee.Add(one); err == nil && Verify(gross, dec, p, bps, up, s.Shares) == nil {
			t.Fatal("fee + 1 verified")
		}
		last := len(s.Shares) - 1
		moved := append([]money.Units(nil), s.Shares...)
		if moved[last], err = moved[last].Add(one); err == nil && Verify(gross, dec, p, bps, s.Fee, moved) == nil {
			t.Fatal("last share + 1 verified")
		}
	})
}

// FuzzShareString: ParseShare accepts only percentages in (0, 100] with at most two decimals,
// and every accepted share formats back to a string that parses to the same basis points.
func FuzzShareString(f *testing.F) {
	for _, c := range loadVectors(f).ShareStrings {
		f.Add(c.Share)
	}
	f.Fuzz(func(t *testing.T, s string) {
		bps, err := ParseShare(s)
		if err != nil {
			if !errors.Is(err, ErrShareFormat) || bps != 0 {
				t.Fatalf("ParseShare(%q) = %d, %v", s, bps, err)
			}
			return
		}
		if bps == 0 || bps > BpsTotal {
			t.Fatalf("ParseShare(%q) = %d, outside (0, 10_000]", s, bps)
		}
		out := FormatShare(bps)
		if back, err := ParseShare(out); err != nil || back != bps {
			t.Fatalf("%q -> %d -> %q -> %d, %v", s, bps, out, back, err)
		}
		if u, err := money.ParseDecimal(s, shareDecimals); err != nil || u.Big().Uint64() != uint64(bps) {
			t.Fatalf("ParseShare(%q) = %d but the decimal reads %s, %v", s, bps, u, err)
		}
	})
}
