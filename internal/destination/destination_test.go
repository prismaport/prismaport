package destination

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/prismaport/prismaport/internal/base58"
	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/evm"
	"pgregory.net/rapid"
)

type fakeSolana map[string]SolanaAccountKind

func (f fakeSolana) Inspect(_ context.Context, _ bool, k string) (SolanaAccountKind, error) {
	return f[k], nil
}

func (fakeSolana) MissingTokenAccounts(context.Context, bool, string) ([]MissingTokenAccount, error) {
	return nil, nil
}

type tokenAccounts struct {
	missing []MissingTokenAccount
	err     error
}

func (tokenAccounts) Inspect(context.Context, bool, string) (SolanaAccountKind, error) {
	return SolanaWallet, nil
}

func (t tokenAccounts) MissingTokenAccounts(context.Context, bool, string) ([]MissingTokenAccount, error) {
	return t.missing, t.err
}

func TestSolanaWalletNeedsTokenAccounts(t *testing.T) {
	ctx := t.Context()
	usdt := MissingTokenAccount{Chain: "solana", Asset: "USDT", Mint: "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB", Account: "Ata1"}
	v := Validator{Registry: chain.Pilot(), Solana: tokenAccounts{missing: []MissingTokenAccount{usdt}}}
	_, err := v.Validate(ctx, chain.Solana, goodSolana, Options{Livemode: true})
	if !errors.Is(err, ErrTokenAccountMissing) || !strings.Contains(err.Error(), "USDT on solana") ||
		!strings.Contains(err.Error(), "Receive any amount") {
		t.Fatalf("missing ATA: %v", err)
	}
	if _, err := v.Validate(ctx, chain.Solana, goodSolana, Options{Livemode: true, AllowUnverified: true}); !errors.Is(err, ErrTokenAccountMissing) {
		t.Fatalf("a missing token account must not be overridable: %v", err)
	}
	v.Solana = tokenAccounts{}
	if _, err := v.Validate(ctx, chain.Solana, goodSolana, Options{Livemode: true}); err != nil {
		t.Fatalf("all token accounts present: %v", err)
	}
	v.Solana = tokenAccounts{err: errors.New("rpc down")}
	if _, err := v.Validate(ctx, chain.Solana, goodSolana, Options{Livemode: true}); !errors.Is(err, ErrUnverifiable) {
		t.Fatalf("lookup failure must fail closed: %v", err)
	}
	if got, err := v.Validate(ctx, chain.Solana, goodSolana, Options{Livemode: true, AllowUnverified: true}); err != nil || got != goodSolana {
		t.Fatalf("unverifiable with override: %s %v", got, err)
	}
}

type fakeCode map[string]bool

func (f fakeCode) HasCode(_ context.Context, chainID, _ string) (bool, error) { return f[chainID], nil }

const (
	goodEVM    = "0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed"
	goodSolana = "9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM"
)

func TestEVM(t *testing.T) {
	v := Validator{Registry: chain.Pilot(), EVMCode: fakeCode{}, ProtocolAddresses: map[chain.Family][]string{chain.EVM: {"0x52908400098527886E0F7030069857D2E4169EE7"}}}
	ctx := t.Context()
	got, err := v.Validate(ctx, chain.EVM, "0x5aaeb6053f3e94c9b9a09f33669435e7ef1beaed", Options{Livemode: true})
	if err != nil || got != goodEVM {
		t.Fatalf("lowercase not canonicalized: %s %v", got, err)
	}
	cases := map[string]error{
		"0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAeD": ErrChecksum,
		"0x0000000000000000000000000000000000000000": ErrZeroAddress,
		"0x833589fcd6edb6e08f4c7c32d4f71b54bda02913": ErrDenylisted,
		"0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48": ErrDenylisted,
		"0x52908400098527886e0f7030069857d2e4169ee7": ErrDenylisted,
		"0x123":    ErrFormat,
		goodSolana: ErrFormat,
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			if _, err := v.Validate(ctx, chain.EVM, in, Options{Livemode: true}); !errors.Is(err, want) {
				t.Errorf("%s: got %v want %v", in, err, want)
			}
		})
	}
}

