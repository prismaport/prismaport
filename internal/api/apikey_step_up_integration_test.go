//go:build integration

package api_test

import (
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/service"
)

func liveKeyCount(t *testing.T, c *dashClient) int {
	t.Helper()
	return len(c.get("/v1/dashboard/api_keys?mode=live")["data"].([]any))
}

func refusedWith(t *testing.T, r apptest.Response, code string) bool {
	t.Helper()
	return r.Status == http.StatusForbidden && r.ErrorCode(t) == code
}

// confirmMethods is the methods list of a confirmation_required answer.
func confirmMethods(t *testing.T, r apptest.Response) []string {
	t.Helper()
	details, _ := r.JSON(t)["error"].(map[string]any)["details"].(map[string]any)
	var out []string
	for _, m := range details["methods"].([]any) {
		out = append(out, m.(string))
	}
	return out
}

func with(body map[string]any, extra map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range body {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// D58: with 2FA off, a dashboard session alone (cookie and CSRF token) cannot create or roll a
// live key: both take a code emailed to the owner for that action. The password confirms
// nothing any more. Test keys keep needing nothing.
func TestLiveAPIKeyNeedsAnEmailedCode(t *testing.T) {
	t.Parallel()
	// Arrange
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	seeded, _, err := app.Svc.CreateAPIKey(t.Context(), app.MerchantID, true, []string{"sessions:read"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	create := map[string]any{"name": "Server", "scopes": scopesExcept(""), "mode": "live"}
	roll := func(extra map[string]any) apptest.Response {
		return c.do("POST", "/v1/dashboard/api_keys/"+seeded.ID+"/roll?mode=live", with(map[string]any{"overlap": "1h"}, extra))
	}
	before := liveKeyCount(t, c)
	walletCode := c.emailCode(service.ConfirmWalletCreate)

	// Act
	noCode := c.do("POST", "/v1/dashboard/api_keys", create)
	passwordOnly := c.do("POST", "/v1/dashboard/api_keys", with(create, map[string]any{"password": ownerPassword}))
	totpWithout2FA := c.do("POST", "/v1/dashboard/api_keys", with(create, map[string]any{"totp": "123456"}))
	otherAction := c.do("POST", "/v1/dashboard/api_keys", with(create, map[string]any{"email_code": walletCode}))
	rollNoCode := roll(nil)
	sent := c.do("POST", "/v1/dashboard/confirmations?mode=live", map[string]any{"purpose": "api_key"})
	code := c.emailCode(service.ConfirmAPIKey)
	wrong := fmt.Sprintf("%06d", (atoi(code)+1)%1_000_000)
	wrongCode := c.do("POST", "/v1/dashboard/api_keys", with(create, map[string]any{"email_code": wrong}))
	afterRefusals := liveKeyCount(t, c)
	testKey := c.do("POST", "/v1/dashboard/api_keys", map[string]any{"name": "Test", "scopes": []string{"sessions:read"}, "mode": "test"})
	created := c.do("POST", "/v1/dashboard/api_keys", with(create, map[string]any{"email_code": code, "password": "ignored"}))
	reused := roll(map[string]any{"email_code": code})
	rolled := roll(map[string]any{"email_code": c.emailCode(service.ConfirmAPIKey)})

	// Assert
	for name, r := range map[string]apptest.Response{"no code": noCode, "the password alone": passwordOnly,
		"a TOTP code with 2FA off": totpWithout2FA, "roll without a code": rollNoCode} {
		t.Run(name, func(t *testing.T) {
			if !refusedWith(t, r, service.CodeConfirmationRequired) || !slices.Equal(confirmMethods(t, r), []string{"email"}) {
				t.Errorf("%s: %d %s", name, r.Status, r.Body)
			}
		})
	}
	for name, r := range map[string]apptest.Response{"a code for another action": otherAction, "a wrong code": wrongCode, "a spent code": reused} {
		t.Run(name, func(t *testing.T) {
			if !refusedWith(t, r, service.CodeInvalidCode) {
				t.Errorf("%s: %d %s", name, r.Status, r.Body)
			}
		})
	}
	if sent.Status != http.StatusOK {
		t.Fatalf("send: %d %s", sent.Status, sent.Body)
	}
	if b := sent.JSON(t); b["sent_to"] != "o•••@example.com" || b["expires_in"] != float64(900) || b["resend_after"] != float64(60) {
		t.Fatalf("send answer: %s", sent.Body)
	}
	if afterRefusals != before {
		t.Fatalf("a refused confirmation issued a live key: %d keys, %d before", afterRefusals, before)
	}
	if testKey.Status != http.StatusOK {
		t.Fatalf("a test key needs no code: %d %s", testKey.Status, testKey.Body)
	}
	if created.Status != http.StatusOK || created.JSON(t)["livemode"] != true {
		t.Fatalf("live key with the code: %d %s", created.Status, created.Body)
	}
	if rolled.Status != http.StatusOK {
		t.Fatalf("live roll with a new code: %d %s", rolled.Status, rolled.Body)
	}
	if !hasAll(auditActions(t, app, app.MerchantID), "user.confirmation_sent", "user.step_up_failed", "user.confirmation_used",
		"api_key.created", "api_key.rolled") {
		t.Fatalf("audit: %v", auditActions(t, app, app.MerchantID))
	}
	var reason string
	if err := app.Store.Pool().QueryRow(t.Context(), `SELECT details->>'reason' FROM audit_log WHERE action = 'user.step_up_failed' ORDER BY id LIMIT 1`).Scan(&reason); err != nil || reason != "email_code" {
		t.Fatalf("step-up failure reason: %q %v", reason, err)
	}
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		n = n*10 + int(r-'0')
	}
	return n
}

// An emailed code works for 15 minutes and 5 tries, a new one is sent at most every minute and
// replaces the one before, and Test mode or an unknown action is refused.
func TestConfirmationCodeRules(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	create := map[string]any{"name": "Server", "scopes": []string{"sessions:read"}, "mode": "live"}
	send := func(purpose, mode string) apptest.Response {
		path := "/v1/dashboard/confirmations"
		if mode != "" {
			path += "?mode=" + mode
		}
		return c.do("POST", path, map[string]any{"purpose": purpose})
	}

	first := c.emailCode(service.ConfirmAPIKey)
	again := send("api_key", "live")
	if again.Status != http.StatusTooManyRequests || again.ErrorCode(t) != "rate_limited" || again.Header.Get("Retry-After") != "60" {
		t.Fatalf("a second code at once: %d %s %q", again.Status, again.Body, again.Header.Get("Retry-After"))
	}
	if r := send("wallet_create", "live"); r.Status != http.StatusOK {
		t.Fatalf("another action has its own wait: %d %s", r.Status, r.Body)
	}
	app.Clock.Advance(61 * time.Second)
	second := c.emailCode(service.ConfirmAPIKey)
	if r := c.do("POST", "/v1/dashboard/api_keys", with(create, map[string]any{"email_code": first})); first != second && !refusedWith(t, r, service.CodeInvalidCode) {
		t.Fatalf("the replaced code: %d %s", r.Status, r.Body)
	}

	// Five wrong tries burn the code: the right one fails afterwards.
	wrong := fmt.Sprintf("%06d", (atoi(second)+1)%1_000_000)
	for i := 0; i < 5; i++ {
		c.do("POST", "/v1/dashboard/api_keys", with(create, map[string]any{"email_code": wrong}))
	}
	if r := c.do("POST", "/v1/dashboard/api_keys", with(create, map[string]any{"email_code": second})); !refusedWith(t, r, service.CodeInvalidCode) {
		t.Fatalf("a burned code: %d %s", r.Status, r.Body)
	}

	// A code works for 15 minutes.
	app.Clock.Advance(61 * time.Second)
	late := c.emailCode(service.ConfirmAPIKey)
	app.Clock.Advance(15 * time.Minute)
	if r := c.do("POST", "/v1/dashboard/api_keys", with(create, map[string]any{"email_code": late})); !refusedWith(t, r, service.CodeInvalidCode) {
		t.Fatalf("an expired code: %d %s", r.Status, r.Body)
	}
	fresh := c.emailCode(service.ConfirmAPIKey)
	app.Clock.Advance(14 * time.Minute)
	if r := c.do("POST", "/v1/dashboard/api_keys", with(create, map[string]any{"email_code": fresh})); r.Status != http.StatusOK {
		t.Fatalf("a code within 15 minutes: %d %s", r.Status, r.Body)
	}

	for name, r := range map[string]apptest.Response{"test mode": send("api_key", "test"), "no mode": send("wallet_create", ""),
		"an unknown action": send("login", "live"), "no purpose": c.do("POST", "/v1/dashboard/confirmations?mode=live", map[string]any{})} {
		t.Run(name, func(t *testing.T) {
			if r.Status != http.StatusBadRequest {
				t.Errorf("%s: %d %s", name, r.Status, r.Body)
			}
		})
	}
}

// With 2FA on, a live key takes one code: the authenticator's or an emailed one, the user's
// choice. Both at once, or a wrong one, is refused.
func TestLiveAPIKeyWith2FATakesEitherCode(t *testing.T) {
	t.Parallel()
	// Arrange
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	secret := c.enable2FA()
	app.Clock.Advance(30 * time.Second)
	seeded, _, err := app.Svc.CreateAPIKey(t.Context(), app.MerchantID, true, []string{"sessions:read"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	create := func(extra map[string]any) apptest.Response {
		return c.do("POST", "/v1/dashboard/api_keys", with(map[string]any{"name": "Server", "scopes": []string{"sessions:read", "sessions:write"}, "mode": "live"}, extra))
	}
	roll := func(extra map[string]any) apptest.Response {
		return c.do("POST", "/v1/dashboard/api_keys/"+seeded.ID+"/roll?mode=live", with(map[string]any{"overlap": "24h"}, extra))
	}
	before := liveKeyCount(t, c)

	// Act
	none := create(map[string]any{"password": ownerPassword})
	email := c.emailCode(service.ConfirmAPIKey)
	both := create(map[string]any{"totp": totpCode(t, app, secret), "email_code": email})
	wrongCode := create(map[string]any{"totp": "000000"})
	afterRefusals := liveKeyCount(t, c)
	byApp := create(map[string]any{"totp": totpCode(t, app, secret)})
	byEmail := roll(map[string]any{"email_code": email})

	// Assert
	if !refusedWith(t, none, service.CodeConfirmationRequired) || !slices.Equal(confirmMethods(t, none), []string{"email", "totp"}) {
		t.Errorf("no code: %d %s", none.Status, none.Body)
	}
	if both.Status != http.StatusBadRequest {
		t.Errorf("both codes: %d %s", both.Status, both.Body)
	}
	if !refusedWith(t, wrongCode, service.CodeInvalidCode) {
		t.Errorf("a wrong app code: %d %s", wrongCode.Status, wrongCode.Body)
	}
	if afterRefusals != before {
		t.Fatalf("a refused confirmation issued a live key: %d keys, %d before", afterRefusals, before)
	}
	if byApp.Status != http.StatusOK {
		t.Fatalf("live key with the app's code: %d %s", byApp.Status, byApp.Body)
	}
	if byEmail.Status != http.StatusOK {
		t.Fatalf("live roll with the emailed code: %d %s", byEmail.Status, byEmail.Body)
	}
}

// ADR 0008 C-3: a stolen session alone cannot point a Live mode webhook endpoint at another host
// or learn its secret. Creating one, moving its URL and rotating its secret take a confirmation
// code, as a live API key does; other edits, and Test mode, take nothing. The confirmation
// window (D59) is off here, so every one takes its own code; TestConfirmWindow covers the window.
func TestLiveWebhookEndpointNeedsACode(t *testing.T) {
	t.Parallel()
	// Arrange
	off := time.Duration(0)
	app, _ := dashAppWith(t, apptest.Options{ConfirmWindow: &off}, nil)
	c := login(t, app, "owner@example.com")
	create := map[string]any{"url": "https://hooks.example.com/live"}
	count := func() int { return len(c.get("/v1/dashboard/webhook_endpoints?mode=live")["data"].([]any)) }

	// Act
	noCode := c.do("POST", "/v1/dashboard/webhook_endpoints?mode=live", with(create, map[string]any{"password": ownerPassword}))
	wrongCode := c.do("POST", "/v1/dashboard/webhook_endpoints?mode=live", with(create, map[string]any{"email_code": "000000"}))
	afterRefusals := count()
	created := c.do("POST", "/v1/dashboard/webhook_endpoints?mode=live", with(create, map[string]any{"email_code": c.emailCode(service.ConfirmWebhookEndpoint)}))
	eid, _ := created.JSON(t)["id"].(string)
	path := "/v1/dashboard/webhook_endpoints/" + eid
	moveNoCode := c.do("PATCH", path+"?mode=live", map[string]any{"url": "https://attacker.example.net/x"})
	rotateNoCode := c.do("POST", path+"/rotate_secret?mode=live", nil)
	sameURL := c.do("PATCH", path+"?mode=live", map[string]any{"url": "https://hooks.example.com/live", "description": "Orders"})
	pause := c.do("PATCH", path+"?mode=live", map[string]any{"status": "disabled"})
	moved := c.do("PATCH", path+"?mode=live", map[string]any{"url": "https://hooks.example.com/v2", "email_code": c.emailCode(service.ConfirmWebhookEndpoint)})
	app.Clock.Advance(61 * time.Second)
	rotated := c.do("POST", path+"/rotate_secret?mode=live", map[string]any{"email_code": c.emailCode(service.ConfirmWebhookEndpoint)})
	testEndpoint := c.do("POST", "/v1/dashboard/webhook_endpoints?mode=test", map[string]any{"url": "https://hooks.example.com/test"})
	tid, _ := testEndpoint.JSON(t)["id"].(string)
	testMove := c.do("PATCH", "/v1/dashboard/webhook_endpoints/"+tid+"?mode=test", map[string]any{"url": "https://hooks.example.com/test2"})
	testRotate := c.do("POST", "/v1/dashboard/webhook_endpoints/"+tid+"/rotate_secret?mode=test", nil)
	// Inside the rotation window a second rotation is refused before any code is asked for.
	rotateAgain := c.do("POST", path+"/rotate_secret?mode=live", nil)

	// Assert
	if rotateAgain.Status != http.StatusConflict || rotateAgain.ErrorCode(t) != service.CodeSecretRotationPending {
		t.Errorf("second live rotation inside the window: %d %s", rotateAgain.Status, rotateAgain.Body)
	}
	for name, r := range map[string]apptest.Response{"create without a code": noCode, "URL change without a code": moveNoCode,
		"rotate without a code": rotateNoCode} {
		t.Run(name, func(t *testing.T) {
			if !refusedWith(t, r, service.CodeConfirmationRequired) {
				t.Errorf("%s: %d %s", name, r.Status, r.Body)
			}
		})
	}
	if !refusedWith(t, wrongCode, service.CodeInvalidCode) {
		t.Errorf("a wrong code: %d %s", wrongCode.Status, wrongCode.Body)
	}
	if afterRefusals != 0 {
		t.Fatalf("a refused confirmation created a live endpoint: %d", afterRefusals)
	}
	for name, r := range map[string]apptest.Response{"create": created, "same URL": sameURL, "pause": pause, "move": moved, "rotate": rotated,
		"test create": testEndpoint, "test move": testMove, "test rotate": testRotate} {
		t.Run(name, func(t *testing.T) {
			if r.Status != http.StatusOK {
				t.Errorf("%s: %d %s", name, r.Status, r.Body)
			}
		})
	}
	if got := c.get("/v1/dashboard/webhook_endpoints?mode=live")["data"].([]any)[0].(map[string]any)["url"]; got != "https://hooks.example.com/v2" {
		t.Fatalf("live URL: %v", got)
	}
}

// With 2FA on, the authenticator's code confirms a live webhook endpoint too.
func TestLiveWebhookEndpointTakesTheAppCodeWith2FA(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	secret := c.enable2FA()
	app.Clock.Advance(30 * time.Second)
	if r := c.do("POST", "/v1/dashboard/webhook_endpoints?mode=live", map[string]any{"url": "https://hooks.example.com/live", "password": ownerPassword}); !refusedWith(t, r, service.CodeConfirmationRequired) {
		t.Fatalf("password only: %d %s", r.Status, r.Body)
	}
	r := c.do("POST", "/v1/dashboard/webhook_endpoints?mode=live", map[string]any{"url": "https://hooks.example.com/live", "totp": totpCode(t, app, secret)})
	if r.Status != http.StatusOK {
		t.Fatalf("app code: %d %s", r.Status, r.Body)
	}
}
