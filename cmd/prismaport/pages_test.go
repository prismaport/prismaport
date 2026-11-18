package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/prismaport/prismaport/internal/api"
	landingweb "github.com/prismaport/prismaport/web/landing"
)

func pagesTestSources() api.PagesSources {
	spa := fstest.MapFS{"index.html": {Data: []byte(`<meta name="prismaport-api" content="" /><meta name="prismaport-docs" content="" />`)},
		"assets/a-abc123.js": {Data: []byte("js")}}
	docs := fstest.MapFS{"index.html": {Data: []byte("docs")}, "openapi.json": {Data: []byte("{}")}}
	return api.PagesSources{Landing: landingweb.Site(), Dashboard: spa, Checkout: spa, Docs: docs}
}

var pagesFlags = []string{"--site-url", "https://prismaport.com", "--app-url", "https://app.prismaport.com",
	"--pay-url", "https://pay.prismaport.com", "--docs-url", "https://docs.prismaport.com", "--api-url", "https://api.prismaport.com"}

func TestPagesBuildCommand(t *testing.T) {
	out := t.TempDir()
	var buf bytes.Buffer
	if err := runPages(append([]string{"build", "--out", out}, pagesFlags...), &buf, pagesTestSources()); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"site", "app", "pay", "docs"} {
		if !strings.Contains(buf.String(), filepath.Join(out, d)) {
			t.Errorf("output does not name %s: %s", d, buf.String())
		}
		for _, f := range []string{"_headers", "_redirects", "404.html"} {
			if _, err := os.Stat(filepath.Join(out, d, f)); err != nil {
				t.Error(err)
			}
		}
	}
	b, err := os.ReadFile(filepath.Join(out, "app", "dashboard", "index.html"))
	if err != nil || !strings.Contains(string(b), `content="https://api.prismaport.com"`) || !strings.Contains(string(b), `content="https://docs.prismaport.com/"`) {
		t.Errorf("dashboard index: %s %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(out, "docs", "openapi.json")); err != nil {
		t.Error(err)
	}
}

func TestPagesBuildCommandNoIndex(t *testing.T) {
	for _, noIndex := range []bool{false, true} {
		out := t.TempDir()
		args := append([]string{"build", "--out", out}, pagesFlags...)
		if noIndex {
			args = append(args, "--noindex")
		}
		if err := runPages(args, &bytes.Buffer{}, pagesTestSources()); err != nil {
			t.Fatal(err)
		}
		for _, d := range []string{"site", "app", "pay", "docs"} {
			b, err := os.ReadFile(filepath.Join(out, d, "_headers"))
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(string(b), "  X-Robots-Tag: noindex, nofollow\n"); got != noIndex {
				t.Errorf("--noindex %v: %s/_headers has X-Robots-Tag %v:\n%s", noIndex, d, got, b)
			}
			if n := strings.Count("\n"+string(b), "\n/*\n"); n != 1 {
				t.Errorf("%s/_headers has %d /* rules", d, n)
			}
		}
	}
}

func TestPagesBuildCommandRefuses(t *testing.T) {
	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"no action":      {nil, "usage: prismaport pages build"},
		"other action":   {[]string{"deploy"}, "usage: prismaport pages build"},
		"no out":         {append([]string{"build"}, pagesFlags...), "--out is required"},
		"stray argument": {append([]string{"build", "--out", t.TempDir()}, append(pagesFlags, "extra")...), `unexpected argument "extra"`},
		"no API":         {[]string{"build", "--out", t.TempDir(), "--site-url", "https://prismaport.com"}, "URL is required"},
		"no docs":        {append([]string{"build", "--out", t.TempDir()}, pagesFlags[:6]...), "docs URL is required"},
	} {
		t.Run(name, func(t *testing.T) {
			err := runPages(c.args, &bytes.Buffer{}, pagesTestSources())
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("%s: err = %v, want %q", name, err, c.want)
			}
		})
	}
}
