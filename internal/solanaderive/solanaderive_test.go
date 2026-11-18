package solanaderive

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"testing"

	"github.com/prismaport/prismaport/internal/fee"
)

type vectorRecipient struct {
	Wallet string `json:"wallet"`
	Bps    uint16 `json:"bps"`
}

type vectorParams struct {
	Treasury       string            `json:"treasury"`
	RentReceiver   string            `json:"rentReceiver"`
	FeeBps         uint16            `json:"feeBps"`
	FixedFeeMicros uint64            `json:"fixedFeeMicros"`
	Recipients     []vectorRecipient `json:"recipients"`
	Mints          []string          `json:"mints"`
	ParamsEncoded  string            `json:"paramsEncoded"`
}

func (v vectorParams) params() Params {
	p := Params{
		Treasury:     MustParse(v.Treasury),
		RentReceiver: MustParse(v.RentReceiver),
		Fee:          fee.Params{FeeBps: v.FeeBps, FixedFeeMicros: v.FixedFeeMicros},
	}
	for _, r := range v.Recipients {
		p.Recipients = append(p.Recipients, Recipient{Wallet: MustParse(r.Wallet), Bps: r.Bps})
	}
	for _, m := range v.Mints {
		p.Mints = append(p.Mints, MustParse(m))
	}
	return p
}

type vectorFile struct {
	ProgramID              string            `json:"programId"`
	TokenProgram           string            `json:"tokenProgram"`
	AssociatedTokenProgram string            `json:"associatedTokenProgram"`
	Mints                  map[string]string `json:"mints"`
	Discriminators         map[string]string `json:"discriminators"`
	MaxRecipients          int               `json:"maxRecipients"`
	ParamsMaxEncodedLen    int               `json:"paramsMaxEncodedLen"`
	MaxMints               int               `json:"maxMints"`
	Count                  int               `json:"count"`
	CurvePoints            []struct {
		Bytes   string `json:"bytes"`
		OnCurve bool   `json:"onCurve"`
	} `json:"curvePoints"`
	Vectors []struct {
		vectorParams
		Name          string              `json:"name"`
		Salt          string              `json:"salt"`
		ParamsHash    string              `json:"paramsHash"`
		PDA           string              `json:"pda"`
		Bump          uint8               `json:"bump"`
		PDAAta        map[string]string   `json:"pdaAta"`
		TreasuryAta   map[string]string   `json:"treasuryAta"`
		RecipientAtas map[string][]string `json:"recipientAtas"`
	} `json:"vectors"`
	InvalidParams []struct {
		vectorParams
		Name  string `json:"name"`
		Error string `json:"error"`
	} `json:"invalidParams"`
}

