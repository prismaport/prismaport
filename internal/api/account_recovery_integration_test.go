//go:build integration

package api_test

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/api"
	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/dashauth"
	"github.com/prismaport/prismaport/internal/mail"
	"github.com/prismaport/prismaport/internal/service"
)

var (
	recoveryFormat = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{5}-[0-9A-HJKMNP-TV-Z]{5}$`)
	lostLine       = regexp.MustCompile(`(?m)^https://\S+/lost-authenticator/confirm#token=([A-Za-z0-9_-]{43})$`)
	cancelLine     = regexp.MustCompile(`(?m)^https://\S+/cancel-2fa-removal#token=([A-Za-z0-9_-]{43})$`)
)

// enable2FA turns two-step sign-in on for c's user and returns the secret and the recovery codes.
func enable2FA(t *testing.T, app *apptest.App, c *dashClient) (string, []string) {
	t.Helper()
	secret := c.send("POST", "/v1/dashboard/auth/totp/enroll", map[string]any{"email_code": c.emailCode(service.ConfirmTOTPEnroll)})["secret"].(string)
	v := c.send("POST", "/v1/dashboard/auth/totp/verify", map[string]any{"code": totpCode(t, app, secret)})
	raw, ok := v["recovery_codes"].([]any)
	if !ok || len(raw) != service.RecoveryCodeCount {
		t.Fatalf("recovery codes: %v", v)
	}
	codes := make([]string, len(raw))
	for i, c := range raw {
		codes[i] = c.(string)
		if !recoveryFormat.MatchString(codes[i]) {
			t.Fatalf("code %q has the wrong format", codes[i])
		}
	}
	app.Clock.Advance(30 * time.Second)
	return secret, codes
}

func meUser(c *dashClient) map[string]any { return c.get("/v1/dashboard/me")["user"].(map[string]any) }

// lostLinkToken is the token of the newest lost link emailed to addr.
func lostLinkToken(t *testing.T, app *apptest.App, addr string) string {
	t.Helper()
	msgs := app.Mail.To(addr)
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Kind != mail.KindTOTPRemovalLink {
			continue
		}
		m := lostLine.FindStringSubmatch(msgs[i].Text)
		if m == nil || !strings.Contains(msgs[i].HTML, "#token="+m[1]+`"`) {
			t.Fatalf("no lost link in the email: %s", msgs[i].Text)
		}
		return m[1]
	}
	t.Fatalf("no lost link email to %s", addr)
	return ""
}

func countKind(app *apptest.App, addr, kind string) int {
	n := 0
	for _, k := range kinds(app, addr) {
		if k == kind {
			n++
		}
	}
	return n
}

// startRemoval asks for a removal and returns the answer's removal_at and cancel_token.
func startRemoval(t *testing.T, app *apptest.App, addr string) (time.Time, string) {
	t.Helper()
	x := newClient(t, app)
	if r := x.do("POST", "/v1/dashboard/auth/totp/lost", map[string]any{"email": addr}); r.Status != http.StatusOK {
		t.Fatalf("lost: %d %s", r.Status, r.Body)
	}
	r := x.do("POST", "/v1/dashboard/auth/totp/lost/confirm", map[string]any{"token": lostLinkToken(t, app, addr), "password": ownerPassword})
	if r.Status != http.StatusOK {
		t.Fatalf("confirm: %d %s", r.Status, r.Body)
	}
	body := r.JSON(t)
	at, err := time.Parse(time.RFC3339Nano, body["removal_at"].(string))
	if err != nil {
		t.Fatal(err)
	}
	return at, body["cancel_token"].(string)
}

func TestRecoveryCodesIssuedOnFirstEnableOnly(t *testing.T) {
	t.Parallel()
	app, userID := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	_, codes := enable2FA(t, app, c)
	if meUser(c)["recovery_codes_left"].(float64) != 10 {
		t.Fatalf("me: %v", meUser(c))
	}
	var hashed int
	if err := app.Store.Pool().QueryRow(t.Context(), `SELECT count(*) FROM dashboard_recovery_codes WHERE user_id = $1 AND length(code_hash) = 32`, userID).Scan(&hashed); err != nil || hashed != 10 {
		t.Fatalf("stored codes: %d %v", hashed, err)
	}

	// A replace (with a recovery code standing in for the current one) returns no codes.
	e := c.send("POST", "/v1/dashboard/auth/totp/enroll", map[string]any{"recovery_code": codes[0]})
	v := c.send("POST", "/v1/dashboard/auth/totp/verify", map[string]any{"code": totpCode(t, app, e["secret"].(string))})
	if _, ok := v["recovery_codes"]; ok {
		t.Fatalf("a replace returned codes: %v", v)
	}
	if v["user"].(map[string]any)["recovery_codes_left"].(float64) != 9 {
		t.Fatalf("after replace: %v", v)
	}
	if !hasAll(auditActions(t, app, app.MerchantID), "user.totp_enabled", "user.recovery_code_used", "user.totp_replaced") {
		t.Fatalf("audit: %v", auditActions(t, app, app.MerchantID))
	}
}

