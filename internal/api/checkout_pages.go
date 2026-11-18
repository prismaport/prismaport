package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"strings"
	"time"
)

// CheckoutCSP is the Content-Security-Policy of the hosted checkout. The page talks only to
// this origin: the brand fonts are self-hosted in the build, and wallets are reached through
// the injected provider, never over the network.
const CheckoutCSP = "default-src 'none'; script-src 'self'; style-src 'self'; font-src 'self'; img-src 'self'; " +
	"connect-src 'self'; manifest-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

var (
	sessionPath = regexp.MustCompile(`^ses_[0-9A-Za-z]{1,60}$`)
	// linkPath matches payment link ids, and the product ids that were links before products had
	// links of their own: the page answers those as no longer available.
	linkPath = regexp.MustCompile(`^(plink_[0-9A-Za-z]{1,58}|prod_[0-9A-Za-z]{1,59})$`)
	// addressPath matches customer address ids (the page of a static customer address).
	addressPath = regexp.MustCompile(`^caddr_[0-9A-Za-z]{1,58}$`)
)

// versionFile is the build identity every Vite build emits (vite.config.ts); it is revalidated on
// every use so a probe never reads the previous deploy's.
const versionFile = "version.json"

// assetTypes pins the content type of every file the build emits. The OS MIME table is not
// used: on some Windows hosts it maps .js to text/plain, which nosniff then refuses to run.
var assetTypes = map[string]string{
	".js":          "text/javascript; charset=utf-8",
	".css":         "text/css; charset=utf-8",
	".svg":         "image/svg+xml",
	".png":         "image/png",
	".ico":         "image/x-icon",
	".woff2":       "font/woff2",
	".json":        "application/json",
	".txt":         "text/plain; charset=utf-8",
	".webmanifest": "application/manifest+json",
}

// pageHeaderValues are the headers every page carries next to its Content-Security-Policy, here
// and in the Pages build's _headers.
var pageHeaderValues = [][2]string{
	{"X-Content-Type-Options", "nosniff"},
	{"Referrer-Policy", "no-referrer"},
	{"X-Frame-Options", "DENY"},
	{"Cross-Origin-Opener-Policy", "same-origin"},
	{"Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()"},
	{"Strict-Transport-Security", HSTS},
}

func pageHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy", CheckoutCSP)
	for _, kv := range pageHeaderValues {
		h.Set(kv[0], kv[1])
	}
}

// MountCheckoutPages serves the hosted checkout, a single-page app built by web/checkout, from
// fsys: /pay/{session_id}, /l/{link_id} and /a/{customer_address_id} return the app shell, /pay/assets/ the hashed
// bundles. /s/{session_id} (the checkout_url of the merchant API) redirects to /pay/. It refuses
// a build made for the Pages directories (see embeddedIndex).
func (s *Server) MountCheckoutPages(fsys fs.FS) error {
	index, _ := fs.ReadFile(fsys, "index.html")
	if err := embeddedIndex(index, "checkout"); err != nil {
		return err
	}
	sum := sha256.Sum256(index)
	etag := `"` + hex.EncodeToString(sum[:8]) + `"`

	shell := func(w http.ResponseWriter, r *http.Request) {
		pageHeaders(w)
		if index == nil {
			w.Header().Set("Cache-Control", "no-store")
			http.Error(w, "the checkout page is not built", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("ETag", etag)
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(index))
	}
	notFound := func(w http.ResponseWriter) {
		pageHeaders(w)
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "not found", http.StatusNotFound)
	}

	s.mux.HandleFunc("GET /pay/{path...}", func(w http.ResponseWriter, r *http.Request) {
		p := r.PathValue("path")
		if sessionPath.MatchString(p) {
			shell(w, r)
			return
		}
		if p == "" || p == "index.html" || strings.Contains(p, "..") || !fs.ValidPath(p) {
			notFound(w)
			return
		}
		ctype, known := assetTypes[path.Ext(p)]
		st, err := fs.Stat(fsys, p)
		if err != nil || st.IsDir() || !known {
			notFound(w)
			return
		}
		pageHeaders(w)
		w.Header().Set("Content-Type", ctype)
		if strings.HasPrefix(p, "assets/") {
			// Vite names every file under assets/ by its content hash.
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else if p == versionFile {
			w.Header().Set("Cache-Control", "no-cache")
		} else {
			w.Header().Set("Cache-Control", "public, max-age=3600")
		}
		http.ServeFileFS(w, r, fsys, p)
	})
	s.mux.HandleFunc("GET /l/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !linkPath.MatchString(r.PathValue("id")) {
			notFound(w)
			return
		}
		shell(w, r)
	})
	s.mux.HandleFunc("GET /a/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !addressPath.MatchString(r.PathValue("id")) {
			notFound(w)
			return
		}
		shell(w, r)
	})
	s.mux.HandleFunc("GET /s/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !sessionPath.MatchString(id) {
			notFound(w)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, "/pay/"+id, http.StatusPermanentRedirect)
	})
	return nil
}
