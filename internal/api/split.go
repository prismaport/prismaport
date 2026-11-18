package api

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/net/publicsuffix"
)

// splitFrontend is split mode: the pages are served elsewhere, the
// api serves only the API and trusts the dashboard's origin across origins. Each field is an
// origin ("https://app.prismaport.com").
type splitFrontend struct {
	site, dashboard, checkout, docs, api string
}

const (
	dashCORSMethods = "GET, POST, PATCH, PUT, DELETE"
	dashCORSHeaders = "Content-Type, " + CSRFHeader + ", Idempotency-Key"
	dashCORSExpose  = "Retry-After, Idempotent-Replayed"
)

var splitEnv = map[string]string{"site": "PRISMAPORT_SITE_URL", "dashboard": "PRISMAPORT_DASHBOARD_URL",
	"checkout": "PRISMAPORT_CHECKOUT_BASE_URL", "docs": "PRISMAPORT_DOCS_URL", "API": "PRISMAPORT_API_URL"}

func newSplitFrontend(h Hosts) (*splitFrontend, error) { return newSplitFrontendNamed(h, splitEnv) }

// newSplitFrontendNamed checks h for split mode; label names each URL's setting in errors.
func newSplitFrontendNamed(h Hosts, label map[string]string) (*splitFrontend, error) {
	urls := map[string]*url.URL{}
	for _, c := range []struct {
		name, raw, path string
	}{
		{"site", h.Site, "/"},
		{"dashboard", h.Dashboard, "/dashboard/"},
		{"checkout", h.Checkout, "/"},
		{"docs", h.Docs, "/"},
		{"API", h.API, "/"},
	} {
		if c.raw == "" {
			return nil, fmt.Errorf("%s (the %s URL) is required with PRISMAPORT_FRONTEND=external", label[c.name], c.name)
		}
		u, err := hostURL(c.name, c.raw, c.path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", label[c.name], err)
		}
		urls[c.name] = u
	}
	origin := func(name string) string { return urls[name].Scheme + "://" + normalHost(urls[name].Host) }
	f := &splitFrontend{site: origin("site"), dashboard: origin("dashboard"), checkout: origin("checkout"), docs: origin("docs"), api: origin("API")}
	for _, other := range []string{f.site, f.dashboard, f.checkout, f.docs} {
		if other == f.api {
			return nil, fmt.Errorf("%s: the API's origin %s also serves a page; the api serves no page in split mode, so give the pages other hosts", label["API"], f.api)
		}
	}
	for _, other := range []string{f.site, f.dashboard, f.checkout} {
		if other == f.docs {
			return nil, fmt.Errorf("%s: the developer portal's origin %s also serves another page; the portal is served at the root of its host, so give it a host of its own", label["docs"], f.docs)
		}
	}
	app, api := urls["dashboard"], urls["API"]
	if app.Scheme != api.Scheme || !sameSiteHosts(strings.ToLower(app.Hostname()), strings.ToLower(api.Hostname())) {
		return nil, fmt.Errorf("%s and %s must be same-site (one scheme and one registrable domain, such as app.example.com and api.example.com), got %s and %s: browsers send the SameSite=Lax session cookie only on same-site requests",
			label["dashboard"], label["API"], f.dashboard, f.api)
	}
	return f, nil
}

// sameSiteHosts says whether two hosts are one site, as browsers decide it for SameSite cookies:
// the same registrable domain on the public suffix list (app.example.com and api.example.com, but
// not a.co.uk and b.co.uk, nor two apps under up.railway.app). An IP address or a name without a
// registrable domain (localhost) is only its own site.
func sameSiteHosts(a, b string) bool {
	if a == b {
		return true
	}
	ra, errA := publicsuffix.EffectiveTLDPlusOne(a)
	rb, errB := publicsuffix.EffectiveTLDPlusOne(b)
	return errA == nil && errB == nil && net.ParseIP(a) == nil && net.ParseIP(b) == nil && ra == rb
}

