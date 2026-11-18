//go:build integration

package api_test

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/api"
	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/mail"
	"github.com/prismaport/prismaport/internal/service"
	"github.com/prismaport/prismaport/internal/store"
)

const newPassword = "A brand new long password 1"

var (
	codeLine  = regexp.MustCompile(`(?m)^    (\d{6})$`)
	resetLine = regexp.MustCompile(`(?m)^https://\S+/reset-password#token=([A-Za-z0-9_-]{43})$`)
)

// lastResetToken is the token of the reset link in the newest password reset email to addr.
func lastResetToken(t *testing.T, app *apptest.App, addr string) string {
	t.Helper()
	msgs := app.Mail.To(addr)
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Kind != mail.KindPasswordReset {
			continue
		}
		m := resetLine.FindStringSubmatch(msgs[i].Text)
		if m == nil || !strings.Contains(msgs[i].HTML, "#token="+m[1]+`"`) {
			t.Fatalf("no reset link in the email: %s", msgs[i].Text)
		}
		return m[1]
	}
	t.Fatalf("no password reset email to %s (sent: %d)", addr, len(msgs))
	return ""
}

// otherToken is a well-formed token that is not tok.
func otherToken(tok string) string {
	if tok[0] == 'A' {
		return "B" + tok[1:]
	}
	return "A" + tok[1:]
}

// lastCode is the code in the newest email of kind sent to addr.
func lastCode(t *testing.T, app *apptest.App, addr, kind string) string {
	t.Helper()
	msgs := app.Mail.To(addr)
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Kind != kind {
			continue
		}
		m := codeLine.FindStringSubmatch(msgs[i].Text)
		if m == nil || !strings.Contains(msgs[i].HTML, ">"+m[1]+"</span>") {
			t.Fatalf("no code in %s email: %s", kind, msgs[i].Text)
		}
		return m[1]
	}
	t.Fatalf("no %s email to %s (sent: %d)", kind, addr, len(msgs))
	return ""
}

func kinds(app *apptest.App, addr string) []string {
	var out []string
	for _, m := range app.Mail.To(addr) {
		out = append(out, m.Kind)
	}
	return out
}

func signup(t *testing.T, c *dashClient, name, email, password string) apptest.Response {
	t.Helper()
	return c.do("POST", "/v1/dashboard/auth/signup", map[string]any{"business_name": name, "email": email, "password": password})
}

func merchantOf(t *testing.T, app *apptest.App, email string) string {
	t.Helper()
	u, err := app.Store.DashboardUserByEmail(t.Context(), email)
	if err != nil {
		t.Fatal(err)
	}
	return u.MerchantID
}