func TestEVMCodeConsistency(t *testing.T) {
	r, err := chain.NewRegistry([]chain.Chain{
		{ID: "a", Family: chain.EVM, Livemode: true, Fee: chain.PilotFee()},
		{ID: "b", Family: chain.EVM, Livemode: true, Fee: chain.PilotFee()},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	for _, tc := range []struct {
		code fakeCode
		want error
	}{
		{fakeCode{}, nil},
		{fakeCode{"a": true, "b": true}, nil},
		{fakeCode{"a": true}, ErrCodeMismatch},
	} {
		v := Validator{Registry: r, EVMCode: tc.code}
		if _, err := v.Validate(ctx, chain.EVM, goodEVM, Options{Livemode: true}); !errors.Is(err, tc.want) {
			t.Errorf("%v: got %v want %v", tc.code, err, tc.want)
		}
	}
}

func TestFailsClosedWithoutRPC(t *testing.T) {
	v := Validator{Registry: chain.Pilot()}
	ctx := t.Context()
	if _, err := v.Validate(ctx, chain.EVM, goodEVM, Options{Livemode: true}); !errors.Is(err, ErrUnverifiable) {
		t.Fatalf("EVM without RPC: %v", err)
	}
	if _, err := v.Validate(ctx, chain.Solana, goodSolana, Options{Livemode: true}); !errors.Is(err, ErrUnverifiable) {
		t.Fatalf("Solana without RPC: %v", err)
	}
	if got, err := v.Validate(ctx, chain.Solana, goodSolana, Options{Livemode: true, AllowUnverified: true}); err != nil || got != goodSolana {
		t.Fatalf("override: %s %v", got, err)
	}
	if _, err := v.Validate(ctx, chain.EVM, "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913", Options{AllowUnverified: true}); !errors.Is(err, ErrDenylisted) {
		t.Fatalf("override must not skip the denylist: %v", err)
	}
}

func TestSolana(t *testing.T) {
	v := Validator{Registry: chain.Pilot(), Solana: fakeSolana{
		goodSolana: SolanaWallet,
		"4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDV": SolanaTokenAccount,
		"So11111111111111111111111111111111111111112":  SolanaMint,
		"TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA":  SolanaProgram,
	}}
	ctx := t.Context()
	cases := map[string]error{
		goodSolana: nil,
		"4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDV": ErrTokenAccount,
		"So11111111111111111111111111111111111111112":  ErrMint,
		"TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA":  ErrProgram,
		"EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v": ErrDenylisted,
		"11111111111111111111111111111111":             ErrZeroAddress,
		"9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWW0": ErrFormat,
		"9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAW":   ErrFormat,
		"1" + goodSolana: ErrFormat,
		goodEVM:          ErrFormat,
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			_, err := v.Validate(ctx, chain.Solana, in, Options{Livemode: true})
			if (want == nil && err != nil) || (want != nil && !errors.Is(err, want)) {
				t.Errorf("%s: got %v want %v", in, err, want)
			}
		})
	}
	if _, err := v.Validate(ctx, chain.Solana, "UnknownKindXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX", Options{}); err == nil {
		t.Error("unknown kind accepted")
	}
}

func TestPropertySolanaFormat(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		b := rapid.SliceOfN(rapid.Byte(), 32, 32).Draw(t, "key")
		s := base58.Encode(b)
		err := ValidateSolanaFormat(s)
		zero := true
		for _, x := range b {
			zero = zero && x == 0
		}
		if zero != errors.Is(err, ErrZeroAddress) || (!zero && err != nil) {
			t.Fatalf("%s: %v", s, err)
		}
		short := base58.Encode(b[:rapid.IntRange(1, 31).Draw(t, "n")])
		if ValidateSolanaFormat(short) == nil && len(short) > 0 {
			if d, _ := base58.Decode(short); len(d) != 32 {
				t.Fatalf("short key %s accepted", short)
			}
		}
	})
}

func TestPropertyEVMNonZeroAccepted(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		var a evm.Address
		copy(a[:], rapid.SliceOfN(rapid.Byte(), 20, 20).Draw(t, "a"))
		_, err := ValidateEVMFormat(a.Hex())
		if a.IsZero() != errors.Is(err, ErrZeroAddress) || (!a.IsZero() && err != nil) {
			t.Fatalf("%s: %v", a.Hex(), err)
		}
	})
}

type fakeBlacklist struct {
	listed map[string]bool
	none   map[string]bool
	err    error
	asked  []string
}

func (f *fakeBlacklist) IsBlacklisted(_ context.Context, chainID, token, address string) (bool, error) {
	f.asked = append(f.asked, chainID+"|"+token)
	if f.none[chainID] {
		return false, ErrNoProviders
	}
	return f.listed[address], f.err
}

func TestCheckBlacklist(t *testing.T) {
	ctx := t.Context()
	reg := chain.Pilot()
	bl := &fakeBlacklist{listed: map[string]bool{goodEVM: true}}
	v := Validator{Registry: reg, Blacklist: bl}
	if err := v.CheckBlacklist(ctx, true, chain.EVM, goodEVM); !errors.Is(err, ErrBlacklisted) || !strings.Contains(err.Error(), "USDC on base") {
		t.Fatalf("listed live address: %v", err)
	}
	if len(bl.asked) != 1 || bl.asked[0] != "base|0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913" {
		t.Fatalf("asked %v: only Base's FiatToken USDC is checked in live mode", bl.asked)
	}
	if err := v.CheckBlacklist(ctx, false, chain.EVM, goodEVM); !errors.Is(err, ErrBlacklisted) {
		t.Fatalf("listed test address: %v", err)
	}
	other := "0xfB6916095ca1df60bB79Ce92cE3Ea74c37c5d359"
	if err := v.CheckBlacklist(ctx, true, chain.EVM, other); err != nil {
		t.Fatalf("clean address: %v", err)
	}
	if err := v.CheckBlacklist(ctx, true, chain.Solana, goodSolana); err != nil {
		t.Fatalf("Solana has no issuer blacklist lookup: %v", err)
	}
	bl.none = map[string]bool{"base": true, "base-sepolia": true}
	if err := v.CheckBlacklist(ctx, true, chain.EVM, other); !errors.Is(err, ErrUnverifiable) {
		t.Fatalf("a live chain without providers must fail closed: %v", err)
	}
	if err := v.CheckBlacklist(ctx, false, chain.EVM, goodEVM); err != nil {
		t.Fatalf("a test chain without providers is skipped: %v", err)
	}
	bl.none, bl.err = nil, errors.New("rpc down")
	if err := v.CheckBlacklist(ctx, false, chain.EVM, other); !errors.Is(err, ErrUnverifiable) {
		t.Fatalf("a failing provider fails closed in test mode too: %v", err)
	}
	if err := (Validator{Registry: reg}).CheckBlacklist(ctx, true, chain.EVM, other); !errors.Is(err, ErrUnverifiable) {
		t.Fatalf("no checker at all must fail closed in live mode: %v", err)
	}
	if err := (Validator{Registry: reg}).CheckBlacklist(ctx, false, chain.EVM, other); err != nil {
		t.Fatalf("no checker in test mode: %v", err)
	}
}