func TestLoginWithRecoveryCode(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	secret, codes := enable2FA(t, app, c)
	x := newClient(t, app)
	loginCode := func(code string) apptest.Response {
		return x.do("POST", "/v1/dashboard/auth/login", map[string]any{"email": "owner@example.com", "password": ownerPassword, "recovery_code": code})
	}
	// Case and hyphen do not matter.
	r := loginCode(strings.ToLower(strings.ReplaceAll(codes[0], "-", "")))
	if r.Status != http.StatusOK || r.JSON(t)["used_recovery_code"] != true || r.JSON(t)["user"].(map[string]any)["recovery_codes_left"].(float64) != 9 {
		t.Fatalf("login with a recovery code: %d %s", r.Status, r.Body)
	}
	// A plain sign-in does not carry the flag.
	app.Clock.Advance(30 * time.Second)
	if r := x.do("POST", "/v1/dashboard/auth/login", map[string]any{"email": "owner@example.com", "password": ownerPassword, "totp": totpCode(t, app, secret)}); r.Status != http.StatusOK || r.JSON(t)["used_recovery_code"] != nil {
		t.Fatalf("plain sign-in: %d %s", r.Status, r.Body)
	}

	// Spent, malformed and wrong codes are named as such; a wrong password answers like any other.
	y := newClient(t, app)
	wrongPassword := y.do("POST", "/v1/dashboard/auth/login", map[string]any{"email": "owner@example.com", "password": "wrong password!", "recovery_code": codes[1]})
	if wrongPassword.Status != http.StatusUnauthorized || wrongPassword.ErrorCode(t) != "unauthenticated" || wrongPassword.Header.Get("Retry-After") != "" {
		t.Fatalf("wrong password: %d %s", wrongPassword.Status, wrongPassword.Body)
	}
	for _, code := range []string{codes[0], "not-a-code", "ZZZZZ-ZZZZZ"} {
		if r := loginCode(code); string(r.Body) != `{"error":{"code":"totp_required","message":"That recovery code is not valid or was already used."}}` || r.Status != http.StatusUnauthorized {
			t.Fatalf("code %q: %d %s", code, r.Status, r.Body)
		}
	}
	// Both second factors at once is refused, and spends nothing.
	both := y.do("POST", "/v1/dashboard/auth/login", map[string]any{"email": "owner@example.com", "password": ownerPassword, "recovery_code": codes[1], "totp": "123456"})
	if both.Status != http.StatusBadRequest || both.ErrorCode(t) != "invalid_request" {
		t.Fatalf("both: %d %s", both.Status, both.Body)
	}
	// The wrong-password try above did not spend codes[1].
	if r := loginCode(codes[1]); r.Status != http.StatusOK {
		t.Fatalf("code spent by a failed sign-in: %d %s", r.Status, r.Body)
	}
	// An unknown email with a recovery code fails like everything else.
	if r := y.do("POST", "/v1/dashboard/auth/login", map[string]any{"email": "nobody@example.com", "password": ownerPassword, "recovery_code": codes[2]}); r.Status != http.StatusUnauthorized || string(r.Body) != string(wrongPassword.Body) {
		t.Fatalf("unknown email: %d %s", r.Status, r.Body)
	}
}

// Wrong recovery codes feed the same wait as wrong TOTP codes.
func TestRecoveryCodePassesTheSignInWait(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	secret, codes := enable2FA(t, app, c)
	x := newClient(t, app)
	body := func(extra map[string]any) map[string]any {
		b := map[string]any{"email": "owner@example.com", "password": ownerPassword}
		for k, v := range extra {
			b[k] = v
		}
		return b
	}
	for i := range service.TOTPFreeFailures {
		if r := x.do("POST", "/v1/dashboard/auth/login", body(map[string]any{"recovery_code": "AAAAA-AAAAA"})); r.Status != http.StatusUnauthorized {
			t.Fatalf("wrong code %d: %d %s", i, r.Status, r.Body)
		}
	}
	if countKind(app, "owner@example.com", mail.KindTOTPFailures) != 1 {
		t.Fatalf("emails: %v", kinds(app, "owner@example.com"))
	}
	// During the wait a right TOTP code and a wrong recovery code fail, and nothing is spent.
	if r := x.do("POST", "/v1/dashboard/auth/login", body(map[string]any{"totp": totpCode(t, app, secret)})); r.Status != http.StatusTooManyRequests {
		t.Fatalf("right TOTP during the wait: %d %s", r.Status, r.Body)
	}
	if r := x.do("POST", "/v1/dashboard/auth/login", body(map[string]any{"recovery_code": "BBBBB-BBBBB"})); r.Status != http.StatusTooManyRequests {
		t.Fatalf("wrong recovery code during the wait: %d %s", r.Status, r.Body)
	}
	if left := meUser(c)["recovery_codes_left"].(float64); left != 10 {
		t.Fatalf("a code was spent during the wait: %v", left)
	}
	// A right recovery code signs in during the wait (50 bits: the wait is not what stops it
	// being guessed), so wrong codes from someone who knows the password never lock the owner
	// out, and it ends the run.
	if r := x.do("POST", "/v1/dashboard/auth/login", body(map[string]any{"recovery_code": codes[0]})); r.Status != http.StatusOK {
		t.Fatalf("right recovery code during the wait: %d %s", r.Status, r.Body)
	}
	var failures int
	if err := app.Store.Pool().QueryRow(t.Context(), `SELECT totp_failures FROM dashboard_users WHERE email = 'owner@example.com'`).Scan(&failures); err != nil || failures != 0 {
		t.Fatalf("failures after a right code: %d %v", failures, err)
	}
}

