package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image/png"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/fee"
	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/payout"
	"github.com/prismaport/prismaport/internal/service"
	checkoutweb "github.com/prismaport/prismaport/web/checkout"
	dashboardweb "github.com/prismaport/prismaport/web/dashboard"
	docsweb "github.com/prismaport/prismaport/web/docs"
	landingweb "github.com/prismaport/prismaport/web/landing"
)

func landingServer(t *testing.T, fsys fs.FS) *httptest.Server {
	t.Helper()
	return landingServerAt(t, fsys, "")
}

func landingServerAt(t *testing.T, fsys fs.FS, publicURL string) *httptest.Server {
	t.Helper()
	s := New(nil, nil, nil)
	if err := s.MountDashboardPages(fstest.MapFS{".gitkeep": {}}); err != nil {
		t.Fatal(err)
	}
	s.MountLandingPages(fsys, publicURL)
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return srv
}

func checkLandingHeaders(t *testing.T, path string, h http.Header) {
	t.Helper()
	if h.Get("Content-Security-Policy") != LandingCSP || h.Get("X-Content-Type-Options") != "nosniff" ||
		h.Get("X-Frame-Options") != "DENY" || h.Get("Referrer-Policy") != "no-referrer" || h.Get("Cross-Origin-Opener-Policy") != "same-origin" ||
		h.Get("Permissions-Policy") == "" || h.Get("Strict-Transport-Security") != HSTS {
		t.Fatalf("%s headers: %v", path, h)
	}
}

func landingIndex(t *testing.T) string {
	t.Helper()
	b, err := fs.ReadFile(landingweb.Site(), "index.html")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestLandingCSPIsStrict(t *testing.T) {
	for _, d := range strings.Split(LandingCSP, "; ") {
		name, value, _ := strings.Cut(d, " ")
		switch value {
		case "'self'", "'none'":
		default:
			t.Errorf("%s allows %s", name, value)
		}
	}
	for _, want := range []string{"default-src 'none'", "script-src 'self'", "style-src 'self'", "font-src 'self'", "img-src 'self'",
		"base-uri 'none'", "form-action 'none'", "frame-ancestors 'none'"} {
		if !strings.Contains(LandingCSP, want) {
			t.Errorf("CSP lacks %s", want)
		}
	}
}

func TestLandingRootServesThePage(t *testing.T) {
	srv := landingServer(t, landingweb.Site())
	resp, body := get(t, srv.URL+"/")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "<title>Prismaport · Get paid in stablecoins, straight to your wallet</title>") {
		t.Fatalf("/: %d %s", resp.StatusCode, body)
	}
	checkLandingHeaders(t, "/", resp.Header)
	if resp.Header.Get("Content-Type") != "text/html; charset=utf-8" || resp.Header.Get("Cache-Control") != "no-cache" || resp.Header.Get("ETag") == "" {
		t.Fatalf("/: %v", resp.Header)
	}
	again, _ := get(t, srv.URL+"/", "If-None-Match", resp.Header.Get("ETag"))
	if again.StatusCode != http.StatusNotModified {
		t.Fatalf("/ conditional: %d", again.StatusCode)
	}
}

func TestLandingRootFallsBackToTheDashboard(t *testing.T) {
	srv := landingServer(t, fstest.MapFS{"landing.css": {Data: []byte(`body{}`)}})
	resp, _ := get(t, srv.URL+"/")
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/dashboard/" || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("/ without a landing page: %d %v", resp.StatusCode, resp.Header)
	}
	if resp, _ := get(t, srv.URL+"/nope"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("/nope: %d", resp.StatusCode)
	}
}

var (
	landingRef      = regexp.MustCompile(`(?:src|href)="(/site/[^"]+)"`)
	landingShareRef = regexp.MustCompile(`content="` + regexp.QuoteMeta(LandingOrigin) + `/site/([^"]+)"`)
	cssURL          = regexp.MustCompile(`url\(\s*['"]?([^'")]+)['"]?\s*\)`)
)

