package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	landingweb "github.com/prismaport/prismaport/web/landing"
)

var prodOrigins = PagesOrigins{Site: "https://prismaport.com", App: "https://app.prismaport.com", Pay: "https://pay.prismaport.com",
	Docs: "https://docs.prismaport.com", API: "https://api.prismaport.com"}

const (
	metaEmpty     = `<meta name="prismaport-api" content="" />`
	docsMetaEmpty = `<meta name="prismaport-docs" content="" />`
)

func spaBuild(title string) fstest.MapFS {
	return fstest.MapFS{
		"index.html":           {Data: []byte(`<!doctype html><head>` + metaEmpty + docsMetaEmpty + `<title>` + title + `</title>`)},
		"assets/app-abc123.js": {Data: []byte("js")},
		"brand/mark.svg":       {Data: []byte("<svg/>")},
		"favicon.svg":          {Data: []byte("<svg/>")},
		".gitkeep":             {Data: nil},
	}
}

func testSources() PagesSources {
	docs := fstest.MapFS{"index.html": {Data: []byte("<title>docs</title>")}, "openapi.json": {Data: []byte(`{"openapi":"3.1.0"}`)},
		"assets/main-abc123.js": {Data: []byte("js")}, ".gitkeep": {}}
	return PagesSources{Landing: landingweb.Site(), Dashboard: spaBuild("dashboard"), Checkout: spaBuild("checkout"), Docs: docs}
}

func buildTestPages(t *testing.T, o PagesOrigins) string {
	t.Helper()
	out := t.TempDir()
	if err := BuildPages(out, o, testSources()); err != nil {
		t.Fatal(err)
	}
	return out
}

