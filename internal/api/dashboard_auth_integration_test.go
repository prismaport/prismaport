//go:build integration

package api_test

import (
	"crypto/sha256"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/api"
	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/mail"
	"github.com/prismaport/prismaport/internal/service"
)

func TestDashboardLogin(t *testing.T) {
	t.Parallel()
	app, userID := dashApp(t, nil)
	c := newClient(t, app)
	wrong := c.do("POST", "/v1/dashboard/auth/login", map[string]any{"email": "owner@example.com", "password": "not the password"})
	unknown := c.do("POST", "/v1/dashboard/auth/login", map[string]any{"email": "nobody@example.com", "password": ownerPassword})
	malformed := c.do("POST", "/v1/dashboard/auth/login", map[string]any{"email": "not an email", "password": ownerPassword})
	for name, r := range map[string]struct {
		status int
		body   []byte
		cookie bool
	}{"wrong password": {wrong.Status, wrong.Body, wrong.Header.Get("Set-Cookie") != ""},
		"unknown email": {unknown.Status, unknown.Body, unknown.Header.Get("Set-Cookie") != ""},
		"malformed":     {malformed.Status, malformed.Body, malformed.Header.Get("Set-Cookie") != ""}} {
		t.Run(name, func(t *testing.T) {
			if r.status != http.StatusUnauthorized || r.cookie ||
				string(r.body) != `{"error":{"code":"unauthenticated","message":"Email or password is incorrect."}}` {
				t.Errorf("%s: %d %s", name, r.status, r.body)
			}
		})
	}

	ok := c.do("POST", "/v1/dashboard/auth/login", map[string]any{"email": " OWNER@example.com ", "password": ownerPassword})
	if ok.Status != http.StatusOK || ok.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("login: %d %s", ok.Status, ok.Body)
	}
	set := ok.Header.Get("Set-Cookie")
	for _, want := range []string{api.SessionCookie + "=", "Path=/v1/dashboard", "Max-Age=604800", "HttpOnly", "SameSite=Lax"} {
		if !strings.Contains(set, want) {
			t.Errorf("Set-Cookie %q lacks %q", set, want)
		}
	}
	if strings.Contains(set, "Secure") {
		t.Errorf("Secure on a loopback host: %s", set)
	}
	u := ok.JSON(t)["user"].(map[string]any)
	if u["id"] != userID || u["email"] != "owner@example.com" || u["totp_enabled"] != false || c.csrf == "" {
		t.Fatalf("login body: %s", ok.Body)
	}
	me := c.get("/v1/dashboard/me")
	if me["csrf_token"] != c.csrf || me["merchant"].(map[string]any)["id"] != app.MerchantID ||
		me["merchant"].(map[string]any)["name"] != "Acme Store" || me["user"].(map[string]any)["id"] != userID {
		t.Fatalf("me: %v", me)
	}
	// Only the hash of the token is stored.
	var n int
	sum := sha256.Sum256([]byte(c.cookie.Value))
	if err := app.Store.Pool().QueryRow(t.Context(), `SELECT count(*) FROM dashboard_sessions WHERE token_hash = $1`, sum[:]).Scan(&n); err != nil || n != 1 {
		t.Fatalf("session row by hash: %d %v", n, err)
	}
	if err := app.Store.Pool().QueryRow(t.Context(), `SELECT count(*) FROM dashboard_sessions WHERE position($1 in encode(token_hash, 'escape')) > 0`,
		c.cookie.Value).Scan(&n); err != nil || n != 0 {
		t.Fatalf("plaintext token stored: %d %v", n, err)
	}
	if !hasAll(auditActions(t, app, app.MerchantID), "user.created", "user.login_failed", "user.login") {
		t.Fatalf("audit: %v", auditActions(t, app, app.MerchantID))
	}

	// A login from a browser holding a session ends that session.
	old := c.cookie.Value
	c.send("POST", "/v1/dashboard/auth/login", map[string]any{"email": "owner@example.com", "password": ownerPassword})
	if c.cookie.Value == old {
		t.Fatal("session token reused across logins")
	}
	stale := newClient(t, app)
	stale.cookie = &http.Cookie{Name: api.SessionCookie, Value: old}
	if r := stale.do("GET", "/v1/dashboard/me", nil); r.Status != http.StatusUnauthorized {
		t.Fatalf("previous session still valid: %d", r.Status)
	}
}

