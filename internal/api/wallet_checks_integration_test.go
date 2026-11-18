//go:build integration

package api_test

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/destination"
	"github.com/prismaport/prismaport/internal/service"
)

// rpc is the on-chain layer a wallet check consults: it can be down (no or failing providers)
// or report that a Solana wallet has no token account yet.
type rpc struct {
	down       atomic.Bool
	missingATA atomic.Bool
	tokenAcct  atomic.Bool
}

func (r *rpc) Inspect(context.Context, bool, string) (destination.SolanaAccountKind, error) {
	if r.down.Load() {
		return destination.SolanaUnknown, destination.ErrUnverifiable
	}
	if r.tokenAcct.Load() {
		return destination.SolanaTokenAccount, nil
	}
	return destination.SolanaWallet, nil
}

func (r *rpc) MissingTokenAccounts(_ context.Context, _ bool, w string) ([]destination.MissingTokenAccount, error) {
	if r.down.Load() {
		return nil, destination.ErrUnverifiable
	}
	if r.missingATA.Load() && w != apptest.SolanaDestination {
		return []destination.MissingTokenAccount{{Chain: "solana-devnet", Asset: "USDC", Mint: "4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU", Account: "x"}}, nil
	}
	return nil, nil
}

func (r *rpc) HasCode(context.Context, string, string) (bool, error) {
	if r.down.Load() {
		return false, destination.ErrUnverifiable
	}
	return false, nil
}

const otherSolana = "DEb5yphxEaPc5BN118svVN4R3GFu9jKs31Gcv5yekjZx"

func wallet(t *testing.T, r apptest.Response) (int, string, bool) {
	t.Helper()
	if r.Status != http.StatusOK {
		_, code, _ := errorOf(t, r)
		return r.Status, code, false
	}
	return r.Status, "", r.JSON(t)["verified"].(bool)
}

// In Test mode a wallet whose on-chain checks cannot confirm it is kept with verified=false;
// Live mode keeps failing closed.
func TestTestModeWalletsWithoutTheChecks(t *testing.T) {
	t.Parallel()
	net := &rpc{}
	app, _ := dashAppWith(t, apptest.Options{SolanaInspector: net, EVMCode: net}, nil)
	c := login(t, app, "owner@example.com")
	dash := func(mode, family, address string) apptest.Response {
		return c.do("POST", "/v1/dashboard/wallets?mode="+mode, map[string]any{"family": family, "address": address})
	}

	// No providers (or failing ones).
	net.down.Store(true)
	for name, r := range map[string]apptest.Response{
		"dashboard EVM":    dash("test", "evm", evmAddr(1)),
		"dashboard Solana": dash("test", "solana", otherSolana),
	} {
		t.Run(name, func(t *testing.T) {
			if status, code, verified := wallet(t, r); status != http.StatusOK || verified {
				t.Errorf("test %s without the checks: %d %s verified=%v", name, status, code, verified)
			}
		})
	}
	for name, r := range map[string]apptest.Response{
		"dashboard EVM":    dash("live", "evm", evmAddr(3)),
		"dashboard Solana": dash("live", "solana", otherSolana),
	} {
		t.Run(name, func(t *testing.T) {
			if status, code, _ := wallet(t, r); status != http.StatusServiceUnavailable || code != "service_unavailable" {
				t.Errorf("live %s without the checks: %d %s", name, status, code)
			}
		})
	}
	// Definite refusals apply in Test mode too.
	if status, code, _ := wallet(t, dash("test", "evm", "0x036CbD53842c5426634e7929541eC2318f3dCF7e")); status != http.StatusBadRequest || code != "invalid_request" {
		t.Fatalf("the test USDC contract as a wallet: %d %s", status, code)
	}
	if status, _, _ := wallet(t, dash("test", "evm", "0x5AAeb6053F3E94C9b9A09f33669435E7Ef1BeAed")); status != http.StatusBadRequest {
		t.Fatalf("bad checksum: %d", status)
	}

	// The providers answer; the Solana wallet never received the test token.
	net.down.Store(false)
	net.missingATA.Store(true)
	if status, code, verified := wallet(t, dash("test", "solana", "9xQeWvG816bUx9EPjHmaT23yvVM2ZWbrrpZb9PusVFin")); status != http.StatusOK || verified {
		t.Fatalf("test Solana wallet without a token account: %d %s verified=%v", status, code, verified)
	}
	r := dash("live", "solana", "9xQeWvG816bUx9EPjHmaT23yvVM2ZWbrrpZb9PusVFin")
	if status, _, msg := errorOf(t, r); status != http.StatusBadRequest || !strings.Contains(msg, "no token account") {
		t.Fatalf("live Solana wallet without a token account: %d %s", status, msg)
	}
	net.missingATA.Store(false)
	net.tokenAcct.Store(true)
	if status, _, msg := errorOf(t, dash("test", "solana", "7vzXgKSkR2tdyMXYTTcVUb6mZoKk6VWNGiuSWxRkgajg")); status != http.StatusBadRequest || !strings.Contains(msg, "token account, not a wallet") {
		t.Fatalf("a token account in test mode: %d %s", status, msg)
	}
	net.tokenAcct.Store(false)

	// The issuer blacklist cannot be read: Test mode keeps the wallet unverified, a listed
	// address is refused in both modes.
	app.Blacklist.Down(true)
	if status, code, verified := wallet(t, dash("test", "evm", evmAddr(8))); status != http.StatusOK || verified {
		t.Fatalf("test wallet with the blacklist down: %d %s %v", status, code, verified)
	}
	if status, code, _ := wallet(t, dash("live", "evm", evmAddr(9))); status != http.StatusServiceUnavailable {
		t.Fatalf("live wallet with the blacklist down: %d %s", status, code)
	}
	app.Blacklist.Down(false)
	app.Blacklist.List(evmAddr(10), true)
	if status, code, _ := wallet(t, dash("test", "evm", evmAddr(10))); status != http.StatusBadRequest || code != service.CodeWalletBlacklisted {
		t.Fatalf("blacklisted test wallet: %d %s", status, code)
	}
	// Checks that run mark the wallet verified.
	if status, code, verified := wallet(t, dash("test", "evm", evmAddr(11))); status != http.StatusOK || !verified {
		t.Fatalf("checked test wallet: %d %s %v", status, code, verified)
	}
}

