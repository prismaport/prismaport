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

// DocsCSP is the developer portal's Content-Security-Policy. The page renders the API contract
// bundled into its script and never fetches anything, so it needs no connect-src.
const DocsCSP = "default-src 'none'; script-src 'self'; style-src 'self'; font-src 'self'; img-src 'self'; " +
	"connect-src 'none'; manifest-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// openAPIFile is the contract the portal renders, published next to it for client generators.
const openAPIFile = "openapi.json"

func contentETag(b []byte) string {
	sum := sha256.Sum256(b)
	return `"` + hex.EncodeToString(sum[:8]) + `"`
}

func docsPageHeaders(w http.ResponseWriter) {
	pageHeaders(w)
	w.Header().Set("Content-Security-Policy", DocsCSP)
}

// MountDocsPages serves the developer portal built by web/docs (Vite base /docs/) from fsys. The
// portal routes with the URL fragment, so only /docs/ itself is the page; hashed bundles under
// assets/ are immutable and openapi.json is revalidated on every use. Without a build the page
// answers 503.
func (s *Server) MountDocsPages(fsys fs.FS) {
	index, _ := fs.ReadFile(fsys, "index.html")
	etag := contentETag(index)
	// The contract is revalidated on every use; an ETag keeps that a 304, not 280 KB.
	contract, _ := fs.ReadFile(fsys, openAPIFile)
	contractETag := contentETag(contract)

	notFound := func(w http.ResponseWriter) {
		docsPageHeaders(w)
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "not found", http.StatusNotFound)
	}
	s.mux.HandleFunc("GET /docs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, "/docs/", http.StatusPermanentRedirect)
	})
	s.mux.HandleFunc("GET /docs/{path...}", func(w http.ResponseWriter, r *http.Request) {
		p := r.PathValue("path")
		if p == "" || p == "index.html" {
			docsPageHeaders(w)
			if index == nil {
				w.Header().Set("Cache-Control", "no-store")
				http.Error(w, "the developer portal is not built", http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("ETag", etag)
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(index))
			return
		}
		if strings.Contains(p, "..") || strings.Contains(p, "\\") || !fs.ValidPath(p) {
			notFound(w)
			return
		}
		ctype, known := assetTypes[path.Ext(p)]
		st, err := fs.Stat(fsys, p)
		if !known || err != nil || st.IsDir() {
			notFound(w)
			return
		}
		docsPageHeaders(w)
		w.Header().Set("Content-Type", ctype)
		switch {
		case strings.HasPrefix(p, "assets/"):
			// Vite names every file under assets/ by its content hash.
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		case p == versionFile:
			w.Header().Set("Cache-Control", "no-cache")
		case p == openAPIFile:
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("ETag", contractETag)
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(contract))
			return
		default:
			w.Header().Set("Cache-Control", "public, max-age=3600")
		}
		http.ServeFileFS(w, r, fsys, p)
	})
}
