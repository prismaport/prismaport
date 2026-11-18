package derive

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/fee"
	"github.com/prismaport/prismaport/internal/payout"
	"github.com/prismaport/prismaport/internal/solanaderive"
)

type solVectors struct {
	ProgramID string `json:"programId"`
	Vectors   []struct {
		Name, Treasury, RentReceiver, Salt, Pda string
		FeeBps                                  uint16 `json:"feeBps"`
		FixedFeeMicros                          uint64 `json:"fixedFeeMicros"`
		Recipients                              []struct {
			Wallet string `json:"wallet"`
			Bps    uint16 `json:"bps"`
		} `json:"recipients"`
		Mints []string `json:"mints"`
	} `json:"vectors"`
	InvalidParams []struct {
		Name, Treasury, RentReceiver, Error string
		FeeBps                              uint16 `json:"feeBps"`
		FixedFeeMicros                      uint64 `json:"fixedFeeMicros"`
		Recipients                          []struct {
			Wallet string `json:"wallet"`
			Bps    uint16 `json:"bps"`
		} `json:"recipients"`
		Mints []string `json:"mints"`
	} `json:"invalidParams"`
}

func TestSolanaForwardersMatchVectors(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "solana", "vectors", "solana-pda.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v solVectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	d := SolanaForwarders{Program: solanaderive.MustParse(v.ProgramID)}
	sol, _ := chain.Pilot().Get("solana")
	counts := map[int]int{}
	for _, c := range v.Vectors {
		counts[len(c.Recipients)]++
		t.Run(c.Name, func(t *testing.T) {
			sb, _ := hex.DecodeString(c.Salt)
			p := Params{Treasury: c.Treasury, RentReceiver: c.RentReceiver, Fee: fee.Params{FeeBps: c.FeeBps, FixedFeeMicros: c.FixedFeeMicros},
				Tokens: c.Mints}
			for _, r := range c.Recipients {
				p.Recipients = append(p.Recipients, payout.Recipient{Address: r.Wallet, Bps: r.Bps})
			}
			got, err := d.Address(sol, [32]byte(sb), p)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.Pda {
				t.Fatalf("pda %s, vector %s", got, c.Pda)
			}
		})
	}
	if counts[1] == 0 || counts[2] == 0 || counts[5] == 0 {
		t.Fatalf("the vectors must cover 1, 2 and 5 recipients: %v", counts)
	}
	errs := map[string]error{
		"FeeBpsTooHigh": fee.ErrFeeBpsCap, "FixedFeeTooHigh": fee.ErrFixedFeeCap, "ZeroAddress": solanaderive.ErrZeroAddress,
		"RecipientCount": solanaderive.ErrRecipientCount, "DuplicateRecipient": solanaderive.ErrDuplicateRecipient,
		"ZeroShare": solanaderive.ErrZeroShare, "SharesNotWhole": solanaderive.ErrSharesNotWhole,
		"MintCount": solanaderive.ErrMintCount, "MintsNotAscending": solanaderive.ErrMintsNotAscending,
	}
	if len(v.InvalidParams) == 0 {
		t.Fatal("no invalid params vectors")
	}
	for _, c := range v.InvalidParams {
		t.Run("invalid "+c.Name, func(t *testing.T) {
			want, ok := errs[c.Error]
			if !ok {
				t.Fatalf("unknown error %q", c.Error)
			}
			p := Params{Treasury: c.Treasury, RentReceiver: c.RentReceiver, Fee: fee.Params{FeeBps: c.FeeBps, FixedFeeMicros: c.FixedFeeMicros},
				Tokens: c.Mints}
			for _, r := range c.Recipients {
				p.Recipients = append(p.Recipients, payout.Recipient{Address: r.Wallet, Bps: r.Bps})
			}
			if _, err := d.Address(sol, [32]byte{}, p); !errors.Is(err, want) {
				t.Fatalf("got %v, want %s", err, c.Error)
			}
		})
	}
}

