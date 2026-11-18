package api

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/prismaport/prismaport/internal/apicontract"
	docsweb "github.com/prismaport/prismaport/web/docs"
)

var docsFS = fstest.MapFS{
	"index.html":                  {Data: []byte(`<!doctype html><title>Prismaport API</title><script type="module" src="./assets/index-d0c5.js"></script>`)},
	"assets/index-d0c5.js":        {Data: []byte(`console.log("docs")`)},
	"assets/index-d0c5.css":       {Data: []byte(`body{}`)},
	"assets/onest-a1b2c3d4.woff2": {Data: []byte{0x77, 0x4f, 0x46, 0x32}},
	"favicon.svg":                 {Data: []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`)},
	"fonts/OFL-Onest.txt":         {Data: []byte(`SIL Open Font License`)},
	"openapi.json":                {Data: []byte(`{"openapi":"3.1.0"}`)},
	"version.json":                {Data: []byte(`{"version":"0.1.0"}`)},
}

func docsPagesServer(t *testing.T, fsys fstest.MapFS) *httptest.Server {
	t.Helper()
	s := New(nil, nil, nil)
	s.MountDocsPages(fsys)
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return srv
}

func checkDocsHeaders(t *testing.T, path string, h http.Header) {
	t.Helper()
	if h.Get("Content-Security-Policy") != DocsCSP || h.Get("X-Content-Type-Options") != "nosniff" ||
		h.Get("X-Frame-Options") != "DENY" || h.Get("Referrer-Policy") != "no-referrer" || h.Get("Cross-Origin-Opener-Policy") != "same-origin" ||
		h.Get("Permissions-Policy") == "" || h.Get("Strict-Transport-Security") != HSTS {
		t.Fatalf("%s headers: %v", path, h)
	}
}

func TestDocsCSPIsStrict(t *testing.T) {
	for _, d := range strings.Split(DocsCSP, "; ") {
		name, value, _ := strings.Cut(d, " ")
		switch value {
		case "'self'", "'none'":
		default:
			t.Errorf("%s allows %s", name, value)
		}
	}
	for _, want := range []string{"default-src 'none'", "script-src 'self'", "style-src 'self'", "connect-src 'none'", "frame-ancestors 'none'", "base-uri 'none'"} {
		if !strings.Contains(DocsCSP, want) {
			t.Errorf("CSP lacks %s", want)
		}
	}
}

func TestDocsPagesShell(t *testing.T) {
	srv := docsPagesServer(t, docsFS)
	for _, path := range []string{"/docs/", "/docs/index.html"} {
		resp, body := get(t, srv.URL+path)
		if resp.StatusCode != http.StatusOK || !strings.Contains(body, "index-d0c5.js") {
			t.Fatalf("%s: %d %s", path, resp.StatusCode, body)
		}
		checkDocsHeaders(t, path, resp.Header)
		if resp.Header.Get("Content-Type") != "text/html; charset=utf-8" || resp.Header.Get("Cache-Control") != "no-cache" || resp.Header.Get("ETag") == "" {
			t.Fatalf("%s: %v", path, resp.Header)
		}
		again, _ := get(t, srv.URL+path, "If-None-Match", resp.Header.Get("ETag"))
		if again.StatusCode != http.StatusNotModified {
			t.Fatalf("%s conditional: %d", path, again.StatusCode)
		}
	}
	resp, _ := get(t, srv.URL+"/docs")
	if resp.StatusCode != http.StatusPermanentRedirect || resp.Header.Get("Location") != "/docs/" {
		t.Fatalf("/docs: %d %v", resp.StatusCode, resp.Header)
	}
}

func TestDocsPagesFiles(t *testing.T) {
	srv := docsPagesServer(t, docsFS)
	cases := []struct{ path, ctype, cache string }{
		{"/docs/assets/index-d0c5.js", "text/javascript; charset=utf-8", "public, max-age=31536000, immutable"},
		{"/docs/assets/index-d0c5.css", "text/css; charset=utf-8", "public, max-age=31536000, immutable"},
		{"/docs/assets/onest-a1b2c3d4.woff2", "font/woff2", "public, max-age=31536000, immutable"},
		{"/docs/favicon.svg", "image/svg+xml", "public, max-age=3600"},
		{"/docs/fonts/OFL-Onest.txt", "text/plain; charset=utf-8", "public, max-age=3600"},
		{"/docs/openapi.json", "application/json", "no-cache"},
		{"/docs/version.json", "application/json", "no-cache"},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			resp, _ := get(t, srv.URL+c.path)
			if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != c.ctype || resp.Header.Get("Cache-Control") != c.cache {
				t.Fatalf("%s: %d %v", c.path, resp.StatusCode, resp.Header)
			}
			checkDocsHeaders(t, c.path, resp.Header)
		})
	}
}

func TestDocsOpenAPIRevalidates(t *testing.T) {
	srv := docsPagesServer(t, docsFS)
	resp, body := get(t, srv.URL+"/docs/openapi.json")
	if resp.StatusCode != http.StatusOK || body != `{"openapi":"3.1.0"}` || resp.Header.Get("ETag") == "" {
		t.Fatalf("openapi.json: %d %v %s", resp.StatusCode, resp.Header, body)
	}
	again, _ := get(t, srv.URL+"/docs/openapi.json", "If-None-Match", resp.Header.Get("ETag"))
	if again.StatusCode != http.StatusNotModified {
		t.Fatalf("openapi.json conditional: %d", again.StatusCode)
	}
}

// The portal routes with the fragment, so an unknown path is a 404, never the page.
func TestDocsPagesUnknownPaths(t *testing.T) {
	srv := docsPagesServer(t, docsFS)
	for _, path := range []string{"/docs/createSession", "/docs/assets/missing.js", "/docs/assets", "/docs/%2e%2e/go.mod", "/docs/index.php", "/docs/fonts"} {
		resp, body := get(t, srv.URL+path)
		if resp.StatusCode != http.StatusNotFound || strings.Contains(body, "index-d0c5.js") {
			t.Fatalf("%s: %d %s", path, resp.StatusCode, body)
		}
	}
}

func TestDocsPagesWithoutBuild(t *testing.T) {
	srv := docsPagesServer(t, fstest.MapFS{".gitkeep": {}})
	resp, _ := get(t, srv.URL+"/docs/")
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("no build: %d %v", resp.StatusCode, resp.Header)
	}
	checkDocsHeaders(t, "/docs/", resp.Header)
}

// The published contract is the one the drift tests check: the build copies docs/openapi.json
// unchanged (web/docs/scripts/check-dist.mjs), and it must stay valid JSON.
func TestDocsOpenAPIIsValidJSON(t *testing.T) {
	path, err := apicontract.File()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		OpenAPI string `json:"openapi"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || doc.OpenAPI != "3.1.0" {
		t.Fatalf("docs/openapi.json: %v %q", err, doc.OpenAPI)
	}
}

// One portal build serves at the binary's /docs/ and at the root of its Pages project: every file
// its page and stylesheets refer to resolves under both.
func TestDocsBuildServesAtBothRoots(t *testing.T) {
	dist := docsweb.Dist()
	index, err := fs.ReadFile(dist, "index.html")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("the developer portal is not built: CI builds web/docs before the Go tests (task web:docs:build)")
		}
		t.Skip("the developer portal is not built (task web:docs:build)")
	}
	type ref struct{ from, link string }
	var refs []ref
	for _, m := range regexp.MustCompile(`(?:src|href)="([^"#]+)"`).FindAllStringSubmatch(string(index), -1) {
		if !strings.HasPrefix(m[1], "/dashboard/") {
			refs = append(refs, ref{"", m[1]})
		}
	}
	css, _ := fs.Glob(dist, "assets/*.css")
	for _, f := range css {
		b, _ := fs.ReadFile(dist, f)
		for _, m := range regexp.MustCompile(`url\(["']?([^)"']+)["']?\)`).FindAllStringSubmatch(string(b), -1) {
			if !strings.HasPrefix(m[1], "data:") {
				refs = append(refs, ref{f, m[1]})
			}
		}
	}
	if len(refs) < 4 {
		t.Fatalf("the portal refers to only %v", refs)
	}
	s := New(nil, nil, nil)
	s.MountDocsPages(dist)
	for _, root := range []string{"/docs/", "/"} {
		base, _ := url.Parse("https://example.test" + root)
		for _, r := range refs {
			from := base
			if r.from != "" {
				from = base.ResolveReference(&url.URL{Path: r.from})
			}
			u, err := from.Parse(r.link)
			if err != nil || u.Host != "example.test" || !strings.HasPrefix(u.Path, root) {
				t.Errorf("%s%s: %q leaves the portal: %v", root, r.from, r.link, u)
				continue
			}
			if root == "/" {
				if _, err := fs.Stat(dist, strings.TrimPrefix(u.Path, "/")); err != nil {
					t.Errorf("at the root, %q (from %s) is not a file of the build", r.link, r.from)
				}
				continue
			}
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, httptest.NewRequest("GET", u.Path, nil))
			if rec.Code != http.StatusOK {
				t.Errorf("at /docs/, %q (from %s) is %s: %d", r.link, r.from, u.Path, rec.Code)
			}
		}
	}
}
