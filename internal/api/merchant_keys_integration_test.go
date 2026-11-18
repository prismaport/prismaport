//go:build integration

package api_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prismaport/prismaport/internal/api"
	"github.com/prismaport/prismaport/internal/apikey"
	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/store"
)

// merchantRequests is one request per merchant API route, keyed by its pattern, with the scope
// the route must demand. Path ids need not exist: the scope is checked before any lookup. Every
// id a request names uses one of the placeholders in foreignIDs, so the cross-merchant probe can
// point it at another merchant's real object.
var merchantRequests = map[string]struct {
	scope  apikey.Scope
	method string
	path   string
	body   any
}{
	"POST /v1/products": {apikey.ProductsWrite, "POST", "/v1/products", map[string]any{"name": "Probe", "price_type": "fixed", "amount": "25.00",
		"payout": map[string]any{"base-sepolia": []map[string]any{{"wallet_id": "wal_01JBNOPE", "share": "100"}}}}},
	"GET /v1/products":                              {apikey.ProductsRead, "GET", "/v1/products", nil},
	"GET /v1/products/{id}":                         {apikey.ProductsRead, "GET", "/v1/products/prod_01JBNOPE", nil},
	"PATCH /v1/products/{id}":                       {apikey.ProductsWrite, "PATCH", "/v1/products/prod_01JBNOPE", map[string]any{}},
	"POST /v1/sessions":                             {apikey.SessionsWrite, "POST", "/v1/sessions", map[string]any{"product_id": "prod_01JBNOPE"}},
	"GET /v1/sessions/{id}":                         {apikey.SessionsRead, "GET", "/v1/sessions/ses_01JBNOPE", nil},
	"POST /v1/sessions/{id}/reveal":                 {apikey.SessionsWrite, "POST", "/v1/sessions/ses_01JBNOPE/reveal", map[string]any{"chain": "base-sepolia"}},
	"POST /v1/sessions/{id}/recheck":                {apikey.SessionsWrite, "POST", "/v1/sessions/ses_01JBNOPE/recheck", nil},
	"GET /v1/events":                                {apikey.EventsRead, "GET", "/v1/events?after=latest", nil},
	"POST /v1/webhook_endpoints":                    {apikey.WebhooksWrite, "POST", "/v1/webhook_endpoints", map[string]any{}},
	"GET /v1/webhook_endpoints":                     {apikey.WebhooksRead, "GET", "/v1/webhook_endpoints", nil},
	"PATCH /v1/webhook_endpoints/{id}":              {apikey.WebhooksWrite, "PATCH", "/v1/webhook_endpoints/we_01JBNOPE", map[string]any{"description": "probe"}},
	"DELETE /v1/webhook_endpoints/{id}":             {apikey.WebhooksWrite, "DELETE", "/v1/webhook_endpoints/we_01JBNOPE", nil},
	"POST /v1/webhook_endpoints/{id}/rotate_secret": {apikey.WebhooksWrite, "POST", "/v1/webhook_endpoints/we_01JBNOPE/rotate_secret", nil},
	"POST /v1/webhook_endpoints/{id}/test":          {apikey.WebhooksWrite, "POST", "/v1/webhook_endpoints/we_01JBNOPE/test", map[string]any{"type": "session.paid"}},
	"POST /v1/test_helpers/sessions/{id}/simulate":  {apikey.SessionsWrite, "POST", "/v1/test_helpers/sessions/ses_01JBNOPE/simulate", map[string]any{"action": "pay_exact"}},
	"POST /v1/payment_links":                        {apikey.PaymentLinksWrite, "POST", "/v1/payment_links", map[string]any{"product_id": "prod_01JBNOPE"}},
	"GET /v1/payment_links":                         {apikey.PaymentLinksRead, "GET", "/v1/payment_links", nil},
	"GET /v1/payment_links/{id}":                    {apikey.PaymentLinksRead, "GET", "/v1/payment_links/plink_01JBNOPE", nil},
	"PATCH /v1/payment_links/{id}":                  {apikey.PaymentLinksWrite, "PATCH", "/v1/payment_links/plink_01JBNOPE", map[string]any{"active": false}},
	"POST /v1/customer_addresses": {apikey.CustomerAddressesWrite, "POST", "/v1/customer_addresses",
		map[string]any{"customer_id": "probe", "product_id": "prod_01JBNOPE"}},
	"GET /v1/customer_addresses":               {apikey.CustomerAddressesRead, "GET", "/v1/customer_addresses", nil},
	"GET /v1/customer_addresses/{id}":          {apikey.CustomerAddressesRead, "GET", "/v1/customer_addresses/caddr_01JBNOPE", nil},
	"POST /v1/customer_addresses/{id}/rotate":  {apikey.CustomerAddressesWrite, "POST", "/v1/customer_addresses/caddr_01JBNOPE/rotate", nil},
	"GET /v1/customer_addresses/{id}/sessions": {apikey.CustomerAddressesRead, "GET", "/v1/customer_addresses/caddr_01JBNOPE/sessions", nil},
	"GET /v1/wallets":                          {apikey.WalletsRead, "GET", "/v1/wallets", nil},
	"GET /v1/wallets/{id}":                     {apikey.WalletsRead, "GET", "/v1/wallets/wal_01JBNOPE", nil},
}