func TestSolanaForwardersFailClosed(t *testing.T) {
	d := SolanaForwarders{Program: solanaderive.MustParse("3DHLtkMYKFmxLbQRHkHcNPibfaKYJNntQEnJvUvcnPya")}
	sol, _ := chain.Pilot().Get("solana")
	base, _ := chain.Pilot().Get("base")
	p := Params{Treasury: "7vzEoA6qPLqGXe5rxmMK7iha63znnLfwGppBrUfELajg", Fee: fee.Params{FeeBps: 100, FixedFeeMicros: 100_000},
		Recipients: []payout.Recipient{{Address: "DEb5yphxEaPc5BN118svVN4R3GFu9jKs31Gcv5yekjZx", Bps: 10_000}},
		Tokens:     []string{"EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"}}
	if _, err := d.Address(sol, [32]byte{}, p); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("missing rent receiver: %v", err)
	}
	p.RentReceiver = "CDYn2if6T4voQoxm5mZKRAg5a65ZVzhQ5RK8H8e2DmS8"
	if _, err := d.Address(base, [32]byte{}, p); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("EVM chain: %v", err)
	}
	if _, err := (SolanaForwarders{}).Address(sol, [32]byte{}, p); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("no program: %v", err)
	}
	bad := p
	bad.Fee.FeeBps = 301
	if _, err := d.Address(sol, [32]byte{}, bad); err == nil {
		t.Fatal("fee above cap derived")
	}
	bad = p
	bad.Treasury = "0x00"
	if _, err := d.Address(sol, [32]byte{}, bad); err == nil {
		t.Fatal("bad treasury derived")
	}
	bad = p
	bad.Recipients = []payout.Recipient{{Address: p.Recipients[0].Address, Bps: 5000}, {Address: p.Recipients[0].Address, Bps: 5000}}
	if _, err := d.Address(sol, [32]byte{}, bad); !errors.Is(err, solanaderive.ErrDuplicateRecipient) {
		t.Fatalf("duplicate recipient: %v", err)
	}
	bad.Recipients = []payout.Recipient{{Address: p.Recipients[0].Address, Bps: 9000}}
	if _, err := d.Address(sol, [32]byte{}, bad); !errors.Is(err, solanaderive.ErrSharesNotWhole) {
		t.Fatalf("shares below 100%%: %v", err)
	}
	bad.Recipients = nil
	if _, err := d.Address(sol, [32]byte{}, bad); !errors.Is(err, solanaderive.ErrRecipientCount) {
		t.Fatalf("no recipients: %v", err)
	}
	bad = p
	bad.Tokens = []string{"Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB", "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"}
	if _, err := d.Address(sol, [32]byte{}, bad); !errors.Is(err, solanaderive.ErrMintsNotAscending) {
		t.Fatalf("unsorted mints: %v", err)
	}
	bad.Tokens = []string{"0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913"}
	if _, err := d.Address(sol, [32]byte{}, bad); !errors.Is(err, solanaderive.ErrInvalidKey) {
		t.Fatalf("EVM token as a mint: %v", err)
	}
	two := p
	two.Tokens = []string{"EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v", "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB"}
	if a, _ := d.Address(sol, [32]byte{}, p); a == "" {
		t.Fatal("one mint")
	} else if b, _ := d.Address(sol, [32]byte{}, two); b == a || b == "" {
		t.Fatal("the bound mints are not committed")
	}
	a, err := d.Address(sol, [32]byte{}, p)
	if err != nil {
		t.Fatal(err)
	}
	p.RentReceiver = "7vzEoA6qPLqGXe5rxmMK7iha63znnLfwGppBrUfELajg"
	if b, _ := d.Address(sol, [32]byte{}, p); b == a {
		t.Fatal("rent receiver is not committed")
	}
}

// The vectors' ATAs are only useful if they use the mints the registry credits; a wrong mint
// once slipped into the file unnoticed.
func TestSolanaVectorMintsMatchRegistry(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "solana", "vectors", "solana-pda.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		ProgramIDNote string            `json:"programIdNote"`
		Mints         map[string]string `json:"mints"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	reg := chain.Pilot()
	for key, want := range map[string]struct{ chain, asset string }{
		"usdc": {"solana", "USDC"}, "usdt": {"solana", "USDT"}, "usdc_devnet": {"solana-devnet", "USDC"},
	} {
		t.Run(key, func(t *testing.T) {
			c, _ := reg.Get(want.chain)
			a, ok := c.Asset(want.asset)
			if !ok || v.Mints[key] != a.Token {
				t.Errorf("vector mint %s = %q, registry %s %s = %q", key, v.Mints[key], want.chain, want.asset, a.Token)
			}
		})
	}
	if v.Mints["usdc"] != "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v" || v.Mints["usdt"] != "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB" {
		t.Fatalf("mainnet mints changed: %v", v.Mints)
	}
	if !strings.Contains(v.ProgramIDNote, "Placeholder") {
		t.Fatalf("program id must stay documented as a placeholder until the deploy key exists: %q", v.ProgramIDNote)
	}
}
