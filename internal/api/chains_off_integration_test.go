//go:build integration

package api_test

import (
	"net/http"
	"testing"

	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/chain"
)

// D62: a chain switched off in an environment (PRISMAPORT_CHAINS_OFF) is gone from the dashboard
// in both modes: Settings offers none of its stablecoins and no wallet of its family is added.
func TestChainsOff(t *testing.T) {
	t.Parallel()
	// Arrange
	app, _ := dashAppWith(t, apptest.Options{Registry: chain.Pilot().Without("solana", "solana-devnet")}, nil)
	c := login(t, app, "owner@example.com")

	// Act
	live := c.get("/v1/dashboard/settings?mode=live")["accepted_assets"].([]any)
	test := c.get("/v1/dashboard/settings?mode=test")["accepted_assets"].([]any)
	wallet := c.do("POST", "/v1/dashboard/wallets?mode=test", map[string]any{"family": "solana", "label": "Sol", "address": "9xQeWvG816bUx9EPjHmaT23yvVM2ZWbrrpZb9PusVFin"})

	// Assert
	for _, list := range [][]any{live, test} {
		if len(list) == 0 {
			t.Fatal("no stablecoin offered at all")
		}
		for _, a := range list {
			if ch := a.(map[string]any)["chain"].(string); ch != "base" && ch != "base-sepolia" {
				t.Errorf("%s offered while off", ch)
			}
		}
	}
	if wallet.Status != http.StatusBadRequest {
		t.Errorf("a Solana wallet while Solana is off: %d %s", wallet.Status, wallet.Body)
	}
}
