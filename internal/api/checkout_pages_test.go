package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/chainpause"
	"github.com/prismaport/prismaport/internal/store"
	"github.com/prismaport/prismaport/internal/view"
)

func pagesServer(t *testing.T, fsys fstest.MapFS) *httptest.Server {
	t.Helper()
	s := New(nil, nil, nil)
	if err := s.MountCheckoutPages(fsys); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, url string, hdr ...string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

var builtFS = fstest.MapFS{
	"index.html":              {Data: []byte(`<!doctype html><title>Prismaport</title><script type="module" src="/pay/assets/index-abc123.js"></script>`)},
	"assets/index-abc123.js":  {Data: []byte(`console.log(1)`)},
	"assets/index-abc123.css": {Data: []byte(`body{}`)},
	"favicon.svg":             {Data: []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`)},
	"brand/mark.svg":          {Data: []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`)},
	"version.json":            {Data: []byte(`{"version":"0.1.0"}`)},
}

func TestCheckoutPagesShell(t *testing.T) {
	srv := pagesServer(t, builtFS)
	for _, path := range []string{"/pay/ses_01JBX5Q2", "/l/plink_01JBX5Q2", "/l/prod_01JBX5Q2", "/a/caddr_01JBX5Q2"} {
		resp, body := get(t, srv.URL+path)
		if resp.StatusCode != http.StatusOK || !strings.Contains(body, "index-abc123.js") {
			t.Fatalf("%s: %d %s", path, resp.StatusCode, body)
		}
		h := resp.Header
		if h.Get("Content-Type") != "text/html; charset=utf-8" || h.Get("Cache-Control") != "no-cache" ||
			h.Get("Content-Security-Policy") != CheckoutCSP || h.Get("X-Content-Type-Options") != "nosniff" ||
			h.Get("X-Frame-Options") != "DENY" || h.Get("Referrer-Policy") != "no-referrer" || h.Get("ETag") == "" {
			t.Fatalf("%s headers: %v", path, h)
		}
		again, _ := get(t, srv.URL+path, "If-None-Match", h.Get("ETag"))
		if again.StatusCode != http.StatusNotModified {
			t.Fatalf("%s conditional: %d", path, again.StatusCode)
		}
	}
}

func TestCheckoutPagesAssets(t *testing.T) {
	srv := pagesServer(t, builtFS)
	resp, body := get(t, srv.URL+"/pay/assets/index-abc123.js")
	if resp.StatusCode != http.StatusOK || body != "console.log(1)" ||
		resp.Header.Get("Cache-Control") != "public, max-age=31536000, immutable" ||
		resp.Header.Get("Content-Type") != "text/javascript; charset=utf-8" ||
		resp.Header.Get("Content-Security-Policy") != CheckoutCSP {
		t.Fatalf("asset: %d %v", resp.StatusCode, resp.Header)
	}
	resp, _ = get(t, srv.URL+"/pay/assets/index-abc123.css")
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/css") {
		t.Fatalf("css type: %v", resp.Header)
	}
	resp, _ = get(t, srv.URL+"/pay/favicon.svg")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Cache-Control") != "public, max-age=3600" ||
		resp.Header.Get("Content-Type") != "image/svg+xml" {
		t.Fatalf("favicon: %d %v", resp.StatusCode, resp.Header)
	}
	if resp, _ := get(t, srv.URL+"/pay/brand/mark.svg"); resp.StatusCode != http.StatusOK {
		t.Fatalf("nested file: %d", resp.StatusCode)
	}
	resp, _ = get(t, srv.URL+"/pay/version.json")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Cache-Control") != "no-cache" ||
		resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("version.json: %d %v", resp.StatusCode, resp.Header)
	}
}

func TestCheckoutPagesNotFound(t *testing.T) {
	srv := pagesServer(t, builtFS)
	for _, path := range []string{
		"/pay/", "/pay/index.html", "/pay/assets/", "/pay/assets/missing.js", "/pay/brand", "/pay/ses_", "/pay/ses_a-b",
		"/pay/" + "ses_" + strings.Repeat("a", 61), "/l/ses_01JB", "/l/prod_", "/l/prod_a.b", "/l/plink_", "/l/plink_a.b", "/l/plink_" + strings.Repeat("a", 59), "/pay/assets/../index.html", "/s/prod_1",
		"/a/", "/a/caddr_", "/a/caddr_a.b", "/a/ses_01JB", "/a/caddr_" + strings.Repeat("a", 59),
	} {
		resp, body := get(t, srv.URL+path)
		if resp.StatusCode != http.StatusNotFound && (resp.StatusCode < 300 || resp.StatusCode > 399) {
			t.Errorf("%s: %d %s", path, resp.StatusCode, body)
		}
		if resp.StatusCode == http.StatusOK && strings.Contains(body, "<title>") {
			t.Errorf("%s served the shell", path)
		}
	}
	if resp, _ := get(t, srv.URL+"/pay/ses_01JB/extra"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("nested session path: %d", resp.StatusCode)
	}
}

