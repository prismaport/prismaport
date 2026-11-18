//go:build integration

package api_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/service"
)

// D59: a right code opens the session's confirmation window. Until it ends, the session adds and
// archives Live wallets, creates Live API keys and adds or moves Live webhook endpoints without
// another code, up to the window's cap of wallets. Rolling a key and rotating a webhook secret
// still take a code, another session of the same user still takes a code, and using the window
// never extends it: only a new code opens a new one, with its own cap.
func TestConfirmWindow(t *testing.T) {
	t.Parallel()
	// Arrange
	app, _ := dashAppWith(t, apptest.Options{ConfirmWindowWallets: 2}, nil)
	c := login(t, app, "owner@example.com")
	other := login(t, app, "owner@example.com")
	wallet := func(c *dashClient, n int, extra map[string]any) apptest.Response {
		return c.do("POST", "/v1/dashboard/wallets?mode=live", with(map[string]any{"family": "evm", "label": "Cold", "address": evmAddr(n)}, extra))
	}
	window := func() map[string]any {
		w, _ := c.get("/v1/dashboard/me")["confirmation_window"].(map[string]any)
		return w
	}
	seeded, _, err := app.Svc.CreateAPIKey(t.Context(), app.MerchantID, true, []string{"sessions:read"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if w := window(); w != nil {
		t.Fatalf("a window before any code: %v", w)
	}

	// Act
	noCode := wallet(c, 1, nil)
	confirmed := wallet(c, 1, map[string]any{"email_code": c.emailCode(service.ConfirmWalletCreate)})
	opened := window()
	second := wallet(c, 2, nil)
	overCap := wallet(c, 3, nil)
	app.Clock.Advance(61 * time.Second)
	recoded := wallet(c, 3, map[string]any{"email_code": c.emailCode(service.ConfirmWalletCreate)})
	reopened := window()
	key := c.do("POST", "/v1/dashboard/api_keys", map[string]any{"name": "Server", "scopes": scopesExcept(""), "mode": "live"})
	hook := c.do("POST", "/v1/dashboard/webhook_endpoints?mode=live", map[string]any{"url": "https://hooks.example.com/window"})
	hookPath := "/v1/dashboard/webhook_endpoints/" + hook.JSON(t)["id"].(string)
	moved := c.do("PATCH", hookPath+"?mode=live", map[string]any{"url": "https://hooks.example.com/moved"})
	rotate := c.do("POST", hookPath+"/rotate_secret?mode=live", nil)
	roll := c.do("POST", "/v1/dashboard/api_keys/"+seeded.ID+"/roll?mode=live", map[string]any{"overlap": "1h"})
	otherSession := wallet(other, 4, nil)
	app.Clock.Advance(9 * time.Minute)
	archived := c.do("POST", "/v1/dashboard/wallets/"+second.JSON(t)["id"].(string)+"/archive?mode=live", nil)
	app.Clock.Advance(2 * time.Minute)
	ended := c.do("POST", "/v1/dashboard/wallets/"+confirmed.JSON(t)["id"].(string)+"/archive?mode=live", nil)
	closed := window()

	// Assert
	if !refusedWith(t, noCode, service.CodeConfirmationRequired) {
		t.Fatalf("no code, no window: %d %s", noCode.Status, noCode.Body)
	}
	if confirmed.Status != http.StatusOK {
		t.Fatalf("a wallet with its code: %d %s", confirmed.Status, confirmed.Body)
	}
	if opened == nil || opened["wallets_left"] != float64(1) || opened["expires_at"] == nil {
		t.Fatalf("the window the code opened: %v", opened)
	}
	if second.Status != http.StatusOK {
		t.Fatalf("a second wallet in the window: %d %s", second.Status, second.Body)
	}
	if !refusedWith(t, overCap, service.CodeConfirmationRequired) {
		t.Fatalf("a wallet over the window's cap: %d %s", overCap.Status, overCap.Body)
	}
	if recoded.Status != http.StatusOK || reopened == nil || reopened["wallets_left"] != float64(1) || reopened["expires_at"] == opened["expires_at"] {
		t.Fatalf("a code inside the window opens a new one: %d %s, window %v", recoded.Status, recoded.Body, reopened)
	}
	if key.Status != http.StatusOK || hook.Status != http.StatusOK {
		t.Fatalf("a live key and an endpoint in the window: %d %s / %d %s", key.Status, key.Body, hook.Status, hook.Body)
	}
	if moved.Status != http.StatusOK {
		t.Fatalf("moving an endpoint in the window: %d %s", moved.Status, moved.Body)
	}
	if !refusedWith(t, roll, service.CodeConfirmationRequired) || !refusedWith(t, rotate, service.CodeConfirmationRequired) {
		t.Fatalf("rolling a key and rotating a secret are never covered: %d %s / %d %s", roll.Status, roll.Body, rotate.Status, rotate.Body)
	}
	if !refusedWith(t, otherSession, service.CodeConfirmationRequired) {
		t.Fatalf("another session of the same user: %d %s", otherSession.Status, otherSession.Body)
	}
	if archived.Status != http.StatusOK {
		t.Fatalf("archiving inside the second window, after the first ended: %d %s", archived.Status, archived.Body)
	}
	if !refusedWith(t, ended, service.CodeConfirmationRequired) || closed != nil {
		t.Fatalf("after 10 minutes, used or not: %d %s, window %v", ended.Status, ended.Body, closed)
	}
	// Each action the window let through is audited: the second wallet, the key, the endpoint, its
	// move and the archive.
	if n := countAudit(t, app, "user.confirmation_window_used"); n != 5 {
		t.Fatalf("user.confirmation_window_used %d times, want 5", n)
	}
}

// PRISMAPORT_CONFIRM_WINDOW=0 turns the window off: every covered Live action takes its code, as
// under D58.
func TestConfirmWindowOff(t *testing.T) {
	t.Parallel()
	// Arrange
	off := time.Duration(0)
	app, _ := dashAppWith(t, apptest.Options{ConfirmWindow: &off}, nil)
	c := login(t, app, "owner@example.com")
	wallet := func(n int, extra map[string]any) apptest.Response {
		return c.do("POST", "/v1/dashboard/wallets?mode=live", with(map[string]any{"family": "evm", "label": "Cold", "address": evmAddr(n)}, extra))
	}

	// Act
	first := wallet(1, map[string]any{"email_code": c.emailCode(service.ConfirmWalletCreate)})
	second := wallet(2, nil)
	unlockCode := c.do("POST", "/v1/dashboard/confirmations?mode=live", map[string]any{"purpose": service.ConfirmUnlock})
	unlock := c.do("POST", "/v1/dashboard/confirmation_window?mode=live", map[string]any{"email_code": "123456"})

	// Assert
	if first.Status != http.StatusOK {
		t.Fatalf("a wallet with its code: %d %s", first.Status, first.Body)
	}
	if !refusedWith(t, second, service.CodeConfirmationRequired) {
		t.Fatalf("the next wallet with the window off: %d %s", second.Status, second.Body)
	}
	if w := c.get("/v1/dashboard/me")["confirmation_window"]; w != nil {
		t.Fatalf("a window while it is off: %v", w)
	}
	if unlockCode.Status != http.StatusConflict || unlock.Status != http.StatusConflict {
		t.Fatalf("no unlock code is sent and nothing unlocks while it is off: %d %s / %d %s", unlockCode.Status, unlockCode.Body, unlock.Status, unlock.Body)
	}
	if s := c.get("/v1/dashboard/me")["confirmation_window_seconds"]; s != float64(0) {
		t.Fatalf("confirmation_window_seconds while it is off: %v", s)
	}
}
