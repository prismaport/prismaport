package api

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/prismaport/prismaport/internal/service"
)

// Hosts is the public address of each surface. Routing by host is
// on only when Site is set; then Dashboard and API are required too. With External the pages are
// served elsewhere (split mode): all five are required and routing by host is
// off.
type Hosts struct {
	// Site serves the landing page ("https://prismaport.com"); www. in front of its host
	// redirects to it.
	Site string
	// Dashboard is the dashboard's address, its path /dashboard/ ("https://app.prismaport.com/dashboard/").
	Dashboard string
	// Checkout serves the hosted checkout and the payer-side API ("https://pay.prismaport.com").
	Checkout string
	// API serves the merchant API, the sandbox inbox and the developer portal
	// ("https://api.prismaport.com").
	API string
	// Docs serves the developer portal at its root in split mode ("https://docs.prismaport.com");
	// unused otherwise, where the binary serves the portal at /docs/.
	Docs string
	// External (PRISMAPORT_FRONTEND=external) is split mode: the api serves no page.
	External bool
}

type hostRole uint8

const (
	roleSite hostRole = 1 << iota
	roleApp
	rolePay
	roleAPI
)

// redirectOrder picks the host a path served by several roles redirects to.
var redirectOrder = []hostRole{roleAPI, rolePay, roleApp, roleSite}

type hostRouter struct {
	roles   map[string]hostRole
	origins map[hostRole]string
	// www redirects to the site; empty when the site's host already starts with www.
	www string
}

// ConfigureHosts turns routing by host on when h.Site is set (and leaves it off otherwise). Call
// it before MountLandingPages, which makes the landing page's links absolute from it.
func (s *Server) ConfigureHosts(h Hosts) error {
	if s.landingMounted {
		return errors.New("configure hosts before mounting the landing page")
	}
	if h.External {
		f, err := newSplitFrontend(h)
		if err != nil {
			return err
		}
		s.split, s.hosts = f, nil
		return nil
	}
	r, err := newHostRouter(h)
	if err != nil {
		return err
	}
	s.hosts, s.split = r, nil
	return nil
}

func newHostRouter(h Hosts) (*hostRouter, error) {
	if h.Site == "" {
		return nil, nil
	}
	r := &hostRouter{roles: map[string]hostRole{}, origins: map[hostRole]string{}}
	var site string
	for _, c := range []struct {
		role hostRole
		name string
		raw  string
		path string
	}{
		{roleSite, "site", h.Site, "/"},
		{roleApp, "dashboard", h.Dashboard, "/dashboard/"},
		{rolePay, "checkout", h.Checkout, "/"},
		{roleAPI, "API", h.API, "/"},
	} {
		u, err := hostURL(c.name, c.raw, c.path)
		if err != nil {
			return nil, err
		}
		host := normalHost(u.Host)
		r.roles[host] |= c.role
		r.origins[c.role] = u.Scheme + "://" + host
		if c.role == roleSite {
			site = host
		}
	}
	if !strings.HasPrefix(site, "www.") {
		if _, taken := r.roles["www."+site]; !taken {
			r.www = "www." + site
		}
	}
	return r, nil
}

// hostURL checks one surface's address: absolute, https (plain http only to this machine, for
// local runs), no credentials, query or fragment, and the path the surface is served at.
func hostURL(name, raw, wantPath string) (*url.URL, error) {
	if raw == "" {
		return nil, fmt.Errorf("the %s URL is required with the site URL", name)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return nil, fmt.Errorf("the %s URL must be an absolute URL without credentials, query or fragment, got %q", name, raw)
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && loopbackName(u.Hostname()):
	default:
		return nil, fmt.Errorf("the %s URL must use https (plain http only to localhost), got %q", name, raw)
	}
	p := u.Path
	if p == "" {
		p = "/"
	}
	if p != wantPath && p+"/" != wantPath {
		return nil, fmt.Errorf("the %s URL must have the path %s, got %q", name, wantPath, raw)
	}
	return u, nil
}

