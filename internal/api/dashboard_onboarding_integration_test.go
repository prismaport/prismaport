//go:build integration

package api_test

import (
	"encoding/json"
	"slices"
	"testing"
)

type onboardingView struct {
	Visible    bool     `json:"visible"`
	FirstRun   bool     `json:"first_run"`
	HasWallet  bool     `json:"has_wallet"`
	HasProduct bool     `json:"has_product"`
	Step       string   `json:"step"`
	Done       int      `json:"done"`
	Steps      []string `json:"steps"`
	Networks   []struct {
		Chain     string `json:"chain"`
		Family    string `json:"family"`
		HasWallet bool   `json:"has_wallet"`
	} `json:"networks"`
}

func onboardingOf(t *testing.T, c *dashClient, mode string) onboardingView {
	t.Helper()
	r := c.do("GET", "/v1/dashboard/onboarding?mode="+mode, nil)
	var o onboardingView
	if r.Status != 200 || json.Unmarshal(r.Body, &o) != nil {
		t.Fatalf("onboarding %s: %d %s", mode, r.Status, r.Body)
	}
	return o
}

// The Get started state follows what the account holds, per mode (D57): "choose" while nothing
// exists anywhere, "wallets" once another mode has a wallet, "product" with a wallet here, and
// gone for good once the mode has a product; a product anywhere ends the first run.
func TestDashboardOnboarding(t *testing.T) {
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	// The seeded merchant has wallets in both modes; archive them all (no product pays them).
	if _, err := app.Store.Pool().Exec(t.Context(), `UPDATE wallets SET archived_at = now(), is_default = false
		WHERE merchant_id = $1 AND archived_at IS NULL`, app.MerchantID); err != nil {
		t.Fatal(err)
	}
	first := []string{"choose", "wallets", "product"}

	// Nothing anywhere: both modes open at "choose".
	for _, mode := range []string{"test", "live"} {
		o := onboardingOf(t, c, mode)
		if !o.Visible || !o.FirstRun || o.HasWallet || o.HasProduct || o.Step != "choose" || o.Done != 0 || !slices.Equal(o.Steps, first) {
			t.Fatalf("%s, empty account: %+v", mode, o)
		}
		if len(o.Networks) == 0 {
			t.Fatalf("%s: no networks", mode)
		}
		var chains []string
		for _, n := range o.Networks {
			if n.HasWallet || (n.Family != "evm" && n.Family != "solana") {
				t.Fatalf("%s network %+v", mode, n)
			}
			chains = append(chains, n.Chain)
		}
		// The networks follow the registry, filtered by the assets the mode accepts.
		var want []string
		for _, ch := range app.Registry.ForMode(mode == "live") {
			for _, a := range c.get("/v1/dashboard/settings?mode=" + mode)["accepted_assets"].([]any) {
				if a := a.(map[string]any); a["chain"] == ch.ID && a["enabled"] == true && !slices.Contains(want, ch.ID) {
					want = append(want, ch.ID)
				}
			}
		}
		if !slices.Equal(chains, want) {
			t.Fatalf("%s networks %v, want %v", mode, chains, want)
		}
	}

	// A wallet in Test mode: Test goes to the product, Live skips "choose".
	w := c.send("POST", "/v1/dashboard/wallets?mode=test", map[string]any{"family": "evm", "label": "Main", "address": evmAddr(50)})
	if o := onboardingOf(t, c, "test"); !o.HasWallet || o.Step != "product" || o.Done != 2 || !o.Visible {
		t.Fatalf("test with a wallet: %+v", o)
	} else {
		for _, n := range o.Networks {
			if n.HasWallet != (n.Family == "evm") {
				t.Fatalf("network %+v with an EVM wallet only", n)
			}
		}
	}
	if o := onboardingOf(t, c, "live"); o.HasWallet || o.Step != "wallets" || o.Done != 1 || !o.FirstRun {
		t.Fatalf("live with a test wallet: %+v", o)
	}

	// A Test product: Get started is gone in Test, and Live is no first run any more.
	dashProduct(t, c, "test", "onboarding-1", map[string]any{"name": "First", "price_type": "fixed", "amount": "25.00",
		"payout": map[string]any{"base-sepolia": items(w["id"], "100")}})
	later := []string{"wallets", "product"}
	if o := onboardingOf(t, c, "test"); o.Visible || !o.HasProduct || o.FirstRun || o.Step != "done" || o.Done != 2 || !slices.Equal(o.Steps, later) {
		t.Fatalf("test with a product: %+v", o)
	}
	if o := onboardingOf(t, c, "live"); !o.Visible || o.HasProduct || o.FirstRun || o.Step != "wallets" || o.Done != 0 || !slices.Equal(o.Steps, later) {
		t.Fatalf("live after a test product: %+v", o)
	}
	if r := c.do("GET", "/v1/dashboard/onboarding", nil); r.Status != 400 {
		t.Fatalf("no mode: %d %s", r.Status, r.Body)
	}
}