func TestPasswordChange(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	other := login(t, app, "owner@example.com")
	change := func(body map[string]any) apptest.Response {
		return c.do("POST", "/v1/dashboard/auth/password/change", body)
	}
	if r := change(map[string]any{"current_password": "wrong password!", "new_password": newPassword}); r.Status != http.StatusForbidden || r.ErrorCode(t) != service.CodeInvalidPassword {
		t.Fatalf("wrong current: %d %s", r.Status, r.Body)
	}
	if r := change(map[string]any{"current_password": ownerPassword, "new_password": ownerPassword}); r.Status != http.StatusBadRequest ||
		r.ErrorCode(t) != service.CodeInvalidRequest || !strings.Contains(string(r.Body), `"param":"new_password"`) ||
		!strings.Contains(string(r.Body), "different from the current one") {
		t.Fatalf("same password: %d %s", r.Status, r.Body)
	}
	if r := change(map[string]any{"current_password": ownerPassword, "new_password": "Sh-pw1!"}); r.Status != http.StatusBadRequest || r.ErrorCode(t) != service.CodeWeakPassword {
		t.Fatalf("under 8: %d %s", r.Status, r.Body)
	}
	if r := change(map[string]any{"current_password": ownerPassword}); r.Status != http.StatusBadRequest {
		t.Fatalf("no new password: %d %s", r.Status, r.Body)
	}
	if r := change(map[string]any{"current_password": ownerPassword, "new_password": newPassword}); r.Status != http.StatusOK || r.JSON(t)["ok"] != true {
		t.Fatalf("change: %d %s", r.Status, r.Body)
	}
	if !hasAll(auditActions(t, app, app.MerchantID), "user.password_changed") {
		t.Fatalf("audit: %v", auditActions(t, app, app.MerchantID))
	}
	if countKind(app, "owner@example.com", mail.KindPasswordChanged) != 1 {
		t.Fatalf("emails: %v", kinds(app, "owner@example.com"))
	}
	// This session stays; the other one ended.
	if r := c.do("GET", "/v1/dashboard/me", nil); r.Status != http.StatusOK {
		t.Fatalf("own session: %d", r.Status)
	}
	if r := other.do("GET", "/v1/dashboard/me", nil); r.Status != http.StatusUnauthorized {
		t.Fatalf("other session: %d", r.Status)
	}
	if r := newClient(t, app).do("POST", "/v1/dashboard/auth/login", map[string]any{"email": "owner@example.com", "password": ownerPassword}); r.Status != http.StatusUnauthorized {
		t.Fatalf("old password: %d", r.Status)
	}
	if r := newClient(t, app).do("POST", "/v1/dashboard/auth/login", map[string]any{"email": "owner@example.com", "password": newPassword}); r.Status != http.StatusOK {
		t.Fatalf("new password: %d %s", r.Status, r.Body)
	}
}

func TestPasswordChangeWith2FA(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	secret, _ := enable2FA(t, app, c)
	change := func(totp any) apptest.Response {
		body := map[string]any{"current_password": ownerPassword, "new_password": newPassword}
		if totp != nil {
			body["totp"] = totp
		}
		return c.do("POST", "/v1/dashboard/auth/password/change", body)
	}
	if r := change(nil); r.Status != http.StatusForbidden || r.ErrorCode(t) != service.CodeTOTPRequired {
		t.Fatalf("no code: %d %s", r.Status, r.Body)
	}
	if r := change("000000"); r.Status != http.StatusForbidden || r.ErrorCode(t) != service.CodeTOTPRequired {
		t.Fatalf("wrong code: %d %s", r.Status, r.Body)
	}
	if r := change(totpCode(t, app, secret)); r.Status != http.StatusOK {
		t.Fatalf("with a code: %d %s", r.Status, r.Body)
	}
}