func readOut(t *testing.T, out, p string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(out, filepath.FromSlash(p)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// pagesHeaders applies a _headers file to a path the way Cloudflare Pages does: every rule whose
// pattern matches contributes, "! Name" removes what earlier rules set, and two values of one
// header are joined with a comma. Only the trailing splat this generator writes is supported.
func pagesHeaders(t *testing.T, file, path string) http.Header {
	t.Helper()
	h := http.Header{}
	match := false
	for _, line := range strings.Split(strings.TrimRight(file, "\n"), "\n") {
		if !strings.HasPrefix(line, "  ") {
			pattern := line
			if p, ok := strings.CutSuffix(pattern, "*"); ok {
				match = strings.HasPrefix(path, p)
			} else {
				match = path == pattern
			}
			continue
		}
		if !match {
			continue
		}
		l := strings.TrimSpace(line)
		if name, ok := strings.CutPrefix(l, "! "); ok {
			h.Del(name)
			continue
		}
		name, val, ok := strings.Cut(l, ": ")
		if !ok {
			t.Fatalf("bad _headers line %q", line)
		}
		if old := h.Get(name); old != "" {
			val = old + ", " + val
		}
		h.Set(name, val)
	}
	return h
}

// served is what the binary's page handler sends besides the per-response headers.
func servedHeaders(set func(http.ResponseWriter)) http.Header {
	rec := httptest.NewRecorder()
	set(rec)
	return rec.Header()
}

// withProdAPI is a page CSP as the generator extends it for the production API origin.
func withProdAPI(csp string) string {
	for _, self := range []string{"connect-src 'self'", "img-src 'self'"} {
		csp = strings.Replace(csp, self, self+" https://api.prismaport.com", 1)
	}
	return csp
}

func TestPagesHeadersMatchTheBinary(t *testing.T) {
	out := buildTestPages(t, prodOrigins)
	site, app, pay, docs := readOut(t, out, "site/_headers"), readOut(t, out, "app/_headers"), readOut(t, out, "pay/_headers"), readOut(t, out, "docs/_headers")
	withAPI := func(set func(http.ResponseWriter)) http.Header {
		h := servedHeaders(set)
		h.Set("Content-Security-Policy", withProdAPI(h.Get("Content-Security-Policy")))
		return h
	}
	for _, c := range []struct {
		file, path string
		want       http.Header
	}{
		{site, "/", servedHeaders(landingPageHeaders)},
		{site, "/site/landing.css", servedHeaders(landingPageHeaders)},
		{site, "/robots.txt", servedHeaders(landingPageHeaders)},
		{docs, "/", servedHeaders(docsPageHeaders)},
		{docs, "/openapi.json", servedHeaders(docsPageHeaders)},
		{docs, "/assets/index-abc123.js", servedHeaders(docsPageHeaders)},
		{app, "/dashboard/", withAPI(dashboardPageHeaders)},
		{app, "/dashboard/products/prod_1", withAPI(dashboardPageHeaders)},
		{pay, "/pay/ses_1", withAPI(pageHeaders)},
		{pay, "/l/plink_1", withAPI(pageHeaders)},
	} {
		t.Run(c.path, func(t *testing.T) {
			got := pagesHeaders(t, c.file, c.path)
			got.Del("Cache-Control")
			if len(got) != len(c.want) {
				t.Errorf("%s: headers %v, want %v", c.path, got, c.want)
			}
			for name := range c.want {
				if got.Get(name) != c.want.Get(name) {
					t.Errorf("%s: %s = %q, want %q", c.path, name, got.Get(name), c.want.Get(name))
				}
			}
		})
	}
	if !strings.Contains(app, "connect-src 'self' https://api.prismaport.com;") || !strings.Contains(pay, "connect-src 'self' https://api.prismaport.com;") ||
		!strings.Contains(app, "img-src 'self' https://api.prismaport.com;") || !strings.Contains(pay, "img-src 'self' https://api.prismaport.com;") ||
		strings.Contains(site, "api.prismaport.com") || strings.Contains(docs, "api.prismaport.com") {
		t.Error("only the dashboard and the checkout may connect to the API")
	}
	for _, c := range []struct{ file, path, want string }{
		{app, "/dashboard/assets/index-abc123.js", cacheImmutable},
		{pay, "/pay/assets/index-abc123.js", cacheImmutable},
		{docs, "/assets/main-abc123.js", cacheImmutable},
		{site, "/site/fonts/onest-latin-400-normal.woff2", cacheFonts},
		{app, "/dashboard/products", ""},
		{pay, "/pay/ses_1", ""},
		{site, "/", ""},
		{docs, "/openapi.json", ""},
		{docs, "/", ""},
	} {
		t.Run(c.path, func(t *testing.T) {
			if got := pagesHeaders(t, c.file, c.path).Get("Cache-Control"); got != c.want {
				t.Errorf("%s: Cache-Control %q, want %q", c.path, got, c.want)
			}
		})
	}
	if strings.Contains(site, DocsCSP) {
		t.Error("the site still carries the developer portal's CSP")
	}
	for _, f := range []string{site, app, pay, docs} {
		if !strings.Contains(f, "! Access-Control-Allow-Origin") {
			t.Error("Pages' default Access-Control-Allow-Origin is kept")
		}
	}
}

func TestPagesRedirects(t *testing.T) {
	out := buildTestPages(t, prodOrigins)
	want := map[string]string{
		"site": `/docs https://docs.prismaport.com/ 302
/dashboard https://app.prismaport.com/dashboard/ 302
/docs/* https://docs.prismaport.com/:splat 302
/dashboard/* https://app.prismaport.com/dashboard/:splat 302
/pay/* https://pay.prismaport.com/pay/:splat 302
/l/* https://pay.prismaport.com/l/:splat 302
/a/* https://pay.prismaport.com/a/:splat 302
/s/* https://pay.prismaport.com/s/:splat 302
`,
		"app": `/ /dashboard/ 302
/docs https://docs.prismaport.com/ 302
/dashboard/favicon.svg /dashboard/favicon.svg 200
/docs/* https://docs.prismaport.com/:splat 302
/pay/* https://pay.prismaport.com/pay/:splat 302
/l/* https://pay.prismaport.com/l/:splat 302
/a/* https://pay.prismaport.com/a/:splat 302
/s/* https://pay.prismaport.com/s/:splat 302
/dashboard/assets/* /dashboard/assets/:splat 200
/dashboard/brand/* /dashboard/brand/:splat 200
/dashboard/* /dashboard/ 200
`,
		"pay": `/ https://prismaport.com/ 302
/docs https://docs.prismaport.com/ 302
/dashboard https://app.prismaport.com/dashboard/ 302
/s/ses_* /pay/ses_:splat 308
/docs/* https://docs.prismaport.com/:splat 302
/dashboard/* https://app.prismaport.com/dashboard/:splat 302
/pay/ses_* /pay/ 200
/l/plink_* /pay/ 200
/l/prod_* /pay/ 200
/a/caddr_* /pay/ 200
`,
		"docs": `/docs https://docs.prismaport.com/ 302
/dashboard https://app.prismaport.com/dashboard/ 302
/docs/* https://docs.prismaport.com/:splat 302
/dashboard/* https://app.prismaport.com/dashboard/:splat 302
/pay/* https://pay.prismaport.com/pay/:splat 302
/l/* https://pay.prismaport.com/l/:splat 302
/a/* https://pay.prismaport.com/a/:splat 302
/s/* https://pay.prismaport.com/s/:splat 302
`,
	}
	for dir, w := range want {
		t.Run(dir, func(t *testing.T) {
			if got := readOut(t, out, dir+"/_redirects"); got != w {
				t.Errorf("%s/_redirects:\n%s\nwant:\n%s", dir, got, w)
			}
		})
	}
}

func TestPagesTree(t *testing.T) {
	out := t.TempDir()
	stale := filepath.Join(out, "app", "dashboard", "assets", "old-000000.js")
	keep := filepath.Join(out, "notes.txt")
	for _, p := range []string{stale, keep} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := BuildPages(out, prodOrigins, testSources()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("a file of an earlier build is left in app/")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Error("BuildPages removed something besides its four directories")
	}
	var files []string
	_ = filepath.WalkDir(out, func(p string, d os.DirEntry, _ error) error {
		if !d.IsDir() {
			rel, _ := filepath.Rel(out, p)
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	for _, want := range []string{"site/index.html", "site/site/landing.css", "site/site/og.png", "site/404.html", "site/_headers", "site/_redirects",
		"app/dashboard/index.html", "app/dashboard/assets/app-abc123.js", "app/dashboard/favicon.svg", "app/404.html",
		"pay/pay/index.html", "pay/pay/brand/mark.svg", "pay/404.html",
		"docs/index.html", "docs/openapi.json", "docs/assets/main-abc123.js", "docs/404.html", "docs/_headers", "docs/_redirects"} {
		if !slices.Contains(files, want) {
			t.Errorf("missing %s", want)
		}
	}
	for _, f := range files {
		if strings.Contains(f, ".gitkeep") || f == "site/site/index.html" || strings.HasPrefix(f, "site/docs/") || strings.HasPrefix(f, "docs/docs/") {
			t.Errorf("unexpected %s", f)
		}
	}

	for _, p := range []string{"app/dashboard/index.html", "pay/pay/index.html"} {
		if got := readOut(t, out, p); !strings.Contains(got, `<meta name="prismaport-api" content="https://api.prismaport.com" />`) || strings.Contains(got, metaEmpty) {
			t.Errorf("%s: %s", p, got)
		}
	}
	if got := readOut(t, out, "app/dashboard/index.html"); !strings.Contains(got, `<meta name="prismaport-docs" content="https://docs.prismaport.com/" />`) {
		t.Errorf("the dashboard does not link the docs host: %s", got)
	}
	if got := readOut(t, out, "docs/index.html"); got != "<title>docs</title>" {
		t.Errorf("the portal's page was changed: %s", got)
	}

	landing := readOut(t, out, "site/index.html")
	for _, want := range []string{`href="https://app.prismaport.com/dashboard/signup"`, `href="https://app.prismaport.com/dashboard/login"`, `href="https://docs.prismaport.com/"`,
		`href="/site/landing.css"`, `<link rel="canonical" href="https://prismaport.com/">`} {
		if !strings.Contains(landing, want) {
			t.Errorf("the landing page lacks %s", want)
		}
	}
	if strings.Contains(landing, `href="/dashboard/`) || strings.Contains(landing, `href="/docs`) {
		t.Error("a landing link to the dashboard or the docs stayed relative")
	}
	if nf := readOut(t, out, "pay/404.html"); !strings.Contains(nf, `<a href="https://prismaport.com/">`) {
		t.Errorf("404: %s", nf)
	}
}

func TestPagesLandingNamesTheSite(t *testing.T) {
	o := PagesOrigins{Site: "http://127.0.0.1:58581", App: "http://127.0.0.1:58582", Pay: "http://127.0.0.1:58583", Docs: "http://127.0.0.1:58587",
		API: "http://127.0.0.1:58580"}
	out := buildTestPages(t, o)
	landing := readOut(t, out, "site/index.html")
	for _, want := range []string{`href="http://127.0.0.1:58582/dashboard/signup"`, `content="http://127.0.0.1:58581/site/og.png"`, `href="http://127.0.0.1:58587/"`} {
		if !strings.Contains(landing, want) {
			t.Errorf("the landing page lacks %s", want)
		}
	}
	if strings.Contains(landing, "https://prismaport.com") {
		t.Error("the landing page still names prismaport.com")
	}
	if got := readOut(t, out, "app/_headers"); !strings.Contains(got, "connect-src 'self' http://127.0.0.1:58580;") {
		t.Errorf("app headers: %s", got)
	}
	// Pages takes absolute redirect targets over https only: plain http origins get none.
	for dir, want := range map[string]string{
		"site": "",
		"app":  "/ /dashboard/ 302\n/dashboard/favicon.svg /dashboard/favicon.svg 200\n/dashboard/assets/* /dashboard/assets/:splat 200\n/dashboard/brand/* /dashboard/brand/:splat 200\n/dashboard/* /dashboard/ 200\n",
		"pay":  "/s/ses_* /pay/ses_:splat 308\n/pay/ses_* /pay/ 200\n/l/plink_* /pay/ 200\n/l/prod_* /pay/ 200\n/a/caddr_* /pay/ 200\n",
		"docs": "/docs / 302\n/docs/* /:splat 302\n",
	} {
		t.Run(dir, func(t *testing.T) {
			if got := readOut(t, out, dir+"/_redirects"); got != want {
				t.Errorf("%s/_redirects with http origins:\n%s", dir, got)
			}
		})
	}
}

func TestPagesLandingLinksThePortalAtTheDocsRoot(t *testing.T) {
	f, err := pagesFrontend(prodOrigins)
	if err != nil {
		t.Fatal(err)
	}
	page := []byte(`<a href="/docs">a</a><a href="/docs/">b</a><a href="/docs/#webhooks">c</a><a href="/docs/openapi.json?v=1">d</a>` +
		`<a href="/%64ocs/x">e</a><a href="/docsite">f</a><a href="/dashboard/login">g</a><a href="#pricing">h</a>`)
	want := `<a href="https://docs.prismaport.com/">a</a><a href="https://docs.prismaport.com/">b</a><a href="https://docs.prismaport.com/#webhooks">c</a>` +
		`<a href="https://docs.prismaport.com/openapi.json?v=1">d</a><a href="https://docs.prismaport.com/x">e</a><a href="/docsite">f</a>` +
		`<a href="https://app.prismaport.com/dashboard/login">g</a><a href="#pricing">h</a>`
	if got := string(landingForPages(page, f)); got != want {
		t.Errorf("landing links:\n%s\nwant:\n%s", got, want)
	}
}

func TestPagesBuildRefuses(t *testing.T) {
	with := func(f func(o *PagesOrigins)) PagesOrigins {
		o := prodOrigins
		f(&o)
		return o
	}
	unbuilt := testSources()
	unbuilt.Dashboard = fstest.MapFS{".gitkeep": {}}
	noMeta := testSources()
	noMeta.Checkout = fstest.MapFS{"index.html": {Data: []byte("<title>checkout</title>")}}
	twoMetas := testSources()
	twoMetas.Dashboard = fstest.MapFS{"index.html": {Data: []byte(metaEmpty + metaEmpty + docsMetaEmpty)}}
	noDocsMeta := testSources()
	noDocsMeta.Dashboard = fstest.MapFS{"index.html": {Data: []byte(metaEmpty)}}
	noContract := testSources()
	noContract.Docs = fstest.MapFS{"index.html": {Data: []byte("docs")}}
	for name, c := range map[string]struct {
		o    PagesOrigins
		src  PagesSources
		want string
	}{
		"no site":            {with(func(o *PagesOrigins) { o.Site = "" }), testSources(), "site URL is required"},
		"no API":             {with(func(o *PagesOrigins) { o.API = "" }), testSources(), "API URL is required"},
		"no docs":            {with(func(o *PagesOrigins) { o.Docs = "" }), testSources(), "docs URL is required"},
		"docs with a path":   {with(func(o *PagesOrigins) { o.Docs = "https://prismaport.com/docs/" }), testSources(), "the docs URL must have the path /"},
		"docs on the site":   {with(func(o *PagesOrigins) { o.Docs = o.Site }), testSources(), "host of its own"},
		"API on the docs":    {with(func(o *PagesOrigins) { o.API = "https://docs.prismaport.com" }), testSources(), "serves a page"},
		"plain http":         {with(func(o *PagesOrigins) { o.Pay = "http://pay.prismaport.com" }), testSources(), "https"},
		"a path":             {with(func(o *PagesOrigins) { o.API = "https://api.prismaport.com/v1" }), testSources(), "path"},
		"API on a page host": {with(func(o *PagesOrigins) { o.API = "https://app.prismaport.com" }), testSources(), "serves a page"},
		"cross-site API":     {with(func(o *PagesOrigins) { o.API = "https://api.example.com" }), testSources(), "same-site"},
		"unbuilt dashboard":  {prodOrigins, unbuilt, "dashboard (task web:dashboard:build) is not built"},
		"no meta tag":        {prodOrigins, noMeta, "0 prismaport-api meta tags"},
		"two meta tags":      {prodOrigins, twoMetas, "2 prismaport-api meta tags"},
		"no docs meta tag":   {prodOrigins, noDocsMeta, "0 prismaport-docs meta tags"},
		"no contract":        {prodOrigins, noContract, "openapi.json"},
	} {
		t.Run(name, func(t *testing.T) {
			out := t.TempDir()
			err := BuildPages(out, c.o, c.src)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("%s: err = %v, want it to mention %q", name, err, c.want)
			}
		})
	}
}

func TestEmbeddedPagesRefuseAPagesBuild(t *testing.T) {
	built := t.TempDir()
	if err := BuildPages(built, prodOrigins, testSources()); err != nil {
		t.Fatal(err)
	}
	stamped := func(p string) fstest.MapFS {
		return fstest.MapFS{"index.html": {Data: []byte(readOut(t, built, p))}}
	}
	if err := New(nil, nil, nil).MountDashboardPages(stamped("app/dashboard/index.html")); err == nil ||
		!strings.Contains(err.Error(), "the embedded dashboard is a Pages build") || !strings.Contains(err.Error(), "https://api.prismaport.com") {
		t.Errorf("dashboard: %v", err)
	}
	if err := New(nil, nil, nil).MountCheckoutPages(stamped("pay/pay/index.html")); err == nil || !strings.Contains(err.Error(), "the embedded checkout is a Pages build") {
		t.Errorf("checkout: %v", err)
	}
	docsOnly := fstest.MapFS{"index.html": {Data: []byte(metaEmpty + `<meta name="prismaport-docs" content="https://docs.prismaport.com/" />`)}}
	if err := New(nil, nil, nil).MountDashboardPages(docsOnly); err == nil || !strings.Contains(err.Error(), "prismaport-docs") {
		t.Errorf("a docs address alone: %v", err)
	}
	for name, fsys := range map[string]fstest.MapFS{
		"empty tags":    spaBuild("dashboard"),
		"not built":     {".gitkeep": {}},
		"no tag at all": {"index.html": {Data: []byte("<title>old build</title>")}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := New(nil, nil, nil).MountDashboardPages(fsys); err != nil {
				t.Errorf("%s: %v", name, err)
			}
			if err := New(nil, nil, nil).MountCheckoutPages(fsys); err != nil {
				t.Errorf("%s: %v", name, err)
			}
		})
	}
}

// A non-production build adds X-Robots-Tag inside the one /* rule of each project: Pages dropped a
// whole _headers file that had a second /* rule.
func TestPagesNoIndex(t *testing.T) {
	plain := buildTestPages(t, prodOrigins)
	o := prodOrigins
	o.NoIndex = true
	hidden := buildTestPages(t, o)
	csp := map[string]string{"site": LandingCSP, "docs": DocsCSP,
		"app": withProdAPI(DashboardCSP), "pay": withProdAPI(CheckoutCSP)}
	for _, dir := range []string{"site", "app", "pay", "docs"} {
		without, with := readOut(t, plain, dir+"/_headers"), readOut(t, hidden, dir+"/_headers")
		if strings.Contains(without, "X-Robots-Tag") {
			t.Errorf("%s: a production build asks not to be indexed:\n%s", dir, without)
		}
		cspLine := "  Content-Security-Policy: " + csp[dir] + "\n"
		if want := strings.Replace(without, cspLine, cspLine+"  X-Robots-Tag: noindex, nofollow\n", 1); with != want {
			t.Errorf("%s/_headers with --noindex:\n%s\nwant:\n%s", dir, with, want)
		}
		if n := strings.Count(with, "\n/*\n"); !strings.HasPrefix(with, "/*\n") || n != 0 {
			t.Errorf("%s: want exactly one /* rule, first:\n%s", dir, with)
		}
		if strings.Count(with, "X-Robots-Tag") != 1 {
			t.Errorf("%s: X-Robots-Tag not exactly once:\n%s", dir, with)
		}
		for _, p := range []string{"/", "/dashboard/", "/pay/ses_1", "/openapi.json", "/assets/main-abc123.js"} {
			if got := pagesHeaders(t, with, p).Get("X-Robots-Tag"); got != "noindex, nofollow" {
				t.Errorf("%s %s: X-Robots-Tag %q", dir, p, got)
			}
		}
	}
	for _, f := range []string{"index.html", "404.html", "_redirects"} {
		if readOut(t, plain, "site/"+f) != readOut(t, hidden, "site/"+f) {
			t.Errorf("--noindex changed site/%s", f)
		}
	}
}

// The generated _headers are pinned to literal values here (TestPagesHeadersMatchTheBinary only
// compares them to what the binary sends, so a weakened header would slip through both): every
// Pages directory serves every path with nosniff, DENY framing, COOP, no referrer, HSTS and a CSP
// that denies by default and forbids framing, and Pages' default wildcard CORS header is removed.
func TestGeneratedPagesHeadersAreHardened(t *testing.T) {
	out := buildTestPages(t, prodOrigins)
	paths := map[string]string{"site": "/", "app": "/dashboard/", "pay": "/pay/ses_1", "docs": "/"}
	for dir, path := range paths {
		t.Run(dir, func(t *testing.T) {
			file := readOut(t, out, dir+"/_headers")
			h := pagesHeaders(t, file, path)
			for name, want := range map[string]string{
				"X-Content-Type-Options":     "nosniff",
				"X-Frame-Options":            "DENY",
				"Cross-Origin-Opener-Policy": "same-origin",
				"Referrer-Policy":            "no-referrer",
				"Strict-Transport-Security":  "max-age=15552000",
			} {
				if got := h.Get(name); got != want {
					t.Errorf("%s/_headers: %s = %q, want %q", dir, name, got, want)
				}
			}
			csp := h.Get("Content-Security-Policy")
			for _, want := range []string{"default-src 'none'", "frame-ancestors 'none'", "base-uri 'none'", "form-action 'none'"} {
				if !strings.Contains(csp, want) {
					t.Errorf("%s/_headers: CSP lacks %q: %s", dir, want, csp)
				}
			}
			if strings.Contains(csp, "unsafe-") || strings.Contains(csp, "*") {
				t.Errorf("%s/_headers: CSP is loosened: %s", dir, csp)
			}
			if got := h.Get("Access-Control-Allow-Origin"); got != "" {
				t.Errorf("%s/_headers: Access-Control-Allow-Origin = %q, want it removed", dir, got)
			}
			if n := strings.Count(file, "\n/*\n"); n+btoi(strings.HasPrefix(file, "/*\n")) != 1 {
				t.Errorf("%s/_headers has %d /* rules, want exactly 1 (Pages drops a file with two)", dir, n+btoi(strings.HasPrefix(file, "/*\n")))
			}
		})
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}
