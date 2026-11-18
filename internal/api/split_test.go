package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prismaport/prismaport/internal/service"
	landingweb "github.com/prismaport/prismaport/web/landing"
)

var splitHosts = Hosts{
	Site:      "https://prismaport.com",
	Dashboard: "https://app.prismaport.com/dashboard/",
	Checkout:  "https://pay.prismaport.com",
	API:       "https://api.prismaport.com",
	Docs:      "https://docs.prismaport.com",
	External:  true,
}

const appOrigin = "https://app.prismaport.com"

// splitServer is the api in split mode. withPages also mounts every page, as a binary that
// mounted them by mistake would: split mode must still serve none.
func splitServer(t *testing.T, withPages bool) *Server {
	t.Helper()
	s := New(nil, nil, nil)
	if err := s.ConfigureHosts(splitHosts); err != nil {
		t.Fatal(err)
	}
	if withPages {
		if err := s.MountCheckoutPages(shell("checkout")); err != nil {
			t.Fatal(err)
		}
		if err := s.MountDashboardPages(shell("dashboard")); err != nil {
			t.Fatal(err)
		}
		s.MountDocsPages(shell("docs"))
		s.MountLandingPages(landingweb.Site(), "")
	}
	s.mountCheckoutPreflight()
	s.mux.HandleFunc("/v1/dashboard/", func(w http.ResponseWriter, r *http.Request) {
		if err := s.fetchSite(r); err != nil {
			s.writeError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	return s
}

func send(s *Server, method, target string, header map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	req.Host = "api.prismaport.com"
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func TestSplitRefusesIncompleteOrUnsafeConfigurations(t *testing.T) {
	with := func(f func(h *Hosts)) Hosts {
		h := splitHosts
		f(&h)
		return h
	}
	for name, c := range map[string]struct {
		hosts Hosts
		want  string
	}{
		"no site":              {with(func(h *Hosts) { h.Site = "" }), "PRISMAPORT_SITE_URL"},
		"no dashboard":         {with(func(h *Hosts) { h.Dashboard = "" }), "PRISMAPORT_DASHBOARD_URL"},
		"no checkout":          {with(func(h *Hosts) { h.Checkout = "" }), "PRISMAPORT_CHECKOUT_BASE_URL"},
		"no API":               {with(func(h *Hosts) { h.API = "" }), "PRISMAPORT_API_URL"},
		"no docs":              {with(func(h *Hosts) { h.Docs = "" }), "PRISMAPORT_DOCS_URL"},
		"docs over plain http": {with(func(h *Hosts) { h.Docs = "http://docs.prismaport.com" }), "PRISMAPORT_DOCS_URL: the docs URL must use https"},
		"docs with a path":     {with(func(h *Hosts) { h.Docs = "https://prismaport.com/docs/" }), "path"},
		"docs on the site":     {with(func(h *Hosts) { h.Docs = "https://prismaport.com" }), "host of its own"},
		"docs on the checkout": {with(func(h *Hosts) { h.Docs = "https://pay.prismaport.com" }), "host of its own"},
		"API on the docs host": {with(func(h *Hosts) { h.API = "https://docs.prismaport.com" }), "serves a page"},
		"plain http":           {with(func(h *Hosts) { h.API = "http://api.prismaport.com" }), "https"},
		"dashboard path":       {with(func(h *Hosts) { h.Dashboard = "https://app.prismaport.com/" }), "/dashboard/"},
		"API with a path":      {with(func(h *Hosts) { h.API = "https://api.prismaport.com/v1" }), "path"},
		"credentials":          {with(func(h *Hosts) { h.Checkout = "https://u:p@pay.prismaport.com" }), "credentials"},
		"API on the site":      {with(func(h *Hosts) { h.API = "https://prismaport.com" }), "serves a page"},
		"API on the dashboard": {with(func(h *Hosts) { h.API = "https://app.prismaport.com" }), "serves a page"},
		"API on the checkout":  {with(func(h *Hosts) { h.Checkout = "https://api.prismaport.com" }), "serves a page"},
		"cross-site API":       {with(func(h *Hosts) { h.API = "https://api.prismaport.net" }), "same-site"},
		"API under a TLD only": {with(func(h *Hosts) { h.Dashboard = "https://app.com/dashboard/"; h.API = "https://api.com" }), "same-site"},
		"mixed schemes": {with(func(h *Hosts) {
			h.Dashboard = "http://localhost:58582/dashboard/"
			h.API = "https://localhost:58580"
		}), "same-site"},
		"siblings on a public suffix": {with(func(h *Hosts) {
			h.Dashboard = "https://app.co.uk/dashboard/"
			h.API = "https://api.co.uk"
		}), "same-site"},
		"two Railway apps": {with(func(h *Hosts) {
			h.Dashboard = "https://app-x.up.railway.app/dashboard/"
			h.API = "https://api-y.up.railway.app"
		}), "same-site"},
		"two Pages projects": {with(func(h *Hosts) {
			h.Dashboard = "https://prismaport-app.pages.dev/dashboard/"
			h.API = "https://prismaport-api.pages.dev"
		}), "same-site"},
		"*.localhost siblings": {with(func(h *Hosts) {
			h.Dashboard = "http://app.localhost/dashboard/"
			h.API = "http://api.localhost"
		}), "same-site"},
		"loopback names differ": {with(func(h *Hosts) {
			h.Dashboard = "http://localhost:58582/dashboard/"
			h.API = "http://127.0.0.1:58580"
		}), "same-site"},
	} {
		t.Run(name, func(t *testing.T) {
			err := New(nil, nil, nil).ConfigureHosts(c.hosts)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("%s: err = %v, want it to mention %q", name, err, c.want)
			}
		})
	}
}

func TestSplitAcceptsSameSiteHosts(t *testing.T) {
	for name, h := range map[string]Hosts{
		"production": splitHosts,
		"dev": {Site: "https://dev.prismaport.com", Dashboard: "https://app-dev.prismaport.com/dashboard/", Checkout: "https://pay-dev.prismaport.com",
			Docs: "https://docs-dev.prismaport.com", API: "https://api-dev.prismaport.com", External: true},
		"apex dashboard": {Site: "https://prismaport.com", Dashboard: "https://prismaport.io/dashboard/", Checkout: "https://pay.prismaport.com",
			Docs: "https://docs.prismaport.com", API: "https://api.prismaport.io", External: true},
		"one loopback host, other ports": {Site: "http://127.0.0.1:58581", Dashboard: "http://127.0.0.1:58582/dashboard/",
			Checkout: "http://127.0.0.1:58583", Docs: "http://127.0.0.1:58587", API: "http://127.0.0.1:58580", External: true},
		"deeper subdomains": {Site: "https://example.co.uk", Dashboard: "https://app.pp.example.co.uk/dashboard/",
			Checkout: "https://pay.example.co.uk", Docs: "https://docs.example.co.uk", API: "https://api.pp.example.co.uk", External: true},
	} {
		t.Run(name, func(t *testing.T) {
			s := New(nil, nil, nil)
			if err := s.ConfigureHosts(h); err != nil {
				t.Errorf("%s: %v", name, err)
				return
			}
			if s.split == nil || s.hosts != nil {
				t.Errorf("%s: split %v, hosts %v", name, s.split, s.hosts)
			}
		})
	}
	for _, c := range []struct {
		a, b string
		same bool
	}{
		{"prismaport.com", "api.prismaport.com", true},
		{"api.prismaport.com", "prismaport.com", true},
		{"app.prismaport.com", "api.v2.prismaport.com", true},
		{"app.example.co.uk", "api.example.co.uk", true},
		{"a.co.uk", "b.co.uk", false},
		{"app-x.up.railway.app", "api-y.up.railway.app", false},
		{"prismaport.com", "prismaport.net", false},
		{"127.0.0.1", "127.0.0.1", true},
		{"127.0.0.1", "127.0.0.2", false},
		{"localhost", "localhost", true},
		{"app.localhost", "api.localhost", false},
		{"localhost", "127.0.0.1", false},
	} {
		t.Run(c.a, func(t *testing.T) {
			if got := sameSiteHosts(c.a, c.b); got != c.same {
				t.Errorf("sameSiteHosts(%q, %q) = %v, want %v", c.a, c.b, got, c.same)
			}
		})
	}
}

func TestSplitServesNoPages(t *testing.T) {
	for _, withPages := range []bool{false, true} {
		s := splitServer(t, withPages)
		for _, c := range []struct{ target, status, location string }{
			{"/", "302", "https://prismaport.com/"},
			{"/?ref=x", "302", "https://prismaport.com/"},
			{"/site/landing.css", "308", "https://prismaport.com/site/landing.css"},
			{"/docs", "308", "https://docs.prismaport.com/"},
			{"/docs/", "308", "https://docs.prismaport.com/"},
			{"/docs/openapi.json", "308", "https://docs.prismaport.com/openapi.json"},
			{"/docs/openapi.json?v=1", "308", "https://docs.prismaport.com/openapi.json?v=1"},
			{"/docs/assets/index-abc123.js", "308", "https://docs.prismaport.com/assets/index-abc123.js"},
			{"/%64ocs/openapi.json", "308", "https://docs.prismaport.com/openapi.json"},
			{"/docs/./x/../openapi.json", "308", "https://docs.prismaport.com/openapi.json"},
			{"/dashboard", "308", "https://app.prismaport.com/dashboard"},
			{"/dashboard/login?next=%2Fproducts", "308", "https://app.prismaport.com/dashboard/login?next=%2Fproducts"},
			{"/d%61shboard/", "308", "https://app.prismaport.com/d%61shboard/"},
			{"/pay/ses_01ABC", "308", "https://pay.prismaport.com/pay/ses_01ABC"},
			{"/l/plink_01ABC?ref=order-1", "308", "https://pay.prismaport.com/l/plink_01ABC?ref=order-1"},
			{"/s/ses_01ABC", "308", "https://pay.prismaport.com/s/ses_01ABC"},
		} {
			t.Run(c.target, func(t *testing.T) {
				for _, method := range []string{"GET", "HEAD"} {
					rec := send(s, method, c.target, nil)
					if got := rec.Result().Status[:3]; got != c.status || rec.Header().Get("Location") != c.location {
						t.Errorf("pages %v: %s %s: %s to %q, want %s to %q", withPages, method, c.target, got, rec.Header().Get("Location"), c.status, c.location)
					}
					if rec.Header().Get("Cache-Control") != "no-store" {
						t.Errorf("%s %s: Cache-Control %q", method, c.target, rec.Header().Get("Cache-Control"))
					}
				}
			})
		}
		for _, target := range []string{"/", "/dashboard/login", "/pay/ses_01ABC", "/docs/"} {
			rec := send(s, "POST", target, nil)
			if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "no such route on this host") || rec.Header().Get("Location") != "" {
				t.Errorf("POST %s: %d %q", target, rec.Code, rec.Body.String())
			}
		}
		if rec := send(s, "GET", "/healthz", nil); rec.Code != http.StatusNoContent {
			t.Errorf("/healthz: %d", rec.Code)
		}
		if rec := send(s, "GET", "/robots.txt", nil); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), `"not_found"`) {
			t.Errorf("/robots.txt: %d %s", rec.Code, rec.Body.String())
		}
		for _, host := range []string{"api.prismaport.com", "api-example.up.railway.app", "app.prismaport.com"} {
			req := httptest.NewRequest("GET", "/v1/products", nil)
			req.Host = host
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "invalid_api_key") {
				t.Errorf("%s/v1/products: %d %s", host, rec.Code, rec.Body.String())
			}
		}
	}
}

