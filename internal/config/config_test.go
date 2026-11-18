package config

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/chainwatch"
	"github.com/prismaport/prismaport/internal/solrpc"
)

func TestLoad(t *testing.T) {
	t.Setenv("PRISMAPORT_DATABASE_URL", "")
	if _, err := Load(); err == nil {
		t.Fatal("missing database url accepted")
	}
	t.Setenv("PRISMAPORT_DATABASE_URL", "postgres://x")
	t.Setenv("PRISMAPORT_TREASURY_EVM_LIVE", "0xfb6916095ca1df60bb79ce92ce3ea74c37c5d359")
	e, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if e.Treasury[true][chain.EVM] != "0xfB6916095ca1df60bB79Ce92cE3Ea74c37c5d359" {
		t.Fatalf("treasury not canonicalized: %v", e.Treasury)
	}
	if _, ok := e.Treasury[false][chain.EVM]; ok {
		t.Fatal("unset treasury present")
	}
	t.Setenv("PRISMAPORT_TREASURY_SOLANA_TEST", "not-base58-0OIl")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "treasury solana") {
		t.Fatalf("bad treasury accepted: %v", err)
	}
	t.Setenv("PRISMAPORT_TREASURY_SOLANA_TEST", "")
	t.Setenv("PRISMAPORT_TREASURY_EVM_TEST", "0x0000000000000000000000000000000000000000")
	if _, err := Load(); err == nil {
		t.Fatal("zero treasury accepted")
	}
}

func TestLiveProtocolGaps(t *testing.T) {
	t.Setenv("PRISMAPORT_DATABASE_URL", "postgres://x")
	for _, k := range []string{"PRISMAPORT_TREASURY_EVM_LIVE", "PRISMAPORT_TREASURY_SOLANA_LIVE", "PRISMAPORT_CHAIN_BASE_FACTORY",
		"PRISMAPORT_CHAIN_BASE_GATEWAY", "PRISMAPORT_CHAIN_SOLANA_PROGRAM_ID", "PRISMAPORT_CHAIN_SOLANA_RENT_RECEIVER"} {
		t.Setenv(k, "")
	}
	// Only the test treasury: the live denylist is empty.
	t.Setenv("PRISMAPORT_TREASURY_EVM_TEST", "0xfb6916095ca1df60bb79ce92ce3ea74c37c5d359")
	e, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(e.LiveProtocolGaps(chain.EVM), " "); got != "PRISMAPORT_TREASURY_EVM_LIVE PRISMAPORT_CHAIN_BASE_FACTORY PRISMAPORT_CHAIN_BASE_GATEWAY" {
		t.Fatalf("evm gaps: %s", got)
	}
	if got := strings.Join(e.LiveProtocolGaps(chain.Solana), " "); got != "PRISMAPORT_TREASURY_SOLANA_LIVE PRISMAPORT_CHAIN_SOLANA_PROGRAM_ID PRISMAPORT_CHAIN_SOLANA_RENT_RECEIVER" {
		t.Fatalf("solana gaps: %s", got)
	}
	t.Setenv("PRISMAPORT_TREASURY_EVM_LIVE", "0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed")
	t.Setenv("PRISMAPORT_CHAIN_BASE_FACTORY", "0xEBC1282Dc447787Db5AaC24c0D23Aa7f25b44083")
	if e, err = Load(); err != nil {
		t.Fatal(err)
	}
	if got := e.LiveProtocolGaps(chain.EVM); len(got) != 1 || got[0] != "PRISMAPORT_CHAIN_BASE_GATEWAY" {
		t.Fatalf("evm gaps without the gateway: %v", got)
	}
	t.Setenv("PRISMAPORT_CHAIN_BASE_GATEWAY", "0x8ba1f109551bD432803012645Ac136ddd64DBA72")
	t.Setenv("PRISMAPORT_TREASURY_SOLANA_LIVE", renter)
	t.Setenv("PRISMAPORT_CHAIN_SOLANA_PROGRAM_ID", program)
	t.Setenv("PRISMAPORT_CHAIN_SOLANA_RENT_RECEIVER", renter)
	if e, err = Load(); err != nil {
		t.Fatal(err)
	}
	if gaps := append(e.LiveProtocolGaps(chain.EVM), e.LiveProtocolGaps(chain.Solana)...); len(gaps) != 0 {
		t.Fatalf("complete live config reports gaps: %v", gaps)
	}
}

