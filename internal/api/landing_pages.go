package api

import (
	"bytes"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

// LandingCSP is the public landing page's Content-Security-Policy. The page is static: its
// stylesheet, script, fonts and images come from this origin and it never fetches anything.
const LandingCSP = "default-src 'none'; script-src 'self'; style-src 'self'; font-src 'self'; img-src 'self'; " +
	"base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// LandingAssetPrefix is where the landing page's files are served; index.html itself is only
// served at the root.
const LandingAssetPrefix = "/site/"

// LandingOrigin is the origin index.html names in its canonical and share-card URLs (og:url,
// og:image, twitter:image). Crawlers need absolute URLs there, so MountLandingPages swaps it for
// the deployment's public origin.
const LandingOrigin = "https://prismaport.com"

// landingPublicOrigin reduces the public base URL to scheme and host; ok is false when it is not
// an absolute http(s) URL.
func landingPublicOrigin(base string) (origin string, ok bool) {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
		return "", false
	}
	return u.Scheme + "://" + u.Host, true
}

// withPublicOrigin points every attribute value that starts with LandingOrigin at origin.
func withPublicOrigin(page []byte, origin string) []byte {
	return bytes.ReplaceAll(page, []byte(`="`+LandingOrigin+`/`), []byte(`="`+origin+`/`))
}

type landingFile struct {
	data  []byte
	ctype string
	etag  string
	cache string
}

func landingPageHeaders(w http.ResponseWriter) {
	pageHeaders(w)
	w.Header().Set("Content-Security-Policy", LandingCSP)
}

// landingCache keeps the stylesheet and script revalidated, because their names carry no hash
// and a stale copy would not match a new page. Fonts and images change rarely.
func landingCache(ext string) string {
	switch ext {
	case ".css", ".js":
		return "no-cache"
	case ".woff2":
		return "public, max-age=2592000"
	default:
		return "public, max-age=86400"
	}
}

// MountLandingPages serves the public landing page (web/landing/site) from fsys: index.html at
// the root and every other file under LandingAssetPrefix, each with an ETag. publicURL is the
// site's address (PRISMAPORT_SITE_URL, empty while routing by host is off): the page's absolute
// URLs follow its origin, and keep LandingOrigin when it is empty or not an http(s) URL. With hosts configured, the page's links to the dashboard and the docs point at
// their hosts. Without an index.html the root keeps redirecting to the dashboard.
func (s *Server) MountLandingPages(fsys fs.FS, publicURL string) {
	s.landingMounted = true
	origin, rewrite := landingPublicOrigin(publicURL)
	files := map[string]landingFile{}
	_ = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		ext := path.Ext(p)
		ctype, known := assetTypes[ext]
		if p == "index.html" {
			ctype, known = "text/html; charset=utf-8", true
		}
		if !known {
			return nil
		}
		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return nil
		}
		if p == "index.html" {
			if rewrite {
				data = withPublicOrigin(data, origin)
			}
			data = s.hosts.absoluteLinks(data)
		}
		files[p] = landingFile{data: data, ctype: ctype, etag: contentETag(data), cache: landingCache(ext)}
		return nil
	})

	serve := func(w http.ResponseWriter, r *http.Request, f landingFile) {
		landingPageHeaders(w)
		w.Header().Set("Content-Type", f.ctype)
		w.Header().Set("Cache-Control", f.cache)
		w.Header().Set("ETag", f.etag)
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(f.data))
	}

	if index, ok := files["index.html"]; ok {
		index.cache = "no-cache"
		s.mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { serve(w, r, index) })
	} else {
		s.mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			http.Redirect(w, r, "/dashboard/", http.StatusFound)
		})
	}
	s.mux.HandleFunc("GET "+LandingAssetPrefix+"{path...}", func(w http.ResponseWriter, r *http.Request) {
		p := r.PathValue("path")
		f, ok := files[p]
		if !ok || p == "index.html" {
			landingPageHeaders(w)
			w.Header().Set("Cache-Control", "no-store")
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		serve(w, r, f)
	})
}