// pageOrigin is the origin that serves the page path clean, or "" when clean is no page's.
func (f *splitFrontend) pageOrigin(clean string) string {
	under := pathUnder(clean)
	switch {
	case under("docs"):
		return f.docs
	case clean == "/" || under("site"):
		return f.site
	case under("dashboard"):
		return f.dashboard
	case under("pay") || under("l") || under("a") || under("s"):
		return f.checkout
	}
	return ""
}

// portalPath is where the docs host serves the developer portal's path clean (/docs, /docs/…):
// at its root, so /docs/ is / and /docs/openapi.json is /openapi.json. clean stays escaped.
func portalPath(clean string) string {
	_, rest, _ := strings.Cut(strings.TrimPrefix(clean, "/"), "/")
	return "/" + rest
}

// serveSplit serves the API on every host and sends page paths to the host that serves them.
func (s *Server) serveSplit(w http.ResponseWriter, r *http.Request) {
	f := s.split
	clean := cleanPath(r.URL.EscapedPath())
	if pathUnder(clean)("v1", "dashboard") {
		if s.dashboardCORS(w, r) {
			return
		}
		s.mux.ServeHTTP(w, r)
		return
	}
	origin := f.pageOrigin(clean)
	if origin == "" {
		s.mux.ServeHTTP(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		s.wrongHost(w, r, origin)
		return
	}
	if clean == "/" {
		redirectTo(w, r, f.site+"/", http.StatusFound)
		return
	}
	target := origin + r.URL.RequestURI()
	if pathUnder(clean)("docs") {
		target = origin + portalPath(clean)
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
	}
	redirectTo(w, r, target, http.StatusPermanentRedirect)
}

// dashboardCORS adds the dashboard API's CORS headers in split mode: the configured dashboard
// origin, compared exactly and never reflected, with credentials. It answers a preflight itself
// and reports whether it did.
func (s *Server) dashboardCORS(w http.ResponseWriter, r *http.Request) bool {
	h := w.Header()
	h.Add("Vary", "Origin")
	allowed := r.Header.Get("Origin") == s.split.dashboard
	if allowed {
		h.Set("Access-Control-Allow-Origin", s.split.dashboard)
		h.Set("Access-Control-Allow-Credentials", "true")
		h.Set("Access-Control-Expose-Headers", dashCORSExpose)
	}
	if r.Method != http.MethodOptions || r.Header.Get("Access-Control-Request-Method") == "" {
		return false
	}
	dashHeaders(w)
	if !allowed {
		s.writeError(w, r, csrfFailed("this origin may not call the dashboard API"))
		return true
	}
	h.Set("Access-Control-Allow-Methods", dashCORSMethods)
	h.Set("Access-Control-Allow-Headers", dashCORSHeaders)
	h.Set("Access-Control-Max-Age", "600")
	w.WriteHeader(http.StatusNoContent)
	return true
}

// fetchSite refuses dashboard requests from pages other than the dashboard. Embedded, the
// dashboard is same-origin, so any request the browser marks same-site or cross-site is refused.
// Split, the dashboard is same-site with the API: a same-site request must come from the
// dashboard's origin exactly, and any request naming another Origin is refused (browsers without
// Sec-Fetch-Site still send Origin on cross-origin requests).
func (s *Server) fetchSite(r *http.Request) error {
	if s.split == nil {
		return sameSite(r)
	}
	origin := r.Header.Get("Origin")
	switch r.Header.Get("Sec-Fetch-Site") {
	case "cross-site":
		return csrfFailed("cross-site requests are not allowed")
	case "same-site":
		if origin != s.split.dashboard {
			return csrfFailed("requests from another host are not allowed")
		}
		return nil
	}
	if origin != "" && origin != s.split.dashboard {
		return csrfFailed("requests from another origin are not allowed")
	}
	return nil
}
