//go:build integration

package api_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/dashauth"
	"github.com/prismaport/prismaport/internal/ids"
	"github.com/prismaport/prismaport/internal/mail"
	"github.com/prismaport/prismaport/internal/service"
	"github.com/prismaport/prismaport/internal/store"
)

const breachedPassword = "Found-in-a-breach-1"

// breachList flags breachedPassword, or fails every check while err is set.
type breachList struct {
	err   error
	calls atomic.Int32
}

func (b *breachList) Breached(_ context.Context, pw string) (bool, error) {
	b.calls.Add(1)
	if b.err != nil {
		return false, b.err
	}
	return pw == breachedPassword, nil
}

func passwordApp(t *testing.T) (*apptest.App, *breachList) {
	t.Helper()
	b := &breachList{}
	app, _ := dashAppWith(t, apptest.Options{PasswordBreaches: b}, nil)
	return app, b
}

// refusals are the passwords every path must refuse, with the code and the field it names.
var refusals = []struct {
	name, password, code string
}{
	{"too short", "Aa1!", service.CodeWeakPassword},
	{"no special character", "Longenough1", service.CodeWeakPassword},
	{"common", "Password123!", service.CodeCommonPassword},
	{"repeated", "Aa1!Aa1!", service.CodeCommonPassword},
	{"breached", breachedPassword, service.CodeBreachedPassword},
}

func wantRefused(t *testing.T, r apptest.Response, code, param string) {
	t.Helper()
	if r.Status != http.StatusBadRequest || r.ErrorCode(t) != code {
		t.Fatalf("got %d %s, want 400 %s", r.Status, r.Body, code)
	}
	e, _ := r.JSON(t)["error"].(map[string]any)
	if e["param"] != param {
		t.Fatalf("param %v in %s, want %s", e["param"], r.Body, param)
	}
}

func TestSignupAppliesThePasswordPolicy(t *testing.T) {
	t.Parallel()
	app, _ := passwordApp(t)
	c := newClient(t, app)
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			wantRefused(t, signup(t, c, "Acme", "new@example.com", tc.password), tc.code, "password")
			if _, err := app.Store.DashboardUserByEmail(t.Context(), "new@example.com"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("an account was created: %v", err)
			}
		})
	}
	if r := signup(t, c, "Acme", "new@example.com", newPassword); r.Status != http.StatusOK {
		t.Fatalf("a good password: %d %s", r.Status, r.Body)
	}
}

func TestBreachCheckRunsOnlyAfterTheRules(t *testing.T) {
	t.Parallel()
	app, b := passwordApp(t)
	c := newClient(t, app)
	before := b.calls.Load()
	for _, pw := range []string{"Aa1!", "Password123!", "Aa1!Aa1!"} {
		signup(t, c, "Acme", "new@example.com", pw)
	}
	if got := b.calls.Load(); got != before {
		t.Fatalf("%d breach checks for passwords the rules refuse", got-before)
	}
}

func TestBreachCheckFailsOpen(t *testing.T) {
	t.Parallel()
	app, b := passwordApp(t)
	b.err = errors.New("pwned: context deadline exceeded")
	c := newClient(t, app)
	if r := signup(t, c, "Acme", "new@example.com", breachedPassword); r.Status != http.StatusOK {
		t.Fatalf("with the check down: %d %s", r.Status, r.Body)
	}
	if _, err := app.Store.DashboardUserByEmail(t.Context(), "new@example.com"); err != nil {
		t.Fatal(err)
	}
}

// A browser not signed in sets the password with the code, so it meets the policy too.
func TestVerifyEmailAppliesThePasswordPolicy(t *testing.T) {
	t.Parallel()
	app, _ := passwordApp(t)
	if r := signup(t, newClient(t, app), "Acme", "new@example.com", newPassword); r.Status != http.StatusOK {
		t.Fatalf("signup: %d %s", r.Status, r.Body)
	}
	code := lastCode(t, app, "new@example.com", mail.KindVerifyEmail)
	other := newClient(t, app)
	verify := func(pw string) apptest.Response {
		return other.do("POST", "/v1/dashboard/auth/verify-email", map[string]any{"email": "new@example.com", "code": code, "password": pw})
	}
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) { wantRefused(t, verify(tc.password), tc.code, "password") })
	}
	if r := verify(newPassword); r.Status != http.StatusOK {
		t.Fatalf("a refused password spent the code: %d %s", r.Status, r.Body)
	}
}

func TestResetAppliesThePasswordPolicy(t *testing.T) {
	t.Parallel()
	app, _ := passwordApp(t)
	c := newClient(t, app)
	if r := c.do("POST", "/v1/dashboard/auth/password/forgot", map[string]any{"email": "owner@example.com"}); r.Status != http.StatusOK {
		t.Fatalf("forgot: %d %s", r.Status, r.Body)
	}
	token := lastResetToken(t, app, "owner@example.com")
	reset := func(pw string) apptest.Response {
		return c.do("POST", "/v1/dashboard/auth/password/reset", map[string]any{"token": token, "new_password": pw})
	}
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) { wantRefused(t, reset(tc.password), tc.code, "new_password") })
	}
	if r := reset(newPassword); r.Status != http.StatusOK {
		t.Fatalf("a refused password spent the link: %d %s", r.Status, r.Body)
	}
}

func TestCreatedUserAppliesThePasswordPolicy(t *testing.T) {
	t.Parallel()
	app, _ := passwordApp(t)
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			_, err := app.Svc.CreateDashboardUser(t.Context(), app.MerchantID, "operator@example.com", tc.password, "test")
			var e *service.Error
			if !errors.As(err, &e) || e.Code != tc.code {
				t.Fatalf("err = %v, want %s", err, tc.code)
			}
		})
	}
}

// Accounts from before the policy keep working: login and step-up only compare, whatever the
// password looks like, and never ask the breach service.
func TestExistingAccountsAreNotHeldToThePolicy(t *testing.T) {
	t.Parallel()
	app, b := passwordApp(t)
	for _, old := range []string{"short", "no classes at all here", "Password123!", breachedPassword} {
		t.Run(old, func(t *testing.T) {
			hash, err := dashauth.NewHasher(0, 0).HashPassword(t.Context(), old, dashauth.DefaultParams)
			if err != nil {
				t.Fatal(err)
			}
			email := strings.ToLower("legacy-" + ids.New(ids.User) + "@example.com")
			if _, err := app.Store.CreateDashboardUser(t.Context(), store.DashboardUser{ID: ids.New(ids.User), MerchantID: app.MerchantID, Email: email, PasswordHash: hash}, "test"); err != nil {
				t.Fatal(err)
			}
			before := b.calls.Load()
			c := newClient(t, app)
			c.send("POST", "/v1/dashboard/auth/login", map[string]any{"email": email, "password": old})
			if r := c.do("POST", "/v1/dashboard/auth/totp/enroll", map[string]any{"email_code": c.emailCode(service.ConfirmTOTPEnroll)}); r.Status != http.StatusOK {
				t.Fatalf("step-up of a user with an old password: %d %s", r.Status, r.Body)
			}
			if got := b.calls.Load(); got != before {
				t.Fatalf("login or step-up ran %d breach checks", got-before)
			}
		})
	}
}