func scopesExcept(sc apikey.Scope) []string {
	var out []string
	for _, s := range apikey.AllScopes {
		if s != sc {
			out = append(out, string(s))
		}
	}
	return out
}

// requireMerchantTable fails unless merchantRequests lists exactly the registered merchant
// routes with the scope each demands, so a new route fails every test built on the table until
// it is added.
func requireMerchantTable(t *testing.T, app *apptest.App) {
	t.Helper()
	h := api.New(app.Svc, app.Store, nil)
	h.MountSandbox(app.Dispatcher)
	var registered []string
	for _, r := range h.MerchantRoutes() {
		registered = append(registered, r.Pattern)
		want, ok := merchantRequests[r.Pattern]
		if !ok {
			t.Errorf("route %s is not in the scope table", r.Pattern)
			continue
		}
		if r.Scope != want.scope {
			t.Errorf("route %s demands %s, want %s", r.Pattern, r.Scope, want.scope)
		}
	}
	for pattern := range merchantRequests {
		if !slices.Contains(registered, pattern) {
			t.Errorf("scope table lists %s, which is not registered", pattern)
		}
	}
}

// Every merchant route demands its scope: a restricted key with every other scope is refused,
// a key with only that scope gets through. The table must list every registered route, so a
// new route fails here until its scope is decided.
func TestEveryMerchantRouteEnforcesItsScope(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	requireMerchantTable(t, app)
	keys := map[string]string{}
	key := func(scopes []string) string {
		t.Helper()
		name := strings.Join(scopes, ",")
		if k, ok := keys[name]; ok {
			return k
		}
		_, plaintext, err := app.Svc.CreateAPIKey(t.Context(), app.MerchantID, false, scopes, "test")
		if err != nil {
			t.Fatal(err)
		}
		keys[name] = plaintext
		return plaintext
	}
	for pattern, rq := range merchantRequests {
		r := app.Do(rq.method, rq.path, key(scopesExcept(rq.scope)), rq.body, "Idempotency-Key", "scope-"+pattern)
		if r.Status != http.StatusForbidden || r.ErrorCode(t) != "insufficient_scope" || !strings.Contains(string(r.Body), string(rq.scope)) {
			t.Errorf("%s with every other scope: %d %s", pattern, r.Status, r.Body)
		}
		r = app.Do(rq.method, rq.path, key([]string{string(rq.scope)}), rq.body, "Idempotency-Key", "scope-only-"+pattern)
		if r.Status == http.StatusForbidden || r.Status == http.StatusUnauthorized || r.Status == http.StatusTooManyRequests {
			t.Errorf("%s with only %s: %d %s", pattern, rq.scope, r.Status, r.Body)
		}
	}
}