func TestSignupConfirmAndTestMode(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := newClient(t, app)
	r := signup(t, c, "  Acme   Studio ", "Founder@Acme.example", ownerPassword)
	if r.Status != http.StatusOK || string(r.Body) != `{"ok":true}` || r.Header.Get("Set-Cookie") != "" || r.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("signup: %d %s %v", r.Status, r.Body, r.Header)
	}
	code := lastCode(t, app, "founder@acme.example", mail.KindVerifyEmail)
	merchant := merchantOf(t, app, "founder@acme.example")
	m, err := app.Store.GetMerchant(t.Context(), merchant)
	if err != nil || m.Name != "Acme Studio" || m.LiveEnabledAt == nil {
		t.Fatalf("merchant: %+v %v", m, err)
	}
	// Only the HMAC of the code is stored.
	var stored []byte
	if err := app.Store.Pool().QueryRow(t.Context(), `SELECT code_hash FROM dashboard_email_codes`).Scan(&stored); err != nil ||
		len(stored) != 32 || strings.Contains(string(stored), code) {
		t.Fatalf("stored code: %x %v", stored, err)
	}

	// An unconfirmed user signs in, but only to the confirmation screen.
	c = login(t, app, "founder@acme.example")
	me := c.get("/v1/dashboard/me")
	if me["user"].(map[string]any)["email_verified"] != false || me["merchant"].(map[string]any)["live_enabled"] != true ||
		me["merchant"].(map[string]any)["has_live_wallet"] != false ||
		me["merchant"].(map[string]any)["name"] != "Acme Studio" {
		t.Fatalf("me before confirming: %v", me)
	}
	for _, p := range []probe{{"GET", "/v1/dashboard/overview?mode=test", nil}, {"GET", "/v1/dashboard/links?mode=test", nil}, {"GET", "/v1/dashboard/products?mode=test", nil},
		{"POST", "/v1/dashboard/products?mode=test", map[string]any{"name": "x", "price_type": "fixed", "amount": "25.00"}},
		{"GET", "/v1/dashboard/overview?mode=live", nil},
		{"POST", "/v1/dashboard/products?mode=live", map[string]any{"name": "x", "price_type": "fixed", "amount": "25.00"}},
		{"POST", "/v1/dashboard/api_keys?mode=live", map[string]any{"name": "x", "password": ownerPassword}},
		{"POST", "/v1/dashboard/wallets?mode=live", map[string]any{"family": "evm", "address": "0x000000000000000000000000000000000000dEaD", "password": ownerPassword}},
		{"POST", "/v1/dashboard/auth/totp/enroll", map[string]any{"password": ownerPassword}},
		{"POST", "/v1/dashboard/wallets?mode=test", map[string]any{"family": "evm", "address": "0x000000000000000000000000000000000000dEaD", "password": ownerPassword}}} {
		if r := c.do(p.method, p.path, p.body, "Idempotency-Key", "x"); r.Status != http.StatusForbidden || r.ErrorCode(t) != service.CodeEmailUnverified ||
			!strings.Contains(string(r.Body), "Confirm your email to continue. Enter the code we sent you.") {
			t.Errorf("unconfirmed %s %s: %d %s", p.method, p.path, r.Status, r.Body)
		}
	}
	other := login(t, app, "founder@acme.example")

	if r := c.do("POST", "/v1/dashboard/auth/verify-email", map[string]any{"email": "founder@acme.example", "code": wrong(code)}); r.Status != http.StatusBadRequest ||
		r.ErrorCode(t) != service.CodeInvalidCode {
		t.Fatalf("wrong code: %d %s", r.Status, r.Body)
	}
	if r := c.do("POST", "/v1/dashboard/auth/verify-email", map[string]any{"email": "FOUNDER@acme.example", "code": code[:3] + " " + code[3:]}); r.Status != http.StatusOK ||
		string(r.Body) != `{"verified":true}` {
		t.Fatalf("verify: %d %s", r.Status, r.Body)
	}
	// The browser that confirmed stays signed in; sessions opened before it did not prove the
	// address and end.
	if me := c.get("/v1/dashboard/me"); me["user"].(map[string]any)["email_verified"] != true {
		t.Fatalf("me after confirming: %v", me)
	}
	if r := other.do("GET", "/v1/dashboard/me", nil); r.Status != http.StatusUnauthorized {
		t.Fatalf("an older session survived the confirmation: %d", r.Status)
	}
	// The code is spent.
	if r := c.do("POST", "/v1/dashboard/auth/verify-email", map[string]any{"email": "founder@acme.example", "code": code}); r.Status != http.StatusBadRequest {
		t.Fatalf("code reused: %d %s", r.Status, r.Body)
	}

	// Both modes work at once, each with the assets the pilot allows.
	settings := c.get("/v1/dashboard/settings?mode=test")
	if n := enabledAssets(settings); n != 2 {
		t.Fatalf("test assets enabled: %d %v", n, settings)
	}
	if r := c.do("POST", "/v1/dashboard/products?mode=test", map[string]any{"name": "Founder", "price_type": "fixed", "amount": "25.00"}, "Idempotency-Key", "no-wallet"); r.Status != http.StatusBadRequest ||
		!strings.Contains(string(r.Body), "Add a wallet first") {
		t.Fatalf("a first product before any wallet: %d %s", r.Status, r.Body)
	}
	c.send("POST", "/v1/dashboard/wallets/sample?mode=test", map[string]any{})
	dashProductLink(t, c, "test", "first", map[string]any{"name": "Founder", "price_type": "fixed", "amount": "25.00"})
	if n := enabledAssets(c.get("/v1/dashboard/settings?mode=live")); n != 3 {
		t.Fatalf("live assets enabled: %d", n)
	}
	c.send("POST", "/v1/dashboard/wallets?mode=live", map[string]any{"family": "evm", "address": "0x1111111111111111111111111111111111111111",
		"email_code": c.emailCode(service.ConfirmWalletCreate)})
	if me := c.get("/v1/dashboard/me"); me["merchant"].(map[string]any)["has_live_wallet"] != true {
		t.Fatalf("me after a live wallet: %v", me)
	}
	dashProductLink(t, c, "live", "live-1", map[string]any{"name": "Founder", "price_type": "fixed", "amount": "25.00"})
	if !hasAll(auditActions(t, app, merchant), "merchant.created", "merchant.live_enabled", "user.signed_up", "accepted_assets.set", "user.verification_sent",
		"user.login", "user.code_failed", "user.email_verified", "product.created", "payment_link.created") {
		t.Fatalf("audit: %v", auditActions(t, app, merchant))
	}
}