func TestDisableWithRecoveryCode(t *testing.T) {
	t.Parallel()
	app, userID := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	other := login(t, app, "owner@example.com")
	secret, codes := enable2FA(t, app, c)
	_, _ = startRemoval(t, app, "owner@example.com")

	both := map[string]any{"code": totpCode(t, app, secret), "recovery_code": codes[0]}
	if r := c.do("POST", "/v1/dashboard/auth/totp/disable", both); r.Status != http.StatusBadRequest {
		t.Fatalf("both: %d %s", r.Status, r.Body)
	}
	// The password is ignored: it confirms nothing (D58).
	if r := c.do("POST", "/v1/dashboard/auth/totp/disable", map[string]any{"password": ownerPassword}); r.Status != http.StatusBadRequest {
		t.Fatalf("neither: %d %s", r.Status, r.Body)
	}
	if r := c.do("POST", "/v1/dashboard/auth/totp/disable", map[string]any{"recovery_code": "AAAAA-AAAAA"}); r.Status != http.StatusForbidden || r.ErrorCode(t) != service.CodeTOTPRequired {
		t.Fatalf("wrong recovery code: %d %s", r.Status, r.Body)
	}
	d := c.send("POST", "/v1/dashboard/auth/totp/disable", map[string]any{"recovery_code": codes[0]})
	u := d["user"].(map[string]any)
	if u["totp_enabled"] != false || u["totp_removal_at"] != nil || u["recovery_codes_left"].(float64) != 0 {
		t.Fatalf("disable: %v", d)
	}
	var codesLeft, removals int
	if err := app.Store.Pool().QueryRow(t.Context(), `SELECT (SELECT count(*) FROM dashboard_recovery_codes WHERE user_id = $1),
		(SELECT count(*) FROM dashboard_totp_removals WHERE user_id = $1)`, userID).Scan(&codesLeft, &removals); err != nil || codesLeft != 0 || removals != 0 {
		t.Fatalf("left behind: %d codes %d removals %v", codesLeft, removals, err)
	}
	if r := other.do("GET", "/v1/dashboard/me", nil); r.Status != http.StatusUnauthorized {
		t.Fatalf("other session survived: %d", r.Status)
	}
	if c.do("GET", "/v1/dashboard/me", nil).Status != http.StatusOK {
		t.Fatal("own session ended")
	}
	if countKind(app, "owner@example.com", mail.KindTOTPDisabled) != 1 {
		t.Fatalf("emails: %v", kinds(app, "owner@example.com"))
	}
}

// A completed replace ends the other sessions, cancels a pending removal and keeps the codes.
func TestReplaceCancelsRemovalKeepsCodes(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	other := login(t, app, "owner@example.com")
	secret, _ := enable2FA(t, app, c)
	startRemoval(t, app, "owner@example.com")

	e := c.send("POST", "/v1/dashboard/auth/totp/enroll", map[string]any{"code": totpCode(t, app, secret)})
	v := c.send("POST", "/v1/dashboard/auth/totp/verify", map[string]any{"code": totpCode(t, app, e["secret"].(string))})
	u := v["user"].(map[string]any)
	if u["totp_removal_at"] != nil || u["recovery_codes_left"].(float64) != 10 {
		t.Fatalf("replace: %v", v)
	}
	if r := other.do("GET", "/v1/dashboard/me", nil); r.Status != http.StatusUnauthorized {
		t.Fatalf("other session: %d", r.Status)
	}
	if countKind(app, "owner@example.com", mail.KindTOTPReplaced) != 1 {
		t.Fatalf("emails: %v", kinds(app, "owner@example.com"))
	}
}

func TestCreateRecoveryCodes(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	if r := c.do("POST", "/v1/dashboard/auth/totp/recovery_codes", map[string]any{"code": "123456"}); r.Status != http.StatusBadRequest {
		t.Fatalf("2FA off: %d %s", r.Status, r.Body)
	}
	secret, old := enable2FA(t, app, c)
	// The password alone, a wrong code or an emailed code confirm nothing here (D58).
	for name, body := range map[string]any{"the password": map[string]any{"password": ownerPassword}, "a wrong code": map[string]any{"code": "000000"},
		"a wrong recovery code": map[string]any{"recovery_code": "AAAAA-AAAAA"}} {
		t.Run(name, func(t *testing.T) {
			if r := c.do("POST", "/v1/dashboard/auth/totp/recovery_codes", body); r.Status != http.StatusForbidden || r.ErrorCode(t) != service.CodeTOTPRequired {
				t.Fatalf("%s: %d %s", name, r.Status, r.Body)
			}
		})
	}
	if r := c.do("POST", "/v1/dashboard/auth/totp/recovery_codes", map[string]any{"email_code": "123456"}); r.Status != http.StatusBadRequest {
		t.Fatalf("an emailed code: %d %s", r.Status, r.Body)
	}
	// A recovery code may stand in for the app's code, as for every change to 2FA itself.
	byRecovery := c.send("POST", "/v1/dashboard/auth/totp/recovery_codes", map[string]any{"recovery_code": old[0]})["recovery_codes"].([]any)
	got := c.send("POST", "/v1/dashboard/auth/totp/recovery_codes", map[string]any{"code": totpCode(t, app, secret)})
	fresh := got["recovery_codes"].([]any)
	if len(fresh) != 10 || len(byRecovery) != 10 {
		t.Fatalf("new codes: %v", got)
	}
	// The old ones stop working, the new ones work.
	x := newClient(t, app)
	for _, stale := range []string{old[1], byRecovery[0].(string)} {
		if r := x.do("POST", "/v1/dashboard/auth/login", map[string]any{"email": "owner@example.com", "password": ownerPassword, "recovery_code": stale}); r.Status != http.StatusUnauthorized {
			t.Fatalf("old code: %d %s", r.Status, r.Body)
		}
	}
	if r := x.do("POST", "/v1/dashboard/auth/login", map[string]any{"email": "owner@example.com", "password": ownerPassword, "recovery_code": fresh[0].(string)}); r.Status != http.StatusOK {
		t.Fatalf("new code: %d %s", r.Status, r.Body)
	}
	if !hasAll(auditActions(t, app, app.MerchantID), "user.recovery_codes_created") {
		t.Fatalf("audit: %v", auditActions(t, app, app.MerchantID))
	}
}

