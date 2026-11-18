//go:build integration

package api_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prismaport/prismaport/internal/api"
	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/checkout"
	"github.com/prismaport/prismaport/internal/store"
)

// instance is one API process: its own Server and limiter, and (when pool is set) its own
// connection pool to the shared database.
func instance(t *testing.T, app *apptest.App, pool *pgxpool.Pool, limits map[string]api.Limit) *httptest.Server {
	t.Helper()
	h := api.New(app.Svc, app.Store, nil)
	o := api.PublicOptions{Limits: limits, ClientIPHeader: "X-Real-IP"}
	if pool != nil {
		o.Store = store.New(pool, nil)
	}
	if err := h.ConfigurePublic(o); err != nil {
		t.Fatal(err)
	}
	co := checkout.New(app.Svc, app.Store, nil)
	h.MountCheckout(co)
	h.MountSolanaCheckout(co)
	h.MountCheckoutLinks(co)
	h.MountCheckoutAddresses(co)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func post(t *testing.T, url, ip, key string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("POST", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Real-IP", ip)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

// Two API instances with separate connection pools share one bucket per client IP: under
// concurrency exactly the burst gets through, however the requests are spread.
func TestRateLimitSharedAcrossInstances(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	pool2, err := pgxpool.New(t.Context(), app.Store.Pool().Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool2.Close)
	limits := map[string]api.Limit{api.RouteLinkOpen: {PerMinute: 1, Burst: 5}}
	servers := []*httptest.Server{instance(t, app, nil, limits), instance(t, app, pool2, limits)}
	_, link := productLink(t, app, app.TestKey, map[string]any{"name": "Founder", "price_type": "fixed", "amount": "25.00"})

	const n = 40
	statuses := make([]int, n)
	retry := make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := post(t, servers[i%2].URL+"/v1/checkout/links/"+link+"/sessions", "203.0.113.7", fmt.Sprintf("k%d", i))
			statuses[i], retry[i] = r.StatusCode, r.Header.Get("Retry-After")
		}()
	}
	wg.Wait()
	ok, limited := map[int]int{}, 0
	for i, s := range statuses {
		switch s {
		case http.StatusOK:
			ok[i%2]++
		case http.StatusTooManyRequests:
			limited++
			if retry[i] == "" || retry[i] == "0" {
				t.Fatalf("429 without Retry-After: %q", retry[i])
			}
		default:
			t.Fatalf("request %d: status %d", i, s)
		}
	}
	if ok[0]+ok[1] != 5 || limited != n-5 {
		t.Fatalf("admitted %d (per instance %v), limited %d; want exactly the burst of 5", ok[0]+ok[1], ok, limited)
	}
	var rows int
	if err := app.Store.Pool().QueryRow(t.Context(), `SELECT count(*) FROM rate_limits WHERE key LIKE 'link_open|%'`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("buckets %d %v", rows, err)
	}
	// Another client and another route have their own buckets.
	if r := post(t, servers[1].URL+"/v1/checkout/links/"+link+"/sessions", "198.51.100.4", "other"); r.StatusCode != http.StatusOK {
		t.Fatalf("other client: %d", r.StatusCode)
	}
}

// Every public endpoint that builds, simulates or relays something is limited per route.
func TestRateLimitedCheckoutRoutes(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	limits := map[string]api.Limit{}
	for route := range api.DefaultLimits() {
		limits[route] = api.Limit{PerMinute: 1, Burst: 2}
	}
	srv := instance(t, app, nil, limits)
	paths := map[string]string{
		api.RouteCheckoutReveal:    "/v1/checkout/sessions/ses_x/reveal",
		api.RouteEIP3009TypedData:  "/v1/checkout/sessions/ses_x/eip3009/typed_data",
		api.RouteEIP3009Submit:     "/v1/checkout/sessions/ses_x/eip3009/submit",
		api.RouteSolanaTransaction: "/v1/checkout/sessions/ses_x/solana/transaction",
		api.RouteSolanaSubmit:      "/v1/checkout/sessions/ses_x/solana/submit",
		api.RouteLinkOpen:          "/v1/checkout/links/plink_x/sessions",
		api.RouteAddressOpen:       "/v1/checkout/addresses/caddr_x/sessions",
	}
	for route, path := range paths {
		t.Run(route, func(t *testing.T) {
			for i := range 2 {
				if r := post(t, srv.URL+path, "203.0.113.9", "k"); r.StatusCode == http.StatusTooManyRequests {
					t.Fatalf("%s request %d limited inside the burst", route, i)
				}
			}
			if r := post(t, srv.URL+path, "203.0.113.9", "k"); r.StatusCode != http.StatusTooManyRequests {
				t.Fatalf("%s: third request %d, want 429", route, r.StatusCode)
			}
			if r := post(t, srv.URL+path, "203.0.113.10", "k"); r.StatusCode == http.StatusTooManyRequests {
				t.Fatalf("%s: another client limited", route)
			}
		})
	}
	// Polling the session view is never limited.
	for range 5 {
		if r, err := http.Get(srv.URL + "/v1/checkout/sessions/ses_x"); err != nil || r.StatusCode == http.StatusTooManyRequests {
			t.Fatalf("session view limited: %v", err)
		}
	}
}

func TestRateLimitPurgeKeepsActiveBuckets(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	ctx := t.Context()
	for _, key := range []string{"old", "new"} {
		if _, _, err := app.Store.TakeRateToken(ctx, key, 10, 5, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := app.Store.Pool().Exec(ctx, `UPDATE rate_limits SET updated_at = now() - interval '2 hours' WHERE key = 'old'`); err != nil {
		t.Fatal(err)
	}
	if n, err := app.Store.PurgeRateLimits(ctx, api.PurgeIdle, nil); err != nil || n != 1 {
		t.Fatalf("purged %d %v", n, err)
	}
	var left string
	if err := app.Store.Pool().QueryRow(ctx, `SELECT key FROM rate_limits`).Scan(&left); err != nil || left != "new" {
		t.Fatalf("left %q %v", left, err)
	}
}