// A self-serve account from before sign-up opened live mode keeps it closed: it can be read but
// not changed until it is opened.
func TestClosedLiveModeIsReadOnly(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := newClient(t, app)
	if r := signup(t, c, "Old Shop", "old@shop.example", ownerPassword); r.Status != http.StatusOK {
		t.Fatalf("signup: %d %s", r.Status, r.Body)
	}
	code := lastCode(t, app, "old@shop.example", mail.KindVerifyEmail)
	merchant := merchantOf(t, app, "old@shop.example")
	if _, err := app.Store.Pool().Exec(t.Context(), `UPDATE merchants SET live_enabled_at = NULL WHERE id = $1`, merchant); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Store.Pool().Exec(t.Context(), `DELETE FROM merchant_assets WHERE merchant_id = $1 AND livemode`, merchant); err != nil {
		t.Fatal(err)
	}
	c = login(t, app, "old@shop.example")
	c.send("POST", "/v1/dashboard/auth/verify-email", map[string]any{"email": "old@shop.example", "code": code})
	if me := c.get("/v1/dashboard/me"); me["merchant"].(map[string]any)["live_enabled"] != false {
		t.Fatalf("me with live mode closed: %v", me)
	}
	for _, p := range []probe{
		{"POST", "/v1/dashboard/products?mode=live", map[string]any{"name": "x", "price_type": "fixed", "amount": "25.00"}},
		{"POST", "/v1/dashboard/wallets?mode=live", map[string]any{"family": "evm", "address": "0x000000000000000000000000000000000000dEaD"}},
		{"POST", "/v1/dashboard/confirmations?mode=live", map[string]any{"purpose": "wallet_create"}},
		{"POST", "/v1/dashboard/api_keys", map[string]any{"name": "x", "scopes": []string{"sessions:read"}, "mode": "live"}},
		{"PUT", "/v1/dashboard/accepted_assets?mode=live", map[string]any{"items": []map[string]any{{"chain": "base", "asset": "USDC", "enabled": true}}}},
		{"POST", "/v1/dashboard/webhook_endpoints?mode=live", map[string]any{"url": "https://example.com/hooks"}},
	} {
		r := c.do(p.method, p.path, p.body, "Idempotency-Key", "live")
		if r.Status != http.StatusForbidden || r.ErrorCode(t) != service.CodeLiveReview ||
			!strings.Contains(string(r.Body), "Live payments open after a review") {
			t.Errorf("live %s %s: %d %s", p.method, p.path, r.Status, r.Body)
		}
	}
	if live := c.get("/v1/dashboard/links?mode=live"); len(live["data"].([]any)) != 0 {
		t.Fatalf("live links: %v", live)
	}

	// Opening it accepts the live assets the pilot allows.
	if changed, err := app.Store.EnableLive(t.Context(), merchant, offeredLive(app), "test"); err != nil || !changed {
		t.Fatalf("enable live: %v %v", changed, err)
	}
	if changed, err := app.Store.EnableLive(t.Context(), merchant, offeredLive(app), "test"); err != nil || changed {
		t.Fatalf("enable live again: %v %v", changed, err)
	}
	if me := c.get("/v1/dashboard/me"); me["merchant"].(map[string]any)["live_enabled"] != true {
		t.Fatalf("me after opening live mode: %v", me)
	}
	c.send("POST", "/v1/dashboard/wallets?mode=live", map[string]any{"family": "evm", "address": "0x1111111111111111111111111111111111111111",
		"email_code": c.emailCode(service.ConfirmWalletCreate)})
	dashProductLink(t, c, "live", "live-1", map[string]any{"name": "Founder", "price_type": "fixed", "amount": "25.00"})
	if n := enabledAssets(c.get("/v1/dashboard/settings?mode=live")); n != 3 {
		t.Fatalf("live assets after opening: %d", n)
	}
}

// offeredLive is every live (chain, asset) the pilot allows.
func offeredLive(app *apptest.App) []store.ChainAsset {
	var out []store.ChainAsset
	for _, a := range app.Svc.OfferedAssets(true) {
		out = append(out, store.ChainAsset{Chain: a.Chain, Asset: a.Asset})
	}
	return out
}

func enabledAssets(settings map[string]any) int {
	n := 0
	for _, a := range settings["accepted_assets"].([]any) {
		if a.(map[string]any)["enabled"] == true {
			n++
		}
	}
	return n
}

// wrong returns another 6-digit code.
func wrong(code string) string {
	if code == "000000" {
		return "000001"
	}
	return "000000"
}

// The answers never say whether an email has an account.
func TestSelfServeAnswersAreGeneric(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := newClient(t, app)
	existing := signup(t, c, "Someone else", "owner@example.com", "Another long password 1")
	fresh := signup(t, c, "New shop", "new@example.com", "Another long password 1")
	if existing.Status != fresh.Status || string(existing.Body) != string(fresh.Body) || existing.Status != http.StatusOK {
		t.Fatalf("sign-up answers differ: %d %s / %d %s", existing.Status, existing.Body, fresh.Status, fresh.Body)
	}
	// The existing account is untouched and its owner is told; nobody gets a code for it.
	login(t, app, "owner@example.com")
	if got := kinds(app, "owner@example.com"); len(got) != 1 || got[0] != mail.KindAccountExists {
		t.Fatalf("emails to an existing account: %v", got)
	}
	m, _ := app.Store.GetMerchant(t.Context(), app.MerchantID)
	if m.Name != "Acme Store" {
		t.Fatalf("existing merchant renamed: %s", m.Name)
	}
	for _, path := range []string{"/v1/dashboard/auth/resend-code", "/v1/dashboard/auth/password/forgot"} {
		known := c.do("POST", path, map[string]any{"email": "new@example.com"})
		unknown := c.do("POST", path, map[string]any{"email": "nobody@example.com"})
		malformed := c.do("POST", path, map[string]any{"email": "not an email"})
		if known.Status != http.StatusOK || string(known.Body) != string(unknown.Body) || string(known.Body) != string(malformed.Body) {
			t.Errorf("%s answers differ: %s / %s / %s", path, known.Body, unknown.Body, malformed.Body)
		}
	}
	if got := kinds(app, "nobody@example.com"); len(got) != 0 {
		t.Fatalf("mail to an unknown address: %v", got)
	}
	// A confirmed account gets no new confirmation code.
	c.do("POST", "/v1/dashboard/auth/resend-code", map[string]any{"email": "owner@example.com"})
	if got := kinds(app, "owner@example.com"); len(got) != 1 {
		t.Fatalf("resend to a confirmed account: %v", got)
	}
	unknownCode := c.do("POST", "/v1/dashboard/auth/verify-email", map[string]any{"email": "nobody@example.com", "code": "123456", "password": ownerPassword})
	confirmedCode := c.do("POST", "/v1/dashboard/auth/verify-email", map[string]any{"email": "owner@example.com", "code": "123456", "password": ownerPassword})
	badShape := c.do("POST", "/v1/dashboard/auth/verify-email", map[string]any{"email": "new@example.com", "code": "12ab56", "password": ownerPassword})
	if string(unknownCode.Body) != string(confirmedCode.Body) || string(unknownCode.Body) != string(badShape.Body) || unknownCode.Status != http.StatusBadRequest ||
		unknownCode.ErrorCode(t) != service.CodeInvalidCode {
		t.Fatalf("code answers differ: %s / %s / %s", unknownCode.Body, confirmedCode.Body, badShape.Body)
	}
	// A reset link names no account: a made-up one, a malformed one and a used one answer alike.
	madeUp := c.do("POST", "/v1/dashboard/auth/password/reset/check", map[string]any{"token": strings.Repeat("A", 43)})
	malformed := c.do("POST", "/v1/dashboard/auth/password/reset/check", map[string]any{"token": "not a token"})
	madeUpReset := c.do("POST", "/v1/dashboard/auth/password/reset", map[string]any{"token": strings.Repeat("A", 43), "new_password": newPassword})
	if madeUp.Status != http.StatusBadRequest || madeUp.ErrorCode(t) != service.CodeInvalidLink ||
		string(madeUp.Body) != string(malformed.Body) || string(madeUp.Body) != string(madeUpReset.Body) {
		t.Fatalf("link answers differ: %d %s / %s / %s", madeUp.Status, madeUp.Body, malformed.Body, madeUpReset.Body)
	}
	var anon int
	if err := app.Store.Pool().QueryRow(t.Context(), `SELECT count(*) FROM audit_log WHERE merchant_id IS NULL AND action = 'auth.unknown_email'
		AND NOT details::text LIKE '%nobody@%'`).Scan(&anon); err != nil || anon != 2 {
		t.Fatalf("unknown emails audited by hash: %d %v", anon, err)
	}
	if !hasAll(auditActions(t, app, app.MerchantID), "user.signup_existing") {
		t.Fatalf("audit: %v", auditActions(t, app, app.MerchantID))
	}
}