func TestResetPasswordWithRecoveryCode(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	_, codes := enable2FA(t, app, c)
	x := newClient(t, app)
	if r := x.do("POST", "/v1/dashboard/auth/password/forgot", map[string]any{"email": "owner@example.com"}); r.Status != http.StatusOK {
		t.Fatal(string(r.Body))
	}
	token := lastResetToken(t, app, "owner@example.com")
	reset := func(extra map[string]any) apptest.Response {
		body := map[string]any{"token": token, "new_password": newPassword}
		for k, v := range extra {
			body[k] = v
		}
		return x.do("POST", "/v1/dashboard/auth/password/reset", body)
	}
	if r := reset(map[string]any{"recovery_code": "AAAAA-AAAAA"}); r.Status != http.StatusUnauthorized || r.ErrorCode(t) != service.CodeTOTPRequired {
		t.Fatalf("wrong code: %d %s", r.Status, r.Body)
	}
	if r := reset(map[string]any{"recovery_code": codes[0], "totp": "123456"}); r.Status != http.StatusBadRequest {
		t.Fatalf("both: %d %s", r.Status, r.Body)
	}
	if r := reset(map[string]any{"recovery_code": codes[0]}); r.Status != http.StatusOK {
		t.Fatalf("with a recovery code: %d %s", r.Status, r.Body)
	}
	// Spent there; the reset itself did not end two-factor.
	if r := x.do("POST", "/v1/dashboard/auth/login", map[string]any{"email": "owner@example.com", "password": newPassword, "recovery_code": codes[0]}); r.Status != http.StatusUnauthorized {
		t.Fatalf("spent code: %d %s", r.Status, r.Body)
	}
}

func TestLostLinkAnswersAlikeAndEmailsOnlyTheEligible(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	x := newClient(t, app)
	lost := func(email string) apptest.Response {
		return x.do("POST", "/v1/dashboard/auth/totp/lost", map[string]any{"email": email})
	}
	// No account, no 2FA, a malformed address: the same answer, and no email.
	for _, email := range []string{"nobody@example.com", "owner@example.com", "not an email"} {
		if r := lost(email); r.Status != http.StatusOK || string(r.Body) != `{"ok":true}`+"\n" && string(r.Body) != `{"ok":true}` {
			t.Fatalf("%s: %d %s", email, r.Status, r.Body)
		}
	}
	if countKind(app, "owner@example.com", mail.KindTOTPRemovalLink) != 0 {
		t.Fatalf("emailed without 2FA: %v", kinds(app, "owner@example.com"))
	}
	enable2FA(t, app, c)
	if r := lost("owner@example.com"); r.Status != http.StatusOK {
		t.Fatalf("eligible: %d %s", r.Status, r.Body)
	}
	if countKind(app, "owner@example.com", mail.KindTOTPRemovalLink) != 1 {
		t.Fatalf("no link: %v", kinds(app, "owner@example.com"))
	}
	// Nothing goes out for an account with a removal pending.
	startRemoval(t, app, "owner@example.com")
	before := len(kinds(app, "owner@example.com"))
	if r := lost("owner@example.com"); r.Status != http.StatusOK || len(kinds(app, "owner@example.com")) != before {
		t.Fatalf("emailed with a removal pending: %d %v", r.Status, kinds(app, "owner@example.com"))
	}
}

