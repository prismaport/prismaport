package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	dashboardweb "github.com/prismaport/prismaport/web/dashboard"
)

var dashboardFS = fstest.MapFS{
	"index.html":              {Data: []byte(`<!doctype html><title>Prismaport</title><script type="module" src="/dashboard/assets/index-d4sh.js"></script>`)},
	"assets/index-d4sh.js":    {Data: []byte(`console.log("dashboard")`)},
	"assets/index-d4sh.css":   {Data: []byte(`body{}`)},
	"assets/font-a1b2.woff2":  {Data: []byte{0x77, 0x4f, 0x46, 0x32}},
	"favicon.svg":             {Data: []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`)},
	"fonts/OFL-Onest.txt":     {Data: []byte(`SIL Open Font License`)},
	"assets/nested/deep.json": {Data: []byte(`{}`)},
	"version.json":            {Data: []byte(`{"version":"0.1.0"}`)},
}

func dashboardPagesServer(t *testing.T, fsys fstest.MapFS) *httptest.Server {
	t.Helper()
	s := New(nil, nil, nil)
	if err := s.MountDashboardPages(fsys); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return srv
}

func checkPageHeaders(t *testing.T, path string, h http.Header) {
	t.Helper()
	if h.Get("Content-Security-Policy") != DashboardCSP || h.Get("X-Content-Type-Options") != "nosniff" ||
		h.Get("X-Frame-Options") != "DENY" || h.Get("Referrer-Policy") != "no-referrer" || h.Get("Cross-Origin-Opener-Policy") != "same-origin" {
		t.Fatalf("%s headers: %v", path, h)
	}
}

func TestDashboardCSPIsStrict(t *testing.T) {
	for _, d := range strings.Split(DashboardCSP, "; ") {
		name, value, _ := strings.Cut(d, " ")
		switch value {
		case "'self'", "'none'":
		default:
			t.Errorf("%s allows %s", name, value)
		}
	}
	for _, want := range []string{"default-src 'none'", "script-src 'self'", "connect-src 'self'", "frame-ancestors 'none'", "base-uri 'none'"} {
		if !strings.Contains(DashboardCSP, want) {
			t.Errorf("CSP lacks %s", want)
		}
	}
	if strings.Contains(DashboardCSP, "unsafe") || strings.Contains(DashboardCSP, "http") || strings.Contains(DashboardCSP, "data:") {
		t.Fatalf("CSP: %s", DashboardCSP)
	}
}

// Every client route gets the app shell, so a reload on /dashboard/links/plink_... works.
func TestDashboardPagesSPAFallback(t *testing.T) {
	srv := dashboardPagesServer(t, dashboardFS)
	for _, path := range []string{"/dashboard/", "/dashboard/index.html", "/dashboard/links", "/dashboard/links/plink_01JBX5Q2", "/dashboard/products/prod_01JBX5Q2",
		"/dashboard/payments/ses_01JBX5Q2", "/dashboard/settings/security", "/dashboard/login"} {
		resp, body := get(t, srv.URL+path)
		if resp.StatusCode != http.StatusOK || !strings.Contains(body, "index-d4sh.js") {
			t.Fatalf("%s: %d %s", path, resp.StatusCode, body)
		}
		checkPageHeaders(t, path, resp.Header)
		if resp.Header.Get("Content-Type") != "text/html; charset=utf-8" || resp.Header.Get("Cache-Control") != "no-cache" || resp.Header.Get("ETag") == "" {
			t.Fatalf("%s: %v", path, resp.Header)
		}
		again, _ := get(t, srv.URL+path, "If-None-Match", resp.Header.Get("ETag"))
		if again.StatusCode != http.StatusNotModified {
			t.Fatalf("%s conditional: %d", path, again.StatusCode)
		}
	}
	resp, _ := get(t, srv.URL+"/dashboard")
	if resp.StatusCode != http.StatusPermanentRedirect || resp.Header.Get("Location") != "/dashboard/" {
		t.Fatalf("/dashboard: %d %v", resp.StatusCode, resp.Header)
	}
}

func TestDashboardPagesAssets(t *testing.T) {
	srv := dashboardPagesServer(t, dashboardFS)
	cases := []struct {
		path, ctype, cache string
	}{
		{"/dashboard/assets/index-d4sh.js", "text/javascript; charset=utf-8", "public, max-age=31536000, immutable"},
		{"/dashboard/assets/index-d4sh.css", "text/css; charset=utf-8", "public, max-age=31536000, immutable"},
		{"/dashboard/assets/font-a1b2.woff2", "font/woff2", "public, max-age=31536000, immutable"},
		{"/dashboard/assets/nested/deep.json", "application/json", "public, max-age=31536000, immutable"},
		{"/dashboard/favicon.svg", "image/svg+xml", "public, max-age=3600"},
		{"/dashboard/fonts/OFL-Onest.txt", "text/plain; charset=utf-8", "public, max-age=3600"},
		{"/dashboard/version.json", "application/json", "no-cache"},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			resp, _ := get(t, srv.URL+c.path)
			if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != c.ctype || resp.Header.Get("Cache-Control") != c.cache {
				t.Fatalf("%s: %d %v", c.path, resp.StatusCode, resp.Header)
			}
			checkPageHeaders(t, c.path, resp.Header)
		})
	}
	for _, path := range []string{"/dashboard/assets/missing.js", "/dashboard/missing.css", "/dashboard/assets/", "/dashboard/assets/nested",
		"/dashboard/..%2fgo.mod", "/dashboard/assets/..%2f..%2fsecret.js", "/dashboard/.gitkeep", "/dashboard/assets/index-d4sh.map"} {
		resp, body := get(t, srv.URL+path)
		if resp.StatusCode != http.StatusNotFound || strings.Contains(body, "index-d4sh.js") {
			t.Errorf("%s: %d %s", path, resp.StatusCode, body)
		}
		if resp.StatusCode == http.StatusNotFound && resp.Header.Get("Content-Security-Policy") != DashboardCSP {
			t.Errorf("%s: 404 without the CSP", path)
		}
	}
}

func TestDashboardPagesWithoutBuild(t *testing.T) {
	srv := dashboardPagesServer(t, fstest.MapFS{".gitkeep": {}})
	for _, path := range []string{"/dashboard/", "/dashboard/links"} {
		resp, _ := get(t, srv.URL+path)
		if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("%s without a build: %d %v", path, resp.StatusCode, resp.Header)
		}
		checkPageHeaders(t, path, resp.Header)
	}
	// The embedded placeholder (no frontend build in this checkout) behaves the same way.
	s := New(nil, nil, nil)
	if err := s.MountDashboardPages(dashboardweb.Dist()); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest("GET", "/dashboard/", nil))
	if rec.Code != http.StatusServiceUnavailable && rec.Code != http.StatusOK {
		t.Fatalf("embedded dashboard: %d", rec.Code)
	}
}