func limitedInstance(t *testing.T, app *apptest.App, pool *pgxpool.Pool, limits map[string]api.Limit) *httptest.Server {
	t.Helper()
	h := api.New(app.Svc, app.Store, nil)
	o := api.PublicOptions{Limits: limits, Now: app.Clock.Now}
	if pool != nil {
		o.Store = store.New(pool, nil)
	}
	if err := h.ConfigurePublic(o); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func call(t *testing.T, srv *httptest.Server, method, path, key string) *http.Response {
	t.Helper()
	var body *strings.Reader
	if method != "GET" {
		body = strings.NewReader("{}")
	} else {
		body = strings.NewReader("")
	}
	req, err := http.NewRequest(method, srv.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func limitedResponse(t *testing.T, srv *httptest.Server, key string) apptest.Response {
	t.Helper()
	req, err := http.NewRequest("GET", srv.URL+"/v1/products", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return apptest.Response{Status: resp.StatusCode, Header: resp.Header, Body: raw}
}

func TestMerchantAPIRateLimitPerKey(t *testing.T) {
	t.Parallel()
	if d := api.DefaultLimits()[api.RouteAPIRead]; d.PerMinute != 600 || d.Burst != 200 {
		t.Fatalf("default read limit: %+v", d)
	}
	if d := api.DefaultLimits()[api.RouteAPIWrite]; d.PerMinute != 120 || d.Burst != 60 {
		t.Fatalf("default write limit: %+v", d)
	}
	app := apptest.New(t, apptest.Options{})
	srv := limitedInstance(t, app, nil, map[string]api.Limit{
		api.RouteAPIRead:  {PerMinute: 60, Burst: 3},
		api.RouteAPIWrite: {PerMinute: 30, Burst: 2},
	})
	_, other, err := app.Svc.CreateAPIKey(t.Context(), app.MerchantID, false, nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if r := call(t, srv, "GET", "/v1/products", app.TestKey); r.StatusCode != http.StatusOK {
			t.Fatalf("read %d: %d", i, r.StatusCode)
		}
	}
	r := call(t, srv, "GET", "/v1/products", app.TestKey)
	if r.StatusCode != http.StatusTooManyRequests || r.Header.Get("Retry-After") != "1" {
		t.Fatalf("read over the burst: %d Retry-After=%q", r.StatusCode, r.Header.Get("Retry-After"))
	}
	limited := limitedResponse(t, srv, app.TestKey)
	if limited.ErrorCode(t) != "rate_limited" || !strings.Contains(string(limited.Body), "API key") {
		t.Fatalf("429 body: %s", limited.Body)
	}
	// Writes have their own bucket: the empty read bucket does not stop them, and a write that
	// fails validation still spends a token.
	for i := range 2 {
		if r := call(t, srv, "POST", "/v1/products", app.TestKey); r.StatusCode != http.StatusBadRequest {
			t.Fatalf("write %d: %d", i, r.StatusCode)
		}
	}
	r = call(t, srv, "POST", "/v1/products", app.TestKey)
	if r.StatusCode != http.StatusTooManyRequests || r.Header.Get("Retry-After") != "2" {
		t.Fatalf("write over the burst: %d Retry-After=%q", r.StatusCode, r.Header.Get("Retry-After"))
	}
	// Buckets are per key: another key of the same merchant is not limited.
	if r := call(t, srv, "GET", "/v1/products", other); r.StatusCode != http.StatusOK {
		t.Fatalf("other key: %d", r.StatusCode)
	}
	// A request refused for its scope still spends a token; an invalid key never reaches a bucket.
	_, readOnly, err := app.Svc.CreateAPIKey(t.Context(), app.MerchantID, false, []string{"sessions:read"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if r := call(t, srv, "GET", "/v1/products", readOnly); r.StatusCode != http.StatusForbidden {
			t.Fatalf("scope refusal: %d", r.StatusCode)
		}
	}
	if r := call(t, srv, "GET", "/v1/products", readOnly); r.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("scope refusals spend tokens: %d", r.StatusCode)
	}
	for range 5 {
		if r := call(t, srv, "GET", "/v1/products", "sk_test_nope"); r.StatusCode != http.StatusUnauthorized {
			t.Fatalf("invalid key: %d", r.StatusCode)
		}
	}
	// The bucket refills at the configured rate.
	app.Clock.Advance(time.Second)
	if r := call(t, srv, "GET", "/v1/products", app.TestKey); r.StatusCode != http.StatusOK {
		t.Fatalf("after a second: %d", r.StatusCode)
	}
	if r := call(t, srv, "GET", "/v1/products", app.TestKey); r.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("one token per second: %d", r.StatusCode)
	}
}

// Two API instances with their own pools share each key's bucket: under concurrency exactly the
// burst gets through.
func TestMerchantAPIRateLimitSharedAcrossInstances(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	pool2, err := pgxpool.New(t.Context(), app.Store.Pool().Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool2.Close)
	limits := map[string]api.Limit{api.RouteAPIRead: {PerMinute: 1, Burst: 4}}
	servers := []*httptest.Server{limitedInstance(t, app, nil, limits), limitedInstance(t, app, pool2, limits)}
	const n = 30
	statuses := make([]int, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			statuses[i] = call(t, servers[i%2], "GET", "/v1/sessions/ses_01JBNOPE", app.TestKey).StatusCode
		})
	}
	wg.Wait()
	passed, limited := 0, 0
	for _, s := range statuses {
		switch s {
		case http.StatusNotFound:
			passed++
		case http.StatusTooManyRequests:
			limited++
		default:
			t.Fatalf("status %d", s)
		}
	}
	if passed != 4 || limited != n-4 {
		t.Fatalf("%d passed, %d limited", passed, limited)
	}
}
