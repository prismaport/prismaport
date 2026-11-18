package api

import (
	"bytes"
	"errors"
	"fmt"
	"html"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Directories BuildPages writes, one per Cloudflare Pages project.
const (
	PagesSiteDir = "site"
	PagesAppDir  = "app"
	PagesPayDir  = "pay"
	PagesDocsDir = "docs"
)

// PagesOrigins are the origins the static pages are built for ("https://app.prismaport.com").
// NoIndex asks search engines to keep every page out of their index (a non-production
// environment).
type PagesOrigins struct {
	Site, App, Pay, Docs, API string
	NoIndex                   bool
}

// PagesSources are the built pages, as the binary embeds them.
type PagesSources struct {
	Landing, Dashboard, Checkout, Docs fs.FS
}

const (
	cacheImmutable = "public, max-age=31536000, immutable"
	cacheFonts     = "public, max-age=2592000"
)

// The meta tags the Pages build writes into the SPAs' index.html; empty in the binary's builds.
const (
	metaAPI  = "prismaport-api"
	metaDocs = "prismaport-docs"
)

func metaTag(name string) *regexp.Regexp {
	return regexp.MustCompile(`<meta name="` + name + `" content="([^"]*)"`)
}

// BuildPages writes the four Cloudflare Pages directories of split mode under out
// (split mode): the landing (PagesSiteDir), the dashboard (PagesAppDir), the
// checkout (PagesPayDir) and the developer portal at the root of its host (PagesDocsDir), each
// with the _headers and _redirects Pages applies. It replaces those four directories and nothing
// else under out.
func BuildPages(out string, o PagesOrigins, src PagesSources) error {
	f, err := pagesFrontend(o)
	if err != nil {
		return err
	}
	type tree map[string][]byte
	site, app, pay, docs := tree{}, tree{}, tree{}, tree{}

	landing, err := readTree(src.Landing, "the landing page (web/landing/site)")
	if err != nil {
		return err
	}
	for p, b := range landing {
		if p == "index.html" {
			site[p] = landingForPages(b, f)
			continue
		}
		site[LandingAssetPrefix[1:]+p] = b
	}
	portal, err := readTree(src.Docs, "the developer portal (task web:docs:build)")
	if err != nil {
		return err
	}
	if _, ok := portal[openAPIFile]; !ok {
		return errors.New("the developer portal build has no " + openAPIFile)
	}
	// Its build refers to its files relatively, so it works at this root as at the binary's /docs/.
	for p, b := range portal {
		docs[p] = b
	}
	dashboard, err := readTree(src.Dashboard, "the dashboard (task web:dashboard:build)")
	if err != nil {
		return err
	}
	if dashboard["index.html"], err = stampMeta(dashboard["index.html"], metaAPI, f.api); err != nil {
		return fmt.Errorf("the dashboard: %w", err)
	}
	if dashboard["index.html"], err = stampMeta(dashboard["index.html"], metaDocs, f.docs+"/"); err != nil {
		return fmt.Errorf("the dashboard: %w", err)
	}
	for p, b := range dashboard {
		app["dashboard/"+p] = b
	}
	checkoutFiles, err := readTree(src.Checkout, "the checkout (task web:build)")
	if err != nil {
		return err
	}
	if checkoutFiles["index.html"], err = stampMeta(checkoutFiles["index.html"], metaAPI, f.api); err != nil {
		return fmt.Errorf("the checkout: %w", err)
	}
	for p, b := range checkoutFiles {
		pay["pay/"+p] = b
	}

	notFound := NotFoundPage(f.site)
	for _, t := range []tree{site, app, pay, docs} {
		t["404.html"] = notFound
	}
	site["_headers"] = siteHeaders(o.NoIndex)
	docs["_headers"] = docsHeaders(o.NoIndex)
	if app["_headers"], err = DashboardPagesHeaders(f.api, o.NoIndex); err != nil {
		return err
	}
	if pay["_headers"], err = spaHeaders(CheckoutCSP, f.api, "/pay/assets/*", o.NoIndex); err != nil {
		return err
	}
	site["_redirects"] = siteRedirects(f)
	app["_redirects"] = appRedirects(f, topLevel(dashboard))
	pay["_redirects"] = payRedirects(f)
	docs["_redirects"] = docsRedirects(f)

	for name, t := range map[string]tree{PagesSiteDir: site, PagesAppDir: app, PagesPayDir: pay, PagesDocsDir: docs} {
		if err := writeTree(filepath.Join(out, name), t); err != nil {
			return err
		}
	}
	return nil
}

// pagesFrontend checks the origins like split mode checks the api's configuration.
func pagesFrontend(o PagesOrigins) (*splitFrontend, error) {
	for _, c := range []struct{ name, raw string }{{"site", o.Site}, {"app", o.App}, {"pay", o.Pay}, {"docs", o.Docs}, {"API", o.API}} {
		if c.raw == "" {
			return nil, fmt.Errorf("the %s URL is required", c.name)
		}
	}
	return newSplitFrontendNamed(Hosts{Site: o.Site, Dashboard: strings.TrimRight(o.App, "/") + "/dashboard/", Checkout: o.Pay, Docs: o.Docs, API: o.API},
		map[string]string{"site": "the site URL", "dashboard": "the app URL", "checkout": "the pay URL", "docs": "the docs URL", "API": "the API URL"})
}

// readTree reads every file of fsys but dotfiles (the dist directories' .gitkeep); what names the
// build for the error when it has no index.html.
func readTree(fsys fs.FS, what string) (map[string][]byte, error) {
	if fsys == nil {
		return nil, fmt.Errorf("%s is not built", what)
	}
	files := map[string][]byte{}
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasPrefix(d.Name(), ".") && p != "." {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		files[p] = b
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	if _, ok := files["index.html"]; !ok {
		return nil, fmt.Errorf("%s is not built: no index.html", what)
	}
	return files, nil
}

// embeddedIndex refuses an index.html whose prismaport-* meta tags are not empty: that is a
// Pages build, whose pages call another origin, copied where the binary embeds its own.
func embeddedIndex(index []byte, what string) error {
	for _, name := range []string{metaAPI, metaDocs} {
		for _, m := range metaTag(name).FindAllSubmatch(index, -1) {
			if len(m[1]) > 0 {
				return fmt.Errorf("the embedded %s is a Pages build: its index.html names %s %q; rebuild web/%s (the binary serves its pages on its own origin)",
					what, name, m[1], what)
			}
		}
	}
	return nil
}

// stampMeta writes value into the page's one meta tag called name.
func stampMeta(page []byte, name, value string) ([]byte, error) {
	re := metaTag(name)
	if n := len(re.FindAll(page, -1)); n != 1 {
		return nil, fmt.Errorf("index.html has %d %s meta tags, want 1", n, name)
	}
	return re.ReplaceAll(page, []byte(`<meta name="`+name+`" content="`+html.EscapeString(value)+`"`)), nil
}

// landingForPages names the site in the page's canonical and share-card URLs and points its links
// to pages the site does not serve at their hosts (/docs/ at the docs host's root); its own files
// stay relative.
func landingForPages(page []byte, f *splitFrontend) []byte {
	page = withPublicOrigin(page, f.site)
	return absoluteLinks(page, func(clean, link string) string {
		under := pathUnder(clean)
		switch o := f.pageOrigin(clean); {
		case under("v1"):
			return f.api + link
		case under("docs"):
			rest := ""
			if i := strings.IndexAny(link, "?#"); i >= 0 {
				rest = link[i:]
			}
			return f.docs + portalPath(clean) + rest
		case o != "" && o != f.site:
			return o + link
		}
		return ""
	})
}

// NotFoundPage is the 404.html of a Pages project: a short page that links to site.
func NotFoundPage(site string) []byte {
	return []byte(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex">
<title>Page not found · Prismaport</title>
</head>
<body>
<h1>Page not found</h1>
<p>There is no page at this address. <a href="` + html.EscapeString(site) + `/">Go to Prismaport</a></p>
</body>
</html>
`)
}

// reachAPI lets a page's CSP reach the API's origin besides its own: its fetches, and the
// merchant logos the API serves (/v1/checkout/logos/*).
func reachAPI(csp, origin string) (string, error) {
	for _, self := range []string{"connect-src 'self'", "img-src 'self'"} {
		if strings.Count(csp, self) != 1 {
			return "", fmt.Errorf("the CSP has no single %q to extend: %s", self, csp)
		}
		csp = strings.Replace(csp, self, self+" "+origin, 1)
	}
	return csp, nil
}

type headerRule struct {
	pattern string
	lines   []string
}

func renderHeaders(rules []headerRule) []byte {
	var b bytes.Buffer
	for _, r := range rules {
		b.WriteString(r.pattern + "\n")
		for _, l := range r.lines {
			b.WriteString("  " + l + "\n")
		}
	}
	return b.Bytes()
}

// noIndexHeader keeps a non-production environment's pages out of search engines.
const noIndexHeader = "X-Robots-Tag: noindex, nofollow"

// commonHeaders are every path's: Pages' default Access-Control-Allow-Origin removed, the page
// headers of the binary, the project's main CSP and, with noIndex, noIndexHeader. That header
// joins this one /* rule: a _headers file with a second /* rule was dropped whole by Pages (no
// CSP, its default Access-Control-Allow-Origin: * back).
func commonHeaders(csp string, noIndex bool) headerRule {
	lines := []string{"! Access-Control-Allow-Origin"}
	for _, kv := range pageHeaderValues {
		lines = append(lines, kv[0]+": "+kv[1])
	}
	lines = append(lines, "Content-Security-Policy: "+csp)
	if noIndex {
		lines = append(lines, noIndexHeader)
	}
	return headerRule{"/*", lines}
}

func siteHeaders(noIndex bool) []byte {
	return renderHeaders([]headerRule{
		commonHeaders(LandingCSP, noIndex),
		{LandingAssetPrefix + "fonts/*", []string{"Cache-Control: " + cacheFonts}},
	})
}

func docsHeaders(noIndex bool) []byte {
	return renderHeaders([]headerRule{
		commonHeaders(DocsCSP, noIndex),
		{"/assets/*", []string{"Cache-Control: " + cacheImmutable}},
	})
}

// DashboardPagesHeaders is the _headers file of the dashboard's Pages project: the page headers,
// the dashboard's CSP extended to apiOrigin, and immutable caching of its assets.
func DashboardPagesHeaders(apiOrigin string, noIndex bool) ([]byte, error) {
	return spaHeaders(DashboardCSP, apiOrigin, "/dashboard/assets/*", noIndex)
}

func spaHeaders(csp, apiOrigin, assets string, noIndex bool) ([]byte, error) {
	csp, err := reachAPI(csp, apiOrigin)
	if err != nil {
		return nil, err
	}
	return renderHeaders([]headerRule{
		commonHeaders(csp, noIndex),
		{assets, []string{"Cache-Control: " + cacheImmutable}},
	}), nil
}

// redirectRule is one _redirects line; static rules (no splat or placeholder) are written first,
// as Pages asks, each group in its order.
type redirectRule struct{ from, to, status string }

func renderRedirects(rules []redirectRule) []byte {
	var static, dynamic bytes.Buffer
	for _, r := range rules {
		dst := &static
		if strings.ContainsAny(r.from, "*:") {
			dst = &dynamic
		}
		fmt.Fprintf(dst, "%s %s %s\n", r.from, r.to, r.status)
	}
	return append(static.Bytes(), dynamic.Bytes()...)
}

// crossHost is a redirect to another origin. Pages accepts absolute targets only over https, so
// with plain http origins (loopback, for local runs) there is none and the path is a 404 there.
func crossHost(rules []redirectRule, r redirectRule) []redirectRule {
	if !strings.HasPrefix(r.to, "https://") {
		return rules
	}
	return append(rules, r)
}

// elsewhere sends the page paths of the surfaces a project does not serve to their hosts.
func elsewhere(f *splitFrontend, skip string) []redirectRule {
	var all []redirectRule
	if skip != f.docs {
		all = append(all, redirectRule{"/docs", f.docs + "/", "302"}, redirectRule{"/docs/*", f.docs + "/:splat", "302"})
	}
	if skip != f.dashboard {
		all = append(all, redirectRule{"/dashboard", f.dashboard + "/dashboard/", "302"}, redirectRule{"/dashboard/*", f.dashboard + "/dashboard/:splat", "302"})
	}
	if skip != f.checkout {
		for _, p := range []string{"pay", "l", "a", "s"} {
			all = append(all, redirectRule{"/" + p + "/*", f.checkout + "/" + p + "/:splat", "302"})
		}
	}
	var rules []redirectRule
	for _, r := range all {
		rules = crossHost(rules, r)
	}
	return rules
}

func siteRedirects(f *splitFrontend) []byte {
	return renderRedirects(elsewhere(f, f.site))
}

// docsRedirects: the portal's paths under /docs/ to the same path at the root, the other surfaces'
// paths to their hosts. On https the target names the docs origin: a relative "/:splat" turns
// /docs//evil.example into the protocol-relative //evil.example. The local http stack keeps the
// relative form because Pages redirects across hosts only to https.
func docsRedirects(f *splitFrontend) []byte {
	root := ""
	if strings.HasPrefix(f.docs, "https://") {
		root = f.docs
	}
	rules := []redirectRule{{"/docs", root + "/", "302"}, {"/docs/*", root + "/:splat", "302"}}
	return renderRedirects(append(rules, elsewhere(f, f.docs)...))
}

// topLevel lists a build's top-level entries but index.html: a directory as "name/", a file as
// "name".
func topLevel(files map[string][]byte) []string {
	seen := map[string]bool{}
	var out []string
	for p := range files {
		first, _, nested := strings.Cut(p, "/")
		entry := first
		if nested {
			entry += "/"
		}
		if entry == "index.html" || seen[entry] {
			continue
		}
		seen[entry] = true
		out = append(out, entry)
	}
	slices.Sort(out)
	return out
}

// appRedirects: every top-level entry of the dashboard build passes through before the
// single-page fallback, because Pages applies a rule even when a file matches. The fallback
// rewrites to the directory: a rewrite to index.html would be redirected back to it.
func appRedirects(f *splitFrontend, entries []string) []byte {
	rules := []redirectRule{{"/", "/dashboard/", "302"}}
	rules = append(rules, elsewhere(f, f.dashboard)...)
	for _, e := range entries {
		p := "/dashboard/" + e
		if strings.HasSuffix(e, "/") {
			rules = append(rules, redirectRule{p + "*", p + ":splat", "200"})
		} else {
			rules = append(rules, redirectRule{p, p, "200"})
		}
	}
	rules = append(rules, redirectRule{"/dashboard/*", "/dashboard/", "200"})
	return renderRedirects(rules)
}

// payRedirects: the checkout shell for session, link and customer address ids (prefixes: Pages has no regular
// expressions; the page answers a malformed id itself), the checkout_url redirect, the site at /.
func payRedirects(f *splitFrontend) []byte {
	rules := crossHost(nil, redirectRule{"/", f.site + "/", "302"})
	rules = append(rules, redirectRule{"/s/ses_*", "/pay/ses_:splat", "308"})
	rules = append(rules, elsewhere(f, f.checkout)...)
	rules = append(rules,
		redirectRule{"/pay/ses_*", "/pay/", "200"},
		redirectRule{"/l/plink_*", "/pay/", "200"},
		redirectRule{"/l/prod_*", "/pay/", "200"},
		redirectRule{"/a/caddr_*", "/pay/", "200"},
	)
	return renderRedirects(rules)
}

func writeTree(dir string, files map[string][]byte) error {
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	for p, b := range files {
		dst := filepath.Join(dir, filepath.FromSlash(path.Clean(p)))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, b, 0o644); err != nil {
			return err
		}
	}
	return nil
}
