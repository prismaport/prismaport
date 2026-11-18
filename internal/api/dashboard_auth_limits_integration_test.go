//go:build integration

package api_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/api"
	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/mail"
	"github.com/prismaport/prismaport/internal/service"
)

// Anonymous requests that email an address (here "forgot password") spend its dashboard_email
// bucket, never the signed-in owner's confirmation codes, which have their own bucket per user.
func TestConfirmationCodesApartFromAnonymousEmails(t *testing.T) {
	t.Parallel()
	limits := map[string]api.Limit{}
	for route := range api.DefaultLimits() {
		limits[route] = api.Limit{PerMinute: 10_000, Burst: 10_000}
	}
	limits[api.RouteDashboardEmail] = api.DefaultLimits()[api.RouteDashboardEmail]
	limits[api.RouteDashboardConfirm] = api.DefaultLimits()[api.RouteDashboardConfirm]
	app, _ := dashApp(t, &api.PublicOptions{Limits: limits})
	c := login(t, app, "owner@example.com")
	x := newClient(t, app)
	for range 4 {
		x.do("POST", "/v1/dashboard/auth/password/forgot", map[string]any{"email": "owner@example.com"})
	}
	if r := x.do("POST", "/v1/dashboard/auth/password/forgot", map[string]any{"email": "owner@example.com"}); r.Status != http.StatusTooManyRequests {
		t.Fatalf("forgot not limited: %d %s", r.Status, r.Body)
	}
	send := func() apptest.Response {
		return c.do("POST", "/v1/dashboard/confirmations", map[string]any{"purpose": service.ConfirmTOTPEnroll})
	}
	// The service also waits a minute between codes for one action; its clock moves on, the
	// buckets' (the database's) does not.
	for i := range 3 {
		if r := send(); r.Status != http.StatusOK {
			t.Fatalf("confirmation code %d: %d %s", i, r.Status, r.Body)
		}
		app.Clock.Advance(61 * time.Second)
	}
	r := send()
	if r.Status != http.StatusTooManyRequests || r.ErrorCode(t) != api.CodeRateLimited || !strings.Contains(string(r.Body), "We just sent you a code") {
		t.Fatalf("fourth confirmation code: %d %s", r.Status, r.Body)
	}
}

// Turning 2FA on for the first time ends every other session: one opened with the password
// alone does not outlive the second factor.
func TestFirstTOTPEnableEndsOtherSessions(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	other := login(t, app, "owner@example.com")
	enable2FA(t, app, c)
	if r := other.do("GET", "/v1/dashboard/me", nil); r.Status != http.StatusUnauthorized {
		t.Fatalf("other session after enabling 2FA: %d %s", r.Status, r.Body)
	}
	if r := c.do("GET", "/v1/dashboard/me", nil); r.Status != http.StatusOK {
		t.Fatalf("the enabling session: %d %s", r.Status, r.Body)
	}
}

// A lost link takes a few wrong passwords, then stops working whatever the rate limits allow.
func TestLostLinkEndsAfterWrongPasswords(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	enable2FA(t, app, c)
	x := newClient(t, app)
	if r := x.do("POST", "/v1/dashboard/auth/totp/lost", map[string]any{"email": "owner@example.com"}); r.Status != http.StatusOK {
		t.Fatal(string(r.Body))
	}
	token := lostLinkToken(t, app, "owner@example.com")
	confirm := func(pw string) apptest.Response {
		return x.do("POST", "/v1/dashboard/auth/totp/lost/confirm", map[string]any{"token": token, "password": pw})
	}
	for i := range service.TOTPLostLinkPasswordTries - 1 {
		if r := confirm("wrong password!"); r.Status != http.StatusForbidden || r.ErrorCode(t) != service.CodeInvalidPassword {
			t.Fatalf("wrong password %d: %d %s", i, r.Status, r.Body)
		}
	}
	if r := confirm("wrong password!"); r.Status != http.StatusBadRequest || r.ErrorCode(t) != service.CodeInvalidLink {
		t.Fatalf("last wrong password: %d %s", r.Status, r.Body)
	}
	if r := confirm(ownerPassword); r.Status != http.StatusBadRequest || r.ErrorCode(t) != service.CodeInvalidLink {
		t.Fatalf("right password on an ended link: %d %s", r.Status, r.Body)
	}
}

// A password reset deletes a pending verify_email code: it cannot set the password afterwards.
func TestResetPasswordEndsVerifyCode(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := newClient(t, app)
	if r := signup(t, c, "Acme", "late@example.com", ownerPassword); r.Status != http.StatusOK {
		t.Fatalf("signup: %d %s", r.Status, r.Body)
	}
	code := lastCode(t, app, "late@example.com", mail.KindVerifyEmail)
	if r := c.do("POST", "/v1/dashboard/auth/password/forgot", map[string]any{"email": "late@example.com"}); r.Status != http.StatusOK {
		t.Fatalf("forgot: %d %s", r.Status, r.Body)
	}
	token := lastResetToken(t, app, "late@example.com")
	if r := c.do("POST", "/v1/dashboard/auth/password/reset", map[string]any{"token": token, "new_password": newPassword}); r.Status != http.StatusOK {
		t.Fatalf("reset: %d %s", r.Status, r.Body)
	}
	r := c.do("POST", "/v1/dashboard/auth/verify-email", map[string]any{"email": "late@example.com", "code": code, "password": "an attacker's password"})
	if r.Status != http.StatusBadRequest {
		t.Fatalf("old verify code after the reset: %d %s", r.Status, r.Body)
	}
	newClient(t, app).send("POST", "/v1/dashboard/auth/login", map[string]any{"email": "late@example.com", "password": newPassword})
}

// Wrong passwords never slow the account down, but the owner hears of a run of them; a right
// password ends the run.
func TestPasswordFailuresEmailTheOwner(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	x := newClient(t, app)
	for i := range service.PasswordNoticeFailures {
		if countKind(app, "owner@example.com", mail.KindPasswordFailures) != 0 {
			t.Fatalf("owner emailed after %d wrong passwords", i)
		}
		if r := x.do("POST", "/v1/dashboard/auth/login", map[string]any{"email": "owner@example.com", "password": "wrong password!"}); r.Status != http.StatusUnauthorized {
			t.Fatalf("wrong password %d: %d %s", i, r.Status, r.Body)
		}
	}
	if countKind(app, "owner@example.com", mail.KindPasswordFailures) != 1 {
		t.Fatalf("emails: %v", kinds(app, "owner@example.com"))
	}
	login(t, app, "owner@example.com")
	var failures int
	if err := app.Store.Pool().QueryRow(t.Context(), `SELECT password_failures FROM dashboard_users WHERE email = 'owner@example.com'`).Scan(&failures); err != nil || failures != 0 {
		t.Fatalf("failures after a right password: %d %v", failures, err)
	}
}
