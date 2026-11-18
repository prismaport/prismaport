package fee

import (
	"errors"
	"math/big"
	"testing"

	"github.com/prismaport/prismaport/internal/money"
)

// Native fuzz targets (ADR 0008 E-2), seeded from TestComputeExamples, TestEighteenDecimals and
// TestMinSettlingCents plus the 2^256-1 bound. Run one with
// `go test -run=^$ -fuzz=FuzzCompute ./internal/fee`.

func grossFrom(b []byte) money.Units {
	if len(b) > 32 {
		b = b[:32]
	}
	u, err := money.FromBig(new(big.Int).SetBytes(b))
	if err != nil {
		panic(err)
	}
	return u
}

func seedGross(s string, dec uint8) []byte { return mustDecimal(s, dec).Big().Bytes() }

func mustDecimal(s string, dec uint8) money.Units {
	u, err := money.ParseDecimal(s, dec)
	if err != nil {
		panic(err)
	}
	return u
}

// oracle is the rule computed directly with big.Int: fixed*10^(dec-6) + floor(gross*bps/10_000).
func oracle(gross *big.Int, dec uint8, p Params) *big.Int {
	fixed := new(big.Int).Mul(new(big.Int).SetUint64(p.FixedFeeMicros), new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(dec)-6), nil))
	pct := new(big.Int).Mul(gross, big.NewInt(int64(p.FeeBps)))
	return fixed.Add(fixed, pct.Quo(pct, big.NewInt(bpsDenominator)))
}

// FuzzCompute: for any gross up to 2^256-1, decimals 0-36 and any parameters, Compute either
// refuses the input with its documented error or gives exactly the oracle's split: fee + net ==
// gross, the fee never above Cap, a positive net, dust exactly when the fee reaches the gross,
// and one more base unit never turns a settling amount into dust or lowers its net.
func FuzzCompute(f *testing.F) {
	for _, g := range []string{"25", "5", "100", "10.01", "10.000199", "12.345678", "0.11", "0.100503", "0.100502", "0.1", "0.05", "0"} {
		f.Add(seedGross(g, 6), uint8(6), uint16(50), uint64(100_000))
	}
	for _, g := range []string{"25", "1", "0.100502512562814071", "0.100502512562814070"} {
		f.Add(seedGross(g, 18), uint8(18), uint16(50), uint64(100_000))
	}
	maxGross := money.MaxUnits().Big().Bytes()
	f.Add(maxGross, uint8(18), uint16(MaxFeeBps), uint64(MaxFixedFeeMicros))
	f.Add(maxGross, uint8(6), uint16(0), uint64(0))
	f.Add(maxGross, uint8(36), uint16(MaxFeeBps), uint64(MaxFixedFeeMicros))
	f.Add([]byte{1}, uint8(5), uint16(50), uint64(100_000))
	f.Add([]byte{1}, uint8(0), uint16(0), uint64(0))
	f.Add([]byte{1}, uint8(37), uint16(0), uint64(0))
	f.Add([]byte{1}, uint8(6), uint16(MaxFeeBps+1), uint64(0))
	f.Add([]byte{1}, uint8(6), uint16(0), uint64(MaxFixedFeeMicros+1))
	f.Add(seedGross("5.000001", 6), uint8(6), uint16(MaxFeeBps), uint64(MaxFixedFeeMicros))
	f.Fuzz(func(t *testing.T, raw []byte, dec uint8, feeBps uint16, fixed uint64) {
		gross := grossFrom(raw)
		p := Params{FeeBps: feeBps, FixedFeeMicros: fixed}
		s, err := Compute(gross, dec, p)
		switch {
		case feeBps > MaxFeeBps:
			if !errors.Is(err, ErrFeeBpsCap) {
				t.Fatalf("feeBps %d: err %v, want ErrFeeBpsCap", feeBps, err)
			}
			return
		case fixed > MaxFixedFeeMicros:
			if !errors.Is(err, ErrFixedFeeCap) {
				t.Fatalf("fixedFeeMicros %d: err %v, want ErrFixedFeeCap", fixed, err)
			}
			return
		case dec < MinTokenDecimals:
			if !errors.Is(err, ErrTokenDecimal) {
				t.Fatalf("decimals %d: err %v, want ErrTokenDecimal", dec, err)
			}
			return
		case dec > money.MaxDecimals:
			if !errors.Is(err, money.ErrDecimals) {
				t.Fatalf("decimals %d: err %v, want money.ErrDecimals", dec, err)
			}
			return
		}
		if err != nil {
			t.Fatalf("Compute(%s, %d, %+v): %v", gross, dec, p, err)
		}
		if !s.Gross.Equal(gross) {
			t.Fatalf("Gross %s, want %s", s.Gross, gross)
		}
		want := oracle(gross.Big(), dec, p)
		if s.Dust {
			if want.Cmp(gross.Big()) < 0 {
				t.Fatalf("gross %s dust although the fee %s is below it", gross, want)
			}
			if !s.Fee.IsZero() || !s.Net.IsZero() {
				t.Fatalf("dust moved money: fee %s net %s", s.Fee, s.Net)
			}
			return
		}
		if s.Fee.Big().Cmp(want) != 0 {
			t.Fatalf("gross %s: fee %s, the rule gives %s", gross, s.Fee, want)
		}
		if sum, err := s.Fee.Add(s.Net); err != nil || !sum.Equal(gross) {
			t.Fatalf("fee %s + net %s = %s (%v), want the gross %s", s.Fee, s.Net, sum, err, gross)
		}
		if s.Net.IsZero() {
			t.Fatalf("gross %s settles with a zero net", gross)
		}
		if c, err := Cap(gross, dec); err != nil || s.Fee.Cmp(c) > 0 {
			t.Fatalf("fee %s above the cap %s (%v)", s.Fee, c, err)
		}
		next, err := gross.Add(money.FromUint64(1))
		if err != nil {
			return // gross is 2^256-1
		}
		s1, err := Compute(next, dec, p)
		if err != nil || s1.Dust || s1.Net.Cmp(s.Net) < 0 {
			t.Fatalf("gross %s settles with net %s but gross+1 gives %+v, %v", gross, s.Net, s1, err)
		}
	})
}

