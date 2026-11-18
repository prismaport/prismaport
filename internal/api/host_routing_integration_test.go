//go:build integration

package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/api"
	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/checkout"
	"github.com/prismaport/prismaport/internal/service"
	"github.com/prismaport/prismaport/internal/webhook"
	landingweb "github.com/prismaport/prismaport/web/landing"
)

const (
	siteHost = "site.test"
	appHost  = "app.test"
	payHost  = "pay.test"
	apiHost  = "api.test"
)

var testHosts = api.Hosts{
	Site:      "https://" + siteHost,
	Dashboard: "https://" + appHost + "/dashboard/",
	Checkout:  apptest.CheckoutBase,
	API:       "https://" + apiHost,
}

// hostApp is dashApp with routing by host on, every surface mounted, and sandbox inboxes on the
// API host.
func hostApp(t *testing.T) *apptest.App {
	t.Helper()
	app := apptest.New(t, apptest.Options{APIBaseURL: testHosts.API})
	h := api.New(app.Svc, app.Store, nil)
	o := api.PublicOptions{Limits: map[string]api.Limit{}}
	for route := range api.DefaultLimits() {
		o.Limits[route] = api.Limit{PerMinute: 10_000, Burst: 10_000}
	}
	if err := h.ConfigurePublic(o); err != nil {
		t.Fatal(err)
	}
	if err := h.ConfigureHosts(testHosts); err != nil {
		t.Fatal(err)
	}
	h.MountRoutes(checkout.New(app.Svc, app.Store, nil), app.Dispatcher)
	h.MountLandingPages(landingweb.Site(), "")
	// Requests that name no host (the apptest helpers) go to the API host.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Host, "127.0.0.1:") {
			r.Host = apiHost
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	app.Server = srv
	if _, err := app.Svc.CreateDashboardUser(t.Context(), app.MerchantID, "owner@example.com", ownerPassword, "test"); err != nil {
		t.Fatal(err)
	}
	return app
}

// raw sends one request to host without following redirects or checking the API contract.
func raw(t *testing.T, app *apptest.App, method, host, path string, body any, headers ...string) apptest.Response {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, app.Server.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return apptest.Response{Status: resp.StatusCode, Header: resp.Header, Body: b}
}

func wrongHost(t *testing.T, what string, r apptest.Response, origin string) {
	t.Helper()
	if r.Status != http.StatusNotFound || !strings.Contains(string(r.Body), `"code":"not_found"`) || !strings.Contains(string(r.Body), "served at "+origin) {
		t.Errorf("%s: %d %s", what, r.Status, r.Body)
	}
}