func TestLostLinkFlow(t *testing.T) {
	t.Parallel()
	app, userID := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	enable2FA(t, app, c)
	x := newClient(t, app)
	if r := x.do("POST", "/v1/dashboard/auth/totp/lost", map[string]any{"email": "owner@example.com"}); r.Status != http.StatusOK {
		t.Fatal(string(r.Body))
	}
	token := lostLinkToken(t, app, "owner@example.com")
	check := func(tok string) apptest.Response {
		return x.do("POST", "/v1/dashboard/auth/totp/lost/check", map[string]any{"token": tok})
	}
	confirm := func(tok, pw string) apptest.Response {
		return x.do("POST", "/v1/dashboard/auth/totp/lost/confirm", map[string]any{"token": tok, "password": pw})
	}
	if r := check(token); r.Status != http.StatusOK || r.JSON(t)["email"] != "owner@example.com" {
		t.Fatalf("check: %d %s", r.Status, r.Body)
	}
	if r := check(otherToken(token)); r.Status != http.StatusBadRequest || r.ErrorCode(t) != service.CodeInvalidLink {
		t.Fatalf("unknown link: %d %s", r.Status, r.Body)
	}
	if r := check("short"); r.Status != http.StatusBadRequest || r.ErrorCode(t) != service.CodeInvalidLink {
		t.Fatalf("malformed link: %d %s", r.Status, r.Body)
	}
	// A wrong password does not spend the link, and is audited.
	if r := confirm(token, "wrong password!"); r.Status != http.StatusForbidden || r.ErrorCode(t) != service.CodeInvalidPassword {
		t.Fatalf("wrong password: %d %s", r.Status, r.Body)
	}
	if !hasAll(auditActions(t, app, app.MerchantID), "user.totp_removal_failed") {
		t.Fatalf("audit: %v", auditActions(t, app, app.MerchantID))
	}
	want := app.Clock.Now().Add(service.TOTPRemovalWait)
	r := confirm(token, ownerPassword)
	if r.Status != http.StatusOK {
		t.Fatalf("confirm: %d %s", r.Status, r.Body)
	}
	at, err := time.Parse(time.RFC3339Nano, r.JSON(t)["removal_at"].(string))
	if err != nil || !at.Equal(want) {
		t.Fatalf("removal_at %v, want %v (%v)", at, want, err)
	}
	cancel := r.JSON(t)["cancel_token"].(string)
	if countKind(app, "owner@example.com", mail.KindTOTPRemovalScheduled) != 1 {
		t.Fatalf("emails: %v", kinds(app, "owner@example.com"))
	}
	if !hasAll(auditActions(t, app, app.MerchantID), "user.totp_removal_requested") {
		t.Fatalf("audit: %v", auditActions(t, app, app.MerchantID))
	}
	// Single use.
	if r := confirm(token, ownerPassword); r.Status != http.StatusBadRequest || r.ErrorCode(t) != service.CodeInvalidLink {
		t.Fatalf("spent link: %d %s", r.Status, r.Body)
	}
	if meUser(c)["totp_removal_at"] == nil {
		t.Fatal("me does not show the pending removal")
	}

	// Later, asking again through a new link while the removal is pending moves nothing and
	// sends no email, and the cancel token it answers still works.
	app.Clock.Advance(10 * time.Hour)
	second, sum, err := dashauth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	u, err := app.Store.DashboardUser(t.Context(), userID)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Store.IssueTOTPLostLink(t.Context(), u, sum[:], app.Clock.Now().Add(service.TOTPLostLinkLifetime)); err != nil {
		t.Fatal(err)
	}
	before := len(kinds(app, "owner@example.com"))
	r2 := confirm(second, ownerPassword)
	if r2.Status != http.StatusOK {
		t.Fatalf("repeat: %d %s", r2.Status, r2.Body)
	}
	at2, _ := time.Parse(time.RFC3339Nano, r2.JSON(t)["removal_at"].(string))
	if !at2.Equal(at) || r2.JSON(t)["cancel_token"] != cancel || len(kinds(app, "owner@example.com")) != before {
		t.Fatalf("repeat moved something: %s vs %v, emails %v", r2.Body, at, kinds(app, "owner@example.com"))
	}

	// The emailed cancel link names the same token: only the POST cancels.
	var mailed string
	for _, m := range app.Mail.To("owner@example.com") {
		if m.Kind == mail.KindTOTPRemovalScheduled {
			mailed = cancelLine.FindStringSubmatch(m.Text)[1]
		}
	}
	if mailed != cancel {
		t.Fatalf("emailed token %q, answered %q", mailed, cancel)
	}
	if r := x.do("POST", "/v1/dashboard/auth/totp/removal/cancel_link", map[string]any{"token": otherToken(cancel)}); r.Status != http.StatusBadRequest || r.ErrorCode(t) != service.CodeInvalidLink {
		t.Fatalf("unknown cancel link: %d %s", r.Status, r.Body)
	}
	if r := x.do("POST", "/v1/dashboard/auth/totp/removal/cancel_link", map[string]any{"token": cancel}); r.Status != http.StatusOK {
		t.Fatalf("cancel: %d %s", r.Status, r.Body)
	}
	if meUser(c)["totp_removal_at"] != nil {
		t.Fatal("still pending after cancel")
	}
	if r := x.do("POST", "/v1/dashboard/auth/totp/removal/cancel_link", map[string]any{"token": cancel}); r.Status != http.StatusBadRequest || r.ErrorCode(t) != service.CodeInvalidLink {
		t.Fatalf("cancel twice: %d %s", r.Status, r.Body)
	}
	if countKind(app, "owner@example.com", mail.KindTOTPRemovalCancelled) != 1 || !hasAll(auditActions(t, app, app.MerchantID), "user.totp_removal_cancelled") {
		t.Fatalf("emails %v, audit %v", kinds(app, "owner@example.com"), auditActions(t, app, app.MerchantID))
	}
}

// Turning 2FA off drops an unused lost link, so it does not come back to life when 2FA is
// turned on again within its 30 minutes.
func TestLostLinkDroppedByTurnOff(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	_, codes := enable2FA(t, app, c)
	x := newClient(t, app)
	if r := x.do("POST", "/v1/dashboard/auth/totp/lost", map[string]any{"email": "owner@example.com"}); r.Status != http.StatusOK {
		t.Fatal(string(r.Body))
	}
	token := lostLinkToken(t, app, "owner@example.com")
	if r := c.do("POST", "/v1/dashboard/auth/totp/disable", map[string]any{"recovery_code": codes[0]}); r.Status != http.StatusOK {
		t.Fatalf("disable: %d %s", r.Status, r.Body)
	}
	app.Clock.Advance(time.Minute)
	enable2FA(t, app, c)
	if r := x.do("POST", "/v1/dashboard/auth/totp/lost/check", map[string]any{"token": token}); r.Status != http.StatusBadRequest || r.ErrorCode(t) != service.CodeInvalidLink {
		t.Fatalf("old link after off and on: %d %s", r.Status, r.Body)
	}
}