func TestDashboardLoginShape(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := newClient(t, app)
	if r := c.do("POST", "/v1/dashboard/auth/login", map[string]any{"email": "owner@example.com"}); r.Status != http.StatusBadRequest {
		t.Fatalf("missing password: %d %s", r.Status, r.Body)
	}
	if r := c.do("POST", "/v1/dashboard/auth/login", map[string]any{"email": "owner@example.com", "password": ownerPassword, "remember": true}); r.Status != http.StatusBadRequest {
		t.Fatalf("unknown field: %d %s", r.Status, r.Body)
	}
	r := c.do("POST", "/v1/dashboard/auth/login", map[string]any{"email": "owner@example.com", "password": ownerPassword}, "Content-Type", "text/plain")
	if r.Status != http.StatusUnsupportedMediaType {
		t.Fatalf("form-encodable content type: %d %s", r.Status, r.Body)
	}
	r = c.do("POST", "/v1/dashboard/auth/login", map[string]any{"email": "owner@example.com", "password": ownerPassword}, "Sec-Fetch-Site", "cross-site")
	if r.Status != http.StatusForbidden || r.ErrorCode(t) != "csrf_failed" {
		t.Fatalf("cross-site login: %d %s", r.Status, r.Body)
	}
	if r := c.do("POST", "/v1/dashboard/auth/login", map[string]any{"email": "owner@example.com", "password": ownerPassword},
		"Host", "dashboard.example.com"); r.Status != http.StatusOK || !strings.Contains(r.Header.Get("Set-Cookie"), "Secure") {
		t.Fatalf("Secure cookie off localhost: %d %s", r.Status, r.Header.Get("Set-Cookie"))
	}
}

func TestDashboardCSRF(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	token := c.csrf
	c.csrf = ""
	if r := c.do("POST", "/v1/dashboard/auth/logout", nil); r.Status != http.StatusForbidden || r.ErrorCode(t) != "csrf_failed" {
		t.Fatalf("no CSRF header: %d %s", r.Status, r.Body)
	}
	if r := c.do("POST", "/v1/dashboard/auth/logout", nil, api.CSRFHeader, token+"x"); r.Status != http.StatusForbidden {
		t.Fatalf("wrong CSRF header: %d %s", r.Status, r.Body)
	}
	other := login(t, app, "owner@example.com")
	if r := c.do("POST", "/v1/dashboard/auth/logout", nil, api.CSRFHeader, other.csrf); r.Status != http.StatusForbidden {
		t.Fatalf("another session's CSRF token: %d %s", r.Status, r.Body)
	}
	c.csrf = token
	if r := c.do("GET", "/v1/dashboard/me", nil, "Sec-Fetch-Site", "cross-site"); r.Status != http.StatusForbidden {
		t.Fatalf("cross-site read: %d %s", r.Status, r.Body)
	}
	if r := c.do("GET", "/v1/dashboard/me", nil, "Sec-Fetch-Site", "same-site"); r.Status != http.StatusForbidden || r.ErrorCode(t) != "csrf_failed" {
		t.Fatalf("read from a sibling host: %d %s", r.Status, r.Body)
	}
	if r := c.do("GET", "/v1/dashboard/me", nil, "Sec-Fetch-Site", "same-origin"); r.Status != http.StatusOK {
		t.Fatalf("same-origin read: %d %s", r.Status, r.Body)
	}
	if r := c.do("POST", "/v1/dashboard/auth/logout", "{}", "Content-Type", "application/x-www-form-urlencoded"); r.Status != http.StatusUnsupportedMediaType {
		t.Fatalf("form body: %d %s", r.Status, r.Body)
	}
	if r := c.do("POST", "/v1/dashboard/auth/logout", nil); r.Status != http.StatusOK {
		t.Fatalf("logout: %d %s", r.Status, r.Body)
	}
	if c.cookie != nil {
		t.Fatal("logout must clear the cookie")
	}
}

