//go:build integration

package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/prismaport/prismaport/internal/apikey"
	"github.com/prismaport/prismaport/internal/apptest"
)

// No API key adds or archives a wallet, whatever its scopes or mode: those routes do not exist,
// and the wallets stay as they were.
func TestWalletWritesAreNotInTheAPI(t *testing.T) {
	t.Parallel()
	// Arrange
	app := apptest.New(t, apptest.Options{})
	for name, key := range map[string]string{"test": app.TestKey, "live": app.LiveKey} {
		t.Run(name, func(t *testing.T) {
			before := app.Do("GET", "/v1/wallets?include_archived=true", key, nil)
			if before.Status != http.StatusOK {
				t.Fatalf("%s key lists wallets: %d %s", name, before.Status, before.Body)
			}
			main := walletID(t, app, key, apptest.EVMDestination)

			// Act
			create := app.Do("POST", "/v1/wallets", key, map[string]any{"family": "evm", "label": "Attacker", "address": evmAddr(1)})
			archive := app.Do("POST", "/v1/wallets/"+main+"/archive", key, nil)

			// Assert
			for route, r := range map[string]apptest.Response{"POST /v1/wallets": create, "POST /v1/wallets/{id}/archive": archive} {
				status, code, msg := errorOf(t, r)
				if status != http.StatusNotFound || code != "not_found" || msg != "no such route" {
					t.Errorf("%s with a full-access %s key: %d %s %q", route, name, status, code, msg)
				}
			}
			if after := app.Do("GET", "/v1/wallets?include_archived=true", key, nil); string(after.Body) != string(before.Body) {
				t.Errorf("%s wallets changed:\n%s\n%s", name, before.Body, after.Body)
			}
		})
	}
}

// A key issued with wallets:write before the scope was retired keeps working for everything
// else it holds, and still cannot add a wallet.
func TestKeyWithRetiredWalletsWriteScope(t *testing.T) {
	t.Parallel()
	// Arrange
	app := apptest.New(t, apptest.Options{})
	g, err := apikey.Generate(false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.Store.InsertAPIKey(t.Context(), app.MerchantID, false, g.Hash, g.Prefix, []string{"wallets:read", "wallets:write"}, "test"); err != nil {
		t.Fatal(err)
	}

	// Act
	list := app.Do("GET", "/v1/wallets", g.Plaintext, nil)
	create := app.Do("POST", "/v1/wallets", g.Plaintext, map[string]any{"family": "evm", "address": evmAddr(1)})

	// Assert
	if list.Status != http.StatusOK || len(list.JSON(t)["data"].([]any)) != 2 {
		t.Fatalf("listing wallets with the old key: %d %s", list.Status, list.Body)
	}
	if create.Status != http.StatusNotFound || create.ErrorCode(t) != "not_found" {
		t.Fatalf("adding a wallet with the old key: %d %s", create.Status, create.Body)
	}
	if r := app.Do("GET", "/v1/products", g.Plaintext, nil); r.Status != http.StatusForbidden || r.ErrorCode(t) != "insufficient_scope" {
		t.Fatalf("the old key gained scopes: %d %s", r.Status, r.Body)
	}
}

// wallets:write is no scope any more: neither the service nor the dashboard issues a key with it.
func TestWalletsWriteScopeIsRefusedOnNewKeys(t *testing.T) {
	t.Parallel()
	// Arrange
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")

	// Act
	_, _, svcErr := app.Svc.CreateAPIKey(t.Context(), app.MerchantID, false, []string{"wallets:read", "wallets:write"}, "test")
	test := c.do("POST", "/v1/dashboard/api_keys", map[string]any{"name": "Wallets", "scopes": []string{"wallets:write"}, "mode": "test"})
	live := c.do("POST", "/v1/dashboard/api_keys", map[string]any{"name": "Wallets", "scopes": []string{"wallets:read", "wallets:write"}, "mode": "live"})
	full := c.send("POST", "/v1/dashboard/api_keys", map[string]any{"name": "Everything",
		"scopes": scopesExcept(""), "mode": "test"})

	// Assert
	if svcErr == nil || !strings.Contains(svcErr.Error(), `unknown scope "wallets:write"`) {
		t.Errorf("service issued a wallets:write key: %v", svcErr)
	}
	for name, r := range map[string]apptest.Response{"test": test, "live": live} {
		t.Run(name, func(t *testing.T) {
			status, code, msg := errorOf(t, r)
			if status != http.StatusBadRequest || code != "invalid_request" || !strings.Contains(msg, `unknown scope "wallets:write"`) {
				t.Errorf("dashboard %s key with wallets:write: %d %s %q", name, status, code, msg)
			}
		})
	}
	for _, sc := range toStrings(full["scopes"]) {
		if sc == "wallets:write" {
			t.Errorf("a full-access key holds wallets:write: %v", full["scopes"])
		}
	}
	if !strings.Contains(strings.Join(toStrings(full["scopes"]), ","), "wallets:read") {
		t.Errorf("a full-access key lost wallets:read: %v", full["scopes"])
	}
}