func TestCheckoutLegacySessionURL(t *testing.T) {
	srv := pagesServer(t, builtFS)
	resp, _ := get(t, srv.URL+"/s/ses_01JBX5Q2")
	if resp.StatusCode != http.StatusPermanentRedirect || resp.Header.Get("Location") != "/pay/ses_01JBX5Q2" {
		t.Fatalf("redirect: %d %v", resp.StatusCode, resp.Header)
	}
}

func TestCheckoutPagesNotBuilt(t *testing.T) {
	srv := pagesServer(t, fstest.MapFS{".gitkeep": {Data: nil}})
	resp, _ := get(t, srv.URL+"/pay/ses_01JBX5Q2")
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("unbuilt: %d %v", resp.StatusCode, resp.Header)
	}
	if resp, _ := get(t, srv.URL+"/pay/.gitkeep"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("placeholder served: %d", resp.StatusCode)
	}
}

func TestCheckoutCSPIsStrict(t *testing.T) {
	for _, bad := range []string{"unsafe-inline", "unsafe-eval", "*", "http:", "https:", "data:", "googleapis", "gstatic"} {
		if strings.Contains(CheckoutCSP, bad) {
			t.Fatalf("CSP allows %q: %s", bad, CheckoutCSP)
		}
	}
	for _, need := range []string{"default-src 'none'", "script-src 'self'", "style-src 'self';", "font-src 'self';",
		"frame-ancestors 'none'", "connect-src 'self'", "base-uri 'none'"} {
		if !strings.Contains(CheckoutCSP, need) {
			t.Fatalf("CSP lacks %q", need)
		}
	}
}

func TestClientIP(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "192.0.2.1:5555"
	r.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.1")
	if got := clientIP(r, ""); got != "192.0.2.1" {
		t.Fatalf("untrusted header used: %s", got)
	}
	if got := clientIP(r, "X-Forwarded-For"); got != "203.0.113.9" {
		t.Fatalf("trusted header: %s", got)
	}
	r.Header.Set("X-Forwarded-For", "not-an-ip")
	if got := clientIP(r, "X-Forwarded-For"); got != "192.0.2.1" {
		t.Fatalf("bad header value: %s", got)
	}
}

func TestPilotNetworksOfferNoEVMTransferPath(t *testing.T) {
	for _, n := range view.Networks(chain.Pilot(), chainpause.Set{}) {
		if n.FeeBps != 50 || n.FixedFee != "0.10" {
			t.Errorf("%s fee %d/%s", n.ID, n.FeeBps, n.FixedFee)
		}
		if n.Family != "evm" {
			continue
		}
		if n.ID != "base" && n.ID != "base-sepolia" {
			t.Errorf("EVM network %s in the pilot", n.ID)
		}
		for _, a := range n.Assets {
			if a.Symbol != "USDC" || !a.EIP3009 {
				t.Errorf("%s offers %s without EIP-3009: the plain transfer path must be unreachable", n.ID, a.Symbol)
			}
		}
	}
}

func TestConfigurePublicValidatesLimits(t *testing.T) {
	s := New(nil, nil, nil)
	st := store.New(nil, nil)
	for name, l := range map[string]map[string]Limit{
		"unknown route":     {"login": {PerMinute: 10, Burst: 5}},
		"zero rate":         {RouteLinkOpen: {PerMinute: 0, Burst: 5}},
		"refill beyond 1 h": {RouteLinkOpen: {PerMinute: 1, Burst: 61}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := s.ConfigurePublic(PublicOptions{Limits: l, Store: st}); err == nil {
				t.Errorf("%s accepted", name)
			}
		})
	}
	if err := s.ConfigurePublic(PublicOptions{Limits: map[string]Limit{RouteSolanaSubmit: {PerMinute: 1, Burst: 60}}, Store: st}); err != nil {
		t.Fatal(err)
	}
	if got := s.limiter.limits; got[RouteSolanaSubmit].Burst != 60 || got[RouteLinkOpen] != DefaultLimits()[RouteLinkOpen] {
		t.Fatalf("limits %v", got)
	}
}

func TestCheckoutServesSelfHostedFonts(t *testing.T) {
	fsys := fstest.MapFS{
		"index.html": builtFS["index.html"],
		"assets/onest-latin-400-normal-9AYXx.woff2": {Data: []byte("wOF2")},
		"fonts/OFL-Onest.txt":                       {Data: []byte("SIL OPEN FONT LICENSE Version 1.1")},
	}
	srv := pagesServer(t, fsys)
	resp, body := get(t, srv.URL+"/pay/assets/onest-latin-400-normal-9AYXx.woff2")
	if resp.StatusCode != http.StatusOK || body != "wOF2" || resp.Header.Get("Content-Type") != "font/woff2" ||
		resp.Header.Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Fatalf("font: %d %v", resp.StatusCode, resp.Header)
	}
	resp, body = get(t, srv.URL+"/pay/fonts/OFL-Onest.txt")
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") || !strings.Contains(body, "OPEN FONT LICENSE") {
		t.Fatalf("license: %d %v", resp.StatusCode, resp.Header)
	}
}
