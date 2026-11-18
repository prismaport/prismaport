//go:build integration

package api_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prismaport/prismaport/internal/api"
	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/checkout"
	"github.com/prismaport/prismaport/internal/mail"
)

const (
	splitAPIHost   = "api.example.test"
	splitAppOrigin = "https://app.example.test"
)

var splitTestHosts = api.Hosts{
	Site:      "https://example.test",
	Dashboard: splitAppOrigin + "/dashboard/",
	Checkout:  apptest.CheckoutBase,
	API:       "https://" + splitAPIHost,
	Docs:      "https://docs.example.test",
	External:  true,
}

// splitApp is the api as production runs it in split mode: the API only, no page mounted.
func splitApp(t *testing.T) *apptest.App {
	t.Helper()
	app := apptest.New(t, apptest.Options{APIBaseURL: splitTestHosts.API})
	h := api.New(app.Svc, app.Store, nil)
	o := api.PublicOptions{Limits: map[string]api.Limit{}}
	for route := range api.DefaultLimits() {
		o.Limits[route] = api.Limit{PerMinute: 10_000, Burst: 10_000}
	}
	if err := h.ConfigurePublic(o); err != nil {
		t.Fatal(err)
	}
	if err := h.ConfigureHosts(splitTestHosts); err != nil {
		t.Fatal(err)
	}
	h.MountRoutes(checkout.New(app.Svc, app.Store, nil), app.Dispatcher)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Host = splitAPIHost
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	app.Server = srv
	if _, err := app.Svc.CreateDashboardUser(t.Context(), app.MerchantID, "owner@example.com", ownerPassword, "test"); err != nil {
		t.Fatal(err)
	}
	return app
}

// fromDashboard is a browser on the dashboard's origin calling the API's.
func fromDashboard(t *testing.T, app *apptest.App) *dashClient {
	t.Helper()
	c := newClient(t, app)
	c.header.Set("Origin", splitAppOrigin)
	c.header.Set("Sec-Fetch-Site", "same-site")
	return c
}

func credentialedCORS(t *testing.T, what string, r apptest.Response) {
	t.Helper()
	if r.Header.Get("Access-Control-Allow-Origin") != splitAppOrigin || r.Header.Get("Access-Control-Allow-Credentials") != "true" ||
		r.Header.Get("Vary") != "Origin" {
		t.Errorf("%s: CORS %v", what, r.Header)
	}
}

func TestSplitDashboardAcrossOrigins(t *testing.T) {
	t.Parallel()
	app := splitApp(t)
	c := fromDashboard(t, app)

	r := signup(t, c, "Split Shop", "split@example.com", ownerPassword)
	if r.Status != http.StatusOK {
		t.Fatalf("signup from the dashboard's origin: %d %s", r.Status, r.Body)
	}
	credentialedCORS(t, "signup", r)
	r = c.do("POST", "/v1/dashboard/auth/verify-email", map[string]any{"email": "split@example.com", "code": lastCode(t, app, "split@example.com", mail.KindVerifyEmail), "password": ownerPassword})
	if r.Status != http.StatusOK {
		t.Fatalf("verify: %d %s", r.Status, r.Body)
	}
	credentialedCORS(t, "verify", r)
	r = c.do("POST", "/v1/dashboard/auth/login", map[string]any{"email": "split@example.com", "password": ownerPassword})
	if r.Status != http.StatusOK {
		t.Fatalf("login: %d %s", r.Status, r.Body)
	}
	credentialedCORS(t, "login", r)
	setCookie := r.Header.Get("Set-Cookie")
	for _, want := range []string{api.SessionCookie + "=", "Path=/v1/dashboard", "HttpOnly", "Secure", "SameSite=Lax"} {
		if !strings.Contains(setCookie, want) {
			t.Errorf("Set-Cookie lacks %s: %s", want, setCookie)
		}
	}
	if strings.Contains(strings.ToLower(setCookie), "domain=") {
		t.Errorf("the session cookie is not host-only: %s", setCookie)
	}

	if me := c.do("GET", "/v1/dashboard/me", nil); me.Status != http.StatusOK {
		t.Fatalf("me after signing up: %d %s", me.Status, me.Body)
	}

	// The seeded merchant has wallets, so its products can be created.
	c = fromDashboard(t, app)
	if r := c.do("POST", "/v1/dashboard/auth/login", map[string]any{"email": "owner@example.com", "password": ownerPassword}); r.Status != http.StatusOK {
		t.Fatalf("login: %d %s", r.Status, r.Body)
	}
	me := c.do("GET", "/v1/dashboard/me", nil)
	if me.Status != http.StatusOK {
		t.Fatalf("me: %d %s", me.Status, me.Body)
	}
	credentialedCORS(t, "me", me)
	c.csrf = me.JSON(t)["csrf_token"].(string)
	body := map[string]any{"name": "Beans", "price_type": "fixed", "amount": "25.00"}
	created := c.do("POST", "/v1/dashboard/products?mode=test", body, "Idempotency-Key", "split-1")
	if created.Status != http.StatusOK {
		t.Fatalf("create a product: %d %s", created.Status, created.Body)
	}
	credentialedCORS(t, "a write", created)
	if again := c.do("POST", "/v1/dashboard/products?mode=test", body, "Idempotency-Key", "split-1"); again.Header.Get("Idempotent-Replayed") != "true" ||
		!strings.Contains(again.Header.Get("Access-Control-Expose-Headers"), "Idempotent-Replayed") {
		t.Errorf("replay: %v", again.Header)
	}

	// The token is still required from the trusted origin.
	csrf := c.csrf
	c.csrf = ""
	if r := c.do("POST", "/v1/dashboard/products?mode=test", body, "Idempotency-Key", "split-2"); r.Status != http.StatusForbidden || r.ErrorCode(t) != "csrf_failed" {
		t.Errorf("a write without the token: %d %s", r.Status, r.Body)
	}
	c.csrf = csrf

	// Any other page, same-site or not, is refused before anything runs and gets no CORS; the
	// dashboard's origin marked cross-site is refused too.
	for _, from := range []struct{ origin, site string }{
		{apptest.CheckoutBase, "same-site"},
		{"https://example.test", "same-site"},
		{"https://evil.example", "cross-site"},
		{splitAppOrigin, "cross-site"},
		{"https://evil.example", ""},
		{"", "same-site"},
	} {
		t.Run(from.origin, func(t *testing.T) {
			c.header.Set("Origin", from.origin)
			c.header.Set("Sec-Fetch-Site", from.site)
			for _, req := range []struct{ method, path string }{
				{"GET", "/v1/dashboard/me"},
				{"POST", "/v1/dashboard/products?mode=test"},
				{"POST", "/v1/dashboard/auth/login"},
				{"POST", "/v1/dashboard/auth/signup"},
				{"POST", "/v1/dashboard/auth/password/forgot"},
			} {
				r := c.do(req.method, req.path, body, "Idempotency-Key", "split-3")
				cors := r.Header.Get("Access-Control-Allow-Origin")
				if r.Status != http.StatusForbidden || r.ErrorCode(t) != "csrf_failed" || (from.origin != splitAppOrigin && cors != "") {
					t.Errorf("%s %s from %q (%s): %d %s %v", req.method, req.path, from.origin, from.site, r.Status, r.Body, r.Header)
				}
			}
		})
	}

	c.header.Set("Origin", splitAppOrigin)
	c.header.Set("Sec-Fetch-Site", "same-site")
	if r := c.do("GET", "/v1/dashboard/overview?mode=test", nil); r.Status != http.StatusOK {
		t.Errorf("overview: %d %s", r.Status, r.Body)
	}
}

