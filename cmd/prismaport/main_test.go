package main

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prismaport/prismaport/internal/api"
	"github.com/prismaport/prismaport/internal/config"
	"github.com/prismaport/prismaport/internal/version"
)

// The landing page names the site in its canonical and share-card URLs, never the origin the
// api answers on while routing by host is off.
func TestLandingCanonicalFollowsTheSiteURLOnly(t *testing.T) {
	for _, c := range []struct {
		name string
		env  config.Env
		want string
	}{
		{"no site URL", config.Env{CheckoutBaseURL: "https://api-example-dev.up.railway.app"}, "https://prismaport.com/"},
		{"local", config.Env{CheckoutBaseURL: "http://localhost:58180"}, "https://prismaport.com/"},
		{"site URL", config.Env{CheckoutBaseURL: "https://pay.example.com", SiteURL: "https://example.com"}, "https://example.com/"},
	} {
		t.Run(c.name, func(t *testing.T) {
			handler := api.New(nil, nil, nil)
			if err := mountPages(handler, c.env); err != nil {
				t.Fatal(err)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
			page := rec.Body.String()
			if !strings.Contains(page, `<link rel="canonical" href="`+c.want+`">`) || !strings.Contains(page, `content="`+c.want+`site/og.png"`) {
				t.Errorf("%s: canonical or og:image is not %s", c.name, c.want)
			}
			if strings.Contains(page, c.env.CheckoutBaseURL) {
				t.Errorf("%s: the page names %s", c.name, c.env.CheckoutBaseURL)
			}
		})
	}
}

func TestVersionCommandPrintsTheReleaseAndCommit(t *testing.T) {
	t.Setenv("RAILWAY_GIT_COMMIT_SHA", "0123456789abcdef")
	var out bytes.Buffer
	if err := run(t.Context(), []string{"version"}, &out); err != nil {
		t.Fatal(err)
	}
	if want := "prismaport " + version.Version + " (commit 0123456"; !strings.HasPrefix(out.String(), want) || !strings.HasSuffix(out.String(), ")\n") {
		t.Fatalf("version output %q, want prefix %q", out.String(), want)
	}
}