func TestLostLinkConfirmIsRateLimited(t *testing.T) {
	t.Parallel()
	limits := map[string]api.Limit{}
	for route := range api.DefaultLimits() {
		limits[route] = api.Limit{PerMinute: 10_000, Burst: 10_000}
	}
	limits[api.RouteDashboardCode] = api.Limit{PerMinute: 2, Burst: 2}
	app, _ := dashApp(t, &api.PublicOptions{Limits: limits})
	x := newClient(t, app)
	for i := range 2 {
		if r := x.do("POST", "/v1/dashboard/auth/totp/lost/confirm", map[string]any{"token": "short", "password": "x"}); r.Status != http.StatusBadRequest {
			t.Fatalf("attempt %d: %d %s", i, r.Status, r.Body)
		}
	}
	if r := x.do("POST", "/v1/dashboard/auth/totp/lost/confirm", map[string]any{"token": "short", "password": "x"}); r.Status != http.StatusTooManyRequests {
		t.Fatalf("not limited: %d %s", r.Status, r.Body)
	}
}

func TestSignInCancelsPendingRemoval(t *testing.T) {
	t.Parallel()
	for _, viaCode := range []bool{false, true} {
		app, _ := dashApp(t, nil)
		c := login(t, app, "owner@example.com")
		secret, codes := enable2FA(t, app, c)
		startRemoval(t, app, "owner@example.com")
		body := map[string]any{"email": "owner@example.com", "password": ownerPassword}
		if viaCode {
			body["recovery_code"] = codes[0]
		} else {
			body["totp"] = totpCode(t, app, secret)
		}
		r := newClient(t, app).do("POST", "/v1/dashboard/auth/login", body)
		if r.Status != http.StatusOK || r.JSON(t)["user"].(map[string]any)["totp_removal_at"] != nil {
			t.Fatalf("sign-in: %d %s", r.Status, r.Body)
		}
		if meUser(c)["totp_removal_at"] != nil {
			t.Fatal("still pending")
		}
		if countKind(app, "owner@example.com", mail.KindTOTPRemovalCancelled) != 1 {
			t.Fatalf("emails: %v", kinds(app, "owner@example.com"))
		}
	}
}

func TestRemovalCancelRoute(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	enable2FA(t, app, c)
	startRemoval(t, app, "owner@example.com")
	// Body ignored: an empty one is fine.
	r := c.do("POST", "/v1/dashboard/auth/totp/removal/cancel", map[string]any{"anything": true})
	if r.Status != http.StatusOK || r.JSON(t)["user"].(map[string]any)["totp_removal_at"] != nil {
		t.Fatalf("cancel: %d %s", r.Status, r.Body)
	}
	if countKind(app, "owner@example.com", mail.KindTOTPRemovalCancelled) != 1 {
		t.Fatalf("emails: %v", kinds(app, "owner@example.com"))
	}
	// Nothing pending: still answers the user, no email.
	if r := c.do("POST", "/v1/dashboard/auth/totp/removal/cancel", nil); r.Status != http.StatusOK || countKind(app, "owner@example.com", mail.KindTOTPRemovalCancelled) != 1 {
		t.Fatalf("again: %d %s", r.Status, r.Body)
	}
}

// D56, kept by D58: a recovery code never confirms a wallet or a live key, in its own field or
// in the place of the app's code.
func TestRecoveryCodeNotAcceptedOnWalletOrLiveKeyStepUp(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	_, codes := enable2FA(t, app, c)
	for _, req := range []struct {
		path string
		body map[string]any
		want int
	}{
		{"/v1/dashboard/wallets?mode=live", map[string]any{"family": "evm", "address": "0x2222222222222222222222222222222222222222", "recovery_code": codes[0]}, http.StatusBadRequest},
		{"/v1/dashboard/api_keys", map[string]any{"name": "k", "scopes": []string{}, "mode": "live", "recovery_code": codes[0]}, http.StatusBadRequest},
		{"/v1/dashboard/wallets?mode=live", map[string]any{"family": "evm", "address": "0x2222222222222222222222222222222222222222", "totp": codes[1]}, http.StatusForbidden},
		{"/v1/dashboard/api_keys", map[string]any{"name": "k", "scopes": []string{"sessions:read"}, "mode": "live", "email_code": codes[1]}, http.StatusForbidden},
	} {
		t.Run(req.path, func(t *testing.T) {
			if r := c.do("POST", req.path, req.body); r.Status != req.want {
				t.Fatalf("%s accepted a recovery code: %d %s", req.path, r.Status, r.Body)
			}
		})
	}
	if left := meUser(c)["recovery_codes_left"].(float64); left != 10 {
		t.Fatalf("a code was spent: %v", left)
	}
}