func TestSplitPreflightsAndPublicRoutes(t *testing.T) {
	t.Parallel()
	app := splitApp(t)

	pre := raw(t, app, "OPTIONS", splitAPIHost, "/v1/dashboard/auth/login", nil, "Origin", splitAppOrigin,
		"Access-Control-Request-Method", "POST", "Access-Control-Request-Headers", "content-type")
	if pre.Status != http.StatusNoContent || pre.Header.Get("Access-Control-Allow-Origin") != splitAppOrigin ||
		!strings.Contains(pre.Header.Get("Access-Control-Allow-Headers"), "X-CSRF-Token") {
		t.Errorf("dashboard preflight: %d %v", pre.Status, pre.Header)
	}
	if pre := raw(t, app, "OPTIONS", splitAPIHost, "/v1/dashboard/auth/login", nil, "Origin", apptest.CheckoutBase,
		"Access-Control-Request-Method", "POST"); pre.Status != http.StatusForbidden || pre.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("dashboard preflight from the checkout: %d %v", pre.Status, pre.Header)
	}

	// The hosted checkout calls the public API from its own origin, without credentials.
	r := raw(t, app, "GET", splitAPIHost, "/v1/checkout/networks", nil, "Origin", apptest.CheckoutBase, "Sec-Fetch-Site", "same-site")
	if r.Status != http.StatusOK || r.Header.Get("Access-Control-Allow-Origin") != "*" || r.Header.Get("Access-Control-Allow-Credentials") != "" ||
		r.Header.Get("Access-Control-Expose-Headers") != "Date, Retry-After" {
		t.Errorf("checkout API: %d %v", r.Status, r.Header)
	}

	// Server-to-server routes answer no CORS, whatever the Origin.
	for _, req := range []struct{ method, path string }{
		{"GET", "/v1/products"},
		{"POST", "/v1/sandbox/inbox/unknown"},
		{"OPTIONS", "/v1/products"},
	} {
		t.Run(req.path, func(t *testing.T) {
			r := raw(t, app, req.method, splitAPIHost, req.path, map[string]any{}, "Origin", splitAppOrigin, "Authorization", "Bearer "+app.TestKey)
			if r.Header.Get("Access-Control-Allow-Origin") != "" || r.Header.Get("Access-Control-Allow-Credentials") != "" {
				t.Errorf("%s %s: %d %v", req.method, req.path, r.Status, r.Header)
			}
		})
	}

	// No page is served; each goes to its host.
	for path, want := range map[string]string{
		"/":                  "https://example.test/",
		"/dashboard/":        splitAppOrigin + "/dashboard/",
		"/docs/":             "https://docs.example.test/",
		"/docs/openapi.json": "https://docs.example.test/openapi.json",
		"/l/plink_01ABC":     apptest.CheckoutBase + "/l/plink_01ABC",
		"/site/landing.css":  "https://example.test/site/landing.css",
	} {
		t.Run(path, func(t *testing.T) {
			if r := raw(t, app, "GET", splitAPIHost, path, nil); r.Header.Get("Location") != want {
				t.Errorf("GET %s: %d %q, want %q", path, r.Status, r.Header.Get("Location"), want)
			}
		})
	}
	if r := raw(t, app, "GET", splitAPIHost, "/readyz", nil); r.Status != http.StatusNoContent {
		t.Errorf("/readyz: %d", r.Status)
	}
}