func TestLoadChains(t *testing.T) {
	t.Setenv("PRISMAPORT_DATABASE_URL", "postgres://x")
	t.Setenv("PRISMAPORT_CHAIN_BASE_FACTORY", "0xEBC1282Dc447787Db5AaC24c0D23Aa7f25b44083")
	t.Setenv("PRISMAPORT_CHAIN_BASE_RPC_URLS", "https://a.example, https://b.example")
	t.Setenv("PRISMAPORT_CHAIN_BASE_CONFIRMATIONS", "12")
	t.Setenv("PRISMAPORT_CHAIN_BASE_FINALITY_DELAY", "20m")
	t.Setenv("PRISMAPORT_CHAIN_BASE_MULTICALL3", "0x000000000000000000000000000000000000ca11")
	t.Setenv("PRISMAPORT_CHAIN_BASE_MAX_FEE_WEI", "1000000000")
	t.Setenv("PRISMAPORT_CHAIN_BASE_SCAN_HOT", "500ms")
	t.Setenv("PRISMAPORT_CHAIN_BASE_MAX_LOG_RANGE", "10")
	e, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	c, ok := e.Chains["base"]
	if !ok || !c.HasRPC() || c.Factory == nil || c.Gateway != nil || c.UseSafeHead || c.Confirmations != 12 ||
		c.FinalityDelay != 20*time.Minute || c.Multicall3 == nil || c.Multicall3.Lower() != "0x000000000000000000000000000000000000ca11" ||
		c.MaxFeePerGas.String() != "1000000000" || c.Scan.Hot != 500*time.Millisecond ||
		c.MaxLogRange != 10 {
		t.Fatalf("parsed %+v", c)
	}
	if _, ok := e.Chains["base-sepolia"]; ok {
		t.Fatal("unconfigured chain present")
	}

	for name, val := range map[string]string{
		"PRISMAPORT_CHAIN_BASE_RPC_URLS":       "https://only-one.example",
		"PRISMAPORT_CHAIN_BASE_FACTORY":        "0x0000000000000000000000000000000000000000",
		"PRISMAPORT_CHAIN_BASE_GATEWAY":        "0xebc1282dc447787db5aac24c0d23aa7f25b4408",
		"PRISMAPORT_CHAIN_BASE_CONFIRMATIONS":  "0",
		"PRISMAPORT_CHAIN_BASE_FINALITY_DELAY": "0s",
		"PRISMAPORT_CHAIN_BASE_MULTICALL3":     "0xca11",
		"PRISMAPORT_CHAIN_BASE_MAX_FEE_WEI":    "1.5",
		"PRISMAPORT_CHAIN_BASE_SCAN_HOT":       "x",
	} {
		old := os.Getenv(name)
		t.Setenv(name, val)
		if _, err := Load(); err == nil {
			t.Fatalf("%s=%q accepted", name, val)
		}
		t.Setenv(name, old)
	}
	t.Setenv("PRISMAPORT_CHAIN_BASE_CONFIRMATIONS", "safe")
	e, err = Load()
	if err != nil || !e.Chains["base"].UseSafeHead {
		t.Fatalf("safe policy: %v", err)
	}
}

// MAX_LOG_RANGE is optional (zero keeps the detector's and sender's defaults) and, when set,
// must be a positive integer.
func TestMaxLogRange(t *testing.T) {
	t.Setenv("PRISMAPORT_DATABASE_URL", "postgres://x")
	t.Setenv("PRISMAPORT_CHAIN_BASE_SEPOLIA_RPC_URLS", "https://a.example,https://b.example")
	const key = "PRISMAPORT_CHAIN_BASE_SEPOLIA_MAX_LOG_RANGE"
	t.Setenv(key, "")
	e, err := Load()
	if err != nil || e.Chains["base-sepolia"].MaxLogRange != 0 {
		t.Fatalf("unset: %v %+v", err, e.Chains["base-sepolia"])
	}
	for v, want := range map[string]uint64{"1": 1, "10": 10, "200": 200, "500": 500} {
		t.Setenv(key, v)
		e, err := Load()
		if err != nil || e.Chains["base-sepolia"].MaxLogRange != want {
			t.Fatalf("%s=%q: %v %d", key, v, err, e.Chains["base-sepolia"].MaxLogRange)
		}
	}
	for _, v := range []string{"0", "-1", "-10", "x", "1.5", "10 blocks", "1e3", "18446744073709551616"} {
		t.Setenv(key, v)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "MAX_LOG_RANGE must be a positive integer") {
			t.Fatalf("%s=%q: %v", key, v, err)
		}
	}
}