// A Solana wallet without a token account is accepted only in test mode and only when asked;
// live mode ignores the option.
func TestMissingTokenAccountsAllowedInTestModeOnly(t *testing.T) {
	ctx := t.Context()
	usdc := MissingTokenAccount{Chain: "solana-devnet", Asset: "USDC", Mint: "4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU", Account: "Ata1"}
	v := Validator{Registry: chain.Pilot(), Solana: tokenAccounts{missing: []MissingTokenAccount{usdc}}}
	if got, err := v.Validate(ctx, chain.Solana, goodSolana, Options{AllowMissingTokenAccounts: true}); err != nil || got != goodSolana {
		t.Fatalf("test mode with the option: %s %v", got, err)
	}
	if _, err := v.Validate(ctx, chain.Solana, goodSolana, Options{}); !errors.Is(err, ErrTokenAccountMissing) {
		t.Fatalf("test mode without the option: %v", err)
	}
	if _, err := v.Validate(ctx, chain.Solana, goodSolana, Options{Livemode: true, AllowUnverified: true, AllowMissingTokenAccounts: true}); !errors.Is(err, ErrTokenAccountMissing) {
		t.Fatalf("live mode must ignore the option: %v", err)
	}
	v.Solana = fakeSolana{goodSolana: SolanaTokenAccount}
	if _, err := v.Validate(ctx, chain.Solana, goodSolana, Options{AllowUnverified: true, AllowMissingTokenAccounts: true}); !errors.Is(err, ErrTokenAccount) {
		t.Fatalf("a token account is never a wallet: %v", err)
	}
}

// ADR 0008 I-2: a token account the issuer froze is refused as frozen, in both modes and even with
// the test-mode allowance for missing accounts, since receiving tokens cannot fix it; binding the
// wallet into a split checks it again, and a lookup failure there is unverifiable.
func TestSolanaWalletWithAFrozenTokenAccount(t *testing.T) {
	ctx := t.Context()
	usdc := MissingTokenAccount{Chain: "solana", Asset: "USDC", Mint: "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v", Account: "Ata0"}
	usdt := MissingTokenAccount{Chain: "solana", Asset: "USDT", Mint: "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB", Account: "Ata1", Frozen: true}
	v := Validator{Registry: chain.Pilot(), Solana: tokenAccounts{missing: []MissingTokenAccount{usdc, usdt}}}
	for _, opt := range []Options{{Livemode: true}, {Livemode: true, AllowUnverified: true}, {AllowMissingTokenAccounts: true}} {
		_, err := v.Validate(ctx, chain.Solana, goodSolana, opt)
		var frozen *FrozenTokenAccountsError
		if !errors.Is(err, ErrTokenAccountFrozen) || errors.Is(err, ErrTokenAccountMissing) || !errors.As(err, &frozen) ||
			frozen.Assets() != "USDT on solana" || !strings.Contains(err.Error(), "Ata1") {
			t.Fatalf("%+v: %v", opt, err)
		}
	}
	if err := v.CheckFrozen(ctx, true, chain.Solana, goodSolana); !errors.Is(err, ErrTokenAccountFrozen) {
		t.Fatalf("check frozen: %v", err)
	}
	v.Solana = tokenAccounts{missing: []MissingTokenAccount{usdc}}
	if err := v.CheckFrozen(ctx, true, chain.Solana, goodSolana); err != nil {
		t.Fatalf("a missing (not frozen) account is not refused at binding: %v", err)
	}
	v.Solana = tokenAccounts{err: errors.New("rpc down")}
	if err := v.CheckFrozen(ctx, true, chain.Solana, goodSolana); !errors.Is(err, ErrUnverifiable) {
		t.Fatalf("lookup failure: %v", err)
	}
	if err := v.CheckFrozen(ctx, true, chain.EVM, goodEVM); err != nil {
		t.Fatalf("EVM: %v", err)
	}
}
