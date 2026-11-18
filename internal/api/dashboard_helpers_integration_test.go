//go:build integration

package api_test

import (
	"bytes"
	"crypto/sha1"
	"encoding/base32"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prismaport/prismaport/internal/api"
	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/checkout"
	"github.com/prismaport/prismaport/internal/dashauth"
	"github.com/prismaport/prismaport/internal/mail"
	"github.com/prismaport/prismaport/internal/service"
)

const ownerPassword = "Correct-horse-battery-1"

// dashApp is an app whose server mounts the public checkout, the payment links and the
// dashboard, with one dashboard user for the seeded merchant. opt nil keeps the rate limits out
// of the way.
func dashApp(t *testing.T, opt *api.PublicOptions) (*apptest.App, string) {
	t.Helper()
	return dashAppWith(t, apptest.Options{}, opt)
}

func dashAppWith(t *testing.T, appOpt apptest.Options, opt *api.PublicOptions) (*apptest.App, string) {
	t.Helper()
	app := apptest.New(t, appOpt)
	h := api.New(app.Svc, app.Store, nil)
	o := api.PublicOptions{Limits: map[string]api.Limit{}}
	for route := range api.DefaultLimits() {
		o.Limits[route] = api.Limit{PerMinute: 10_000, Burst: 10_000}
	}
	if opt != nil {
		o = *opt
	}
	if err := h.ConfigurePublic(o); err != nil {
		t.Fatal(err)
	}
	co := checkout.New(app.Svc, app.Store, nil)
	h.MountCheckout(co)
	h.MountCheckoutLinks(co)
	h.MountDashboard()
	h.MountSandbox(app.Dispatcher)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	app.Server = srv
	u, err := app.Svc.CreateDashboardUser(t.Context(), app.MerchantID, "Owner@Example.com", ownerPassword, "test")
	if err != nil {
		t.Fatal(err)
	}
	if u.Email != "owner@example.com" {
		t.Fatalf("email not normalized: %s", u.Email)
	}
	return app, u.ID
}