// Every file the page or its stylesheet names is embedded and served with its pinned type.
func TestLandingReferencedAssetsAreServed(t *testing.T) {
	site := landingweb.Site()
	srv := landingServer(t, site)
	refs := map[string]bool{}
	for _, m := range landingRef.FindAllStringSubmatch(landingIndex(t), -1) {
		refs[strings.TrimPrefix(m[1], LandingAssetPrefix)] = true
	}
	for p := range refs {
		if path.Ext(p) != ".css" {
			continue
		}
		css, err := fs.ReadFile(site, p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		for _, m := range cssURL.FindAllStringSubmatch(string(css), -1) {
			refs[path.Join(path.Dir(p), m[1])] = true
		}
	}
	for _, m := range landingShareRef.FindAllStringSubmatch(landingIndex(t), -1) {
		refs[m[1]] = true
	}
	for _, want := range []string{"landing.css", "landing.js", "favicon.svg", "og.png", "logos/base-64.png",
		"fonts/familjen-grotesk-latin-700-normal.woff2", "fonts/onest-latin-600-normal.woff2", "fonts/geist-mono-latin-500-normal.woff2"} {
		if !refs[want] {
			t.Errorf("the page does not reference %s", want)
		}
	}
	for p := range refs {
		if _, err := fs.Stat(site, p); err != nil {
			t.Errorf("%s is referenced but not embedded: %v", p, err)
			continue
		}
		resp, body := get(t, srv.URL+LandingAssetPrefix+p)
		if resp.StatusCode != http.StatusOK || body == "" || resp.Header.Get("Content-Type") != assetTypes[path.Ext(p)] || resp.Header.Get("ETag") == "" {
			t.Errorf("%s: %d %v", p, resp.StatusCode, resp.Header)
			continue
		}
		checkLandingHeaders(t, p, resp.Header)
		if got, want := resp.Header.Get("Cache-Control"), landingCache(path.Ext(p)); got != want {
			t.Errorf("%s: Cache-Control %q, want %q", p, got, want)
		}
	}
}

func TestLandingCachePolicy(t *testing.T) {
	srv := landingServer(t, landingweb.Site())
	cases := []struct{ path, ctype, cache string }{
		{"/site/landing.css", "text/css; charset=utf-8", "no-cache"},
		{"/site/landing.js", "text/javascript; charset=utf-8", "no-cache"},
		{"/site/fonts/onest-latin-400-normal.woff2", "font/woff2", "public, max-age=2592000"},
		{"/site/logos/base-64.png", "image/png", "public, max-age=86400"},
		{"/site/favicon.svg", "image/svg+xml", "public, max-age=86400"},
		{"/site/fonts/OFL-Onest.txt", "text/plain; charset=utf-8", "public, max-age=86400"},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			resp, _ := get(t, srv.URL+c.path)
			if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != c.ctype || resp.Header.Get("Cache-Control") != c.cache {
				t.Fatalf("%s: %d %v", c.path, resp.StatusCode, resp.Header)
			}
			again, _ := get(t, srv.URL+c.path, "If-None-Match", resp.Header.Get("ETag"))
			if again.StatusCode != http.StatusNotModified {
				t.Fatalf("%s conditional: %d", c.path, again.StatusCode)
			}
		})
	}
}

func TestLandingUnknownPaths(t *testing.T) {
	srv := landingServer(t, landingweb.Site())
	for _, p := range []string{"/site/", "/site/index.html", "/site/missing.css", "/site/fonts", "/site/fonts/", "/site/..%2fembed.go",
		"/site/%2e%2e/embed.go", "/site/logos/base-128.png", "/index.html", "/landing.css", "/nope"} {
		resp, body := get(t, srv.URL+p)
		if resp.StatusCode != http.StatusNotFound || strings.Contains(body, "<title>") {
			t.Errorf("%s: %d %s", p, resp.StatusCode, body)
		}
		if strings.HasPrefix(p, LandingAssetPrefix) && resp.Header.Get("Content-Security-Policy") != LandingCSP {
			t.Errorf("%s: 404 without the CSP", p)
		}
	}
}