// FuzzSettlesUSD: a whole-cent amount settles exactly from MinSettlingCents on, and the answer
// matches Compute on the amount scaled to every decimals from 6 to 18.
func FuzzSettlesUSD(f *testing.F) {
	for _, c := range []struct {
		cents  uint64
		bps    uint16
		fixed  uint64
		tokDec uint8
	}{
		{10, 50, 100_000, 6}, {11, 50, 100_000, 18}, {9, 50, 100_000, 6}, {2500, 50, 100_000, 8},
		{10, 50, 99_499, 6}, {11, 50, 99_500, 18}, {515, MaxFeeBps, MaxFixedFeeMicros, 6}, {516, MaxFeeBps, MaxFixedFeeMicros, 18},
		{0, 0, 0, 6}, {1, 0, 0, 18}, {^uint64(0), MaxFeeBps, MaxFixedFeeMicros, 18},
	} {
		f.Add(c.cents, c.bps, c.fixed, c.tokDec)
	}
	f.Fuzz(func(t *testing.T, cents uint64, feeBps uint16, fixed uint64, tokDec uint8) {
		p := Params{FeeBps: feeBps % (MaxFeeBps + 1), FixedFeeMicros: fixed % (MaxFixedFeeMicros + 1)}
		tokDec = MinTokenDecimals + tokDec%13 // 6..18
		c := money.FromUint64(cents)
		ok, err := SettlesUSD(c, p)
		if err != nil {
			t.Fatal(err)
		}
		least, err := MinSettlingCents(p)
		if err != nil {
			t.Fatal(err)
		}
		if ok != (c.Cmp(least) >= 0) {
			t.Fatalf("%d cents under %+v: settles=%v, smallest settling amount %s", cents, p, ok, least)
		}
		gross, err := money.USDToToken(c, tokDec)
		if err != nil {
			t.Fatal(err)
		}
		s, err := Compute(gross, tokDec, p)
		if err != nil || s.Dust == ok {
			t.Fatalf("%d cents at %d decimals: SettlesUSD=%v but Compute dust=%v (%v)", cents, tokDec, ok, s.Dust, err)
		}
	})
}