func TestDashboardSessionLifetime(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	// Activity keeps the session alive past the idle timeout...
	for range 3 {
		app.Clock.Advance(11 * time.Hour)
		c.get("/v1/dashboard/me")
	}
	// ...but 12 h without a request ends it.
	app.Clock.Advance(12*time.Hour + time.Second)
	r := c.do("GET", "/v1/dashboard/me", nil)
	if r.Status != http.StatusUnauthorized || r.ErrorCode(t) != "unauthenticated" || c.cookie != nil {
		t.Fatalf("idle session: %d %s cookie=%v", r.Status, r.Body, c.cookie)
	}

	// The absolute lifetime ends a busy session after 7 days.
	c = login(t, app, "owner@example.com")
	start := app.Clock.Now()
	for app.Clock.Now().Before(start.Add(7*24*time.Hour - 11*time.Hour)) {
		app.Clock.Advance(11 * time.Hour)
		c.get("/v1/dashboard/me")
	}
	app.Clock.Set(start.Add(7 * 24 * time.Hour))
	if r := c.do("GET", "/v1/dashboard/me", nil); r.Status != http.StatusUnauthorized {
		t.Fatalf("session past 7 days: %d %s", r.Status, r.Body)
	}

	// Logout deletes the row: the old cookie no longer works.
	c = login(t, app, "owner@example.com")
	kept := *c.cookie
	c.send("POST", "/v1/dashboard/auth/logout", nil)
	replay := newClient(t, app)
	replay.cookie = &kept
	if r := replay.do("GET", "/v1/dashboard/me", nil); r.Status != http.StatusUnauthorized {
		t.Fatalf("cookie after logout: %d %s", r.Status, r.Body)
	}
	for _, bad := range []string{"", "garbage", strings.Repeat("A", 43)} {
		x := newClient(t, app)
		x.cookie = &http.Cookie{Name: api.SessionCookie, Value: bad}
		if r := x.do("GET", "/v1/dashboard/me", nil); r.Status != http.StatusUnauthorized {
			t.Errorf("cookie %q: %d", bad, r.Status)
		}
	}
	if !hasAll(auditActions(t, app, app.MerchantID), "user.logout") {
		t.Fatal("logout not audited")
	}
}