// The worker: a reminder once, 24 hours before; never early; at the time, everything goes in one
// step and live credentials lock.
func TestRemovalSweepAndLock(t *testing.T) {
	t.Parallel()
	app, userID := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	enable2FA(t, app, c)
	removalAt, _ := startRemoval(t, app, "owner@example.com")
	key, _, err := app.Svc.CreateAPIKey(t.Context(), app.MerchantID, true, []string{"sessions:read"}, "test")
	if err != nil {
		t.Fatal(err)
	}

	sweep := func() int {
		t.Helper()
		n, err := app.Svc.SweepTOTPRemovals(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	// Too early for anything.
	app.Clock.Set(removalAt.Add(-25 * time.Hour))
	if n := sweep(); n != 0 || countKind(app, "owner@example.com", mail.KindTOTPRemovalReminder) != 0 {
		t.Fatalf("early sweep: %d %v", n, kinds(app, "owner@example.com"))
	}
	// The reminder, once.
	app.Clock.Set(removalAt.Add(-24 * time.Hour))
	if n := sweep(); n != 1 {
		t.Fatalf("reminder sweep: %d", n)
	}
	sweep()
	if countKind(app, "owner@example.com", mail.KindTOTPRemovalReminder) != 1 {
		t.Fatalf("emails: %v", kinds(app, "owner@example.com"))
	}
	reminder := app.Mail.To("owner@example.com")
	last := reminder[len(reminder)-1]
	if !cancelLine.MatchString(last.Text) {
		t.Fatalf("no cancel link in the reminder: %s", last.Text)
	}
	// A moment before the time, nothing is removed.
	app.Clock.Set(removalAt.Add(-time.Second))
	sweep()
	if u, err := app.Store.DashboardUser(t.Context(), userID); err != nil || !u.TOTPEnabled() || u.TOTPRemovalAt == nil {
		t.Fatalf("removed early: %v %+v", err, u)
	}

	// At the time: the secret, codes and the row go, sessions end, the lock starts.
	app.Clock.Set(removalAt)
	if n := sweep(); n != 1 {
		t.Fatalf("removal sweep: %d", n)
	}
	var secretNil bool
	var locked *time.Time
	var codes, removals, sessions int
	err = app.Store.Pool().QueryRow(t.Context(), `SELECT totp_secret_ciphertext IS NULL, recovery_locked_until,
		(SELECT count(*) FROM dashboard_recovery_codes WHERE user_id = $1), (SELECT count(*) FROM dashboard_totp_removals WHERE user_id = $1),
		(SELECT count(*) FROM dashboard_sessions WHERE user_id = $1) FROM dashboard_users WHERE id = $1`, userID).Scan(&secretNil, &locked, &codes, &removals, &sessions)
	if err != nil || !secretNil || codes != 0 || removals != 0 || sessions != 0 || locked == nil || !locked.Equal(removalAt.Add(service.RecoveryLockDuration)) {
		t.Fatalf("after removal: %v %v %v %d %d %d", err, secretNil, locked, codes, removals, sessions)
	}
	if r := c.do("GET", "/v1/dashboard/me", nil); r.Status != http.StatusUnauthorized {
		t.Fatalf("session survived: %d", r.Status)
	}
	if !hasAll(auditActions(t, app, app.MerchantID), "user.totp_removed_by_recovery", "user.totp_removal_reminded") {
		t.Fatalf("audit: %v", auditActions(t, app, app.MerchantID))
	}
	var removedMail mail.Message
	for _, m := range app.Mail.To("owner@example.com") {
		if m.Kind == mail.KindTOTPRemoved {
			removedMail = m
		}
	}
	if !strings.Contains(removedMail.Text, "locked until") {
		t.Fatalf("removed email: %s", removedMail.Text)
	}

	// The lock: Live wallet and key create/roll answer 403 before any step-up runs.
	c = login(t, app, "owner@example.com")
	app.Clock.Advance(time.Minute)
	check := func(r apptest.Response) {
		t.Helper()
		if r.Status != http.StatusForbidden || r.ErrorCode(t) != service.CodeRecoveryLocked || r.Header.Get("Retry-After") == "" ||
			r.JSON(t)["error"].(map[string]any)["details"].(map[string]any)["recovery_locked_until"] == nil {
			t.Fatalf("locked: %d %s", r.Status, r.Body)
		}
	}
	wallet := map[string]any{"family": "evm", "address": "0x3333333333333333333333333333333333333333"}
	check(c.do("POST", "/v1/dashboard/wallets?mode=live", wallet)) // no password at all: the lock answers first
	check(c.do("POST", "/v1/dashboard/api_keys", map[string]any{"name": "k", "scopes": []string{"sessions:read"}, "mode": "live"}))
	check(c.do("POST", "/v1/dashboard/api_keys/"+key.ID+"/roll?mode=live", map[string]any{"overlap": "now"}))
	// Test mode is untouched.
	if r := c.do("POST", "/v1/dashboard/wallets?mode=test", wallet); r.Status == http.StatusForbidden && r.ErrorCode(t) == service.CodeRecoveryLocked {
		t.Fatalf("test mode locked: %d %s", r.Status, r.Body)
	}
	// Once the lock ends, the live route answers something else again.
	app.Clock.Advance(service.RecoveryLockDuration)
	if r := c.do("POST", "/v1/dashboard/wallets?mode=live", wallet); r.Status == http.StatusForbidden && r.ErrorCode(t) == service.CodeRecoveryLocked {
		t.Fatalf("still locked after the end: %d %s", r.Status, r.Body)
	}
}
