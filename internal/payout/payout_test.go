package payout

import (
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	"github.com/prismaport/prismaport/internal/fee"
	"github.com/prismaport/prismaport/internal/money"
	"pgregory.net/rapid"
)

type vectorFile struct {
	Splits []struct {
		Name   string   `json:"name"`
		Net    string   `json:"net"`
		Bps    []uint16 `json:"bps"`
		Shares []string `json:"shares"`
	} `json:"splits"`
	Settlements []struct {
		Name           string   `json:"name"`
		Gross          string   `json:"gross"`
		Decimals       uint8    `json:"decimals"`
		FeeBps         uint16   `json:"feeBps"`
		FixedFeeMicros uint64   `json:"fixedFeeMicros"`
		Bps            []uint16 `json:"bps"`
		Dust           bool     `json:"dust"`
		Fee            string   `json:"fee"`
		Net            string   `json:"net"`
		Shares         []string `json:"shares"`
	} `json:"settlements"`
	InvalidBps []struct {
		Name  string `json:"name"`
		Bps   []int  `json:"bps"`
		Error string `json:"error"`
	} `json:"invalidBps"`
	ShareStrings []struct {
		Share string `json:"share"`
		Bps   uint16 `json:"bps"`
		Error bool   `json:"error"`
	} `json:"shareStrings"`
}

func loadVectors(t testing.TB) vectorFile {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "contracts", "test", "vectors", "splits.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v vectorFile
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Splits) == 0 || len(v.Settlements) == 0 || len(v.InvalidBps) == 0 || len(v.ShareStrings) == 0 {
		t.Fatal("vector file is missing a section")
	}
	return v
}

func units(t *testing.T, s string) money.Units {
	t.Helper()
	u, err := money.ParseInteger(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestSplitVectors(t *testing.T) {
	for _, c := range loadVectors(t).Splits {
		t.Run(c.Name, func(t *testing.T) {
			got, err := Shares(units(t, c.Net), c.Bps)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(c.Shares) {
				t.Fatalf("%d shares, want %d", len(got), len(c.Shares))
			}
			for i := range got {
				if got[i].String() != c.Shares[i] {
					t.Fatalf("share %d = %s, want %s", i, got[i], c.Shares[i])
				}
			}
		})
	}
}

func TestSettlementVectors(t *testing.T) {
	for _, c := range loadVectors(t).Settlements {
		t.Run(c.Name, func(t *testing.T) {
			p := fee.Params{FeeBps: c.FeeBps, FixedFeeMicros: c.FixedFeeMicros}
			gross := units(t, c.Gross)
			got, err := Compute(gross, c.Decimals, p, c.Bps)
			if err != nil {
				t.Fatal(err)
			}
			if got.Dust != c.Dust || got.Fee.String() != c.Fee || got.Net.String() != c.Net || len(got.Shares) != len(c.Shares) {
				t.Fatalf("got dust=%v fee=%s net=%s shares=%v", got.Dust, got.Fee, got.Net, got.Shares)
			}
			observed := make([]money.Units, len(c.Shares))
			for i := range c.Shares {
				if got.Shares[i].String() != c.Shares[i] {
					t.Fatalf("share %d = %s, want %s", i, got.Shares[i], c.Shares[i])
				}
				observed[i] = units(t, c.Shares[i])
			}
			err = Verify(gross, c.Decimals, p, c.Bps, units(t, c.Fee), observed)
			if c.Dust != (err != nil) {
				t.Fatalf("verify: %v", err)
			}
		})
	}
}

func TestInvalidBpsVectors(t *testing.T) {
	want := map[string]error{"count": ErrCount, "zero_share": ErrZeroShare, "sum": ErrSum}
	for _, c := range loadVectors(t).InvalidBps {
		t.Run(c.Name, func(t *testing.T) {
			bps := make([]uint16, len(c.Bps))
			sumWraps := false
			for i, b := range c.Bps {
				if b > 0xffff {
					sumWraps = true
				}
				bps[i] = uint16(min(b, 0xffff))
			}
			if sumWraps {
				t.Fatal("vector holds a value above a uint16")
			}
			if err := CheckBps(bps); !errors.Is(err, want[c.Error]) {
				t.Fatalf("got %v, want %v", err, want[c.Error])
			}
		})
	}
}

func TestShareStringVectors(t *testing.T) {
	for _, c := range loadVectors(t).ShareStrings {
		got, err := ParseShare(c.Share)
		if c.Error {
			if !errors.Is(err, ErrShareFormat) {
				t.Errorf("%q accepted as %d", c.Share, got)
			}
			continue
		}
		if err != nil || got != c.Bps {
			t.Errorf("%q = %d (%v), want %d", c.Share, got, err, c.Bps)
		}
	}
	for _, c := range []struct {
		bps  uint16
		want string
	}{{7000, "70.00"}, {1, "0.01"}, {10_000, "100.00"}, {3333, "33.33"}, {110, "1.10"}} {
		t.Run(c.want, func(t *testing.T) {
			if got := FormatShare(c.bps); got != c.want {
				t.Errorf("FormatShare(%d) = %s, want %s", c.bps, got, c.want)
			}
			if back, err := ParseShare(FormatShare(c.bps)); err != nil || back != c.bps {
				t.Errorf("round trip of %d: %d %v", c.bps, back, err)
			}
		})
	}
	for sum, want := range map[int]string{9000: "90", 9050: "90.5", 10_001: "100.01", 0: "0", 75_536: "755.36"} {
		if got := FormatShareSum(sum); got != want {
			t.Errorf("FormatShareSum(%d) = %s, want %s", sum, got, want)
		}
	}
}

func TestSumMessageSaysTheCurrentTotal(t *testing.T) {
	err := CheckBps([]uint16{5000, 4000})
	if !errors.Is(err, ErrSum) || err.Error() != "payout: shares must add up to exactly 100% (now 90%)" {
		t.Fatalf("%v", err)
	}
}

func genBps(t *rapid.T) []uint16 {
	n := rapid.IntRange(1, MaxRecipients).Draw(t, "n")
	out := make([]uint16, n)
	left := BpsTotal
	for i := 0; i < n-1; i++ {
		v := rapid.IntRange(1, left-(n-1-i)).Draw(t, "bps")
		out[i] = uint16(v)
		left -= v
	}
	out[n-1] = uint16(left)
	return out
}

// The invariant the contracts fuzz: the shares add up to the net, each later share is the floor
// of its exact share, and the first carries at most n - 1 units of rounding dust.
func TestSharesProperty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		bps := genBps(t)
		netBig := new(big.Int).SetBytes(rapid.SliceOfN(rapid.Byte(), 1, 32).Draw(t, "net"))
		net, err := money.FromBig(netBig)
		if err != nil {
			t.Fatal(err)
		}
		s, err := Shares(net, bps)
		if err != nil {
			t.Fatal(err)
		}
		total := new(big.Int)
		for i, x := range s {
			total.Add(total, x.Big())
			exact := new(big.Int).Quo(new(big.Int).Mul(netBig, big.NewInt(int64(bps[i]))), big.NewInt(BpsTotal))
			if i > 0 && x.Big().Cmp(exact) != 0 {
				t.Fatalf("share %d = %s, floor is %s", i, x, exact)
			}
			if i == 0 {
				if x.Big().Cmp(exact) < 0 || new(big.Int).Sub(x.Big(), exact).Cmp(big.NewInt(int64(len(bps)-1))) > 0 {
					t.Fatalf("first share %s, exact %s, n %d", x, exact, len(bps))
				}
				if netBig.Sign() > 0 && x.IsZero() {
					t.Fatal("first share is zero for a non-zero net")
				}
			}
		}
		if total.Cmp(netBig) != 0 {
			t.Fatalf("shares add up to %s, net %s", total, netBig)
		}
	})
}