// The session cookie is set by the dashboard's host for that host only, and the dashboard API
// answers nowhere else.
func TestHostRoutingDashboardSession(t *testing.T) {
	t.Parallel()
	app := hostApp(t)
	creds := map[string]any{"email": "owner@example.com", "password": ownerPassword}

	wrongHost(t, "login on the API host", raw(t, app, "POST", apiHost, "/v1/dashboard/auth/login", creds), "https://"+appHost)
	wrongHost(t, "login on the site", raw(t, app, "POST", siteHost, "/v1/dashboard/auth/login", creds), "https://"+appHost)
	if r := raw(t, app, "POST", appHost, "/v1/dashboard/auth/login", creds, "Sec-Fetch-Site", "same-site"); r.Status != http.StatusForbidden ||
		!strings.Contains(string(r.Body), "csrf_failed") {
		t.Fatalf("login from a sibling host: %d %s", r.Status, r.Body)
	}

	c := newClient(t, app)
	c.header.Set("Sec-Fetch-Site", "same-origin")
	r := c.do("POST", "/v1/dashboard/auth/login", creds, "Host", appHost)
	if r.Status != http.StatusOK {
		t.Fatalf("login on the dashboard host: %d %s", r.Status, r.Body)
	}
	setCookie := r.Header.Get("Set-Cookie")
	for _, want := range []string{api.SessionCookie + "=", "Path=/v1/dashboard", "HttpOnly", "Secure", "SameSite=Lax"} {
		if !strings.Contains(setCookie, want) {
			t.Errorf("Set-Cookie lacks %s: %s", want, setCookie)
		}
	}
	if strings.Contains(strings.ToLower(setCookie), "domain=") {
		t.Errorf("the session cookie is not host-only: %s", setCookie)
	}
	if me := c.do("GET", "/v1/dashboard/me", nil, "Host", appHost); me.Status != http.StatusOK {
		t.Fatalf("me on the dashboard host: %d %s", me.Status, me.Body)
	}
	if r := c.do("POST", "/v1/dashboard/api_keys?mode=test", map[string]any{"name": "x", "mode": "test"}, "Host", appHost, "Sec-Fetch-Site", "same-site"); r.Status != http.StatusForbidden {
		t.Fatalf("a change from a sibling host: %d %s", r.Status, r.Body)
	}
	cookie := api.SessionCookie + "=" + c.cookie.Value
	if r := raw(t, app, "GET", apiHost, "/v1/dashboard/me", nil, "Cookie", cookie); r.Status != http.StatusPermanentRedirect ||
		r.Header.Get("Location") != "https://"+appHost+"/v1/dashboard/me" || strings.Contains(string(r.Body), "owner@example.com") {
		t.Fatalf("me on the API host: %d %v %s", r.Status, r.Header, r.Body)
	}
	wrongHost(t, "logout on the checkout host", raw(t, app, "POST", payHost, "/v1/dashboard/auth/logout", nil, "Cookie", cookie,
		api.CSRFHeader, c.csrf), "https://"+appHost)
	if me := c.do("GET", "/v1/dashboard/me", nil, "Host", appHost); me.Status != http.StatusOK {
		t.Fatalf("the session ended from another host: %d", me.Status)
	}
}

// The merchant API answers on the API host; the checkout API on the checkout and API hosts, with
// the checkout_url on the checkout host.
func TestHostRoutingMerchantAndCheckoutAPI(t *testing.T) {
	t.Parallel()
	app := hostApp(t)
	opened := app.Session(app.TestKey, map[string]any{"amount": "1.00"}, "Idempotency-Key", "ses-1", "Host", apiHost)
	if opened.Status != http.StatusOK {
		t.Fatalf("session on the API host: %d %s", opened.Status, opened.Body)
	}
	ses := opened.JSON(t)
	id := ses["id"].(string)
	u, err := url.Parse(ses["checkout_url"].(string))
	if err != nil || u.Host != payHost || u.Path != "/s/"+id {
		t.Fatalf("checkout_url %v", ses["checkout_url"])
	}

	body := map[string]any{"product_id": app.Link(app.TestKey), "amount": "1.00"}
	for _, host := range []string{payHost, appHost, siteHost} {
		wrongHost(t, "POST /v1/sessions on "+host, raw(t, app, "POST", host, "/v1/sessions", body, "Authorization", "Bearer "+app.TestKey,
			"Idempotency-Key", "k-"+host), "https://"+apiHost)
	}
	if r := raw(t, app, "GET", payHost, "/v1/sessions/"+id, nil, "Authorization", "Bearer "+app.TestKey); r.Status != http.StatusPermanentRedirect ||
		r.Header.Get("Location") != "https://"+apiHost+"/v1/sessions/"+id {
		t.Fatalf("GET session on the checkout host: %d %v", r.Status, r.Header)
	}

	for _, host := range []string{payHost, apiHost} {
		r := app.Do("GET", "/v1/checkout/sessions/"+id, "", nil, "Host", host)
		if r.Status != http.StatusOK || r.Header.Get("Access-Control-Allow-Origin") != "*" {
			t.Fatalf("checkout session on %s: %d %v", host, r.Status, r.Header)
		}
		if r := raw(t, app, "OPTIONS", host, "/v1/checkout/sessions/"+id, nil, "Origin", "https://shop.example", "Access-Control-Request-Method", "POST"); r.Status != http.StatusNoContent {
			t.Fatalf("preflight on %s: %d", host, r.Status)
		}
	}
	link := app.PaymentLink(app.TestKey, app.Link(app.TestKey), map[string]any{"amount": "2.00"})
	if r := app.Do("POST", "/v1/checkout/links/"+link+"/sessions", "", nil, "Idempotency-Key", "pay-1", "Host", payHost); r.Status != http.StatusOK {
		t.Fatalf("open a link session on the checkout host: %d %s", r.Status, r.Body)
	}
	wrongHost(t, "open a link session on the dashboard host", raw(t, app, "POST", appHost, "/v1/checkout/links/"+link+"/sessions", nil,
		"Idempotency-Key", "pay-2"), "https://"+apiHost)
	if r := raw(t, app, "GET", siteHost, "/s/"+id, nil); r.Status != http.StatusPermanentRedirect || r.Header.Get("Location") != "https://"+payHost+"/s/"+id {
		t.Fatalf("checkout_url path on the site: %d %v", r.Status, r.Header)
	}
}

