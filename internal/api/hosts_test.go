package api

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/prismaport/prismaport/internal/version"
	landingweb "github.com/prismaport/prismaport/web/landing"
)

var prodHosts = Hosts{
	Site:      "https://prismaport.com",
	Dashboard: "https://app.prismaport.com/dashboard/",
	Checkout:  "https://pay.prismaport.com",
	API:       "https://api.prismaport.com",
}

func shell(title string) fstest.MapFS {
	return fstest.MapFS{"index.html": {Data: []byte("<!doctype html><title>" + title + "</title>")}, "openapi.json": {Data: []byte(`{}`)}}
}

// hostServer mounts every page like the binary does; hosts nil leaves routing by host off.
func hostServer(t *testing.T, hosts *Hosts) *Server {
	t.Helper()
	s := New(nil, nil, nil)
	if hosts != nil {
		if err := s.ConfigureHosts(*hosts); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.MountCheckoutPages(shell("checkout")); err != nil {
		t.Fatal(err)
	}
	if err := s.MountDashboardPages(shell("dashboard")); err != nil {
		t.Fatal(err)
	}
	s.MountDocsPages(shell("docs"))
	s.MountLandingPages(landingweb.Site(), "")
	// The dashboard API needs a service; a stub stands for it.
	s.mux.HandleFunc("/v1/dashboard/", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
	return s
}

func hit(s *Server, method, host, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	req.Host = host
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

// served is a route answered on the requested host: no cross-host redirect, no wrong-host 404.
func served(rec *httptest.ResponseRecorder) bool {
	loc := rec.Header().Get("Location")
	return !strings.HasPrefix(loc, "http") && !strings.Contains(rec.Body.String(), "no such route on this host")
}

var everySurface = []struct{ path, want string }{
	{"/", "<title>Prismaport ·"},
	{"/dashboard/", "<title>dashboard</title>"},
	{"/dashboard/login", "<title>dashboard</title>"},
	{"/pay/ses_01ABC", "<title>checkout</title>"},
	{"/l/plink_01ABC", "<title>checkout</title>"},
	{"/a/caddr_01ABC", "<title>checkout</title>"},
	{"/docs/", "<title>docs</title>"},
	{"/site/landing.css", ""},
}

func TestWithoutASiteURLEveryHostServesEveryRoute(t *testing.T) {
	for name, hosts := range map[string]*Hosts{
		"not configured": nil,
		"no site URL":    {Dashboard: prodHosts.Dashboard, Checkout: prodHosts.Checkout, API: prodHosts.API},
	} {
		t.Run(name, func(t *testing.T) {
			s := hostServer(t, hosts)
			if s.hosts != nil {
				t.Fatalf("%s: routing by host is on", name)
			}
			for _, host := range []string{"localhost:58180", "127.0.0.1:8080", "api-example-dev.up.railway.app", "app.prismaport.com", "pay.prismaport.com", "www.prismaport.com"} {
				for _, c := range everySurface {
					rec := hit(s, "GET", host, c.path)
					if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), c.want) {
						t.Errorf("%s: %s%s: %d %s", name, host, c.path, rec.Code, rec.Body.String())
					}
				}
				if rec := hit(s, "GET", host, "/s/ses_01ABC"); rec.Code != http.StatusPermanentRedirect || rec.Header().Get("Location") != "/pay/ses_01ABC" {
					t.Errorf("%s: %s/s/: %d %v", name, host, rec.Code, rec.Header())
				}
				if rec := hit(s, "POST", host, "/v1/products"); rec.Code != http.StatusUnauthorized || rec.Header().Get("Strict-Transport-Security") != HSTS {
					t.Errorf("%s: %s POST /v1/products: %d %v", name, host, rec.Code, rec.Header())
				}
				if rec := hit(s, "GET", host, "/healthz"); rec.Code != http.StatusNoContent || rec.Header().Get("Strict-Transport-Security") != HSTS {
					t.Errorf("%s: %s/healthz: %d %v", name, host, rec.Code, rec.Header())
				}
				if rec := hit(s, "GET", host, "/version"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"version":"`+version.Version+`"`) {
					t.Errorf("%s: %s/version: %d %s", name, host, rec.Code, rec.Body.String())
				}
			}
			page := hit(s, "GET", "prismaport.com", "/").Body.String()
			if !strings.Contains(page, `href="/dashboard/signup"`) || !strings.Contains(page, `href="/docs/"`) || strings.Contains(page, "https://app.") {
				t.Errorf("%s: the landing links changed", name)
			}
		})
	}
}

func TestHostRouting(t *testing.T) {
	s := hostServer(t, &prodHosts)
	cases := []struct {
		method, host, path string
		status             int
		location           string
	}{
		// Each host serves its own surface.
		{"GET", "prismaport.com", "/", 200, ""},
		{"GET", "prismaport.com", "/site/landing.css", 200, ""},
		{"GET", "app.prismaport.com", "/dashboard/", 200, ""},
		{"GET", "app.prismaport.com", "/dashboard/settings", 200, ""},
		{"GET", "app.prismaport.com", "/dashboard", 308, "/dashboard/"},
		{"GET", "app.prismaport.com", "/v1/dashboard/me", 401, ""},
		{"POST", "app.prismaport.com", "/v1/dashboard/auth/logout", 401, ""},
		{"GET", "pay.prismaport.com", "/pay/ses_01ABC", 200, ""},
		{"GET", "pay.prismaport.com", "/l/plink_01ABC?ref=order-7", 200, ""},
		{"GET", "pay.prismaport.com", "/s/ses_01ABC", 308, "/pay/ses_01ABC"},
		{"OPTIONS", "pay.prismaport.com", "/v1/checkout/sessions/ses_01ABC", 404, ""},
		{"GET", "api.prismaport.com", "/docs/", 200, ""},
		{"GET", "api.prismaport.com", "/docs/openapi.json", 200, ""},
		{"GET", "api.prismaport.com", "/docs", 308, "/docs/"},
		{"GET", "api.prismaport.com", "/v1/sessions/ses_01ABC", 401, ""},
		{"POST", "api.prismaport.com", "/v1/sessions", 401, ""},
		{"GET", "api.prismaport.com", "/v1/nope", 404, ""},
		// Case, a trailing dot and the default port do not make another host.
		{"GET", "APP.Prismaport.com.", "/dashboard/", 200, ""},
		{"GET", "api.prismaport.com:443", "/docs/", 200, ""},
		// The root of a host without a landing page.
		{"GET", "app.prismaport.com", "/", 302, "/dashboard/"},
		{"GET", "api.prismaport.com", "/", 302, "/docs/"},
		{"GET", "pay.prismaport.com", "/", 308, "https://prismaport.com/"},
		{"POST", "app.prismaport.com", "/", 404, ""},
		// Another surface's GET goes to its host, path and query kept.
		{"GET", "prismaport.com", "/dashboard/login", 308, "https://app.prismaport.com/dashboard/login"},
		{"GET", "prismaport.com", "/dashboard/signup?plan=test", 308, "https://app.prismaport.com/dashboard/signup?plan=test"},
		{"GET", "prismaport.com", "/docs/", 308, "https://api.prismaport.com/docs/"},
		{"HEAD", "prismaport.com", "/docs/", 308, "https://api.prismaport.com/docs/"},
		{"GET", "prismaport.com", "/pay/ses_01ABC", 308, "https://pay.prismaport.com/pay/ses_01ABC"},
		{"GET", "api.prismaport.com", "/dashboard/developers/api-keys", 308, "https://app.prismaport.com/dashboard/developers/api-keys"},
		{"GET", "app.prismaport.com", "/docs/", 308, "https://api.prismaport.com/docs/"},
		{"GET", "app.prismaport.com", "/v1/sessions/ses_01ABC", 308, "https://api.prismaport.com/v1/sessions/ses_01ABC"},
		{"GET", "app.prismaport.com", "/site/landing.css", 308, "https://prismaport.com/site/landing.css"},
		{"GET", "pay.prismaport.com", "/v1/dashboard/me", 308, "https://app.prismaport.com/v1/dashboard/me"},
		{"GET", "api.prismaport.com", "/l/plink_01ABC", 308, "https://pay.prismaport.com/l/plink_01ABC"},
		// The mux's cleaning cannot smuggle a path past its host.
		{"GET", "api.prismaport.com", "/docs/../dashboard/", 308, "https://app.prismaport.com/docs/../dashboard/"},
		{"POST", "api.prismaport.com", "/x/../v1/dashboard/auth/login", 404, ""},
		// Nor can escapes: the owner is looked up on the escaped path, segment by segment, as the mux routes.
		{"GET", "prismaport.com", "/dashboard/..%2F", 308, "https://app.prismaport.com/dashboard/..%2F"},
		{"GET", "prismaport.com", "/dashboard/..%2f", 308, "https://app.prismaport.com/dashboard/..%2f"},
		{"GET", "prismaport.com", "/dashboard/%2e%2e/", 308, "https://app.prismaport.com/dashboard/%2e%2e/"},
		{"GET", "prismaport.com", "/dashboard/..%5C", 308, "https://app.prismaport.com/dashboard/..%5C"},
		{"GET", "prismaport.com", "/d%61shboard/login", 308, "https://app.prismaport.com/d%61shboard/login"},
		{"GET", "app.prismaport.com", "/%64ocs/", 308, "https://api.prismaport.com/%64ocs/"},
		// Any other method there is refused.
		{"POST", "api.prismaport.com", "/v1/dashboard/auth/login", 404, ""},
		{"POST", "app.prismaport.com", "/v1/sessions", 404, ""},
		{"DELETE", "pay.prismaport.com", "/v1/webhook_endpoints/we_1", 404, ""},
		{"POST", "prismaport.com", "/v1/sandbox/inbox/tok", 404, ""},
		// Paths no surface owns are the mux's 404 on every host.
		{"GET", "prismaport.com", "/robots.txt", 404, ""},
		{"GET", "pay.prismaport.com", "/payments", 404, ""},
		{"GET", "api.prismaport.com", "/docsx", 404, ""},
		{"POST", "api.prismaport.com", "/v1%2Fdashboard/auth/login", 404, ""},
		// Probes answer on every host.
		{"GET", "prismaport.com", "/healthz", 204, ""},
		{"GET", "www.prismaport.com", "/healthz", 204, ""},
		{"GET", "api-example.up.railway.app", "/healthz", 204, ""},
		{"GET", "healthcheck.railway.app", "/healthz", 204, ""},
		{"GET", "api.prismaport.com", "/version", 200, ""},
		{"GET", "api-example.up.railway.app", "/version", 200, ""},
		// www goes to the apex.
		{"GET", "www.prismaport.com", "/", 301, "https://prismaport.com/"},
		{"GET", "www.prismaport.com", "/dashboard/login?x=1", 301, "https://prismaport.com/dashboard/login?x=1"},
		{"POST", "www.prismaport.com", "/", 404, ""},
		// Any other host (the generated Railway domain) redirects to the surface's own host.
		{"GET", "api-example.up.railway.app", "/", 308, "https://prismaport.com/"},
		{"GET", "api-example.up.railway.app", "/dashboard/", 308, "https://app.prismaport.com/dashboard/"},
		{"GET", "api-example.up.railway.app", "/v1/checkout/networks", 308, "https://api.prismaport.com/v1/checkout/networks"},
		{"POST", "api-example.up.railway.app", "/v1/sessions", 404, ""},
	}
	muxNotFound := map[string]bool{"/v1/nope": true, "/v1%2Fdashboard/auth/login": true}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			rec := hit(s, c.method, c.host, c.path)
			if rec.Code != c.status || rec.Header().Get("Location") != c.location {
				t.Errorf("%s %s%s: %d %q, want %d %q", c.method, c.host, c.path, rec.Code, rec.Header().Get("Location"), c.status, c.location)
				return
			}
			if c.status == 308 && strings.HasPrefix(c.location, "https://") && rec.Header().Get("Cache-Control") != "no-store" {
				t.Errorf("%s %s%s: cross-host redirect cached: %v", c.method, c.host, c.path, rec.Header())
			}
			if c.status == 404 && c.method != "GET" && c.method != "OPTIONS" && !muxNotFound[c.path] {
				if !strings.Contains(rec.Body.String(), `"code":"not_found"`) || !strings.Contains(rec.Body.String(), "it is served at https://") {
					t.Errorf("%s %s%s: %s", c.method, c.host, c.path, rec.Body.String())
				}
			}
		})
	}
}

// The checkout API is served on the checkout host (the hosted checkout calls its own origin)
// and on the API host (where the API reference documents it).
func TestCheckoutAPIIsOnPayAndAPIHosts(t *testing.T) {
	s := New(nil, nil, nil)
	if err := s.ConfigureHosts(prodHosts); err != nil {
		t.Fatal(err)
	}
	s.mux.HandleFunc("GET /v1/checkout/networks", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	s.mux.HandleFunc("OPTIONS /v1/checkout/", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	for _, host := range []string{"pay.prismaport.com", "api.prismaport.com"} {
		if rec := hit(s, "GET", host, "/v1/checkout/networks"); rec.Code != http.StatusTeapot {
			t.Errorf("%s: %d", host, rec.Code)
		}
		if rec := hit(s, "OPTIONS", host, "/v1/checkout/networks"); rec.Code != http.StatusNoContent {
			t.Errorf("%s preflight: %d", host, rec.Code)
		}
	}
	if rec := hit(s, "GET", "app.prismaport.com", "/v1/checkout/networks"); rec.Code != http.StatusPermanentRedirect ||
		rec.Header().Get("Location") != "https://api.prismaport.com/v1/checkout/networks" {
		t.Errorf("app: %d %v", rec.Code, rec.Header())
	}
	if rec := hit(s, "OPTIONS", "app.prismaport.com", "/v1/checkout/networks"); rec.Code != http.StatusNotFound {
		t.Errorf("app preflight: %d", rec.Code)
	}
}

func TestPathRole(t *testing.T) {
	cases := map[string]hostRole{
		"/": roleSite, "/site/landing.css": roleSite, "/site": roleSite,
		"/dashboard": roleApp, "/dashboard/": roleApp, "/dashboard/x/y": roleApp, "/v1/dashboard": roleApp, "/v1/dashboard/me": roleApp,
		"/pay/ses_1": rolePay, "/pay": rolePay, "/l/plink_1": rolePay, "/a/caddr_1": rolePay, "/s/ses_1": rolePay,
		"/v1/checkout/networks": rolePay | roleAPI, "/v1/checkout": rolePay | roleAPI,
		"/v1/checkout/logos/0123456789abcdef0123456789abcdef.png": rolePay | roleAPI | roleApp,
		"/v1/checkout/logos": rolePay | roleAPI | roleApp, "/v1/checkout/logosx": rolePay | roleAPI,
		"/docs": roleAPI, "/docs/openapi.json": roleAPI, "/v1/sessions": roleAPI, "/v1/sandbox/inbox/tok": roleAPI, "/v1": roleAPI,
		"/v1/dashboardx": roleAPI, "/v1/checkoutx": roleAPI,
		"/dashboard/..%2F": roleApp, "/dashboard/%2E%2E/": roleApp, "/d%61shboard/": roleApp, "/%64ocs/": roleAPI, "/site%2F..%2Fdocs/": 0,
		"/v1%2Fdashboard/me": 0, "/v1/dashboard%2Fme": roleAPI,
		"/payments": 0, "/docsx": 0, "/sx": 0, "/robots.txt": 0, "/favicon.ico": 0, "/sitemap.xml": 0, "/healthz": 0,
	}
	for p, want := range cases {
		t.Run(p, func(t *testing.T) {
			if got := pathRole(p); got != want {
				t.Errorf("%s: %b, want %b", p, got, want)
			}
		})
	}
	for in, want := range map[string]string{"": "/", "/": "/", "/a/../b/": "/b/", "/x/../v1/dashboard/me": "/v1/dashboard/me", "//docs": "/docs", "/docs/.": "/docs"} {
		t.Run(in, func(t *testing.T) {
			if got := cleanPath(in); got != want {
				t.Errorf("cleanPath(%q) = %q, want %q", in, got, want)
			}
		})
	}
}

func TestConfigureHostsRefusesBadAddresses(t *testing.T) {
	with := func(f func(*Hosts)) Hosts {
		h := prodHosts
		f(&h)
		return h
	}
	bad := map[string]Hosts{
		"no dashboard":            with(func(h *Hosts) { h.Dashboard = "" }),
		"no API":                  with(func(h *Hosts) { h.API = "" }),
		"no checkout":             with(func(h *Hosts) { h.Checkout = "" }),
		"plain http":              with(func(h *Hosts) { h.API = "http://api.prismaport.com" }),
		"relative":                with(func(h *Hosts) { h.Site = "prismaport.com" }),
		"credentials":             with(func(h *Hosts) { h.API = "https://u:p@api.prismaport.com" }),
		"query":                   with(func(h *Hosts) { h.Checkout = "https://pay.prismaport.com/?x=1" }),
		"fragment":                with(func(h *Hosts) { h.Site = "https://prismaport.com/#top" }),
		"dashboard at the root":   with(func(h *Hosts) { h.Dashboard = "https://app.prismaport.com/" }),
		"dashboard elsewhere":     with(func(h *Hosts) { h.Dashboard = "https://app.prismaport.com/app/" }),
		"checkout under a path":   with(func(h *Hosts) { h.Checkout = "https://pay.prismaport.com/checkout" }),
		"API under a path":        with(func(h *Hosts) { h.API = "https://api.prismaport.com/v1" }),
		"ftp":                     with(func(h *Hosts) { h.Site = "ftp://prismaport.com" }),
		"http to a private range": with(func(h *Hosts) { h.Checkout = "http://10.0.0.5" }),
	}
	for name, h := range bad {
		t.Run(name, func(t *testing.T) {
			if err := New(nil, nil, nil).ConfigureHosts(h); err == nil {
				t.Errorf("%s: accepted", name)
			}
		})
	}
	good := []Hosts{
		prodHosts,
		with(func(h *Hosts) {
			h.Dashboard = "https://app.prismaport.com/dashboard"
			h.Site = "https://prismaport.com/"
		}),
		{Site: "http://site.localhost:58180", Dashboard: "http://app.localhost:58180/dashboard/", Checkout: "http://pay.localhost:58180", API: "http://127.0.0.1:58180"},
	}
	for _, h := range good {
		if err := New(nil, nil, nil).ConfigureHosts(h); err != nil {
			t.Errorf("%+v: %v", h, err)
		}
	}
	s := New(nil, nil, nil)
	s.MountLandingPages(landingweb.Site(), "")
	if err := s.ConfigureHosts(prodHosts); err == nil {
		t.Error("hosts configured after the landing page was mounted")
	}
}

// Several surfaces may share a host; that host serves all of them.
func TestSharedHostServesEachOfItsSurfaces(t *testing.T) {
	s := hostServer(t, &Hosts{Site: "https://prismaport.com", Dashboard: "https://prismaport.com/dashboard/",
		Checkout: "https://pay.prismaport.com", API: "https://pay.prismaport.com"})
	for _, c := range []struct{ host, path string }{
		{"prismaport.com", "/"}, {"prismaport.com", "/dashboard/"}, {"pay.prismaport.com", "/pay/ses_01ABC"}, {"pay.prismaport.com", "/docs/"},
	} {
		t.Run(c.path, func(t *testing.T) {
			if rec := hit(s, "GET", c.host, c.path); rec.Code != http.StatusOK || !served(rec) {
				t.Errorf("%s%s: %d %v", c.host, c.path, rec.Code, rec.Header())
			}
		})
	}
	if rec := hit(s, "GET", "pay.prismaport.com", "/dashboard/"); rec.Header().Get("Location") != "https://prismaport.com/dashboard/" {
		t.Errorf("pay /dashboard/: %d %v", rec.Code, rec.Header())
	}
	if page := hit(s, "GET", "prismaport.com", "/").Body.String(); !strings.Contains(page, `href="/dashboard/login"`) ||
		!strings.Contains(page, `href="https://pay.prismaport.com/docs/"`) {
		t.Error("links to the site's own host must stay relative, others absolute")
	}
}

// A site already on www. has no www alias to redirect.
func TestWWWSiteHasNoAlias(t *testing.T) {
	s := hostServer(t, &Hosts{Site: "https://www.prismaport.com", Dashboard: prodHosts.Dashboard, Checkout: prodHosts.Checkout, API: prodHosts.API})
	if rec := hit(s, "GET", "www.prismaport.com", "/"); rec.Code != http.StatusOK {
		t.Fatalf("www site: %d %v", rec.Code, rec.Header())
	}
	if rec := hit(s, "GET", "www.www.prismaport.com", "/"); rec.Header().Get("Location") != "https://www.prismaport.com/" || rec.Code != http.StatusPermanentRedirect {
		t.Fatalf("www.www: %d %v", rec.Code, rec.Header())
	}
}

// With hosts, the landing page links straight to the dashboard's and the docs' hosts; its own
// assets and anchors stay relative.
func TestLandingLinksPointAtTheirHosts(t *testing.T) {
	s := hostServer(t, &prodHosts)
	rec := hit(s, "GET", "prismaport.com", "/")
	page := rec.Body.String()
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Security-Policy") != LandingCSP {
		t.Fatalf("/: %d %v", rec.Code, rec.Header())
	}
	for _, want := range []string{`href="https://app.prismaport.com/dashboard/login"`, `href="https://app.prismaport.com/dashboard/signup"`,
		`href="https://api.prismaport.com/docs/"`, `href="/site/landing.css"`, `href="/"`, `href="#pricing"`, `src="/site/landing.js"`} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %s", want)
		}
	}
	for _, m := range regexp.MustCompile(`href="(/[^"]*)"`).FindAllStringSubmatch(page, -1) {
		if m[1] != "/" && !strings.HasPrefix(m[1], LandingAssetPrefix) {
			t.Errorf("relative link to another surface left: %s", m[1])
		}
	}
	plain := hit(hostServer(t, nil), "GET", "localhost", "/")
	if plain.Header().Get("ETag") == rec.Header().Get("ETag") {
		t.Error("the rewritten page kept the original ETag")
	}
	if again := hitWith(s, "prismaport.com", "/", "If-None-Match", rec.Header().Get("ETag")); again.Code != http.StatusNotModified {
		t.Errorf("conditional: %d", again.Code)
	}
}

func hitWith(s *Server, host, target string, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", target, nil)
	req.Host = host
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func TestAbsoluteLinksKeepsQueriesAndFragments(t *testing.T) {
	r, err := newHostRouter(prodHosts)
	if err != nil {
		t.Fatal(err)
	}
	in := `<a href="/dashboard/signup?ref=x#top">a</a><a href="/docs/#webhooks">b</a><a href="/site/x.css">c</a><a href="/nope">d</a><a href="https://e.com/docs/">e</a><use href="#pm"/>`
	want := `<a href="https://app.prismaport.com/dashboard/signup?ref=x#top">a</a><a href="https://api.prismaport.com/docs/#webhooks">b</a><a href="/site/x.css">c</a><a href="/nope">d</a><a href="https://e.com/docs/">e</a><use href="#pm"/>`
	if got := string(r.absoluteLinks([]byte(in))); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	var off *hostRouter
	if got := string(off.absoluteLinks([]byte(in))); got != in {
		t.Fatalf("routing off changed the page: %s", got)
	}
}

// The binary mounts the landing page with the site URL when hosts are on: its share-card URLs follow
// the site's origin, never the checkout's, and its dashboard links point at the dashboard's host.
func TestLandingOnItsHostUsesTheSiteOrigin(t *testing.T) {
	s := New(nil, nil, nil)
	if err := s.ConfigureHosts(Hosts{Site: "https://site.example.com", Dashboard: "https://app.example.com/dashboard/",
		Checkout: "https://pay.example.com", API: "https://api.example.com"}); err != nil {
		t.Fatal(err)
	}
	s.MountLandingPages(landingweb.Site(), "https://site.example.com")
	page := hit(s, "GET", "site.example.com", "/").Body.String()
	for _, want := range []string{`content="https://site.example.com/site/og.png"`, `href="https://app.example.com/dashboard/login"`} {
		if !strings.Contains(page, want) {
			t.Errorf("landing page lacks %s", want)
		}
	}
	if strings.Contains(page, "pay.example.com") {
		t.Error("landing page names the checkout host")
	}
}