// Verify accepts exactly the rule's amounts: any one-unit change to the fee or to a share, a
// dropped or an extra recipient, or swapped amounts are refused.
func TestVerifyRefusesTampering(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		bps := genBps(t)
		p := fee.Params{FeeBps: uint16(rapid.IntRange(0, fee.MaxFeeBps).Draw(t, "feeBps")),
			FixedFeeMicros: uint64(rapid.IntRange(0, fee.MaxFixedFeeMicros).Draw(t, "fixedFee"))}
		gross := money.FromUint64(rapid.Uint64Range(1, 1<<62).Draw(t, "gross"))
		want, err := Compute(gross, 6, p, bps)
		if err != nil {
			t.Fatal(err)
		}
		if want.Dust {
			if Verify(gross, 6, p, bps, money.Zero, nil) == nil {
				t.Fatal("dust verified")
			}
			return
		}
		if err := Verify(gross, 6, p, bps, want.Fee, want.Shares); err != nil {
			t.Fatal(err)
		}
		one := money.FromUint64(1)
		bumped, _ := want.Fee.Add(one)
		if Verify(gross, 6, p, bps, bumped, want.Shares) == nil {
			t.Fatal("fee + 1 verified")
		}
		i := rapid.IntRange(0, len(bps)-1).Draw(t, "i")
		tampered := append([]money.Units(nil), want.Shares...)
		tampered[i], _ = tampered[i].Add(one)
		if Verify(gross, 6, p, bps, want.Fee, tampered) == nil {
			t.Fatal("share + 1 verified")
		}
		if Verify(gross, 6, p, bps, want.Fee, append(append([]money.Units(nil), want.Shares...), money.Zero)) == nil {
			t.Fatal("extra recipient verified")
		}
		if len(bps) > 1 {
			if Verify(gross, 6, p, bps, want.Fee, want.Shares[:len(bps)-1]) == nil {
				t.Fatal("missing recipient verified")
			}
			swapped := append([]money.Units(nil), want.Shares...)
			swapped[0], swapped[1] = swapped[1], swapped[0]
			if !swapped[0].Equal(swapped[1]) && Verify(gross, 6, p, bps, want.Fee, swapped) == nil {
				t.Fatal("swapped shares verified")
			}
		}
	})
}

func TestCheckBps(t *testing.T) {
	for _, c := range []struct {
		bps  []uint16
		want error
	}{
		{[]uint16{10_000}, nil},
		{[]uint16{2000, 2000, 2000, 2000, 2000}, nil},
		{nil, ErrCount},
		{[]uint16{2000, 2000, 2000, 2000, 1000, 1000}, ErrCount},
		{[]uint16{10_000, 0}, ErrZeroShare},
		{[]uint16{65_535, 10_001}, ErrSum},
	} {
		if err := CheckBps(c.bps); !errors.Is(err, c.want) || (c.want == nil && err != nil) {
			t.Errorf("%v: %v, want %v", c.bps, err, c.want)
		}
	}
}