func TestGasKeyNeverInEnv(t *testing.T) {
	t.Setenv("PRISMAPORT_GAS_KEY", "0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80")
	e := Env{}
	k, err := e.GasKey()
	if err != nil || k.Address().Hex() != "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266" {
		t.Fatalf("gas key: %v", err)
	}
	if strings.Contains(fmt.Sprintf("%+v %v", e, k), "ac0974") {
		t.Fatal("key material reachable through formatting")
	}
}

// Payments count after N blocks on EVM and at confirmed on Solana unless the configuration asks
// for the slower safe/finalized policy.
func TestCreditDefaults(t *testing.T) {
	t.Setenv("PRISMAPORT_DATABASE_URL", "postgres://x")
	t.Setenv("PRISMAPORT_CHAIN_BASE_RPC_URLS", "https://a.example,https://b.example")
	t.Setenv("PRISMAPORT_CHAIN_BASE_SEPOLIA_RPC_URLS", "https://a.example,https://b.example")
	t.Setenv("PRISMAPORT_CHAIN_SOLANA_RPC_URLS", "https://a.example,https://b.example")
	e, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"base", "base-sepolia"} {
		if c := e.Chains[id]; c.UseSafeHead || c.Confirmations != chainwatch.DefaultConfirmations || chainwatch.DefaultConfirmations != 10 {
			t.Fatalf("%s default policy: safe=%v confirmations=%d", id, c.UseSafeHead, c.Confirmations)
		}
	}
	if c := e.Solana["solana"]; c.Credit != solrpc.Confirmed {
		t.Fatalf("solana default credit %q", c.Credit)
	}
	t.Setenv("PRISMAPORT_CHAIN_BASE_CONFIRMATIONS", "safe")
	t.Setenv("PRISMAPORT_CHAIN_BASE_SEPOLIA_CONFIRMATIONS", "3")
	t.Setenv("PRISMAPORT_CHAIN_SOLANA_CREDIT", "finalized")
	if e, err = Load(); err != nil {
		t.Fatal(err)
	}
	if c := e.Chains["base"]; !c.UseSafeHead || c.Confirmations != 0 {
		t.Fatalf("safe override: %+v", c)
	}
	if c := e.Chains["base-sepolia"]; c.UseSafeHead || c.Confirmations != 3 {
		t.Fatalf("per-chain override: %+v", c)
	}
	if c := e.Solana["solana"]; c.Credit != solrpc.Finalized {
		t.Fatalf("finalized override: %q", c.Credit)
	}
}

func TestLoadFrontend(t *testing.T) {
	t.Setenv("PRISMAPORT_DATABASE_URL", "postgres://x")
	for _, c := range []struct {
		val      string
		external bool
		bad      bool
	}{
		{"", false, false},
		{"embedded", false, false},
		{"external", true, false},
		{"External", false, true},
		{"pages", false, true},
	} {
		t.Setenv("PRISMAPORT_FRONTEND", c.val)
		e, err := Load()
		if c.bad {
			if err == nil || !strings.Contains(err.Error(), "PRISMAPORT_FRONTEND") {
				t.Errorf("%q: err = %v, want a PRISMAPORT_FRONTEND error", c.val, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%q: %v", c.val, err)
		}
		if e.ExternalFrontend != c.external {
			t.Errorf("%q: ExternalFrontend = %v, want %v", c.val, e.ExternalFrontend, c.external)
		}
	}
	t.Setenv("PRISMAPORT_FRONTEND", "external")
	t.Setenv("PRISMAPORT_DOCS_URL", "https://docs.prismaport.com")
	if e, err := Load(); err != nil || e.DocsURL != "https://docs.prismaport.com" {
		t.Errorf("PRISMAPORT_DOCS_URL: %q %v", e.DocsURL, err)
	}
}

func TestPasswordBreachCheck(t *testing.T) {
	t.Setenv("PRISMAPORT_DATABASE_URL", "postgres://x")
	t.Setenv("PRISMAPORT_PASSWORD_BREACH_CHECK", "")
	e, err := Load()
	if err != nil || !e.PasswordBreachCheck {
		t.Fatalf("default: %v %v", e.PasswordBreachCheck, err)
	}
	t.Setenv("PRISMAPORT_PASSWORD_BREACH_CHECK", "false")
	if e, err = Load(); err != nil || e.PasswordBreachCheck {
		t.Fatalf("off: %v %v", e.PasswordBreachCheck, err)
	}
}