// secondMerchant creates another merchant with its own dashboard user and API keys.
func secondMerchant(t *testing.T, app *apptest.App, email string) (merchantID, testKey string) {
	t.Helper()
	seed, err := app.Svc.SeedMerchant(t.Context(), service.SeedInput{Name: "Other shop", EVMDestination: "0x1111111111111111111111111111111111111111",
		Actor: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.Svc.CreateDashboardUser(t.Context(), seed.Merchant.ID, email, ownerPassword, "test"); err != nil {
		t.Fatal(err)
	}
	return seed.Merchant.ID, seed.Plaintext
}

// dashClient is a browser: it keeps the session cookie and sends the CSRF token.
type dashClient struct {
	t      *testing.T
	app    *apptest.App
	cookie *http.Cookie
	csrf   string
	header http.Header
}

func newClient(t *testing.T, app *apptest.App) *dashClient {
	return &dashClient{t: t, app: app, header: http.Header{}}
}

// login signs in and fails the test unless it succeeds.
func login(t *testing.T, app *apptest.App, email string, extra ...string) *dashClient {
	t.Helper()
	c := newClient(t, app)
	body := map[string]any{"email": email, "password": ownerPassword}
	if len(extra) > 0 {
		body["totp"] = extra[0]
	}
	r := c.do("POST", "/v1/dashboard/auth/login", body)
	if r.Status != http.StatusOK {
		t.Fatalf("login %s: %d %s", email, r.Status, r.Body)
	}
	return c
}

func (c *dashClient) do(method, path string, body any, headers ...string) apptest.Response {
	c.t.Helper()
	var rdr io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rdr = bytes.NewBufferString(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			c.t.Fatal(err)
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.app.Server.URL+path, rdr)
	if err != nil {
		c.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.cookie != nil {
		req.AddCookie(c.cookie)
	}
	if c.csrf != "" && method != "GET" {
		req.Header.Set(api.CSRFHeader, c.csrf)
	}
	for k, v := range c.header {
		req.Header[k] = v
	}
	if h := c.header.Get("Host"); h != "" {
		req.Host = h
	}
	for i := 0; i+1 < len(headers); i += 2 {
		if headers[i] == "Host" {
			req.Host = headers[i+1]
			continue
		}
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	for _, ck := range resp.Cookies() {
		if ck.Name != api.SessionCookie {
			continue
		}
		if ck.MaxAge < 0 || ck.Value == "" {
			c.cookie = nil
		} else {
			c.cookie = &http.Cookie{Name: ck.Name, Value: ck.Value}
		}
	}
	out := apptest.Response{Status: resp.StatusCode, Header: resp.Header, Body: raw}
	if resp.StatusCode == http.StatusOK && len(raw) > 0 && raw[0] == '{' {
		var m map[string]any
		if json.Unmarshal(raw, &m) == nil {
			if tok, ok := m["csrf_token"].(string); ok {
				c.csrf = tok
			}
		}
	}
	return out
}

func (c *dashClient) get(path string) map[string]any {
	c.t.Helper()
	r := c.do("GET", path, nil)
	if r.Status != http.StatusOK {
		c.t.Fatalf("GET %s: %d %s", path, r.Status, r.Body)
	}
	return r.JSON(c.t)
}

func (c *dashClient) send(method, path string, body any, headers ...string) map[string]any {
	c.t.Helper()
	r := c.do(method, path, body, headers...)
	if r.Status != http.StatusOK {
		c.t.Fatalf("%s %s: %d %s", method, path, r.Status, r.Body)
	}
	return r.JSON(c.t)
}

// emailCode asks for a code that confirms action (in Live mode, but for totp_enroll) and reads
// it from the user's newest email for that action, as the owner would. Asked again within a
// minute, the code already sent is still the one that works.
func (c *dashClient) emailCode(action string) string {
	c.t.Helper()
	path := "/v1/dashboard/confirmations"
	if action != service.ConfirmTOTPEnroll {
		path += "?mode=live"
	}
	if r := c.do("POST", path, map[string]any{"purpose": action}); r.Status != http.StatusOK && r.Status != http.StatusTooManyRequests {
		c.t.Fatalf("confirmation code for %s: %d %s", action, r.Status, r.Body)
	}
	email := c.get("/v1/dashboard/me")["user"].(map[string]any)["email"].(string)
	msgs := c.app.Mail.To(email)
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Kind == mail.KindConfirmationCode && msgs[i].Subject == mail.ConfirmationSubject(action) {
			if m := codeLine.FindStringSubmatch(msgs[i].Text); m != nil {
				return m[1]
			}
		}
	}
	c.t.Fatalf("no confirmation code for %s emailed to %s", action, email)
	return ""
}

// enable2FA turns two-factor authentication on, confirmed by an emailed code and the app's first
// code, and answers the secret.
func (c *dashClient) enable2FA() string {
	c.t.Helper()
	secret := c.send("POST", "/v1/dashboard/auth/totp/enroll", map[string]any{"email_code": c.emailCode(service.ConfirmTOTPEnroll)})["secret"].(string)
	c.send("POST", "/v1/dashboard/auth/totp/verify", map[string]any{"code": totpCode(c.t, c.app, secret)})
	return secret
}

func totpCode(t *testing.T, app *apptest.App, secret string) string {
	t.Helper()
	raw, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	if err != nil {
		t.Fatal(err)
	}
	return dashauth.TOTP(raw, app.Clock.Now(), dashauth.TOTPPeriod, dashauth.TOTPDigits, sha1.New)
}

func auditActions(t *testing.T, app *apptest.App, merchantID string) []string {
	t.Helper()
	list, err := app.Store.AuditLog(t.Context(), merchantID)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range list {
		out = append(out, e.Action)
	}
	return out
}

func hasAll(have []string, want ...string) bool {
	set := map[string]int{}
	for _, h := range have {
		set[h]++
	}
	for _, w := range want {
		if set[w] == 0 {
			return false
		}
		set[w]--
	}
	return true
}