// ADR 0008 C-1: a second sign-up with the email of an unconfirmed account must not set the
// password that the owner's code then confirms. The newest sign-up only gets a fresh code; the
// password is the one of whoever enters the code.
func TestSignupResubmitCannotTakeOverAnUnconfirmedAccount(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	victim := newClient(t, app)
	signup(t, victim, "Victim Ltd", "victim@example.com", ownerPassword)
	oldCode := lastCode(t, app, "victim@example.com", mail.KindVerifyEmail)
	attacker := newClient(t, app)
	signup(t, attacker, "Squatter", "victim@example.com", "The squatter password 1")
	newCode := lastCode(t, app, "victim@example.com", mail.KindVerifyEmail)
	// The re-submit changed neither the password nor the name.
	if r := attacker.do("POST", "/v1/dashboard/auth/login", map[string]any{"email": "victim@example.com", "password": "The squatter password 1"}); r.Status != http.StatusUnauthorized {
		t.Fatalf("the re-submitted password signs in: %d %s", r.Status, r.Body)
	}
	if oldCode != newCode {
		if r := victim.do("POST", "/v1/dashboard/auth/verify-email", map[string]any{"email": "victim@example.com", "code": oldCode, "password": ownerPassword}); r.Status != http.StatusBadRequest {
			t.Fatalf("replaced code still works: %d", r.Status)
		}
	}
	// A browser without this user's session must send the password; nothing is checked without it.
	for _, body := range []map[string]any{{"email": "victim@example.com", "code": newCode}, {"email": "victim@example.com", "code": newCode, "password": ""}} {
		if r := victim.do("POST", "/v1/dashboard/auth/verify-email", body); r.Status != http.StatusBadRequest || r.ErrorCode(t) != service.CodeInvalidRequest ||
			!strings.Contains(string(r.Body), `"param":"password"`) {
			t.Fatalf("verify without the password: %d %s", r.Status, r.Body)
		}
	}
	// The owner enters the newest code with the password they chose.
	if r := victim.do("POST", "/v1/dashboard/auth/verify-email", map[string]any{"email": "victim@example.com", "code": newCode, "password": ownerPassword}); r.Status != http.StatusOK {
		t.Fatalf("verify: %d %s", r.Status, r.Body)
	}
	if r := attacker.do("POST", "/v1/dashboard/auth/login", map[string]any{"email": "victim@example.com", "password": "The squatter password 1"}); r.Status != http.StatusUnauthorized {
		t.Fatalf("the attacker's password signs in after the owner confirmed: %d", r.Status)
	}
	c := login(t, app, "victim@example.com")
	if me := c.get("/v1/dashboard/me"); me["merchant"].(map[string]any)["name"] != "Victim Ltd" || me["user"].(map[string]any)["email_verified"] != true {
		t.Fatalf("me: %v", me)
	}
	if !hasAll(auditActions(t, app, merchantOf(t, app, "victim@example.com")), "user.signup_restarted") {
		t.Fatal("restart not audited")
	}
	// Once confirmed, a new sign-up with the email changes nothing.
	signup(t, newClient(t, app), "Squatter again", "victim@example.com", "The squatter password 1")
	login(t, app, "victim@example.com")
	if got := kinds(app, "victim@example.com"); got[len(got)-1] != mail.KindAccountExists {
		t.Fatalf("emails: %v", got)
	}
}