func TestSplitDashboardCORS(t *testing.T) {
	s := splitServer(t, false)

	rec := send(s, "GET", "/v1/dashboard/me", map[string]string{"Origin": appOrigin, "Sec-Fetch-Site": "same-site"})
	h := rec.Header()
	if rec.Code != http.StatusOK || h.Get("Access-Control-Allow-Origin") != appOrigin || h.Get("Access-Control-Allow-Credentials") != "true" ||
		h.Get("Vary") != "Origin" || h.Get("Access-Control-Expose-Headers") != "Retry-After, Idempotent-Replayed" {
		t.Errorf("dashboard origin: %d %v", rec.Code, h)
	}

	for _, origin := range []string{"https://pay.prismaport.com", "https://prismaport.com", "https://evil.example", "null", "https://app.prismaport.com:8443", "http://app.prismaport.com", ""} {
		rec := send(s, "GET", "/v1/dashboard/me", map[string]string{"Origin": origin})
		h := rec.Header()
		if h.Get("Access-Control-Allow-Origin") != "" || h.Get("Access-Control-Allow-Credentials") != "" || h.Get("Vary") != "Origin" {
			t.Errorf("origin %q: %v", origin, h)
		}
	}

	pre := send(s, "OPTIONS", "/v1/dashboard/products?mode=test", map[string]string{"Origin": appOrigin,
		"Access-Control-Request-Method": "POST", "Access-Control-Request-Headers": "content-type,x-csrf-token,idempotency-key"})
	h = pre.Header()
	if pre.Code != http.StatusNoContent || h.Get("Access-Control-Allow-Origin") != appOrigin || h.Get("Access-Control-Allow-Credentials") != "true" ||
		h.Get("Access-Control-Allow-Methods") != "GET, POST, PATCH, PUT, DELETE" || h.Get("Access-Control-Allow-Headers") != "Content-Type, X-CSRF-Token, Idempotency-Key" ||
		h.Get("Access-Control-Max-Age") != "600" {
		t.Errorf("preflight from the dashboard: %d %v", pre.Code, h)
	}
	for _, origin := range []string{"https://pay.prismaport.com", "https://evil.example"} {
		pre := send(s, "OPTIONS", "/v1/dashboard/auth/login", map[string]string{"Origin": origin, "Access-Control-Request-Method": "POST"})
		if pre.Code != http.StatusForbidden || !strings.Contains(pre.Body.String(), "csrf_failed") || pre.Header().Get("Access-Control-Allow-Origin") != "" ||
			pre.Header().Get("Access-Control-Allow-Methods") != "" {
			t.Errorf("preflight from %s: %d %v %s", origin, pre.Code, pre.Header(), pre.Body.String())
		}
	}

	// The public checkout API keeps * without credentials; the merchant API has no CORS.
	pre = send(s, "OPTIONS", "/v1/checkout/sessions/ses_1", map[string]string{"Origin": "https://pay.prismaport.com", "Access-Control-Request-Method": "GET"})
	h = pre.Header()
	if pre.Code != http.StatusNoContent || h.Get("Access-Control-Allow-Origin") != "*" || h.Get("Access-Control-Allow-Credentials") != "" ||
		h.Get("Access-Control-Expose-Headers") != "Date, Retry-After" {
		t.Errorf("checkout preflight: %d %v", pre.Code, h)
	}
	for _, method := range []string{"GET", "OPTIONS"} {
		rec := send(s, method, "/v1/products", map[string]string{"Origin": appOrigin, "Access-Control-Request-Method": "GET"})
		if rec.Header().Get("Access-Control-Allow-Origin") != "" || rec.Header().Get("Access-Control-Allow-Credentials") != "" {
			t.Errorf("merchant API %s: %v", method, rec.Header())
		}
	}
}