func TestDashboardLoginRateLimit(t *testing.T) {
	t.Parallel()
	limits := map[string]api.Limit{}
	for route := range api.DefaultLimits() {
		limits[route] = api.Limit{PerMinute: 10_000, Burst: 10_000}
	}
	limits[api.RouteDashboardLogin] = api.Limit{PerMinute: 4, Burst: 4}
	limits[api.RouteDashboardLoginEmailIP] = api.Limit{PerMinute: 2, Burst: 2}
	limits[api.RouteDashboardLoginEmail] = api.Limit{PerMinute: 3, Burst: 3}
	app, _ := dashApp(t, &api.PublicOptions{Limits: limits, ClientIPHeader: "X-Real-IP"})
	c := newClient(t, app)
	try := func(ip, email, pw string) int {
		return c.do("POST", "/v1/dashboard/auth/login", map[string]any{"email": email, "password": pw}, "X-Real-IP", ip).Status
	}
	// Per account and address: two guesses, then 429 for that address only.
	if s := try("203.0.113.1", "owner@example.com", "guess one"); s != http.StatusUnauthorized {
		t.Fatalf("guess 1: %d", s)
	}
	if s := try("203.0.113.1", "OWNER@example.com", "guess two"); s != http.StatusUnauthorized {
		t.Fatalf("guess 2: %d", s)
	}
	r := c.do("POST", "/v1/dashboard/auth/login", map[string]any{"email": "owner@example.com", "password": ownerPassword}, "X-Real-IP", "203.0.113.1")
	if r.Status != http.StatusTooManyRequests || r.ErrorCode(t) != "rate_limited" || r.Header.Get("Retry-After") == "" {
		t.Fatalf("third try from one address: %d %s", r.Status, r.Body)
	}
	// The owner elsewhere still signs in: one address cannot hold the account at 429.
	owner := newClient(t, app)
	if r := owner.do("POST", "/v1/dashboard/auth/login", map[string]any{"email": "owner@example.com", "password": ownerPassword}, "X-Real-IP", "203.0.113.3"); r.Status != http.StatusOK {
		t.Fatalf("owner from another address: %d %s", r.Status, r.Body)
	}
	// Per account over every address: the loose bucket (three here) is spent, the refused third
	// try took nothing from it.
	if s := try("203.0.113.4", "owner@example.com", "guess three"); s != http.StatusTooManyRequests {
		t.Fatalf("fourth try on one account: %d", s)
	}
	// Per address: four tries over any accounts.
	for i, email := range []string{"a@example.com", "b@example.com"} {
		if s := try("198.51.100.9", email, "x"); s != http.StatusUnauthorized {
			t.Fatalf("address try %d: %d", i, s)
		}
	}
	if s := try("198.51.100.9", "c@example.com", "x"); s != http.StatusUnauthorized {
		t.Fatalf("address try 3: %d", s)
	}
	if s := try("198.51.100.9", "d@example.com", "x"); s != http.StatusUnauthorized {
		t.Fatalf("address try 4: %d", s)
	}
	if s := try("198.51.100.9", "e@example.com", "x"); s != http.StatusTooManyRequests {
		t.Fatalf("address try 5: %d", s)
	}
	// The buckets are the shared Postgres ones.
	var n int
	if err := app.Store.Pool().QueryRow(t.Context(), `SELECT count(*) FROM rate_limits WHERE key LIKE 'dashboard_login%'`).Scan(&n); err != nil || n == 0 {
		t.Fatalf("buckets: %d %v", n, err)
	}
	if err := app.Store.Pool().QueryRow(t.Context(), `SELECT count(*) FROM rate_limits WHERE key LIKE '%@%'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("emails stored in bucket keys: %d %v", n, err)
	}
}

func TestDashboardTOTP(t *testing.T) {
	t.Parallel()
	app, userID := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	enroll := c.send("POST", "/v1/dashboard/auth/totp/enroll", map[string]any{"email_code": c.emailCode(service.ConfirmTOTPEnroll)})
	secret := enroll["secret"].(string)
	if len(secret) != 32 || !strings.HasPrefix(enroll["otpauth_url"].(string), "otpauth://totp/Prismaport:owner@example.com?") {
		t.Fatalf("enroll: %v", enroll)
	}
	// Not on until a code proves the app holds the secret.
	if c.get("/v1/dashboard/me")["user"].(map[string]any)["totp_enabled"] != false {
		t.Fatal("enabled before verification")
	}
	login(t, app, "owner@example.com")
	if r := c.do("POST", "/v1/dashboard/auth/totp/verify", map[string]any{"code": "000000"}); r.Status != http.StatusBadRequest || r.ErrorCode(t) != "totp_required" {
		t.Fatalf("wrong code: %d %s", r.Status, r.Body)
	}
	v := c.send("POST", "/v1/dashboard/auth/totp/verify", map[string]any{"code": totpCode(t, app, secret)})
	if v["user"].(map[string]any)["totp_enabled"] != true {
		t.Fatalf("verify: %v", v)
	}
	if r := c.do("POST", "/v1/dashboard/auth/totp/enroll", map[string]any{"password": ownerPassword}); r.Status != http.StatusForbidden || r.ErrorCode(t) != "totp_required" {
		t.Fatalf("enroll while enabled without a code: %d %s", r.Status, r.Body)
	}
	var stored []byte
	if err := app.Store.Pool().QueryRow(t.Context(), `SELECT totp_secret_ciphertext FROM dashboard_users WHERE id = $1`, userID).Scan(&stored); err != nil ||
		strings.Contains(string(stored), secret) {
		t.Fatalf("secret stored in the clear: %v", err)
	}

	x := newClient(t, app)
	// With 2FA on, the right password without a code (or with a wrong one) asks for the code by
	// name, so the page shows the code step only to accounts that have one. A wrong password
	// still answers like an unknown email, code or not.
	loginAs := func(password string, code ...string) apptest.Response {
		body := map[string]any{"email": "owner@example.com", "password": password}
		if len(code) > 0 {
			body["totp"] = code[0]
		}
		return x.do("POST", "/v1/dashboard/auth/login", body)
	}
	rightNoCode, wrongNoCode := loginAs(ownerPassword), loginAs("wrong password!")
	if string(rightNoCode.Body) != `{"error":{"code":"totp_required","message":"Enter the 6-digit code from your authenticator app."}}` ||
		rightNoCode.Status != http.StatusUnauthorized || x.cookie != nil {
		t.Fatalf("login without code: %d %s", rightNoCode.Status, rightNoCode.Body)
	}
	if wrongNoCode.Status != http.StatusUnauthorized || wrongNoCode.ErrorCode(t) != "unauthenticated" {
		t.Fatalf("wrong password: %d %s", wrongNoCode.Status, wrongNoCode.Body)
	}
	rightBadCode, wrongWithCode := loginAs(ownerPassword, "000000"), loginAs("wrong password!", "123456")
	if rightBadCode.Status != http.StatusUnauthorized || rightBadCode.ErrorCode(t) != "totp_required" ||
		!strings.Contains(string(rightBadCode.Body), "That code is not valid.") || rightBadCode.Header.Get("Retry-After") != "" {
		t.Fatalf("wrong code: %d %s", rightBadCode.Status, rightBadCode.Body)
	}
	if string(wrongWithCode.Body) != string(wrongNoCode.Body) {
		t.Fatalf("wrong password with a code: %s", wrongWithCode.Body)
	}
	// The code used to enable is spent: the same step cannot sign in.
	if r := loginAs(ownerPassword, totpCode(t, app, secret)); r.Status != http.StatusUnauthorized || r.ErrorCode(t) != "totp_required" {
		t.Fatalf("replayed code: %d %s", r.Status, r.Body)
	}
	app.Clock.Advance(30 * time.Second)
	code := totpCode(t, app, secret)
	login(t, app, "owner@example.com", code)
	if r := x.do("POST", "/v1/dashboard/auth/login", map[string]any{"email": "owner@example.com", "password": ownerPassword, "totp": code}); r.Status != http.StatusUnauthorized {
		t.Fatalf("code replayed after a login: %d %s", r.Status, r.Body)
	}

	app.Clock.Advance(30 * time.Second)
	// D58: the password confirms nothing here, and an emailed code is not a field of this route.
	if r := c.do("POST", "/v1/dashboard/auth/totp/disable", map[string]any{"email_code": "123456"}); r.Status != http.StatusBadRequest {
		t.Fatalf("disable with an emailed code: %d %s", r.Status, r.Body)
	}
	if r := c.do("POST", "/v1/dashboard/auth/totp/disable", map[string]any{"code": "000000"}); r.Status != http.StatusForbidden || r.ErrorCode(t) != "totp_required" {
		t.Fatalf("disable with a wrong code: %d %s", r.Status, r.Body)
	}
	if r := c.do("POST", "/v1/dashboard/auth/totp/disable", map[string]any{"password": ownerPassword}); r.Status != http.StatusBadRequest {
		t.Fatalf("disable without a code: %d %s", r.Status, r.Body)
	}
	d := c.send("POST", "/v1/dashboard/auth/totp/disable", map[string]any{"code": totpCode(t, app, secret)})
	if d["user"].(map[string]any)["totp_enabled"] != false {
		t.Fatalf("disable: %v", d)
	}
	login(t, app, "owner@example.com")
	if !hasAll(auditActions(t, app, app.MerchantID), "user.totp_enabled", "user.totp_disabled", "user.step_up_failed") {
		t.Fatalf("audit: %v", auditActions(t, app, app.MerchantID))
	}
}

func TestDashboardTOTPRateLimit(t *testing.T) {
	t.Parallel()
	limits := map[string]api.Limit{}
	for route := range api.DefaultLimits() {
		limits[route] = api.Limit{PerMinute: 10_000, Burst: 10_000}
	}
	limits[api.RouteDashboardTOTP] = api.Limit{PerMinute: 3, Burst: 3}
	app, _ := dashApp(t, &api.PublicOptions{Limits: limits})
	c := login(t, app, "owner@example.com")
	c.send("POST", "/v1/dashboard/auth/totp/enroll", map[string]any{"email_code": c.emailCode(service.ConfirmTOTPEnroll)})
	for i := range 2 {
		if r := c.do("POST", "/v1/dashboard/auth/totp/verify", map[string]any{"code": "000000"}); r.Status != http.StatusBadRequest {
			t.Fatalf("attempt %d: %d %s", i, r.Status, r.Body)
		}
	}
	if r := c.do("POST", "/v1/dashboard/auth/totp/verify", map[string]any{"code": "000000"}); r.Status != http.StatusTooManyRequests {
		t.Fatalf("brute force not limited: %d %s", r.Status, r.Body)
	}

	// Enrollment checks a code, so it spends a bucket too.
	if _, err := app.Svc.CreateDashboardUser(t.Context(), app.MerchantID, "second@example.com", ownerPassword, "test"); err != nil {
		t.Fatal(err)
	}
	x := login(t, app, "second@example.com")
	for i := range 3 {
		if r := x.do("POST", "/v1/dashboard/auth/totp/enroll", map[string]any{"email_code": "000000"}); r.Status != http.StatusForbidden {
			t.Fatalf("enroll attempt %d: %d %s", i, r.Status, r.Body)
		}
	}
	if r := x.do("POST", "/v1/dashboard/auth/totp/enroll", map[string]any{"email_code": "111111"}); r.Status != http.StatusTooManyRequests {
		t.Fatalf("code guessing through enrollment not limited: %d %s", r.Status, r.Body)
	}
}

// Wrong TOTP codes after a correct password are counted per user across login and every
// step-up: the first few are free, then each makes the next attempt wait longer, and the owner
// is emailed once per run. A code is never checked during the wait, and an accepted code starts
// over. A wrong password never reaches the count.
func TestDashboardTOTPBackoff(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	secret := c.enable2FA()
	app.Clock.Advance(30 * time.Second)
	alerts := func() int {
		n := 0
		for _, m := range app.Mail.To("owner@example.com") {
			if m.Kind == mail.KindTOTPFailures {
				n++
			}
		}
		return n
	}
	x := newClient(t, app)
	loginWith := func(password, code string) apptest.Response {
		return x.do("POST", "/v1/dashboard/auth/login", map[string]any{"email": "owner@example.com", "password": password, "totp": code})
	}
	waited := func(r apptest.Response, seconds string) bool {
		return r.Status == http.StatusTooManyRequests && r.ErrorCode(t) == "rate_limited" && r.Header.Get("Retry-After") == seconds
	}

	for i := range service.TOTPFreeFailures + 2 {
		if r := loginWith("wrong password!", "000000"); r.Status != http.StatusUnauthorized || r.ErrorCode(t) != "unauthenticated" {
			t.Fatalf("wrong password %d: %d %s", i, r.Status, r.Body)
		}
	}
	// With the right password a wrong code is named, and the wait shows as at a step-up.
	refused := func(r apptest.Response) bool {
		return r.Status == http.StatusUnauthorized && r.ErrorCode(t) == "totp_required" && r.Header.Get("Retry-After") == "" && x.cookie == nil
	}
	for i := range service.TOTPFreeFailures {
		if alerts() != 0 {
			t.Fatalf("owner emailed after %d wrong codes", i)
		}
		if r := loginWith(ownerPassword, "000000"); !refused(r) {
			t.Fatalf("wrong code %d at login: %d %s", i, r.Status, r.Body)
		}
	}
	if alerts() != 1 {
		t.Fatalf("owner emails after %d wrong codes: %d", service.TOTPFreeFailures, alerts())
	}
	alert := app.Mail.To("owner@example.com")[len(app.Mail.To("owner@example.com"))-1]
	if !strings.Contains(alert.Text, "entered 5 wrong two-factor codes in a row") || !strings.Contains(alert.Text, "/dashboard/forgot-password") {
		t.Fatalf("alert: %s", alert.Text)
	}
	if r := loginWith(ownerPassword, totpCode(t, app, secret)); !waited(r, "60") || x.cookie != nil {
		t.Fatalf("right code during the wait: %d %v %s", r.Status, r.Header.Get("Retry-After"), r.Body)
	}
	// The signed-in owner is not held back by sign-in failures: step-ups count apart.
	codes := c.send("POST", "/v1/dashboard/auth/totp/recovery_codes", map[string]any{"code": totpCode(t, app, secret)})["recovery_codes"].([]any)
	// A recovery code still signs in during the wait, and ends the sign-in run.
	app.Clock.Advance(30 * time.Second)
	if r := loginWith(ownerPassword, "000000"); !waited(r, "30") {
		t.Fatalf("wrong code during the wait: %d %v %s", r.Status, r.Header.Get("Retry-After"), r.Body)
	}
	if r := x.do("POST", "/v1/dashboard/auth/login", map[string]any{"email": "owner@example.com", "password": ownerPassword,
		"recovery_code": "AAAAA-AAAAA"}); !waited(r, "30") {
		t.Fatalf("wrong recovery code during the wait: %d %v %s", r.Status, r.Header.Get("Retry-After"), r.Body)
	}
	if r := x.do("POST", "/v1/dashboard/auth/login", map[string]any{"email": "owner@example.com", "password": ownerPassword,
		"recovery_code": codes[0].(string)}); r.Status != http.StatusOK || x.cookie == nil {
		t.Fatalf("recovery code during the wait: %d %s", r.Status, r.Body)
	}
	app.Clock.Advance(30 * time.Second)
	login(t, app, "owner@example.com", totpCode(t, app, secret))

	// Step-ups make their own run: five wrong codes there start a wait for step-ups only.
	app.Clock.Advance(30 * time.Second)
	disable := func(code string) apptest.Response {
		return c.do("POST", "/v1/dashboard/auth/totp/disable", map[string]any{"code": code})
	}
	for i := range service.TOTPFreeFailures {
		if r := disable("000000"); r.Status != http.StatusForbidden || r.ErrorCode(t) != "totp_required" {
			t.Fatalf("wrong code %d on a step-up: %d %s", i, r.Status, r.Body)
		}
	}
	if alerts() != 2 {
		t.Fatalf("owner emails after the step-up run: %d", alerts())
	}
	if r := disable(totpCode(t, app, secret)); !waited(r, "60") {
		t.Fatalf("right code on a step-up during the wait: %d %v %s", r.Status, r.Header.Get("Retry-After"), r.Body)
	}
	app.Clock.Advance(30 * time.Second)
	login(t, app, "owner@example.com", totpCode(t, app, secret))
	if c.get("/v1/dashboard/me")["user"].(map[string]any)["totp_enabled"] != true {
		t.Fatal("a step-up refused during the wait changed the account")
	}
	// Each further wrong code doubles the wait.
	app.Clock.Advance(30 * time.Second)
	if r := disable("000000"); r.Status != http.StatusForbidden {
		t.Fatalf("sixth wrong code: %d %s", r.Status, r.Body)
	}
	if r := disable(totpCode(t, app, secret)); !waited(r, "120") {
		t.Fatalf("second wait: %d %v %s", r.Status, r.Header.Get("Retry-After"), r.Body)
	}
	if alerts() != 2 {
		t.Fatalf("emailed again inside one run: %d", alerts())
	}
	app.Clock.Advance(2 * time.Minute)
	d := c.send("POST", "/v1/dashboard/auth/totp/disable", map[string]any{"code": totpCode(t, app, secret)})
	if d["user"].(map[string]any)["totp_enabled"] != false {
		t.Fatalf("disable after the wait: %v", d)
	}
}

// A stolen session cannot put its own authenticator on the account: turning 2FA on takes a code
// emailed to the owner (D58), and only the session that started it can finish it, within the
// window.
func TestDashboardTOTPEnrollmentStepUp(t *testing.T) {
	t.Parallel()
	app, userID := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	pending := func() bool {
		t.Helper()
		var n int
		if err := app.Store.Pool().QueryRow(t.Context(), `SELECT count(*) FROM dashboard_users WHERE id = $1 AND totp_pending_ciphertext IS NOT NULL`,
			userID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	for name, body := range map[string]any{"no body": nil, "no code": map[string]any{}, "empty code": map[string]any{"email_code": ""},
		"the password": map[string]any{"password": ownerPassword}, "an app code": map[string]any{"code": "123456"}} {
		t.Run(name, func(t *testing.T) {
			if r := c.do("POST", "/v1/dashboard/auth/totp/enroll", body); r.Status != http.StatusForbidden || r.ErrorCode(t) != service.CodeConfirmationRequired {
				t.Errorf("%s: %d %s", name, r.Status, r.Body)
			}
		})
	}
	if r := c.do("POST", "/v1/dashboard/auth/totp/enroll", map[string]any{"email_code": "000000"}); r.Status != http.StatusForbidden || r.ErrorCode(t) != service.CodeInvalidCode {
		t.Errorf("wrong code: %d %s", r.Status, r.Body)
	}
	if r := c.do("POST", "/v1/dashboard/auth/totp/enroll", map[string]any{"email_code": "000000", "extra": true}); r.Status != http.StatusBadRequest {
		t.Fatalf("unknown field: %d %s", r.Status, r.Body)
	}
	// A code sent for another action does not turn 2FA on.
	if r := c.do("POST", "/v1/dashboard/auth/totp/enroll", map[string]any{"email_code": c.emailCode(service.ConfirmWalletCreate)}); r.Status != http.StatusForbidden || r.ErrorCode(t) != service.CodeInvalidCode {
		t.Errorf("another action's code: %d %s", r.Status, r.Body)
	}
	if pending() {
		t.Fatal("a refused enrollment stored a secret")
	}
	if !hasAll(auditActions(t, app, app.MerchantID), "user.step_up_failed") {
		t.Fatalf("wrong code not audited: %v", auditActions(t, app, app.MerchantID))
	}

	enroll := c.send("POST", "/v1/dashboard/auth/totp/enroll", map[string]any{"email_code": c.emailCode(service.ConfirmTOTPEnroll)})
	secret := enroll["secret"].(string)
	if !pending() || !hasAll(auditActions(t, app, app.MerchantID), "user.totp_enrollment_started") {
		t.Fatalf("enrollment not stored or audited: %v", auditActions(t, app, app.MerchantID))
	}
	// Another session of the same user cannot finish it, even with the right code.
	other := login(t, app, "owner@example.com")
	if r := other.do("POST", "/v1/dashboard/auth/totp/verify", map[string]any{"code": totpCode(t, app, secret)}); r.Status != http.StatusBadRequest ||
		r.ErrorCode(t) != "invalid_request" {
		t.Fatalf("verify from another session: %d %s", r.Status, r.Body)
	}
	if other.get("/v1/dashboard/me")["user"].(map[string]any)["totp_enabled"] != false {
		t.Fatal("another session enabled 2FA")
	}
	// Past the window the enrollment is gone.
	app.Clock.Advance(service.TOTPEnrollmentWindow)
	if r := c.do("POST", "/v1/dashboard/auth/totp/verify", map[string]any{"code": totpCode(t, app, secret)}); r.Status != http.StatusBadRequest ||
		r.ErrorCode(t) != "invalid_request" {
		t.Fatalf("verify after the window: %d %s", r.Status, r.Body)
	}
	// After a logout, the next session cannot finish what the previous one started.
	enroll = c.send("POST", "/v1/dashboard/auth/totp/enroll", map[string]any{"email_code": c.emailCode(service.ConfirmTOTPEnroll)})
	c.send("POST", "/v1/dashboard/auth/logout", nil)
	c = login(t, app, "owner@example.com")
	if r := c.do("POST", "/v1/dashboard/auth/totp/verify", map[string]any{"code": totpCode(t, app, enroll["secret"].(string))}); r.Status != http.StatusBadRequest {
		t.Fatalf("verify from the next session: %d %s", r.Status, r.Body)
	}
	if c.get("/v1/dashboard/me")["user"].(map[string]any)["totp_enabled"] != false {
		t.Fatal("2FA enabled without a finished enrollment")
	}
}

// Replacing the authenticator while 2FA is on takes a current code, never the password or an
// emailed code (D58); the old app keeps working until a code from the new one finishes the
// switch.
func TestDashboardTOTPReplace(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	old := c.enable2FA()
	app.Clock.Advance(30 * time.Second)

	for name, body := range map[string]any{"the password": map[string]any{"password": ownerPassword}, "an emailed code": map[string]any{"email_code": "123456"}} {
		t.Run(name, func(t *testing.T) {
			if r := c.do("POST", "/v1/dashboard/auth/totp/enroll", body); r.Status != http.StatusForbidden || r.ErrorCode(t) != "totp_required" {
				t.Fatalf("replace with %s: %d %s", name, r.Status, r.Body)
			}
		})
	}
	if r := c.do("POST", "/v1/dashboard/confirmations", map[string]any{"purpose": "totp_enroll"}); r.Status != http.StatusBadRequest {
		t.Fatalf("an emailed code to replace the app: %d %s", r.Status, r.Body)
	}
	if r := c.do("POST", "/v1/dashboard/auth/totp/enroll", map[string]any{"code": "000000"}); r.Status != http.StatusForbidden ||
		r.ErrorCode(t) != "totp_required" {
		t.Fatalf("replace with a wrong code: %d %s", r.Status, r.Body)
	}
	repl := c.send("POST", "/v1/dashboard/auth/totp/enroll", map[string]any{"code": totpCode(t, app, old)})
	next := repl["secret"].(string)
	if next == old {
		t.Fatal("the replacement reused the secret")
	}
	// The code that authorized the replacement is spent.
	if r := c.do("POST", "/v1/dashboard/auth/totp/enroll", map[string]any{"code": totpCode(t, app, old)}); r.Status != http.StatusForbidden {
		t.Fatalf("step-up code replayed: %d %s", r.Status, r.Body)
	}

	// Until verified, sign-in still takes the old app.
	app.Clock.Advance(30 * time.Second)
	login(t, app, "owner@example.com", totpCode(t, app, old))
	if r := c.do("POST", "/v1/dashboard/auth/totp/verify", map[string]any{"code": totpCode(t, app, old)}); r.Status != http.StatusBadRequest ||
		r.ErrorCode(t) != "totp_required" {
		t.Fatalf("verify with the old app's code: %d %s", r.Status, r.Body)
	}
	v := c.send("POST", "/v1/dashboard/auth/totp/verify", map[string]any{"code": totpCode(t, app, next)})
	if v["user"].(map[string]any)["totp_enabled"] != true {
		t.Fatalf("verify: %v", v)
	}
	app.Clock.Advance(30 * time.Second)
	x := newClient(t, app)
	if r := x.do("POST", "/v1/dashboard/auth/login", map[string]any{"email": "owner@example.com", "password": ownerPassword, "totp": totpCode(t, app, old)}); r.Status != http.StatusUnauthorized {
		t.Fatalf("old app still signs in: %d %s", r.Status, r.Body)
	}
	login(t, app, "owner@example.com", totpCode(t, app, next))
	if !hasAll(auditActions(t, app, app.MerchantID), "user.totp_enabled", "user.totp_enrollment_started", "user.totp_enrollment_started",
		"user.totp_replaced", "user.step_up_failed") {
		t.Fatalf("audit: %v", auditActions(t, app, app.MerchantID))
	}
}