// The CSP allows no inline style or script, and every link goes somewhere real on this origin.
func TestLandingPageFitsItsCSPAndLinks(t *testing.T) {
	page := landingIndex(t)
	if strings.Contains(page, "style=") || strings.Contains(page, "<style") || strings.Count(page, "<script") != 1 ||
		!strings.Contains(page, `<script src="/site/landing.js" defer></script>`) ||
		regexp.MustCompile(`\son[a-z]+=`).MatchString(page) || strings.Contains(page, "http://") {
		t.Fatal("index.html carries inline style or script, or an insecure URL")
	}
	ids := map[string]bool{}
	for _, m := range regexp.MustCompile(`\sid="([^"]+)"`).FindAllStringSubmatch(page, -1) {
		ids[m[1]] = true
	}
	allowed := map[string]bool{"/": true, "/docs/": true, "/dashboard/login": true, "/dashboard/signup": true, "https://prismaport.com/": true}
	hrefs := regexp.MustCompile(`\shref="([^"]+)"`).FindAllStringSubmatch(page, -1)
	if len(hrefs) == 0 {
		t.Fatal("no links")
	}
	for _, m := range hrefs {
		h := m[1]
		switch {
		case strings.HasPrefix(h, "#"):
			if !ids[h[1:]] {
				t.Errorf("%s points at no element", h)
			}
		case strings.HasPrefix(h, LandingAssetPrefix), allowed[h]:
		default:
			t.Errorf("unexpected link %s", h)
		}
	}
	for _, want := range []string{`href="/dashboard/signup"`, `href="/dashboard/login"`, `href="/docs/"`, `href="#how-it-works"`, `href="#pricing"`,
		`<span class="chip chip-soon">Solana coming later</span>`, `<html lang="en">`, `<link rel="canonical" href="https://prismaport.com/">`,
		`<button class="nav-toggle" type="button" aria-expanded="false" aria-controls="nav-menu" aria-label="Menu">`, `<div class="nav-menu" id="nav-menu">`,
		`<span data-a="explorer">Settlement and fee visible on Basescan</span>`, "Anyone can check it on Basescan.",
		`<div class="demo">pay.prismaport.com/l/plink_01JB8YX4Q7R8KZ3M5N2TPWH6AD</div>`} {
		if !strings.Contains(page, want) {
			t.Errorf("index.html lacks %s", want)
		}
	}
	for _, gone := range []string{"Talk to us", ">Status<", ">Terms<", "fonts.googleapis", "Switch to live", "Readable on", "Solscan", "data-anet", "netswitch", "USDT", "PrismaPort", "\u2014"} {
		if strings.Contains(page, gone) {
			t.Errorf("index.html still has %q", gone)
		}
	}
}

// The page repeats the fee the contracts charge: a new fee has to change the calculator, the
// headline and the worked example with it.
func TestLandingShowsThePilotFee(t *testing.T) {
	script, err := fs.ReadFile(landingweb.Site(), "landing.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		fmt.Sprintf("const FEE_BPS = %d;", chain.PilotFee().FeeBps),
		fmt.Sprintf("const FIXED_FEE = %d;", chain.PilotFee().FixedFeeMicros),
	} {
		if !strings.Contains(string(script), want) {
			t.Errorf("landing.js lacks %s", want)
		}
	}
	page := landingIndex(t)
	pct := strconv.FormatFloat(float64(chain.PilotFee().FeeBps)/100, 'f', -1, 64) + "%"
	usdc := func(u money.Units) string { return money.FormatMinPlaces(u, 6, 2) }
	fixed := usdc(money.FromUint64(chain.PilotFee().FixedFeeMicros))
	for _, want := range []string{
		`<div class="bigpct">` + pct + `</div>`,
		`<span class="bigplus">+ ` + fixed + ` USDC</span>`,
		`<span class="label">Fee ` + pct + ` + ` + fixed + ` USDC</span>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("index.html lacks %s", want)
		}
	}
	const example = 30_000_000
	split, err := fee.Compute(money.FromUint64(example), 6, chain.PilotFee())
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]money.Units{"gross": split.Gross, "fee": split.Fee, "net": split.Net} {
		t.Run(key, func(t *testing.T) {
			shown := regexp.MustCompile(`data-v="`+key+`">([^<]+)<`).FindAllStringSubmatch(page, -1)
			if len(shown) == 0 {
				t.Errorf("index.html shows no %s", key)
			}
			for _, m := range shown {
				got, err := money.ParseDecimal(m[1], 6)
				if err != nil || got.Cmp(want) != 0 {
					t.Errorf("the worked example shows %s %s, want %s base units (%v)", key, m[1], want, err)
				}
			}
		})
	}
	// The rows landing.js rebuilds still show without JavaScript, and the share card is an image.
	shares, err := payout.Shares(split.Net, []uint16{7000, 3000})
	if err != nil {
		t.Fatal(err)
	}
	paid := strconv.FormatFloat(float64(split.Fee.Big().Int64())/example*100, 'f', 2, 64) + "%"
	for _, want := range []string{
		`<span class="s-amt">` + usdc(shares[0]) + ` USDC</span>`,
		`<span class="s-amt">` + usdc(shares[1]) + ` USDC</span>`,
		`<td class="a">` + usdc(shares[0]) + ` USDC</td>`,
		`<td class="a">` + usdc(shares[1]) + ` USDC</td>`,
		`<td class="a">` + usdc(split.Fee) + ` USDC</td>`,
		`data-v="pct">` + paid + `<`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("index.html lacks %s", want)
		}
	}
	og, err := os.ReadFile("../../web/landing/og/og.svg")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		">Fee " + pct + " + " + fixed + ", split on-chain.<",
		">" + usdc(split.Gross) + " USDC<",
		">" + usdc(split.Net) + " USDC<",
		">" + usdc(split.Fee) + " USDC<",
	} {
		if !strings.Contains(string(og), want) {
			t.Errorf("og/og.svg lacks %s", want)
		}
	}
}

// The api mounts every web app on one mux; their routes must not collide.
func TestAllPagesMountTogether(t *testing.T) {
	s := New(nil, nil, nil)
	if err := s.MountCheckoutPages(checkoutweb.Dist()); err != nil {
		t.Fatal(err)
	}
	if err := s.MountDashboardPages(dashboardweb.Dist()); err != nil {
		t.Fatal(err)
	}
	s.MountDocsPages(docsweb.Dist())
	s.MountLandingPages(landingweb.Site(), "https://pay.example.com")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Security-Policy") != LandingCSP {
		t.Fatalf("/: %d %v", rec.Code, rec.Header())
	}
}

func metaContent(t *testing.T, page, attr, name string) string {
	t.Helper()
	m := regexp.MustCompile(`<meta `+attr+`="`+regexp.QuoteMeta(name)+`" content="([^"]*)">`).FindAllStringSubmatch(page, -1)
	if len(m) != 1 {
		t.Fatalf("index.html has %d %s meta tags", len(m), name)
	}
	return m[0][1]
}