func TestSampleTestWallet(t *testing.T) {
	t.Parallel()
	net := &rpc{}
	app, _ := dashAppWith(t, apptest.Options{SolanaInspector: net, EVMCode: net}, nil)
	// A new self-serve merchant, as onboarding sees it.
	signup(t, newClient(t, app), "Sample shop", "sample@example.com", ownerPassword)
	anon := newClient(t, app)
	anon.send("POST", "/v1/dashboard/auth/verify-email", map[string]any{"email": "sample@example.com",
		"code": lastCode(t, app, "sample@example.com", "verify_email"), "password": ownerPassword})
	c := login(t, app, "sample@example.com")
	// Test mode, so no confirmation code (D58).
	net.down.Store(true)
	w := c.send("POST", "/v1/dashboard/wallets/sample?mode=test", map[string]any{})
	if w["address"] != service.SampleWalletAddress(chain.EVM) || w["label"] != service.SampleWalletLabel || w["verified"] != false || w["family"] != "evm" {
		t.Fatalf("sample wallet: %v", w)
	}
	again := c.send("POST", "/v1/dashboard/wallets/sample?mode=test", map[string]any{"family": "evm"})
	if again["id"] != w["id"] {
		t.Fatalf("a second sample wallet: %v", again)
	}
	sol := c.send("POST", "/v1/dashboard/wallets/sample?mode=test", map[string]any{"family": "solana"})
	if sol["address"] != service.SampleWalletAddress(chain.Solana) || sol["verified"] != false {
		t.Fatalf("Solana sample wallet: %v", sol)
	}
	// They are the first wallets, so a product created now pays them by default.
	product := dashProduct(t, c, "test", "sample-link", map[string]any{"name": "Try it", "price_type": "fixed", "amount": "25.00"})
	if payoutOf(product, "base-sepolia") != w["id"].(string)+":100.00" || payoutOf(product, "solana-devnet") != sol["id"].(string)+":100.00" {
		t.Fatalf("a first product pays the sample wallets: %v", product["payout"])
	}
	link := dashLink(t, c, "test", "sample-link", map[string]any{"product_id": product["id"]})
	if link["id"] == nil {
		t.Fatalf("link: %v", link)
	}
	ses := openLink(app, link["id"].(string), "sample-open", nil)
	if ses.Status != http.StatusOK || len(ses.JSON(t)["pay_to"].([]any)) != 2 {
		t.Fatalf("session paying the sample wallets: %d %s", ses.Status, ses.Body)
	}
	// Never in live mode, whatever the merchant.
	if r := c.do("POST", "/v1/dashboard/wallets/sample?mode=live", map[string]any{}); r.Status != http.StatusForbidden {
		t.Fatalf("sample in live mode under review: %d %s", r.Status, r.Body)
	}
	owner := login(t, app, "owner@example.com")
	if r := owner.do("POST", "/v1/dashboard/wallets/sample?mode=live", map[string]any{}); r.Status != http.StatusForbidden ||
		r.ErrorCode(t) != service.CodeTestModeOnly {
		t.Fatalf("sample in live mode: %d %s", r.Status, r.Body)
	}
	if r := owner.do("POST", "/v1/dashboard/wallets/sample?mode=test", map[string]any{"family": "tron"}); r.Status != http.StatusBadRequest {
		t.Fatalf("unknown family: %d %s", r.Status, r.Body)
	}
	if !hasAll(auditActions(t, app, merchantOf(t, app, "sample@example.com")), "wallet.created", "wallet.created", "product.created", "payment_link.created") {
		t.Fatalf("audit: %v", auditActions(t, app, merchantOf(t, app, "sample@example.com")))
	}
}
