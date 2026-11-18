//go:build integration

package api_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/service"
)

// D61: Unlock opens the session's confirmation window without an action, with a code emailed for
// "unlock" and for nothing else, spent once. The window then covers the session's Live wallets,
// keys and endpoints as D59 says, never another session, never a key roll. Lock now ends it at
// once and is fine to repeat. Test mode has nothing to unlock.
func TestUnlockAndLock(t *testing.T) {
	t.Parallel()
	// Arrange
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	other := login(t, app, "owner@example.com")
	unlock := func(c *dashClient, body map[string]any) apptest.Response {
		return c.do("POST", "/v1/dashboard/confirmation_window?mode=live", body)
	}
	wallet := func(c *dashClient, n int) apptest.Response {
		return c.do("POST", "/v1/dashboard/wallets?mode=live", map[string]any{"family": "evm", "label": "Cold", "address": evmAddr(n)})
	}
	seeded, _, err := app.Svc.CreateAPIKey(t.Context(), app.MerchantID, true, []string{"sessions:read"}, "test")
	if err != nil {
		t.Fatal(err)
	}

	// Act
	noCode := unlock(c, map[string]any{})
	testMode := c.do("POST", "/v1/dashboard/confirmation_window?mode=test", map[string]any{"email_code": "123456"})
	wrongCode := unlock(c, map[string]any{"email_code": "000000"})
	otherPurpose := unlock(c, map[string]any{"email_code": c.emailCode(service.ConfirmWalletCreate)})
	code := c.emailCode(service.ConfirmUnlock)
	unlocked := unlock(c, map[string]any{"email_code": code})
	me := c.get("/v1/dashboard/me")
	reused := unlock(c, map[string]any{"email_code": code})
	inWindow := wallet(c, 1)
	otherSession := wallet(other, 2)
	roll := c.do("POST", "/v1/dashboard/api_keys/"+seeded.ID+"/roll?mode=live", map[string]any{"overlap": "1h"})
	locked := c.do("DELETE", "/v1/dashboard/confirmation_window", nil)
	afterLock := wallet(c, 3)
	lockedAgain := c.do("DELETE", "/v1/dashboard/confirmation_window", nil)
	closed := c.get("/v1/dashboard/me")["confirmation_window"]

	// Assert
	if !refusedWith(t, noCode, service.CodeConfirmationRequired) {
		t.Errorf("unlock without a code: %d %s", noCode.Status, noCode.Body)
	}
	if testMode.Status != http.StatusBadRequest {
		t.Errorf("unlock in Test mode: %d %s", testMode.Status, testMode.Body)
	}
	if !refusedWith(t, wrongCode, service.CodeInvalidCode) || !refusedWith(t, otherPurpose, service.CodeInvalidCode) {
		t.Errorf("a wrong code and a wallet's code: %d %s / %d %s", wrongCode.Status, wrongCode.Body, otherPurpose.Status, otherPurpose.Body)
	}
	if unlocked.Status != http.StatusOK {
		t.Fatalf("unlock with its code: %d %s", unlocked.Status, unlocked.Body)
	}
	win, _ := unlocked.JSON(t)["confirmation_window"].(map[string]any)
	if win == nil || win["wallets_left"] != float64(service.DefaultConfirmWindowWallets) {
		t.Errorf("the window unlock answers: %v", win)
	}
	if mw, _ := me["confirmation_window"].(map[string]any); mw == nil || mw["expires_at"] != win["expires_at"] {
		t.Errorf("/me after unlock: %v", me["confirmation_window"])
	}
	if me["confirmation_window_seconds"] != float64(service.DefaultConfirmWindow/time.Second) {
		t.Errorf("confirmation_window_seconds: %v", me["confirmation_window_seconds"])
	}
	if !refusedWith(t, reused, service.CodeInvalidCode) {
		t.Errorf("the unlock code a second time: %d %s", reused.Status, reused.Body)
	}
	if inWindow.Status != http.StatusOK {
		t.Errorf("a wallet once unlocked: %d %s", inWindow.Status, inWindow.Body)
	}
	if !refusedWith(t, otherSession, service.CodeConfirmationRequired) || !refusedWith(t, roll, service.CodeConfirmationRequired) {
		t.Errorf("another session and a key roll stay locked: %d %s / %d %s", otherSession.Status, otherSession.Body, roll.Status, roll.Body)
	}
	if locked.Status != http.StatusOK || lockedAgain.Status != http.StatusOK || closed != nil {
		t.Errorf("lock now, twice: %d %s / %d %s, window %v", locked.Status, locked.Body, lockedAgain.Status, lockedAgain.Body, closed)
	}
	if !refusedWith(t, afterLock, service.CodeConfirmationRequired) {
		t.Errorf("a wallet after lock now: %d %s", afterLock.Status, afterLock.Body)
	}
	if n := countAudit(t, app, "user.confirmation_window_opened"); n != 1 {
		t.Errorf("user.confirmation_window_opened %d times, want 1", n)
	}
	if n := countAudit(t, app, "user.confirmation_window_closed"); n != 1 {
		t.Errorf("user.confirmation_window_closed %d times, want 1: only the lock that closed something", n)
	}
}

// With 2FA on, the authenticator's code unlocks too, once per step: the code that just verified
// the app cannot unlock, and a recovery code never does.
func TestUnlockWithTheAuthenticator(t *testing.T) {
	t.Parallel()
	// Arrange
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	secret := c.enable2FA()
	unlock := func(body map[string]any) apptest.Response {
		return c.do("POST", "/v1/dashboard/confirmation_window?mode=live", body)
	}

	// Act
	replayed := unlock(map[string]any{"totp": totpCode(t, app, secret)})
	recovery := unlock(map[string]any{"recovery_code": "K7M2Q-9XQPA"})
	noCode := unlock(map[string]any{})
	app.Clock.Advance(30 * time.Second)
	unlocked := unlock(map[string]any{"totp": totpCode(t, app, secret)})

	// Assert
	if !refusedWith(t, replayed, service.CodeInvalidCode) {
		t.Errorf("the step that verified the app: %d %s", replayed.Status, replayed.Body)
	}
	if recovery.Status == http.StatusOK {
		t.Errorf("a recovery code unlocked: %s", recovery.Body)
	}
	if !refusedWith(t, noCode, service.CodeConfirmationRequired) || len(noCode.JSON(t)["error"].(map[string]any)["details"].(map[string]any)["methods"].([]any)) != 2 {
		t.Errorf("without a code, both methods listed: %d %s", noCode.Status, noCode.Body)
	}
	if unlocked.Status != http.StatusOK {
		t.Errorf("unlock with the app's next code: %d %s", unlocked.Status, unlocked.Body)
	}
}