// Inboxes receive on the API host, whose address their URL carries.
func TestHostRoutingSandboxInbox(t *testing.T) {
	t.Parallel()
	app := hostApp(t)
	c := newClient(t, app)
	c.do("POST", "/v1/dashboard/auth/login", map[string]any{"email": "owner@example.com", "password": ownerPassword}, "Host", appHost)
	c.header.Set("Sec-Fetch-Site", "same-origin")
	r := c.do("POST", "/v1/dashboard/sandbox/inbox?mode=test", nil, "Host", appHost)
	if r.Status != http.StatusOK {
		t.Fatalf("create inbox: %d %s", r.Status, r.Body)
	}
	inbox := r.JSON(t)["inbox"].(map[string]any)
	inboxURL := inbox["url"].(string)
	if !strings.HasPrefix(inboxURL, testHosts.API+service.InboxPath) {
		t.Fatalf("inbox url %s", inboxURL)
	}
	u, _ := url.Parse(inboxURL)
	post := func(host string) apptest.Response {
		t.Helper()
		req, err := webhook.NewDeliveryRequest(t.Context(), app.Server.URL+u.Path, "evt_x", []byte(`{"id":"evt_x"}`), time.Now(), inbox["secret"].(string))
		if err != nil {
			t.Fatal(err)
		}
		req.Host = host
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return apptest.Response{Status: resp.StatusCode, Body: b}
	}
	if r := post(apiHost); r.Status != http.StatusOK {
		t.Fatalf("inbox on the API host: %d %s", r.Status, r.Body)
	}
	wrongHost(t, "inbox on the checkout host", post(payHost), "https://"+apiHost)
	c.header.Set("Host", appHost)
	if got := inboxRequests(t, c); len(got) != 1 {
		t.Fatalf("inbox requests: %d", len(got))
	}
}

func TestHostRoutingProbesAndPages(t *testing.T) {
	t.Parallel()
	app := hostApp(t)
	for _, host := range []string{siteHost, "www." + siteHost, appHost, payHost, apiHost, "api-example.up.railway.app", "healthcheck.railway.app"} {
		for _, p := range []string{"/healthz", "/readyz"} {
			if r := raw(t, app, "GET", host, p, nil); r.Status != http.StatusNoContent {
				t.Errorf("%s%s: %d", host, p, r.Status)
			}
		}
	}
	page := raw(t, app, "GET", siteHost, "/", nil)
	if page.Status != http.StatusOK || !strings.Contains(string(page.Body), `href="https://`+appHost+`/dashboard/signup"`) ||
		!strings.Contains(string(page.Body), `href="https://`+apiHost+`/docs/"`) {
		t.Fatalf("landing: %d", page.Status)
	}
	for _, c := range []struct{ host, path, location string }{
		{"www." + siteHost, "/", "https://" + siteHost + "/"},
		{siteHost, "/dashboard/login", "https://" + appHost + "/dashboard/login"},
		{appHost, "/", "/dashboard/"},
		{apiHost, "/", "/docs/"},
		{"api-example.up.railway.app", "/dashboard/", "https://" + appHost + "/dashboard/"},
	} {
		t.Run(c.path, func(t *testing.T) {
			if r := raw(t, app, "GET", c.host, c.path, nil); r.Header.Get("Location") != c.location {
				t.Errorf("%s%s: %d %v", c.host, c.path, r.Status, r.Header)
			}
		})
	}
}
