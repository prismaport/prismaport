package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

// DashboardCSP is the dashboard's Content-Security-Policy: like the checkout, everything comes
// from this origin. QR codes are drawn as inline SVG or on a canvas; a PNG download goes through
// a blob URL on an <a download>, which no fetch or image directive governs.
const DashboardCSP = "default-src 'none'; script-src 'self'; style-src 'self'; font-src 'self'; img-src 'self'; " +
	"connect-src 'self'; manifest-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

func dashboardPageHeaders(w http.ResponseWriter) {
	pageHeaders(w)
	w.Header().Set("Content-Security-Policy", DashboardCSP)
}

// MountDashboardPages serves the dashboard single-page app built by web/dashboard (Vite base
// /dashboard/) from fsys. Files with a known extension are served as they are (hashed bundles
// under assets/ are immutable); every other path under /dashboard/ is a client route and gets
// the app shell. Without a build the shell answers 503. It refuses a build made for the Pages
// directories (see embeddedIndex).
func (s *Server) MountDashboardPages(fsys fs.FS) error {
	index, _ := fs.ReadFile(fsys, "index.html")
	if err := embeddedIndex(index, "dashboard"); err != nil {
		return err
	}
	sum := sha256.Sum256(index)
	etag := `"` + hex.EncodeToString(sum[:8]) + `"`

	shell := func(w http.ResponseWriter, r *http.Request) {
		dashboardPageHeaders(w)
		if index == nil {
			w.Header().Set("Cache-Control", "no-store")
			http.Error(w, "the dashboard is not built", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("ETag", etag)
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(index))
	}
	notFound := func(w http.ResponseWriter) {
		dashboardPageHeaders(w)
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "not found", http.StatusNotFound)
	}

	s.mux.HandleFunc("GET /dashboard", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, "/dashboard/", http.StatusPermanentRedirect)
	})
	s.mux.HandleFunc("GET /dashboard/{path...}", func(w http.ResponseWriter, r *http.Request) {
		p := r.PathValue("path")
		if strings.Contains(p, "..") || strings.Contains(p, "\\") || (p != "" && !fs.ValidPath(p)) {
			notFound(w)
			return
		}
		ext := path.Ext(p)
		ctype, known := assetTypes[ext]
		if p == "" || p == "index.html" || (ext == "" && !strings.HasPrefix(p, "assets/")) {
			shell(w, r)
			return
		}
		st, err := fs.Stat(fsys, p)
		if !known || err != nil || st.IsDir() {
			notFound(w)
			return
		}
		dashboardPageHeaders(w)
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
	return nil
}