// The other order: someone signs up first with an email that is not theirs and signs in (an
// unconfirmed session only reaches the confirmation screen). The owner's code, entered with the
// owner's password, confirms the account with that password and ends the squatter's session.
func TestSignupSquatterLosesToTheEmailOwner(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	squatter := newClient(t, app)
	signup(t, squatter, "Squatter", "victim@example.com", "The squatter password 1")
	if r := squatter.do("POST", "/v1/dashboard/auth/login", map[string]any{"email": "victim@example.com", "password": "The squatter password 1"}); r.Status != http.StatusOK {
		t.Fatalf("squatter login: %d", r.Status)
	}
	if r := squatter.do("POST", "/v1/dashboard/auth/totp/enroll", map[string]any{"password": "The squatter password 1"}); r.Status != http.StatusForbidden ||
		r.ErrorCode(t) != service.CodeEmailUnverified {
		t.Fatalf("2FA enrolment before confirming: %d %s", r.Status, r.Body)
	}
	victim := newClient(t, app)
	signup(t, victim, "Victim Ltd", "victim@example.com", ownerPassword)
	code := lastCode(t, app, "victim@example.com", mail.KindVerifyEmail)
	if r := victim.do("POST", "/v1/dashboard/auth/verify-email", map[string]any{"email": "victim@example.com", "code": code, "password": ownerPassword}); r.Status != http.StatusOK {
		t.Fatalf("verify: %d %s", r.Status, r.Body)
	}
	if r := squatter.do("GET", "/v1/dashboard/me", nil); r.Status != http.StatusUnauthorized {
		t.Fatalf("the squatter's session survived the confirmation: %d", r.Status)
	}
	if r := squatter.do("POST", "/v1/dashboard/auth/login", map[string]any{"email": "victim@example.com", "password": "The squatter password 1"}); r.Status != http.StatusUnauthorized {
		t.Fatalf("the squatter's password signs in: %d", r.Status)
	}
	c := login(t, app, "victim@example.com")
	if me := c.get("/v1/dashboard/me"); me["user"].(map[string]any)["email_verified"] != true {
		t.Fatalf("me: %v", me)
	}
	var setPw bool
	if err := app.Store.Pool().QueryRow(t.Context(), `SELECT (details->>'password_set')::bool FROM audit_log WHERE action = 'user.email_verified'
		ORDER BY created_at DESC LIMIT 1`).Scan(&setPw); err != nil || !setPw {
		t.Fatalf("confirmation audit: %v %v", setPw, err)
	}
}