func loadVectors(t *testing.T) vectorFile {
	t.Helper()
	raw, err := os.ReadFile("../../solana/vectors/solana-pda.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectorFile
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	if v.Count == 0 || v.Count != len(v.Vectors) {
		t.Fatalf("count %d, vectors %d", v.Count, len(v.Vectors))
	}
	return v
}

func TestVectorsParity(t *testing.T) {
	v := loadVectors(t)
	program := MustParse(v.ProgramID)
	if TokenProgramID.String() != v.TokenProgram || AssociatedTokenProgramID.String() != v.AssociatedTokenProgram {
		t.Fatal("program ids differ from the vectors")
	}
	if v.MaxMints != MaxMints || v.MaxRecipients != MaxRecipients || v.ParamsMaxEncodedLen != ParamsMaxEncodedLen {
		t.Fatalf("sizes %d/%d/%d", v.MaxMints, v.MaxRecipients, v.ParamsMaxEncodedLen)
	}
	if _, ok := v.Discriminators["account.Forwarder"]; ok {
		t.Fatal("the Forwarder state account is gone (D41); the vectors still list it")
	}
	counts, mintCounts := map[int]bool{}, map[int]bool{}
	for _, c := range v.Vectors {
		counts[len(c.Recipients)] = true
		mintCounts[len(c.Mints)] = true
		t.Run(c.Name, func(t *testing.T) {
			saltBytes, err := hex.DecodeString(c.Salt)
			if err != nil || len(saltBytes) != 32 {
				t.Fatalf("salt %q", c.Salt)
			}
			p := c.params()
			if got := hex.EncodeToString(p.Encode()); got != c.ParamsEncoded {
				t.Fatalf("encoded %s, want %s", got, c.ParamsEncoded)
			}
			if n := len(p.Encode()); n != ParamsFixedLen+ParamsRecipientLen*len(p.Recipients)+ParamsMintLen*len(p.Mints) {
				t.Fatalf("encoded length %d", n)
			}
			if !slices.Equal(SortMints(p.Mints), p.Mints) {
				t.Fatal("vector mints are not in canonical order")
			}
			h := p.Hash()
			if got := hex.EncodeToString(h[:]); got != c.ParamsHash {
				t.Fatalf("hash %s, want %s", got, c.ParamsHash)
			}
			pda, bump, err := ForwarderAddress(program, [32]byte(saltBytes), p)
			if err != nil {
				t.Fatal(err)
			}
			if pda.String() != c.PDA || bump != c.Bump {
				t.Fatalf("pda %s/%d, want %s/%d", pda, bump, c.PDA, c.Bump)
			}
			if IsOnCurve(pda) {
				t.Fatal("pda on curve")
			}
			for name, want := range c.PDAAta {
				ata, err := AssociatedTokenAddress(pda, MustParse(v.Mints[name]))
				if err != nil || ata.String() != want {
					t.Fatalf("%s ata %s (%v), want %s", name, ata, err, want)
				}
			}
			for _, name := range []string{"usdc", "usdt"} {
				mint := MustParse(v.Mints[name])
				if a, _ := AssociatedTokenAddress(p.Treasury, mint); a.String() != c.TreasuryAta[name] {
					t.Fatalf("%s treasury ata %s", name, a)
				}
				want := c.RecipientAtas[name]
				if len(want) != len(p.Recipients) {
					t.Fatalf("%s: %d recipient atas for %d recipients", name, len(want), len(p.Recipients))
				}
				for i, r := range p.Recipients {
					if a, _ := AssociatedTokenAddress(r.Wallet, mint); a.String() != want[i] {
						t.Fatalf("%s recipient %d ata %s, want %s", name, i, a, want[i])
					}
				}
			}
		})
	}
	for _, n := range []int{1, 2, MaxRecipients} {
		if !counts[n] {
			t.Errorf("no vector with %d recipients", n)
		}
	}
	for _, n := range []int{1, 2, MaxMints} {
		if !mintCounts[n] {
			t.Errorf("no vector with %d mints", n)
		}
	}
}

func TestInvalidParamsVectors(t *testing.T) {
	v := loadVectors(t)
	program := MustParse(v.ProgramID)
	errs := map[string]error{
		"FeeBpsTooHigh":      fee.ErrFeeBpsCap,
		"FixedFeeTooHigh":    fee.ErrFixedFeeCap,
		"ZeroAddress":        ErrZeroAddress,
		"RecipientCount":     ErrRecipientCount,
		"DuplicateRecipient": ErrDuplicateRecipient,
		"ZeroShare":          ErrZeroShare,
		"SharesNotWhole":     ErrSharesNotWhole,
		"MintCount":          ErrMintCount,
		"MintsNotAscending":  ErrMintsNotAscending,
	}
	if len(v.InvalidParams) == 0 {
		t.Fatal("no invalid params vectors")
	}
	for _, c := range v.InvalidParams {
		t.Run(c.Name, func(t *testing.T) {
			want, ok := errs[c.Error]
			if !ok {
				t.Fatalf("unknown error %q", c.Error)
			}
			p := c.params()
			if got := hex.EncodeToString(p.Encode()); got != c.ParamsEncoded {
				t.Fatalf("encoded %s, want %s", got, c.ParamsEncoded)
			}
			if _, _, err := ForwarderAddress(program, [32]byte{}, p); !errors.Is(err, want) {
				t.Fatalf("got %v, want %v", err, want)
			}
			if err := p.Validate(); !errors.Is(err, want) {
				t.Fatalf("validate: got %v, want %v", err, want)
			}
		})
	}
}

func TestCurveVectors(t *testing.T) {
	v := loadVectors(t)
	on := 0
	for _, c := range v.CurvePoints {
		b, err := hex.DecodeString(c.Bytes)
		if err != nil || len(b) != 32 {
			t.Fatalf("bytes %q", c.Bytes)
		}
		if got := IsOnCurve(PublicKey(b)); got != c.OnCurve {
			t.Fatalf("%s: got %v, want %v", c.Bytes, got, c.OnCurve)
		}
		if c.OnCurve {
			on++
		}
	}
	if len(v.CurvePoints) < 32 || on == 0 || on == len(v.CurvePoints) {
		t.Fatalf("%d points, %d on curve: not a useful mix", len(v.CurvePoints), on)
	}
}

func TestEd25519KeysAreOnCurve(t *testing.T) {
	for i := 0; i < 500; i++ {
		pub, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		if !IsOnCurve(PublicKey(pub)) {
			t.Fatalf("generated key %x reported off curve", pub)
		}
	}
	// Well-known wallet-style keys: the system program id (all zeros) is y = 0, on curve.
	if !IsOnCurve(PublicKey{}) {
		t.Fatal("zero key is a valid point")
	}
}

func TestRandomBytesAreOftenOffCurve(t *testing.T) {
	off := 0
	for i := 0; i < 2000; i++ {
		var k PublicKey
		if _, err := rand.Read(k[:]); err != nil {
			t.Fatal(err)
		}
		if !IsOnCurve(k) {
			off++
		}
	}
	// About half of all 32-byte strings decode to a point.
	if off < 800 || off > 1200 {
		t.Fatalf("%d of 2000 off curve", off)
	}
}

func TestForwarderAddressRejectsInvalidParams(t *testing.T) {
	k := MustParse("EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v")
	k2 := MustParse("Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB")
	one := []Recipient{{Wallet: k, Bps: BpsTotal}}
	usdc := []PublicKey{k}
	good := Params{Treasury: k, RentReceiver: k, Fee: fee.Params{FeeBps: 300, FixedFeeMicros: 5_000_000}, Recipients: one, Mints: usdc}
	if _, _, err := ForwarderAddress(k, [32]byte{}, good); err != nil {
		t.Fatal(err)
	}
	split := func(rs ...Recipient) Params { return Params{Treasury: k, RentReceiver: k, Recipients: rs, Mints: usdc} }
	mints := func(ms ...PublicKey) Params { p := good; p.Mints = ms; return p }
	var low, high PublicKey
	low[0], high[0] = 1, 2
	cases := []struct {
		p    Params
		want error
	}{
		{Params{RentReceiver: k, Recipients: one, Mints: usdc}, ErrZeroAddress},
		{Params{Treasury: k, Recipients: one, Mints: usdc}, ErrZeroAddress},
		{Params{Treasury: k, RentReceiver: k, Fee: fee.Params{FeeBps: 301}, Recipients: one}, fee.ErrFeeBpsCap},
		{Params{Treasury: k, RentReceiver: k, Fee: fee.Params{FixedFeeMicros: 5_000_001}, Recipients: one}, fee.ErrFixedFeeCap},
		{split(), ErrRecipientCount},
		{split(make([]Recipient, MaxRecipients+1)...), ErrRecipientCount},
		{split(Recipient{Wallet: k, Bps: 5_000}, Recipient{Bps: 5_000}), ErrZeroAddress},
		{split(Recipient{Wallet: k, Bps: 5_000}, Recipient{Wallet: k, Bps: 5_000}), ErrDuplicateRecipient},
		{split(Recipient{Wallet: k, Bps: BpsTotal}, Recipient{Wallet: k2}), ErrZeroShare},
		{split(Recipient{Wallet: k, Bps: 5_000}, Recipient{Wallet: k2, Bps: 4_999}), ErrSharesNotWhole},
		// 60000 + 15536 wraps a uint16 to exactly 10000.
		{split(Recipient{Wallet: k, Bps: 60_000}, Recipient{Wallet: k2, Bps: 15_536}), ErrSharesNotWhole},
		{mints(), ErrMintCount},
		{mints(low, high, k, k2, PublicKey{0xff}), ErrMintCount},
		{mints(PublicKey{}), ErrZeroAddress},
		{mints(PublicKey{}, low), ErrZeroAddress},
		{mints(high, low), ErrMintsNotAscending},
		{mints(low, low), ErrMintsNotAscending},
		// Byte order: the pair differs only in the last byte.
		{mints(PublicKey{9, 31: 9}, PublicKey{9, 31: 8}), ErrMintsNotAscending},
		// Recipient errors come before mint errors, as on-chain.
		{Params{Treasury: k, RentReceiver: k}, ErrRecipientCount},
	}
	for _, c := range cases {
		if _, _, err := ForwarderAddress(k, [32]byte{}, c.p); !errors.Is(err, c.want) {
			t.Fatalf("%+v: got %v, want %v", c.p, err, c.want)
		}
	}
}

func TestEncodeLayout(t *testing.T) {
	var a, b, tr, rr PublicKey
	for i := range a {
		a[i], b[i], tr[i], rr[i] = 10, 11, 2, 3
	}
	m1, m2 := PublicKey{0x21}, PublicKey{0x22}
	p := Params{Treasury: tr, RentReceiver: rr, Fee: fee.Params{FeeBps: 0x0102, FixedFeeMicros: 0x0102030405060708},
		Recipients: []Recipient{{Wallet: a, Bps: 0x0A0B}, {Wallet: b, Bps: 0x0A0C}}, Mints: []PublicKey{m1, m2}}
	e := p.Encode()
	if len(e) != ParamsFixedLen+2*ParamsRecipientLen+2*ParamsMintLen || ParamsFixedLen != 82 {
		t.Fatalf("len %d", len(e))
	}
	if got, want := hex.EncodeToString(e[64:78]), "0201080706050403020102000000"; got != want {
		t.Fatalf("fixed tail %s, want %s", got, want)
	}
	if e[78] != 10 || hex.EncodeToString(e[110:112]) != "0b0a" || e[112] != 11 || hex.EncodeToString(e[144:146]) != "0c0a" {
		t.Fatalf("recipients %x", e[78:])
	}
	if hex.EncodeToString(e[146:150]) != "02000000" || e[150] != 0x21 || e[182] != 0x22 {
		t.Fatalf("mints %x", e[146:])
	}
	swapped := p
	swapped.Recipients = []Recipient{p.Recipients[1], p.Recipients[0]}
	if p.Hash() == swapped.Hash() || p.Equal(swapped) {
		t.Fatal("recipient order must be committed")
	}
	one := p
	one.Mints = []PublicKey{m1}
	if p.Hash() == one.Hash() || p.Equal(one) {
		t.Fatal("the bound mints must be committed")
	}
	if !p.Allows(m2) || one.Allows(m2) || p.Allows(a) {
		t.Fatal("Allows")
	}
}

func TestSortMints(t *testing.T) {
	usdc := MustParse("EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v")
	usdt := MustParse("Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB")
	in := []PublicKey{usdt, usdc}
	got := SortMints(in)
	if !slices.Equal(got, []PublicKey{usdc, usdt}) || in[0] != usdt {
		t.Fatalf("sorted %v, input changed: %v", got, in)
	}
	if err := ValidateMints(got); err != nil {
		t.Fatal(err)
	}
	// Sorting keeps duplicates, so validation still refuses them.
	if err := ValidateMints(SortMints([]PublicKey{usdc, usdc})); !errors.Is(err, ErrMintsNotAscending) {
		t.Fatal(err)
	}
}

func TestSeedLimits(t *testing.T) {
	k := MustParse("EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v")
	if _, err := CreateProgramAddress([][]byte{make([]byte, 33)}, k); !errors.Is(err, ErrSeeds) {
		t.Fatal(err)
	}
	if _, _, err := FindProgramAddress(make([][]byte, 16), k); !errors.Is(err, ErrSeeds) {
		t.Fatal(err)
	}
	if _, err := Parse("not-base58!"); !errors.Is(err, ErrInvalidKey) {
		t.Fatal(err)
	}
	if _, err := Parse("1111"); !errors.Is(err, ErrInvalidKey) {
		t.Fatal(err)
	}
}
