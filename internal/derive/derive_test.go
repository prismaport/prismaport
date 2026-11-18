package derive

import (
	"encoding/hex"
	"errors"
	"testing"

	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/destination"
	"github.com/prismaport/prismaport/internal/evm"
	"github.com/prismaport/prismaport/internal/fee"
	"github.com/prismaport/prismaport/internal/payout"
	"github.com/prismaport/prismaport/internal/solanaderive"
)

const (
	recipientA = "0x8ba1f109551bD432803012645Ac136ddd64DBA72"
	recipientB = "0xdbF03B407c01E7cD3CBea99509d93f8DDDC8C6FB"
	baseUSDC   = "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913"
	zeroAddr   = "0x0000000000000000000000000000000000000000"
)

var params = Params{
	Treasury:   "0xfB6916095ca1df60bB79Ce92cE3Ea74c37c5d359",
	Fee:        fee.Params{FeeBps: 100, FixedFeeMicros: 100_000},
	Recipients: []payout.Recipient{{Address: "0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed", Bps: 10_000}},
	Tokens:     []string{baseUSDC},
}

func withTokens(ts ...string) Params {
	p := params
	p.Tokens = ts
	return p
}

func withRecipients(rs ...payout.Recipient) Params {
	p := params
	p.Recipients = rs
	return p
}

func TestPackEVMArgs(t *testing.T) {
	got, err := PackEVMArgs(params)
	if err != nil {
		t.Fatal(err)
	}
	want := "fb6916095ca1df60bb79ce92ce3ea74c37c5d359" + "0064" + "00000000000186a0" + "01" + "01" +
		"5aaeb6053f3e94c9b9a09f33669435e7ef1beaed" + "2710" + "833589fcd6edb6e08f4c7c32d4f71b54bda02913"
	if hex.EncodeToString(got) != want {
		t.Fatalf("packed %x", got)
	}
	two, err := PackEVMArgs(withRecipients(payout.Recipient{Address: recipientA, Bps: 7000}, payout.Recipient{Address: recipientB, Bps: 3000}))
	if err != nil {
		t.Fatal(err)
	}
	if len(two) != 32+22*2+20 || two[30] != 2 || two[31] != 1 || hex.EncodeToString(two[52:54]) != "1b58" || hex.EncodeToString(two[74:76]) != "0bb8" {
		t.Fatalf("two recipients packed %x", two)
	}
	for name, c := range map[string]struct {
		p    Params
		want error
	}{
		"bad recipient":      {withRecipients(payout.Recipient{Address: "0x1", Bps: 10_000}), evm.ErrFormat},
		"zero recipient":     {withRecipients(payout.Recipient{Address: "0x0000000000000000000000000000000000000000", Bps: 10_000}), payout.ErrZeroRecipient},
		"duplicate":          {withRecipients(payout.Recipient{Address: recipientA, Bps: 5000}, payout.Recipient{Address: recipientA, Bps: 5000}), payout.ErrDuplicateRecipient},
		"no recipients":      {withRecipients(), payout.ErrCount},
		"zero share":         {withRecipients(payout.Recipient{Address: recipientA, Bps: 10_000}, payout.Recipient{Address: recipientB, Bps: 0}), payout.ErrZeroShare},
		"shares below 100%":  {withRecipients(payout.Recipient{Address: recipientA, Bps: 5000}, payout.Recipient{Address: recipientB, Bps: 4999}), payout.ErrSum},
		"fee above cap":      {Params{Treasury: params.Treasury, Recipients: params.Recipients, Fee: fee.Params{FeeBps: 301}}, fee.ErrFeeBpsCap},
		"zero treasury":      {Params{Treasury: zeroAddr, Recipients: params.Recipients}, ErrZeroAddress},
		"malformed treasury": {Params{Treasury: "0x12", Recipients: params.Recipients}, evm.ErrFormat},
		"no tokens":          {withTokens(), ErrTokenCount},
		"five tokens":        {withTokens(recipientA, recipientA, recipientA, recipientA, recipientA), ErrTokenCount},
		"zero token":         {withTokens(zeroAddr), ErrZeroToken},
		"malformed token":    {withTokens("0x12"), evm.ErrFormat},
		"unsorted tokens":    {withTokens(recipientA, baseUSDC), ErrTokensNotAscending},
		"duplicate token":    {withTokens(baseUSDC, baseUSDC), ErrTokensNotAscending},
		// Recipients are checked before tokens, as in Factory.encodeArgs.
		"recipients first": {Params{Treasury: params.Treasury, Fee: params.Fee}, payout.ErrCount},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := PackEVMArgs(c.p); !errors.Is(err, c.want) {
				t.Errorf("%s: got %v, want %v", name, err, c.want)
			}
		})
	}
	six := make([]payout.Recipient, 6)
	for i := range six {
		var a evm.Address
		a[19] = byte(i + 1)
		six[i] = payout.Recipient{Address: a.Hex(), Bps: 1000}
	}
	six[0].Bps = 5000
	if _, err := PackEVMArgs(withRecipients(six...)); !errors.Is(err, payout.ErrCount) {
		t.Fatalf("six recipients: %v", err)
	}
}