func loopbackName(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// normalHost compares hosts the way browsers address them: case-insensitive, without a trailing
// dot or a default port.
func normalHost(h string) string {
	h = strings.ToLower(h)
	for _, port := range []string{":443", ":80"} {
		h = strings.TrimSuffix(h, port)
	}
	return strings.TrimSuffix(h, ".")
}

// pathRole says which surface serves an escaped, cleaned path; 0 for a path no surface owns (it
// stays the mux's 404 on every host). Like the mux it unescapes each segment on its own, so an
// escaped slash stays inside its segment ("/dashboard/..%2F" is the dashboard's).
func pathRole(p string) hostRole {
	under := pathUnder(p)
	switch {
	case p == "/" || under("site"):
		return roleSite
	case under("dashboard") || under("v1", "dashboard"):
		return roleApp
	case under("pay") || under("l") || under("a") || under("s"):
		return rolePay
	case under("v1", "checkout", "logos"):
		// Merchant logos are public files; the dashboard shows the merchant its own from its host.
		return rolePay | roleAPI | roleApp
	case under("v1", "checkout"):
		// The hosted checkout calls it on its own origin; integrators find it on the API host.
		return rolePay | roleAPI
	case under("docs") || under("v1"):
		return roleAPI
	}
	return 0
}

// pathUnder returns a test of whether the escaped, cleaned path p starts with the given segments,
// each segment unescaped on its own as the mux does.
func pathUnder(p string) func(prefix ...string) bool {
	segs := strings.Split(strings.TrimPrefix(p, "/"), "/")
	for i, seg := range segs {
		if u, err := url.PathUnescape(seg); err == nil {
			segs[i] = u
		}
	}
	return func(prefix ...string) bool {
		return len(segs) >= len(prefix) && slices.Equal(segs[:len(prefix)], prefix)
	}
}

// cleanPath is the path the mux would route p to; p is escaped, as the mux cleans it.
func cleanPath(p string) string {
	if p == "" {
		return "/"
	}
	c := path.Clean(p)
	if strings.HasSuffix(p, "/") && c != "/" {
		c += "/"
	}
	return c
}

func (h *hostRouter) origin(owner hostRole) string {
	for _, role := range redirectOrder {
		if owner&role != 0 {
			return h.origins[role]
		}
	}
	return ""
}

// serveHost answers on every host what is its own, redirects a GET for another surface's path to
// that surface's host and refuses any other method there.
func (s *Server) serveHost(w http.ResponseWriter, r *http.Request) {
	h := s.hosts
	if p := r.URL.Path; p == "/healthz" || p == "/readyz" || p == "/version" {
		s.mux.ServeHTTP(w, r)
		return
	}
	read := r.Method == http.MethodGet || r.Method == http.MethodHead
	host := normalHost(r.Host)
	if h.www != "" && host == h.www {
		if !read {
			s.wrongHost(w, r, h.origins[roleSite])
			return
		}
		redirectTo(w, r, h.origins[roleSite]+r.URL.RequestURI(), http.StatusMovedPermanently)
		return
	}
	roles := h.roles[host]
	clean := cleanPath(r.URL.EscapedPath())
	if clean == "/" && roles&roleSite == 0 && read {
		switch {
		case roles&roleApp != 0:
			redirectTo(w, r, "/dashboard/", http.StatusFound)
			return
		case roles&roleAPI != 0:
			redirectTo(w, r, "/docs/", http.StatusFound)
			return
		}
	}
	owner := pathRole(clean)
	if owner == 0 || roles&owner != 0 {
		s.mux.ServeHTTP(w, r)
		return
	}
	if !read {
		s.wrongHost(w, r, h.origin(owner))
		return
	}
	redirectTo(w, r, h.origin(owner)+r.URL.RequestURI(), http.StatusPermanentRedirect)
}

func redirectTo(w http.ResponseWriter, r *http.Request, target string, status int) {
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, target, status)
}

func (s *Server) wrongHost(w http.ResponseWriter, r *http.Request, origin string) {
	s.writeError(w, r, &service.Error{Kind: service.KindNotFound, Code: service.CodeNotFound,
		Message: "no such route on this host; it is served at " + origin})
}

var rootHref = regexp.MustCompile(`href="(/[^"]*)"`)

// absoluteLinks points every root-relative link of page that another surface serves at that
// surface's host, so the landing page links straight to the dashboard and the docs.
func (h *hostRouter) absoluteLinks(page []byte) []byte {
	if h == nil {
		return page
	}
	return absoluteLinks(page, func(clean, link string) string {
		owner := pathRole(clean)
		if owner == 0 || owner&roleSite != 0 || h.origin(owner) == h.origins[roleSite] {
			return ""
		}
		return h.origin(owner) + link
	})
}

// absoluteLinks replaces every root-relative href of page with what elsewhere returns for its
// cleaned path and the link as written; an empty answer leaves the link as it is.
func absoluteLinks(page []byte, elsewhere func(clean, link string) string) []byte {
	return rootHref.ReplaceAllFunc(page, func(m []byte) []byte {
		link := string(rootHref.FindSubmatch(m)[1])
		p, _, _ := strings.Cut(link, "#")
		p, _, _ = strings.Cut(p, "?")
		to := elsewhere(cleanPath(p), link)
		if to == "" {
			return m
		}
		return []byte(`href="` + to + `"`)
	})
}
