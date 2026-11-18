//go:build integration

package api_test

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/service"
	"github.com/prismaport/prismaport/internal/store"
)

// countAudit counts the merchant's audit entries of one action.
func countAudit(t *testing.T, app *apptest.App, action string) int {
	t.Helper()
	n := 0
	for _, a := range auditActions(t, app, app.MerchantID) {
		if a == action {
			n++
		}
	}
	return n
}

// A Live write the server refuses after the code (a duplicate address, the endpoint limit) does
// not spend the code: the same code then confirms the corrected request, once.
func TestRefusedLiveWriteKeepsTheEmailedCode(t *testing.T) {
	// Arrange
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	wallet := func(addr, code string) apptest.Response {
		return c.do("POST", "/v1/dashboard/wallets?mode=live", map[string]any{"family": "evm", "label": "Cold", "address": addr, "email_code": code})
	}
	if r := wallet(evmAddr(10), c.emailCode(service.ConfirmWalletCreate)); r.Status != http.StatusOK {
		t.Fatalf("first live wallet: %d %s", r.Status, r.Body)
	}
	app.Clock.Advance(61 * time.Second)
	a := service.Auth{KeyID: "test", MerchantID: app.MerchantID, Livemode: true, Scopes: []string{"webhooks:write"}}
	var last string
	for i := range store.MaxWebhookEndpoints {
		url := fmt.Sprintf("https://hooks.example.com/full/%d", i)
		e, err := app.Svc.CreateWebhookEndpoint(t.Context(), a, service.EndpointInput{URL: &url})
		if err != nil {
			t.Fatal(err)
		}
		last = e.ID
	}

	// Act
	code := c.emailCode(service.ConfirmWalletCreate)
	duplicate := wallet(evmAddr(10), code)
	corrected := wallet(evmAddr(11), code)
	reused := wallet(evmAddr(12), code)
	hookCode := c.emailCode(service.ConfirmWebhookEndpoint)
	overLimit := c.do("POST", "/v1/dashboard/webhook_endpoints?mode=live", map[string]any{"url": "https://hooks.example.com/one-more", "email_code": hookCode})
	if err := app.Svc.DeleteWebhookEndpoint(t.Context(), a, last); err != nil {
		t.Fatal(err)
	}
	created := c.do("POST", "/v1/dashboard/webhook_endpoints?mode=live", map[string]any{"url": "https://hooks.example.com/one-more", "email_code": hookCode})

	// Assert
	if duplicate.Status != http.StatusBadRequest || !strings.Contains(string(duplicate.Body), "already has the address") {
		t.Fatalf("a duplicate address: %d %s", duplicate.Status, duplicate.Body)
	}
	if corrected.Status != http.StatusOK {
		t.Fatalf("the same code for a new address: %d %s", corrected.Status, corrected.Body)
	}
	if !refusedWith(t, reused, service.CodeInvalidCode) {
		t.Fatalf("the code spent by the wallet: %d %s", reused.Status, reused.Body)
	}
	if overLimit.Status != http.StatusBadRequest || !strings.Contains(string(overLimit.Body), "at most") {
		t.Fatalf("over the endpoint limit: %d %s", overLimit.Status, overLimit.Body)
	}
	if created.Status != http.StatusOK {
		t.Fatalf("the same code once there is room: %d %s", created.Status, created.Body)
	}
	// Only the codes that confirmed a write were used; the refusals left no trace of one.
	if n := countAudit(t, app, "user.confirmation_used"); n != 3 {
		t.Fatalf("user.confirmation_used %d times, want 3", n)
	}
}

// Rolling a refused write back never gives a code its tries back: the right code with a refused
// write leaves the wrong ones counted, and the fifth wrong one burns the code.
func TestRefusedLiveWriteKeepsTheTriesCounted(t *testing.T) {
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	wallet := func(addr, code string) apptest.Response {
		return c.do("POST", "/v1/dashboard/wallets?mode=live", map[string]any{"family": "evm", "label": "Cold", "address": addr, "email_code": code})
	}
	if r := wallet(evmAddr(20), c.emailCode(service.ConfirmWalletCreate)); r.Status != http.StatusOK {
		t.Fatalf("first live wallet: %d %s", r.Status, r.Body)
	}
	app.Clock.Advance(61 * time.Second)
	code := c.emailCode(service.ConfirmWalletCreate)
	wrong := fmt.Sprintf("%06d", (atoi(code)+1)%1_000_000)
	for i := 0; i < 4; i++ {
		if r := wallet(evmAddr(21), wrong); !refusedWith(t, r, service.CodeInvalidCode) {
			t.Fatalf("wrong code %d: %d %s", i+1, r.Status, r.Body)
		}
	}
	if r := wallet(evmAddr(20), code); r.Status != http.StatusBadRequest {
		t.Fatalf("the right code, a duplicate address: %d %s", r.Status, r.Body)
	}
	if r := wallet(evmAddr(21), wrong); !refusedWith(t, r, service.CodeInvalidCode) {
		t.Fatalf("the fifth wrong code: %d %s", r.Status, r.Body)
	}
	if r := wallet(evmAddr(21), code); !refusedWith(t, r, service.CodeInvalidCode) {
		t.Fatalf("a code burned by five wrong tries: %d %s", r.Status, r.Body)
	}
}