// The image people see is site/og.png: an edit to its source has to be rendered too.
func TestLandingShareCardIsRendered(t *testing.T) {
	svg, err := os.ReadFile("../../web/landing/og/og.svg")
	if err != nil {
		t.Fatal(err)
	}
	stamp, err := os.ReadFile("../../web/landing/og/og.svg.sha256")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(svg)
	if got, want := strings.TrimSpace(string(stamp)), hex.EncodeToString(sum[:]); got != want {
		t.Errorf("og/og.svg changed since site/og.png was rendered, run task web:landing:og (rendered %s, source %s)", got, want)
	}
}

// The share card repeats the hero, and its image is a 1200 x 630 PNG the site embeds.
func TestLandingShareCard(t *testing.T) {
	page := landingIndex(t)
	h1 := regexp.MustCompile(`<h1 class="h-xl">(.+?)</h1>`).FindStringSubmatch(page)
	if h1 == nil {
		t.Fatal("no hero heading")
	}
	h1[1] = regexp.MustCompile(`<[^>]+>`).ReplaceAllString(h1[1], "")
	const lede = "Every payment lands in your wallet. Every fee shows on-chain."
	image := LandingOrigin + "/site/og.png"
	for _, c := range []struct{ attr, name, want string }{
		{"property", "og:title", h1[1]},
		{"property", "og:description", lede},
		{"property", "og:url", LandingOrigin + "/"},
		{"property", "og:image", image},
		{"property", "og:image:type", "image/png"},
		{"property", "og:image:width", "1200"},
		{"property", "og:image:height", "630"},
		{"name", "twitter:card", "summary_large_image"},
		{"name", "twitter:title", h1[1]},
		{"name", "twitter:description", lede},
		{"name", "twitter:image", image},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := metaContent(t, page, c.attr, c.name); got != c.want {
				t.Errorf("%s = %q, want %q", c.name, got, c.want)
			}
		})
	}
	for _, c := range [][2]string{{"property", "og:image:alt"}, {"name", "twitter:image:alt"}} {
		if alt := metaContent(t, page, c[0], c[1]); !strings.HasPrefix(alt, "Prismaport: ") {
			t.Errorf("%s = %q", c[1], alt)
		}
	}
	b, err := fs.ReadFile(landingweb.Site(), "og.png")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(b))
	if err != nil || cfg.Width != 1200 || cfg.Height != 630 {
		t.Fatalf("og.png: %v %dx%d", err, cfg.Width, cfg.Height)
	}
	if len(b) > 100_000 {
		t.Errorf("og.png is %d bytes; a crawler fetches it on every share", len(b))
	}
}