// The service refuses 2FA to an unconfirmed user even past the route gate.
func TestTOTPNeedsAConfirmedEmail(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	signup(t, newClient(t, app), "Fresh", "fresh@example.com", ownerPassword)
	var id string
	if err := app.Store.Pool().QueryRow(t.Context(), `SELECT id FROM dashboard_users WHERE email = 'fresh@example.com'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	ds := service.DashSession{User: service.DashUser{ID: id, Email: "fresh@example.com"}}
	pw := ownerPassword
	if _, err := app.Svc.EnrollTOTP(t.Context(), ds, &pw, nil, nil); service.AsError(err).Code != service.CodeEmailUnverified {
		t.Fatalf("enrol: %v", err)
	}
	if _, err := app.Svc.VerifyTOTP(t.Context(), ds, "123456"); service.AsError(err).Code != service.CodeEmailUnverified {
		t.Fatalf("verify: %v", err)
	}
}

func TestEmailCodeExpiryAndAttempts(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := newClient(t, app)
	verify := func(code string) apptest.Response {
		return c.do("POST", "/v1/dashboard/auth/verify-email", map[string]any{"email": "late@example.com", "code": code, "password": ownerPassword})
	}
	signup(t, c, "Late", "late@example.com", ownerPassword)
	code := lastCode(t, app, "late@example.com", mail.KindVerifyEmail)
	app.Clock.Advance(service.EmailCodeLifetime)
	if r := verify(code); r.Status != http.StatusBadRequest {
		t.Fatalf("expired code accepted: %d", r.Status)
	}
	// Five tries per code: after five wrong ones even the right code is refused.
	c.do("POST", "/v1/dashboard/auth/resend-code", map[string]any{"email": "late@example.com"})
	code = lastCode(t, app, "late@example.com", mail.KindVerifyEmail)
	for i := 0; i < service.MaxCodeAttempts; i++ {
		if r := verify(wrong(code)); r.Status != http.StatusBadRequest {
			t.Fatalf("wrong code %d: %d", i, r.Status)
		}
	}
	if r := verify(code); r.Status != http.StatusBadRequest {
		t.Fatalf("right code accepted after %d wrong ones: %d", service.MaxCodeAttempts, r.Status)
	}
	// New codes do not reset the account's budget of wrong codes for the day.
	failures := service.MaxCodeAttempts
	for failures < service.MaxCodeFailures {
		c.do("POST", "/v1/dashboard/auth/resend-code", map[string]any{"email": "late@example.com"})
		code = lastCode(t, app, "late@example.com", mail.KindVerifyEmail)
		for i := 0; i < service.MaxCodeAttempts && failures < service.MaxCodeFailures; i++ {
			verify(wrong(code))
			failures++
		}
	}
	// With the day's budget spent no code would be checked, so asking for one (a resend or a new
	// sign-up) emails a reset link instead (ADR 0008 C-4): anyone can spend the budget, and the
	// link confirms the email without being counted against codes.
	sentBefore := len(app.Mail.To("late@example.com"))
	c.do("POST", "/v1/dashboard/auth/resend-code", map[string]any{"email": "late@example.com"})
	signup(t, newClient(t, app), "Late", "late@example.com", ownerPassword)
	if got := kinds(app, "late@example.com")[sentBefore:]; len(got) != 2 || got[0] != mail.KindPasswordReset || got[1] != mail.KindPasswordReset {
		t.Fatalf("emails with the budget spent: %v", got)
	}
	if r := verify(code); r.Status != http.StatusBadRequest {
		t.Fatalf("right code accepted with the day's budget spent: %d %s", r.Status, r.Body)
	}
	if r := c.do("POST", "/v1/dashboard/auth/password/reset", map[string]any{"token": lastResetToken(t, app, "late@example.com"), "new_password": newPassword}); r.Status != http.StatusOK {
		t.Fatalf("reset with the budget spent: %d %s", r.Status, r.Body)
	}
	var verified bool
	if err := app.Store.Pool().QueryRow(t.Context(), `SELECT email_verified_at IS NOT NULL FROM dashboard_users WHERE email = 'late@example.com'`).Scan(&verified); err != nil || !verified {
		t.Fatalf("the reset link did not confirm the email: %v %v", verified, err)
	}
}

// The budget still ends with its window: a day later codes work again.
func TestEmailCodeBudgetWindow(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := newClient(t, app)
	verify := func(code string) apptest.Response {
		return c.do("POST", "/v1/dashboard/auth/verify-email", map[string]any{"email": "late@example.com", "code": code, "password": ownerPassword})
	}
	signup(t, c, "Late", "late@example.com", ownerPassword)
	code := lastCode(t, app, "late@example.com", mail.KindVerifyEmail)
	for failures := 0; failures < service.MaxCodeFailures; failures++ {
		if failures%service.MaxCodeAttempts == 0 {
			c.do("POST", "/v1/dashboard/auth/resend-code", map[string]any{"email": "late@example.com"})
			code = lastCode(t, app, "late@example.com", mail.KindVerifyEmail)
		}
		verify(wrong(code))
	}
	app.Clock.Advance(service.CodeFailureWindow)
	c.do("POST", "/v1/dashboard/auth/resend-code", map[string]any{"email": "late@example.com"})
	code = lastCode(t, app, "late@example.com", mail.KindVerifyEmail)
	if r := verify(code); r.Status != http.StatusOK {
		t.Fatalf("right code a day later: %d %s", r.Status, r.Body)
	}
	var attempts int
	if err := app.Store.Pool().QueryRow(t.Context(), `SELECT count(*) FROM dashboard_email_codes`).Scan(&attempts); err != nil || attempts != 0 {
		t.Fatalf("codes left: %d %v", attempts, err)
	}
}

func TestPasswordReset(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	// With 2FA on, the reset also takes an authenticator code, and 2FA stays on afterwards.
	owner := login(t, app, "owner@example.com")
	secret := owner.enable2FA()
	app.Clock.Advance(30 * time.Second)
	signedIn := login(t, app, "owner@example.com", totpCode(t, app, secret))

	anon := newClient(t, app)
	if r := anon.do("POST", "/v1/dashboard/auth/password/forgot", map[string]any{"email": "Owner@example.com"}); r.Status != http.StatusOK {
		t.Fatalf("forgot: %d %s", r.Status, r.Body)
	}
	token := lastResetToken(t, app, "owner@example.com")
	check := func(token string) apptest.Response {
		return anon.do("POST", "/v1/dashboard/auth/password/reset/check", map[string]any{"token": token})
	}
	reset := func(body map[string]any) apptest.Response {
		return anon.do("POST", "/v1/dashboard/auth/password/reset", body)
	}
	// Opening the link spends nothing, however often.
	for i := 0; i < 2; i++ {
		if r := check(token); r.Status != http.StatusOK || string(r.Body) != `{"email":"owner@example.com","totp_required":true}` {
			t.Fatalf("check %d: %d %s", i, r.Status, r.Body)
		}
	}
	if r := check(otherToken(token)); r.Status != http.StatusBadRequest || r.ErrorCode(t) != service.CodeInvalidLink {
		t.Fatalf("another token: %d %s", r.Status, r.Body)
	}
	if r := reset(map[string]any{"token": token, "new_password": "short"}); r.Status != http.StatusBadRequest || r.ErrorCode(t) != service.CodeWeakPassword {
		t.Fatalf("weak password: %d %s", r.Status, r.Body)
	}
	if r := reset(map[string]any{"token": token, "new_password": newPassword}); r.Status != http.StatusUnauthorized || r.ErrorCode(t) != service.CodeTOTPRequired {
		t.Fatalf("no second factor: %d %s", r.Status, r.Body)
	}
	app.Clock.Advance(30 * time.Second)
	code := totpCode(t, app, secret)
	if r := reset(map[string]any{"token": token, "new_password": newPassword, "totp": wrong(code)}); r.Status != http.StatusUnauthorized ||
		r.ErrorCode(t) != service.CodeTOTPRequired {
		t.Fatalf("wrong second factor: %d %s", r.Status, r.Body)
	}
	if r := reset(map[string]any{"token": otherToken(token), "new_password": newPassword, "totp": code}); r.Status != http.StatusBadRequest ||
		r.ErrorCode(t) != service.CodeInvalidLink {
		t.Fatalf("another token: %d %s", r.Status, r.Body)
	}
	if r := reset(map[string]any{"token": token, "new_password": newPassword, "totp": code}); r.Status != http.StatusOK || string(r.Body) != `{"ok":true}` {
		t.Fatalf("reset: %d %s", r.Status, r.Body)
	}
	if r := signedIn.do("GET", "/v1/dashboard/me", nil); r.Status != http.StatusUnauthorized {
		t.Fatalf("a session survived the reset: %d", r.Status)
	}
	// A link works once.
	app.Clock.Advance(30 * time.Second)
	if r := reset(map[string]any{"token": token, "new_password": "Yet another long password 1", "totp": totpCode(t, app, secret)}); r.Status != http.StatusBadRequest ||
		r.ErrorCode(t) != service.CodeInvalidLink {
		t.Fatalf("link reused: %d %s", r.Status, r.Body)
	}
	if r := check(token); r.Status != http.StatusBadRequest {
		t.Fatalf("a spent link still checks: %d", r.Status)
	}
	c := newClient(t, app)
	if r := c.do("POST", "/v1/dashboard/auth/login", map[string]any{"email": "owner@example.com", "password": ownerPassword}); r.Status != http.StatusUnauthorized {
		t.Fatalf("old password: %d", r.Status)
	}
	// The new password with 2FA on asks for the code.
	if r := c.do("POST", "/v1/dashboard/auth/login", map[string]any{"email": "owner@example.com", "password": newPassword}); r.Status != http.StatusUnauthorized ||
		r.ErrorCode(t) != service.CodeTOTPRequired {
		t.Fatalf("new password without the second factor: %d %s", r.Status, r.Body)
	}
	app.Clock.Advance(30 * time.Second)
	if r := c.do("POST", "/v1/dashboard/auth/login", map[string]any{"email": "owner@example.com", "password": newPassword, "totp": totpCode(t, app, secret)}); r.Status != http.StatusOK {
		t.Fatalf("new password: %d %s", r.Status, r.Body)
	}
	got := kinds(app, "owner@example.com")
	if got[len(got)-1] != mail.KindPasswordChanged {
		t.Fatalf("no notice after the reset: %v", got)
	}
	if !hasAll(auditActions(t, app, app.MerchantID), "user.password_reset_requested", "user.password_reset_failed", "user.password_reset") {
		t.Fatalf("audit: %v", auditActions(t, app, app.MerchantID))
	}
	var stored int
	if err := app.Store.Pool().QueryRow(t.Context(), `SELECT count(*) FROM dashboard_email_codes WHERE purpose = 'password_reset'`).Scan(&stored); err != nil || stored != 0 {
		t.Fatalf("links left: %d %v", stored, err)
	}

	// A newer link replaces the one before, and a link ends after 30 minutes. Only the token's
	// hash is stored.
	signup(t, anon, "Forgetful", "forgetful@example.com", ownerPassword)
	anon.do("POST", "/v1/dashboard/auth/password/forgot", map[string]any{"email": "forgetful@example.com"})
	first := lastResetToken(t, app, "forgetful@example.com")
	anon.do("POST", "/v1/dashboard/auth/password/forgot", map[string]any{"email": "forgetful@example.com"})
	second := lastResetToken(t, app, "forgetful@example.com")
	if r := check(first); first == second || r.Status != http.StatusBadRequest {
		t.Fatalf("the older link still works: %d", r.Status)
	}
	var hash []byte
	if err := app.Store.Pool().QueryRow(t.Context(), `SELECT code_hash FROM dashboard_email_codes WHERE purpose = 'password_reset'`).Scan(&hash); err != nil ||
		strings.Contains(string(hash), second) || len(hash) != 32 {
		t.Fatalf("stored: %x %v", hash, err)
	}
	app.Clock.Advance(service.ResetLinkLifetime)
	if r := check(second); r.Status != http.StatusBadRequest {
		t.Fatalf("an expired link still works: %d", r.Status)
	}

	// A reset also confirms an unconfirmed email (the link proved it); without 2FA no code is asked.
	anon.do("POST", "/v1/dashboard/auth/password/forgot", map[string]any{"email": "forgetful@example.com"})
	third := lastResetToken(t, app, "forgetful@example.com")
	if r := check(third); string(r.Body) != `{"email":"forgetful@example.com","totp_required":false}` {
		t.Fatalf("check: %d %s", r.Status, r.Body)
	}
	if r := reset(map[string]any{"token": third, "new_password": newPassword}); r.Status != http.StatusOK {
		t.Fatalf("reset of an unconfirmed account: %d %s", r.Status, r.Body)
	}
	f := newClient(t, app)
	f.send("POST", "/v1/dashboard/auth/login", map[string]any{"email": "forgetful@example.com", "password": newPassword})
	if me := f.get("/v1/dashboard/me"); me["user"].(map[string]any)["email_verified"] != true {
		t.Fatalf("me: %v", me)
	}
}

func TestSelfServeRateLimits(t *testing.T) {
	t.Parallel()
	limits := api.PublicOptions{Limits: map[string]api.Limit{}}
	for route := range api.DefaultLimits() {
		limits.Limits[route] = api.Limit{PerMinute: 10_000, Burst: 10_000}
	}
	limits.Limits[api.RouteDashboardEmail] = api.DefaultLimits()[api.RouteDashboardEmail]
	limits.Limits[api.RouteDashboardCode] = api.Limit{PerMinute: 1, Burst: 3}
	app, _ := dashApp(t, &limits)
	c := newClient(t, app)
	// Three emails to one address, then one a minute, whoever asks and whether or not the
	// address has an account.
	for i, email := range []string{"limit@example.com", "LIMIT@example.com", "limit@example.com"} {
		var r apptest.Response
		if i == 0 {
			r = signup(t, c, "Limit", email, ownerPassword)
		} else {
			r = c.do("POST", "/v1/dashboard/auth/resend-code", map[string]any{"email": email})
		}
		if r.Status != http.StatusOK {
			t.Fatalf("send %d: %d %s", i, r.Status, r.Body)
		}
	}
	r := c.do("POST", "/v1/dashboard/auth/password/forgot", map[string]any{"email": "limit@example.com"})
	if r.Status != http.StatusTooManyRequests || r.ErrorCode(t) != api.CodeRateLimited || r.Header.Get("Retry-After") == "" {
		t.Fatalf("fourth email: %d %s", r.Status, r.Body)
	}
	if got := len(app.Mail.To("limit@example.com")); got != 3 {
		t.Fatalf("emails sent: %d", got)
	}
	for _, email := range []string{"nobody1@example.com", "nobody1@example.com", "nobody1@example.com"} {
		c.do("POST", "/v1/dashboard/auth/password/forgot", map[string]any{"email": email})
	}
	if r := c.do("POST", "/v1/dashboard/auth/password/forgot", map[string]any{"email": "nobody1@example.com"}); r.Status != http.StatusTooManyRequests {
		t.Fatalf("an unknown address is limited the same way: %d", r.Status)
	}
	// Code checks are limited per client IP.
	for i := 0; i < 3; i++ {
		if r := c.do("POST", "/v1/dashboard/auth/verify-email", map[string]any{"email": "limit@example.com", "code": "000000"}); r.Status != http.StatusBadRequest {
			t.Fatalf("check %d: %d", i, r.Status)
		}
	}
	if r := c.do("POST", "/v1/dashboard/auth/password/reset/check", map[string]any{"token": strings.Repeat("A", 43)}); r.Status != http.StatusTooManyRequests {
		t.Fatalf("fourth code check: %d %s", r.Status, r.Body)
	}
}

func TestSelfServeRequestShape(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := newClient(t, app)
	long := strings.Repeat("a", 201)
	for name, tc := range map[string]struct {
		body    any
		headers []string
		status  int
	}{
		"missing password": {map[string]any{"business_name": "A", "email": "a@example.com"}, nil, 400},
		"unknown field":    {map[string]any{"business_name": "A", "email": "a@example.com", "password": ownerPassword, "plan": "pro"}, nil, 400},
		"bad email":        {map[string]any{"business_name": "A", "email": "not an email", "password": ownerPassword}, nil, 400},
		"short password":   {map[string]any{"business_name": "A", "email": "a@example.com", "password": "short"}, nil, 400},
		"long name":        {map[string]any{"business_name": long, "email": "a@example.com", "password": ownerPassword}, nil, 400},
		"blank name":       {map[string]any{"business_name": "   ", "email": "a@example.com", "password": ownerPassword}, nil, 400},
		"control in name":  {map[string]any{"business_name": "A\x07B", "email": "a@example.com", "password": ownerPassword}, nil, 400},
		"not a string":     {map[string]any{"business_name": 1, "email": "a@example.com", "password": ownerPassword}, nil, 400},
		"cross-site":       {map[string]any{"business_name": "A", "email": "a@example.com", "password": ownerPassword}, []string{"Sec-Fetch-Site", "cross-site"}, 403},
		"same-site":        {map[string]any{"business_name": "A", "email": "a@example.com", "password": ownerPassword}, []string{"Sec-Fetch-Site", "same-site"}, 403},
		"form encoded":     {map[string]any{"business_name": "A", "email": "a@example.com", "password": ownerPassword}, []string{"Content-Type", "text/plain"}, 415},
	} {
		t.Run(name, func(t *testing.T) {
			if r := c.do("POST", "/v1/dashboard/auth/signup", tc.body, tc.headers...); r.Status != tc.status {
				t.Errorf("%s: %d %s", name, r.Status, r.Body)
			}
		})
	}
	if len(app.Mail.Sent()) != 0 {
		t.Fatalf("a refused sign-up sent email: %d", len(app.Mail.Sent()))
	}
	for _, path := range []string{"/auth/signup", "/auth/verify-email", "/auth/resend-code", "/auth/password/forgot", "/auth/password/reset/check", "/auth/password/reset"} {
		if r := c.do("GET", "/v1/dashboard"+path, nil); r.Status != http.StatusNotFound && r.Status != http.StatusMethodNotAllowed {
			t.Errorf("GET %s: %d", path, r.Status)
		}
	}
}

func TestSignupNeedsMail(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	svc := service.New(app.Store, service.Config{Registry: app.Registry, SecretBox: app.Box, CheckoutBaseURL: apptest.CheckoutBase})
	h := api.New(svc, app.Store, nil)
	h.MountDashboard()
	app.Server.Config.Handler = h
	c := newClient(t, app)
	for _, p := range []probe{
		{"POST", "/v1/dashboard/auth/signup", map[string]any{"business_name": "A", "email": "a@example.com", "password": ownerPassword}},
		{"POST", "/v1/dashboard/auth/resend-code", map[string]any{"email": "a@example.com"}},
		{"POST", "/v1/dashboard/auth/password/forgot", map[string]any{"email": "a@example.com"}},
	} {
		if r := c.do(p.method, p.path, p.body); r.Status != http.StatusServiceUnavailable {
			t.Errorf("%s without mail: %d %s", p.path, r.Status, r.Body)
		}
	}
	if _, err := app.Store.DashboardUserByEmail(t.Context(), "a@example.com"); err == nil {
		t.Fatal("an account was created that nobody could confirm")
	}
}

func TestProvisionedUsersAreConfirmed(t *testing.T) {
	t.Parallel()
	app, userID := dashApp(t, nil)
	u, err := app.Store.DashboardUser(t.Context(), userID)
	if err != nil || !u.EmailVerified() || !u.MerchantLiveEnabled {
		t.Fatalf("provisioned user: %+v %v", u, err)
	}
	c := login(t, app, "owner@example.com")
	if me := c.get("/v1/dashboard/me"); fmt.Sprint(me["user"].(map[string]any)["email_verified"], me["merchant"].(map[string]any)["live_enabled"]) != "true true" {
		t.Fatalf("me: %v", me)
	}
}