// With 2FA on, the authenticator's code behaves alike: a refused write leaves it unused and
// counts no failed attempt, and it is used once by the write that succeeds.
func TestRefusedLiveWriteKeepsTheAppCode(t *testing.T) {
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	secret := c.enable2FA()
	app.Clock.Advance(30 * time.Second)
	wallet := func(addr, code string) apptest.Response {
		return c.do("POST", "/v1/dashboard/wallets?mode=live", map[string]any{"family": "evm", "label": "Cold", "address": addr, "totp": code})
	}
	if r := wallet(evmAddr(30), totpCode(t, app, secret)); r.Status != http.StatusOK {
		t.Fatalf("first live wallet: %d %s", r.Status, r.Body)
	}
	app.Clock.Advance(30 * time.Second)
	code := totpCode(t, app, secret)

	duplicate := wallet(evmAddr(30), code)
	var failures int
	if err := app.Store.Pool().QueryRow(t.Context(), `SELECT totp_failures FROM dashboard_users WHERE email = 'owner@example.com'`).Scan(&failures); err != nil {
		t.Fatal(err)
	}
	corrected := wallet(evmAddr(31), code)
	replayed := wallet(evmAddr(32), code)

	if duplicate.Status != http.StatusBadRequest {
		t.Fatalf("a duplicate address: %d %s", duplicate.Status, duplicate.Body)
	}
	if failures != 0 {
		t.Fatalf("a right code with a refused write counted %d failures", failures)
	}
	if corrected.Status != http.StatusOK {
		t.Fatalf("the same app code for a new address: %d %s", corrected.Status, corrected.Body)
	}
	if !refusedWith(t, replayed, service.CodeInvalidCode) {
		t.Fatalf("the app code used by the wallet: %d %s", replayed.Status, replayed.Body)
	}
}

// When the provider refuses the email, the code is not kept: the answer is 503
// email_send_failed (transient), nothing can be confirmed with it, and asking again works at
// once instead of waiting a minute. A server with no email at all keeps service_unavailable.
func TestConfirmationEmailSendFailure(t *testing.T) {
	// Arrange
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	app.Mail.Fail = errors.New("provider down")

	// Act
	failed := c.do("POST", "/v1/dashboard/confirmations?mode=live", map[string]any{"purpose": "wallet_create"})
	var codes int
	if err := app.Store.Pool().QueryRow(t.Context(), `SELECT count(*) FROM dashboard_email_codes c JOIN dashboard_users u ON u.id = c.user_id
		WHERE u.email = 'owner@example.com' AND c.purpose = $1`, store.ConfirmPurpose(service.ConfirmWalletCreate)).Scan(&codes); err != nil {
		t.Fatal(err)
	}
	app.Mail.Fail = nil
	retried := c.do("POST", "/v1/dashboard/confirmations?mode=live", map[string]any{"purpose": "wallet_create"})

	// Assert
	if failed.Status != http.StatusServiceUnavailable || failed.ErrorCode(t) != service.CodeEmailSendFailed ||
		!strings.Contains(string(failed.Body), "We couldn't send the code. Try again in a moment.") {
		t.Fatalf("a refused email: %d %s", failed.Status, failed.Body)
	}
	if codes != 0 {
		t.Fatalf("%d codes kept after the email failed", codes)
	}
	if retried.Status != http.StatusOK {
		t.Fatalf("asking again after a failed email: %d %s", retried.Status, retried.Body)
	}
	if !hasAll(auditActions(t, app, app.MerchantID), "user.confirmation_not_sent", "user.confirmation_sent") {
		t.Fatalf("audit %v", auditActions(t, app, app.MerchantID))
	}
	if r := c.do("POST", "/v1/dashboard/wallets?mode=live", map[string]any{"family": "evm", "label": "Cold", "address": evmAddr(40),
		"email_code": c.emailCode(service.ConfirmWalletCreate)}); r.Status != http.StatusOK {
		t.Fatalf("the code sent on the retry: %d %s", r.Status, r.Body)
	}
}
