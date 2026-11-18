//go:build integration

package api_test

import (
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/prismaport/prismaport/internal/api"
)

// Route-coverage contract (ADR 0008 E-7): a full `go test -tags integration ./internal/api` run
// fails when a registered route got no request from any test.
//
// How it works. The inventory is every pattern on the router of a server mounted like
// production (api.RegisteredRoutes, read off the http.ServeMux). TestMain wraps
// http.DefaultTransport, which every test request goes through (apptest.App.Do, http.Get/Post
// and clients without their own Transport), and records each request answered by an api.Server
// (its response carries the HSTS header that Server.ServeHTTP always sets, so webhook consumers
// and other test servers are ignored). After the run each recorded request is routed through a
// mux holding the inventory's patterns, and the pattern it lands on counts as hit. A request a
// test sends to a server that lacks the route (fewer mounts) still credits the pattern the full
// router would use; direct ServeHTTP calls on a recorder are not seen at all.
//
// The check runs only on a whole-package pass (no -run, -skip or -list, nothing failed), so
// focused runs stay quiet. When it fails: add a test that sends a request to the route (an error
// path counts), or, when a route is deliberately left to another suite, add it to
// untestedRoutes with the reason. An allow-listed route that is hit, or no longer registered,
// fails the check too, so the list cannot go stale.

// untestedRoutes are routes no integration test is expected to hit, with the reason.
var untestedRoutes = map[string]string{}

type routeRecorder struct {
	base http.RoundTripper
	mu   sync.Mutex
	hits []*http.Request
}

func (r *routeRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := r.base.RoundTrip(req)
	if err == nil && resp.Header.Get("Strict-Transport-Security") == api.HSTS {
		hit := httptest.NewRequest(req.Method, req.URL.String(), nil)
		if req.Host != "" {
			hit.Host = req.Host
		}
		r.mu.Lock()
		r.hits = append(r.hits, hit)
		r.mu.Unlock()
	}
	return resp, err
}

func TestMain(m *testing.M) {
	rec := &routeRecorder{base: http.DefaultTransport}
	http.DefaultTransport = rec
	code := m.Run()
	if code == 0 && wholeRun() {
		if err := checkRouteCoverage(rec); err != nil {
			fmt.Fprintln(os.Stderr, err)
			code = 1
		}
	}
	os.Exit(code)
}

// wholeRun reports whether every test of the package ran (no -run, -skip or -list filter).
func wholeRun() bool {
	for _, name := range []string{"test.run", "test.skip", "test.list"} {
		if f := flag.Lookup(name); f != nil && f.Value.String() != "" {
			return false
		}
	}
	return true
}

func checkRouteCoverage(rec *routeRecorder) error {
	routes, err := api.RegisteredRoutes()
	if err != nil {
		return fmt.Errorf("route coverage: route inventory: %w", err)
	}
	shadow := http.NewServeMux()
	for _, p := range routes {
		shadow.HandleFunc(p, func(http.ResponseWriter, *http.Request) {})
	}
	hit := map[string]int{}
	rec.mu.Lock()
	for _, r := range rec.hits {
		if _, p := shadow.Handler(r); p != "" {
			hit[p]++
		}
	}
	n := len(rec.hits)
	rec.mu.Unlock()

	var problems []string
	for _, p := range routes {
		reason, allowed := untestedRoutes[p]
		switch {
		case hit[p] == 0 && !allowed:
			problems = append(problems, fmt.Sprintf("  %-60s no test sends it a request", p))
		case hit[p] > 0 && allowed:
			problems = append(problems, fmt.Sprintf("  %-60s is tested now (%d requests): drop it from untestedRoutes (%s)", p, hit[p], reason))
		}
	}
	for p := range untestedRoutes {
		if !slices.Contains(routes, p) {
			problems = append(problems, fmt.Sprintf("  %-60s is in untestedRoutes but no longer registered", p))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	slices.Sort(problems)
	return fmt.Errorf("route coverage (ADR 0008 E-7, route_coverage_integration_test.go): %d of %d routes need attention "+
		"(%d requests recorded):\n%s\nAdd a test that requests the route, or list it in untestedRoutes with the reason.",
		len(problems), len(routes), n, strings.Join(problems, "\n"))
}