// Crawlers need absolute URLs, so the served page names the deployment's own origin.
func TestLandingAbsoluteURLsFollowThePublicOrigin(t *testing.T) {
	srv := landingServerAt(t, landingweb.Site(), "https://pay.example.com/ignored/path?q=1")
	rewritten, body := get(t, srv.URL+"/")
	for _, want := range []string{`<link rel="canonical" href="https://pay.example.com/">`, `<meta property="og:url" content="https://pay.example.com/">`,
		`<meta property="og:image" content="https://pay.example.com/site/og.png">`, `<meta name="twitter:image" content="https://pay.example.com/site/og.png">`} {
		if !strings.Contains(body, want) {
			t.Errorf("/ lacks %s", want)
		}
	}
	if strings.Contains(body, LandingOrigin) {
		t.Errorf("/ still names %s", LandingOrigin)
	}
	if strings.ReplaceAll(body, "https://pay.example.com/", LandingOrigin+"/") != landingIndex(t) {
		t.Error("the rewrite touched more than the absolute URLs")
	}

	for _, publicURL := range []string{"", "pay.example.com", "ftp://pay.example.com", "https://", "https://user:pw@pay.example.com", "::"} {
		srv := landingServerAt(t, landingweb.Site(), publicURL)
		resp, body := get(t, srv.URL+"/")
		if body != landingIndex(t) {
			t.Errorf("%q: the page was rewritten", publicURL)
		}
		if resp.Header.Get("ETag") == rewritten.Header.Get("ETag") {
			t.Errorf("%q: the ETag does not follow the served bytes", publicURL)
		}
	}

	if o, ok := landingPublicOrigin(" http://localhost:58180 "); !ok || o != "http://localhost:58180" {
		t.Errorf("localhost: %q %v", o, ok)
	}
}

func markShape(svg string) []string {
	var shape []string
	for _, m := range regexp.MustCompile(`<path d="([^"]+)"[^>]*stroke-width="([^"]+)"`).FindAllStringSubmatch(svg, -1) {
		shape = append(shape, m[1]+" @"+m[2])
	}
	return shape
}

// The landing, its favicon and its share card draw the same prism as the apps' brand files.
func TestLandingDrawsTheBrandMark(t *testing.T) {
	read := func(p string) string {
		t.Helper()
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	mark := strings.Join(markShape(read("../../web/checkout/public/brand/prismaport-mark-white.svg")), "|")
	if strings.Count(mark, "|") != 2 {
		t.Fatalf("brand mark: %s", mark)
	}
	symbol := regexp.MustCompile(`(?s)<symbol id="pm"[^>]*>(.*?)</symbol>`).FindStringSubmatch(landingIndex(t))
	if symbol == nil || strings.Join(markShape(symbol[1]), "|") != mark {
		t.Errorf("the page's mark differs from the brand mark: %v", symbol)
	}
	favicon, err := fs.ReadFile(landingweb.Site(), "favicon.svg")
	if err != nil || string(favicon) != read("../../web/checkout/public/favicon.svg") {
		t.Errorf("site/favicon.svg differs from the checkout's: %v", err)
	}
	if og := markShape(read("../../web/landing/og/og.svg")); len(og) < 3 || strings.Join(og[:3], "|") != mark {
		t.Errorf("og/og.svg draws a different mark: %v", og)
	}
}

// The page says how long one code unlocks Live changes. It cannot read PRISMAPORT_CONFIRM_WINDOW,
// so landing.js holds the number once and the HTML its fallback, both guarded by the default.
func TestLandingShowsTheConfirmWindow(t *testing.T) {
	minutes := int(service.DefaultConfirmWindow / time.Minute)
	script, err := fs.ReadFile(landingweb.Site(), "landing.js")
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("const CONFIRM_WINDOW_MINUTES = %d;", minutes); !strings.Contains(string(script), want) {
		t.Errorf("landing.js lacks %s", want)
	}
	page := landingIndex(t)
	for _, want := range []string{
		fmt.Sprintf(`<span data-confirm-minutes>%d</span> minutes`, minutes),
		fmt.Sprintf(`<span class="mono" data-confirm-clock>%d:00</span>`, minutes),
	} {
		if !strings.Contains(page, want) {
			t.Errorf("landing page lacks %s", want)
		}
	}
	if n := strings.Count(page, "data-confirm-minutes"); n != 2 {
		t.Errorf("landing page has %d confirm-window minutes, want 2", n)
	}
}