func TestEmbeddedModeSendsNoDashboardCORS(t *testing.T) {
	for name, hosts := range map[string]*Hosts{"single host": nil, "host routing": &prodHosts} {
		t.Run(name, func(t *testing.T) {
			s := hostServer(t, hosts)
			req := httptest.NewRequest("GET", "/v1/dashboard/me", nil)
			req.Host = "app.prismaport.com"
			req.Header.Set("Origin", appOrigin)
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, req)
			if rec.Header().Get("Access-Control-Allow-Origin") != "" || rec.Header().Get("Vary") != "" {
				t.Errorf("%s: %v", name, rec.Header())
			}
		})
	}
}

func TestFetchSite(t *testing.T) {
	embedded := New(nil, nil, nil)
	split := New(nil, nil, nil)
	if err := split.ConfigureHosts(splitHosts); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		site, origin    string
		embedded, split bool
	}{
		{"", "", true, true},
		{"none", "", true, true},
		{"same-origin", "", true, true},
		{"same-origin", appOrigin, true, true},
		{"same-origin", "https://api.prismaport.com", true, false},
		{"", "https://evil.example", true, false},
		{"", appOrigin, true, true},
		{"same-site", appOrigin, false, true},
		{"same-site", "", false, false},
		{"same-site", "https://pay.prismaport.com", false, false},
		{"same-site", "https://prismaport.com", false, false},
		{"same-site", "http://app.prismaport.com", false, false},
		{"same-site", "https://app.prismaport.com:444", false, false},
		{"same-site", "null", false, false},
		{"cross-site", appOrigin, false, false},
		{"cross-site", "https://evil.example", false, false},
	} {
		t.Run(c.site, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/v1/dashboard/auth/login", nil)
			if c.site != "" {
				req.Header.Set("Sec-Fetch-Site", c.site)
			}
			if c.origin != "" {
				req.Header.Set("Origin", c.origin)
			}
			for mode, want := range map[*Server]bool{embedded: c.embedded, split: c.split} {
				err := mode.fetchSite(req)
				if (err == nil) != want {
					t.Errorf("split %v, Sec-Fetch-Site %q, Origin %q: err %v, want accepted %v", mode == split, c.site, c.origin, err, want)
				}
				if e := service.AsError(err); err != nil && (e.Code != service.CodeCSRFFailed || e.Kind != service.KindForbidden) {
					t.Errorf("refusal %s %s", e.Kind, e.Code)
				}
			}
		})
	}
}