func TestTokenLists(t *testing.T) {
	r := chain.Pilot()
	base, _ := r.Get("base")
	sol, _ := r.Get("solana")
	got, err := BoundTokens(base, []string{"USDC", "USDC"})
	if err != nil || len(got) != 1 || got[0] != baseUSDC {
		t.Fatalf("base USDC: %v %v", got, err)
	}
	if _, err := BoundTokens(base, []string{"USDC", "DOGE"}); !errors.Is(err, ErrTokenNotRegistered) {
		t.Fatalf("unregistered asset: %v", err)
	}
	if _, err := BoundTokens(base, nil); !errors.Is(err, ErrTokenCount) {
		t.Fatalf("no assets: %v", err)
	}
	usdc, _ := sol.Asset("USDC")
	usdt, _ := sol.Asset("USDT")
	for _, order := range [][]string{{"USDT", "USDC"}, {"USDC", "USDT"}} {
		got, err := BoundTokens(sol, order)
		if err != nil || len(got) != 2 || got[0] != usdc.Token || got[1] != usdt.Token {
			t.Fatalf("%v: %v %v", order, got, err)
		}
	}
	// Byte order, not text order: "...AB" sorts before "...aa" as text (A < a) but after it as bytes.
	a, b := "0x00000000000000000000000000000000000000aa", "0x00000000000000000000000000000000000000AB"
	if s, err := SortTokens(chain.EVM, []string{b, a}); err != nil || s[0] != a || s[1] != b {
		t.Fatalf("sorted %v %v", s, err)
	}
	if _, err := SortTokens(chain.EVM, []string{"nope"}); err == nil {
		t.Fatal("malformed token sorted")
	}
	var tle *TokenListError
	if err := CheckTokens(chain.EVM, []string{recipientA, baseUSDC}); !errors.As(err, &tle) || tle.Index != 1 ||
		hex.EncodeToString(tle.RevertData()) != "23c0511c"+"0000000000000000000000000000000000000000000000000000000000000001" {
		t.Fatalf("unsorted: %v", err)
	}
	if err := CheckTokens(chain.Solana, []string{usdt.Token, usdc.Token}); !errors.Is(err, solanaderive.ErrMintsNotAscending) {
		t.Fatalf("solana unsorted: %v", err)
	}
	if err := CheckTokens(chain.Solana, []string{usdc.Token, usdt.Token}); err != nil {
		t.Fatal(err)
	}
}

func TestEVMCreate2UsesInitCodeHash(t *testing.T) {
	d := EVMCreate2{InitCode: func(args []byte) ([]byte, error) { return append([]byte{0xde, 0xad}, args...), nil }}
	base, _ := chain.Pilot().Get("base")
	var salt [32]byte
	a, err := d.Address(base, salt, params)
	if err != nil {
		t.Fatal(err)
	}
	args, _ := PackEVMArgs(params)
	var h [32]byte
	copy(h[:], evm.Keccak256(append([]byte{0xde, 0xad}, args...)))
	if want := evm.Create2Address(evm.Address{}, salt, h).Hex(); a != want {
		t.Fatalf("got %s want %s", a, want)
	}
	salt[31] = 1
	if b, _ := d.Address(base, salt, params); b == a {
		t.Fatal("salt does not change the address")
	}
	if _, err := (EVMCreate2{}).Address(base, salt, params); !errors.Is(err, ErrUnavailable) {
		t.Fatal("missing init code must be unavailable")
	}
}

func TestStub(t *testing.T) {
	r := chain.Pilot()
	var salt [32]byte
	salt[0] = 9
	for _, id := range []string{"base", "solana"} {
		c, _ := r.Get(id)
		if _, err := (Stub{}).Address(c, salt, params); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("stub produced a live %s address: %v", id, err)
		}
	}
	sepolia, _ := r.Get("base-sepolia")
	a1, err := Stub{}.Address(sepolia, salt, params)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := destination.ValidateEVMFormat(a1); err != nil {
		t.Fatalf("stub EVM address invalid: %v", err)
	}
	a2, _ := Stub{}.Address(sepolia, salt, params)
	if a1 != a2 {
		t.Fatal("stub not deterministic")
	}
	split := withRecipients(payout.Recipient{Address: recipientA, Bps: 7000}, payout.Recipient{Address: recipientB, Bps: 3000})
	swapped := withRecipients(payout.Recipient{Address: recipientB, Bps: 3000}, payout.Recipient{Address: recipientA, Bps: 7000})
	s1, _ := Stub{}.Address(sepolia, salt, split)
	s2, _ := Stub{}.Address(sepolia, salt, swapped)
	if s1 == s2 || s1 == a1 {
		t.Fatal("stub address does not commit to the recipients and their order")
	}
	if _, err := (Stub{}).Address(sepolia, salt, withRecipients()); !errors.Is(err, payout.ErrCount) {
		t.Fatalf("stub derived an address without recipients: %v", err)
	}
	devnet, _ := r.Get("solana-devnet")
	sp := Params{Treasury: "x", Fee: params.Fee, Tokens: []string{"4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU"},
		Recipients: []payout.Recipient{{Address: "9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM", Bps: 10_000}}}
	s, err := Stub{}.Address(devnet, salt, sp)
	if err != nil || destination.ValidateSolanaFormat(s) != nil {
		t.Fatalf("stub solana address %s: %v", s, err)
	}
	sp.Tokens = nil
	if _, err := (Stub{}).Address(devnet, salt, sp); !errors.Is(err, solanaderive.ErrMintCount) {
		t.Fatalf("stub derived a solana address without mints: %v", err)
	}
	if _, err := (Stub{}).Address(sepolia, salt, withTokens(recipientB, recipientA)); !errors.Is(err, ErrTokensNotAscending) {
		t.Fatalf("stub derived with unsorted tokens: %v", err)
	}
	if t2, _ := (Stub{}).Address(sepolia, salt, withTokens(recipientA, recipientB)); t2 == a1 {
		t.Fatal("stub address does not commit to the tokens")
	}
	bf := ByFamily{chain.EVM: Stub{}}
	if _, err := bf.Address(devnet, salt, params); !errors.Is(err, ErrUnavailable) {
		t.Fatal("missing family must be unavailable")
	}
}
