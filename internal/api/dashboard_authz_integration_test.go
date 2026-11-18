//go:build integration

package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/api"
	"github.com/prismaport/prismaport/internal/apikey"
	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/service"
)

// fixture is one merchant's dashboard objects in test mode.
type fixture struct {
	// product is the product link sells; paid is a product whose payout pays wallet on
	// base-sepolia; address is customer's address, of product.
	product, link, session, key, endpoint, delivery, wallet, paid, address, customer string
}

// dashIDs are the id placeholders dashboardRequests uses, by the fixture object they stand for.
// None of them exists, so a probe as written names a missing object.
func (f fixture) dashIDs() map[string]string {
	return map[string]string{
		"prod_01JBNOPE": f.product, "prod_01JBPAID": f.paid, "plink_01JBNOPE": f.link, "ses_01JBNOPE": f.session,
		"key_01JBNOPE": f.key, "we_01JBNOPE": f.endpoint, "whd_01JBNOPE": f.delivery, "wal_01JBNOPE": f.wallet,
		"caddr_01JBNOPE": f.address, "probe-customer": f.customer,
	}
}

// substitute points every placeholder in s at f's objects.
func (f fixture) substitute(s string) string {
	for placeholder, id := range f.dashIDs() {
		s = strings.ReplaceAll(s, placeholder, id)
	}
	return s
}

// names reports whether s names one of the fixture's objects through a placeholder.
func names(s string) bool { return fixture{}.substitute(s) != s }

// bodyJSON is a probe body as JSON text, empty for none.
func bodyJSON(t *testing.T, v any) string {
	t.Helper()
	if v == nil {
		return ""
	}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func seedFixture(t *testing.T, c *dashClient) fixture {
	t.Helper()
	app := c.app
	consumer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	t.Cleanup(consumer.Close)
	var f fixture
	l := dashProductLink(t, c, "test", "fx-"+c.cookie.Value[:8], map[string]any{"name": "Founder", "price_type": "fixed", "amount": "25.00"})
	f.link, f.product = l["id"].(string), l["product_id"].(string)
	f.endpoint = c.send("POST", "/v1/dashboard/webhook_endpoints?mode=test", map[string]any{"url": consumer.URL})["id"].(string)
	ses := openLink(app, f.link, "fx-"+f.link, nil).JSON(t)
	f.session = ses["id"].(string)
	payFull(t, app, ses, "0x"+fmt.Sprintf("%064x", time.Now().UnixNano()))
	if _, err := app.Store.SequenceEvents(t.Context(), 100); err != nil {
		t.Fatal(err)
	}
	f.key = c.send("POST", "/v1/dashboard/api_keys", map[string]any{"name": "fx", "scopes": []string{"sessions:read"}, "mode": "test"})["id"].(string)
	dl := c.get("/v1/dashboard/webhook_endpoints/" + f.endpoint + "/deliveries?mode=test")["data"].([]any)
	if len(dl) == 0 {
		t.Fatal("fixture has no delivery")
	}
	f.delivery = dl[0].(map[string]any)["id"].(string)
	f.wallet = c.send("POST", "/v1/dashboard/wallets?mode=test", map[string]any{"family": "evm", "label": "Fixture",
		"address": "0x000000000000000000000000000000000000f1ce"})["id"].(string)
	f.paid = dashProduct(t, c, "test", "fx-paid-"+c.cookie.Value[:8], map[string]any{"name": "Fixture split", "price_type": "fixed", "amount": "25.00",
		"payout": map[string]any{"base-sepolia": items(f.wallet, "100.00")}})["id"].(string)
	// The dashboard does not create customer addresses; the merchant's server does.
	merchant := c.get("/v1/dashboard/me")["merchant"].(map[string]any)["id"].(string)
	f.customer = "fx-" + merchant
	var scopes []string
	for _, s := range apikey.AllScopes {
		scopes = append(scopes, string(s))
	}
	ca, _, _, err := app.Svc.CreateCustomerAddress(t.Context(), service.Auth{MerchantID: merchant, Scopes: scopes, Actor: "test"}, "", nil,
		service.CustomerAddressInput{CustomerID: &f.customer, ProductID: &f.product})
	if err != nil {
		t.Fatal(err)
	}
	f.address = ca.ID
	return f
}

type probe struct {
	method, path string
	body         any
}

// dashboardRequests is one request per signed-in dashboard route, keyed by its pattern. Every
// object a request names uses a placeholder of fixture.dashIDs, so the cross-merchant probe can
// point it at another merchant's real object; the mode is appended.
var dashboardRequests = map[string]probe{
	"GET /v1/dashboard/me":                                    {"GET", "/v1/dashboard/me", nil},
	"POST /v1/dashboard/auth/logout":                          {"POST", "/v1/dashboard/auth/logout", nil},
	"POST /v1/dashboard/auth/totp/enroll":                     {"POST", "/v1/dashboard/auth/totp/enroll", nil},
	"POST /v1/dashboard/auth/totp/verify":                     {"POST", "/v1/dashboard/auth/totp/verify", map[string]any{"code": "123456"}},
	"POST /v1/dashboard/auth/totp/disable":                    {"POST", "/v1/dashboard/auth/totp/disable", map[string]any{"code": "123456"}},
	"POST /v1/dashboard/auth/totp/recovery_codes":             {"POST", "/v1/dashboard/auth/totp/recovery_codes", map[string]any{"code": "123456"}},
	"POST /v1/dashboard/confirmations":                        {"POST", "/v1/dashboard/confirmations", map[string]any{"purpose": "wallet_create"}},
	"POST /v1/dashboard/confirmation_window":                  {"POST", "/v1/dashboard/confirmation_window?mode=live", map[string]any{"email_code": "123456"}},
	"DELETE /v1/dashboard/confirmation_window":                {"DELETE", "/v1/dashboard/confirmation_window", nil},
	"POST /v1/dashboard/auth/totp/removal/cancel":             {"POST", "/v1/dashboard/auth/totp/removal/cancel", nil},
	"POST /v1/dashboard/auth/password/change":                 {"POST", "/v1/dashboard/auth/password/change", map[string]any{"current_password": "x", "new_password": "y"}},
	"PUT /v1/dashboard/logo":                                  {"PUT", "/v1/dashboard/logo", map[string]any{"image": ""}},
	"DELETE /v1/dashboard/logo":                               {"DELETE", "/v1/dashboard/logo", nil},
	"GET /v1/dashboard/overview":                              {"GET", "/v1/dashboard/overview", nil},
	"GET /v1/dashboard/onboarding":                            {"GET", "/v1/dashboard/onboarding", nil},
	"GET /v1/dashboard/settings":                              {"GET", "/v1/dashboard/settings", nil},
	"PUT /v1/dashboard/accepted_assets":                       {"PUT", "/v1/dashboard/accepted_assets", map[string]any{"items": []map[string]any{{"chain": "base-sepolia", "asset": "USDC", "enabled": false}}}},
	"GET /v1/dashboard/products":                              {"GET", "/v1/dashboard/products", nil},
	"POST /v1/dashboard/products":                             {"POST", "/v1/dashboard/products", map[string]any{"name": "Steal", "price_type": "fixed", "amount": "25.00", "payout": map[string]any{"base-sepolia": items("wal_01JBNOPE", "100")}}},
	"GET /v1/dashboard/products/{id}":                         {"GET", "/v1/dashboard/products/prod_01JBNOPE", nil},
	"PATCH /v1/dashboard/products/{id}":                       {"PATCH", "/v1/dashboard/products/prod_01JBNOPE", map[string]any{"active": false}},
	"GET /v1/dashboard/products/{id}/links":                   {"GET", "/v1/dashboard/products/prod_01JBNOPE/links", nil},
	"GET /v1/dashboard/products/{id}/sessions":                {"GET", "/v1/dashboard/products/prod_01JBNOPE/sessions", nil},
	"GET /v1/dashboard/links":                                 {"GET", "/v1/dashboard/links", nil},
	"POST /v1/dashboard/links":                                {"POST", "/v1/dashboard/links", map[string]any{"product_id": "prod_01JBNOPE"}},
	"GET /v1/dashboard/links/{id}":                            {"GET", "/v1/dashboard/links/plink_01JBNOPE", nil},
	"PATCH /v1/dashboard/links/{id}":                          {"PATCH", "/v1/dashboard/links/plink_01JBNOPE", map[string]any{"active": false}},
	"GET /v1/dashboard/links/{id}/sessions":                   {"GET", "/v1/dashboard/links/plink_01JBNOPE/sessions", nil},
	"GET /v1/dashboard/sessions":                              {"GET", "/v1/dashboard/sessions", nil},
	"GET /v1/dashboard/sessions/{id}":                         {"GET", "/v1/dashboard/sessions/ses_01JBNOPE", nil},
	"POST /v1/dashboard/sessions/{id}/recheck":                {"POST", "/v1/dashboard/sessions/ses_01JBNOPE/recheck", nil},
	"POST /v1/dashboard/sessions/{id}/simulate":               {"POST", "/v1/dashboard/sessions/ses_01JBNOPE/simulate", map[string]any{"action": "under_review"}},
	"GET /v1/dashboard/customer_addresses":                    {"GET", "/v1/dashboard/customer_addresses", nil},
	"GET /v1/dashboard/customer_addresses/{id}":               {"GET", "/v1/dashboard/customer_addresses/caddr_01JBNOPE", nil},
	"GET /v1/dashboard/customer_addresses/{id}/sessions":      {"GET", "/v1/dashboard/customer_addresses/caddr_01JBNOPE/sessions", nil},
	"POST /v1/dashboard/customer_addresses/{id}/rotate":       {"POST", "/v1/dashboard/customer_addresses/caddr_01JBNOPE/rotate", nil},
	"GET /v1/dashboard/api_keys":                              {"GET", "/v1/dashboard/api_keys", nil},
	"POST /v1/dashboard/api_keys":                             {"POST", "/v1/dashboard/api_keys", map[string]any{"name": "x", "scopes": []string{"sessions:read"}, "mode": "test"}},
	"POST /v1/dashboard/api_keys/{id}/revoke":                 {"POST", "/v1/dashboard/api_keys/key_01JBNOPE/revoke", nil},
	"POST /v1/dashboard/api_keys/{id}/roll":                   {"POST", "/v1/dashboard/api_keys/key_01JBNOPE/roll", map[string]any{"overlap": "1h"}},
	"GET /v1/dashboard/webhook_endpoints":                     {"GET", "/v1/dashboard/webhook_endpoints", nil},
	"POST /v1/dashboard/webhook_endpoints":                    {"POST", "/v1/dashboard/webhook_endpoints", map[string]any{"url": "https://example.com"}},
	"PATCH /v1/dashboard/webhook_endpoints/{id}":              {"PATCH", "/v1/dashboard/webhook_endpoints/we_01JBNOPE", map[string]any{"description": "probe"}},
	"DELETE /v1/dashboard/webhook_endpoints/{id}":             {"DELETE", "/v1/dashboard/webhook_endpoints/we_01JBNOPE", nil},
	"POST /v1/dashboard/webhook_endpoints/{id}/rotate_secret": {"POST", "/v1/dashboard/webhook_endpoints/we_01JBNOPE/rotate_secret", nil},
	"POST /v1/dashboard/webhook_endpoints/{id}/test":          {"POST", "/v1/dashboard/webhook_endpoints/we_01JBNOPE/test", map[string]any{"type": "session.paid"}},
	"GET /v1/dashboard/webhook_endpoints/{id}/deliveries":     {"GET", "/v1/dashboard/webhook_endpoints/we_01JBNOPE/deliveries", nil},
	"POST /v1/dashboard/webhook_deliveries/{id}/resend":       {"POST", "/v1/dashboard/webhook_deliveries/whd_01JBNOPE/resend", nil},
	"GET /v1/dashboard/sandbox/inbox":                         {"GET", "/v1/dashboard/sandbox/inbox", nil},
	"POST /v1/dashboard/sandbox/inbox":                        {"POST", "/v1/dashboard/sandbox/inbox", nil},
	"DELETE /v1/dashboard/sandbox/inbox":                      {"DELETE", "/v1/dashboard/sandbox/inbox", nil},
	"GET /v1/dashboard/sandbox/inbox/requests":                {"GET", "/v1/dashboard/sandbox/inbox/requests", nil},
	"GET /v1/dashboard/wallets":                               {"GET", "/v1/dashboard/wallets?include_archived=true", nil},
	"POST /v1/dashboard/wallets":                              {"POST", "/v1/dashboard/wallets", map[string]any{"family": "evm", "address": "0x000000000000000000000000000000000000beef"}},
	"POST /v1/dashboard/wallets/sample":                       {"POST", "/v1/dashboard/wallets/sample", map[string]any{"family": "evm"}},
	"POST /v1/dashboard/wallets/{id}/archive":                 {"POST", "/v1/dashboard/wallets/wal_01JBNOPE/archive", nil},
	"POST /v1/dashboard/wallets/{id}/default":                 {"POST", "/v1/dashboard/wallets/wal_01JBNOPE/default", nil},
}

// dashReferencesNothing are the dashboard writes that name no existing object: they act on the
// signed-in user or merchant alone, so there is nothing of another merchant's to point them at.
// A new write must name a placeholder or be added here.
var dashReferencesNothing = map[string]bool{
	"POST /v1/dashboard/auth/logout": true, "POST /v1/dashboard/auth/totp/enroll": true, "POST /v1/dashboard/auth/totp/verify": true,
	"POST /v1/dashboard/auth/totp/disable": true, "POST /v1/dashboard/auth/totp/recovery_codes": true,
	"POST /v1/dashboard/auth/totp/removal/cancel": true, "POST /v1/dashboard/auth/password/change": true, "PUT /v1/dashboard/logo": true, "DELETE /v1/dashboard/logo": true,
	"PUT /v1/dashboard/accepted_assets": true, "POST /v1/dashboard/api_keys": true, "POST /v1/dashboard/webhook_endpoints": true,
	"POST /v1/dashboard/sandbox/inbox": true, "DELETE /v1/dashboard/sandbox/inbox": true, "POST /v1/dashboard/wallets": true,
	"POST /v1/dashboard/wallets/sample": true, "POST /v1/dashboard/confirmations": true,
	"POST /v1/dashboard/confirmation_window": true, "DELETE /v1/dashboard/confirmation_window": true,
}

// extraDashboardProbes are requests the route table does not cover on its own: list filters that
// name an object, and payouts naming a wallet on a product that already exists.
var extraDashboardProbes = []probe{
	{"GET", "/v1/dashboard/sessions?link_id=plink_01JBNOPE", nil},
	{"GET", "/v1/dashboard/sessions?product_id=prod_01JBNOPE", nil},
	{"GET", "/v1/dashboard/sessions?q=ses_01JBNOPE", nil},
	{"GET", "/v1/dashboard/sessions?customer_address_id=caddr_01JBNOPE", nil},
	{"GET", "/v1/dashboard/customer_addresses?customer_id=probe-customer", nil},
	{"PATCH", "/v1/dashboard/products/prod_01JBPAID", map[string]any{"payout": map[string]any{"base-sepolia": items("wal_01JBNOPE", "100")}}},
	{"PATCH", "/v1/dashboard/products/" + ownProduct, map[string]any{"payout": map[string]any{"base-sepolia": items("wal_01JBNOPE", "100")}}},
}

// requireDashboardTable fails unless dashboardRequests lists exactly the registered dashboard
// routes, so a new route fails every test built on the table until it is added.
func requireDashboardTable(t *testing.T, app *apptest.App) {
	t.Helper()
	h := api.New(app.Svc, app.Store, nil)
	h.MountDashboard()
	h.MountSandbox(app.Dispatcher)
	registered := h.DashboardRoutes()
	for _, pattern := range registered {
		if _, ok := dashboardRequests[pattern]; !ok {
			t.Errorf("dashboard route %s is not in dashboardRequests", pattern)
		}
	}
	for pattern, p := range dashboardRequests {
		if !slices.Contains(registered, pattern) {
			t.Errorf("dashboardRequests lists %s, which is not registered", pattern)
		}
		named := names(p.path + bodyJSON(t, p.body))
		if p.method != "GET" && !named && !dashReferencesNothing[pattern] {
			t.Errorf("%s names no placeholder: point its request at an object or add it to dashReferencesNothing", pattern)
		}
		if strings.Contains(pattern, "{id}") && !named {
			t.Errorf("%s takes an id but its request names no placeholder", pattern)
		}
	}
}

// withMode appends the mode to a probe path.
func withMode(path, mode string) string {
	if strings.Contains(path, "?") {
		return path + "&mode=" + mode
	}
	return path + "?mode=" + mode
}

// byID are the requests that address one of f's objects by id; mode is appended.
func (f fixture) byID() []probe {
	var out []probe
	for pattern, p := range dashboardRequests {
		if strings.Contains(pattern, "{id}") {
			out = append(out, probe{p.method, f.substitute(p.path), p.body})
		}
	}
	return out
}

// lists are the reads that name no object.
func lists() []string {
	var out []string
	for _, p := range dashboardRequests {
		if p.method == "GET" && !names(p.path) {
			out = append(out, p.path)
		}
	}
	return out
}

// shows names what of f, or of its merchant, a body shows.
func (f fixture) shows(app *apptest.App, body []byte) []string {
	var out []string
	secrets := []string{app.MerchantID, "Founder", "Fixture split", apptest.EVMDestination, "0x000000000000000000000000000000000000f1ce"}
	for _, id := range f.dashIDs() {
		secrets = append(secrets, id)
	}
	for _, s := range secrets {
		if strings.Contains(string(body), s) {
			out = append(out, s)
		}
	}
	return out
}

// untouched checks that A's objects still are what they were.
func (f fixture) untouched(t *testing.T, owner *dashClient) {
	t.Helper()
	if l := owner.get("/v1/dashboard/links/" + f.link + "?mode=test"); l["active"] != true || l["product"].(map[string]any)["active"] != true {
		t.Fatalf("link or product changed: %v", l)
	}
	for _, k := range owner.get("/v1/dashboard/api_keys?mode=test")["data"].([]any) {
		if m := k.(map[string]any); m["id"] == f.key && (m["revoked_at"] != nil || m["replaced_by"] != nil || m["status"] != "active") {
			t.Fatalf("key revoked or rolled: %v", m)
		}
	}
	if eps := owner.get("/v1/dashboard/webhook_endpoints?mode=test")["data"].([]any); len(eps) != 1 {
		t.Fatalf("endpoint deleted: %v", eps)
	}
	if p := owner.get("/v1/dashboard/products/" + f.paid + "?mode=test"); payoutOf(p, "base-sepolia") != f.wallet+":100.00" {
		t.Fatalf("product payout changed: %v", p["payout"])
	}
	for _, w := range owner.get("/v1/dashboard/wallets?mode=test&include_archived=true")["data"].([]any) {
		if m := w.(map[string]any); m["id"] == f.wallet && m["archived_at"] != nil {
			t.Fatalf("wallet archived: %v", m)
		}
	}
	if ca := owner.get("/v1/dashboard/customer_addresses/" + f.address + "?mode=test"); ca["status"] != "active" {
		t.Fatalf("customer address rotated: %v", ca)
	}
}

// A dashboard user can never read, change or reference another merchant's objects, in either
// mode: every dashboard route that names an object answers for another merchant's real object
// exactly as for one that does not exist (404 on the by-id routes), the owner's objects stay as
// they were, and no list shows them.
func TestDashboardAuthzAcrossMerchants(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	requireDashboardTable(t, app)
	owner := login(t, app, "owner@example.com")
	f := seedFixture(t, owner)
	otherID, _ := secondMerchant(t, app, "other@example.com")
	intruder := login(t, app, "other@example.com")
	if intruder.get("/v1/dashboard/me")["merchant"].(map[string]any)["id"] != otherID {
		t.Fatal("intruder signed into the wrong merchant")
	}
	// The probes address real objects: their owner can read them.
	for _, p := range f.byID() {
		if p.method == "GET" {
			if r := owner.do("GET", withMode(p.path, "test"), nil); r.Status != http.StatusOK {
				t.Fatalf("owner GET %s: %d %s", p.path, r.Status, r.Body)
			}
		}
	}
	intruder.send("POST", "/v1/dashboard/wallets?mode=live", map[string]any{"family": "evm", "label": "Own live",
		"address": "0x2222222222222222222222222222222222222222", "email_code": intruder.emailCode(service.ConfirmWalletCreate)})
	own := map[string]string{}
	for _, mode := range []string{"test", "live"} {
		own[mode] = dashProduct(t, intruder, mode, "own-"+mode, map[string]any{"name": "Own", "price_type": "fixed", "amount": "25.00"})["id"].(string)
	}

	// request is a probe with its body as JSON text, so placeholders can be replaced in it.
	type request struct{ pattern, method, path, body string }
	var named []request
	for pattern, p := range dashboardRequests {
		if r := (request{pattern, p.method, p.path, bodyJSON(t, p.body)}); names(r.path + r.body) {
			named = append(named, r)
		}
	}
	for _, p := range extraDashboardProbes {
		named = append(named, request{p.method + " " + p.path, p.method, p.path, bodyJSON(t, p.body)})
	}
	before := len(auditActions(t, app, app.MerchantID))
	sent := 0
	do := func(r request, path, body string) apptest.Response {
		sent++
		var b any
		if body != "" {
			b = body
		}
		return intruder.do(r.method, path, b, "Idempotency-Key", fmt.Sprintf("authz-%d", sent))
	}
	for _, mode := range []string{"test", "live"} {
		for _, r := range named {
			path := strings.ReplaceAll(r.path, ownProduct, own[mode])
			body := r.body
			if mode == "live" {
				// The probes' payouts name a Test network; a live request needs a live one to reach
				// the wallet lookup.
				body = strings.ReplaceAll(body, `"base-sepolia"`, `"base"`)
			}
			missing := outcomeOf(t, do(r, withMode(path, mode), body))
			byID := strings.Contains(r.pattern, "{id}")
			if byID && mode == "test" && missing.status != http.StatusNotFound {
				t.Errorf("%s answers %+v for a missing id, want 404", r.pattern, missing)
			}
			if r.method != "GET" && missing.status < 400 {
				t.Errorf("%s?mode=%s succeeds for a missing object: the probe no longer reaches its lookup", r.pattern, mode)
			}
			resp := do(r, withMode(f.substitute(path), mode), f.substitute(body))
			got := outcomeOf(t, resp)
			if got != missing || (byID && got.status < 400) {
				t.Errorf("%s %s?mode=%s on the owner's object: %d %s; a missing one answers %+v", r.method, path, mode, resp.Status, resp.Body, missing)
			}
			if s := f.shows(app, resp.Body); got.status < 400 && len(s) > 0 {
				t.Errorf("%s %s?mode=%s shows %v", r.method, path, mode, s)
			}
		}
		// A list answers the intruder's own data, never the owner's. Only the sandbox inbox may
		// refuse: it does not exist before the intruder creates one, and not in live mode.
		for _, path := range lists() {
			r := intruder.do("GET", withMode(path, mode), nil)
			refused := strings.HasPrefix(path, "/v1/dashboard/sandbox/inbox") && r.Status >= 400 && r.Status < 500
			if s := f.shows(app, r.Body); (r.Status != http.StatusOK && !refused) || len(s) > 0 {
				t.Errorf("GET %s?mode=%s: %d, shows %v", path, mode, r.Status, s)
			}
		}
	}
	f.untouched(t, owner)
	if after := len(auditActions(t, app, app.MerchantID)); after != before {
		t.Fatalf("intruder wrote %d audit entries for the victim", after-before)
	}
	// Each sees its own data.
	mine := seedFixture(t, intruder)
	if ids := sessionIDs(t, owner, "/v1/dashboard/sessions?mode=test"); strings.Contains(strings.Join(ids, ","), mine.session) {
		t.Fatal("owner sees the other merchant's session")
	}
}

// Every list and every object is scoped to the mode it was created in.
func TestDashboardModeScoping(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	requireDashboardTable(t, app)
	c := login(t, app, "owner@example.com")
	f := seedFixture(t, c)
	for pattern, p := range dashboardRequests {
		if !strings.Contains(pattern, "{id}") {
			continue
		}
		missing := outcomeOf(t, c.do(p.method, withMode(p.path, "live"), p.body, "Idempotency-Key", "live-missing"))
		path := f.substitute(p.path)
		r := c.do(p.method, withMode(path, "live"), p.body, "Idempotency-Key", "live-real")
		if got := outcomeOf(t, r); got != missing || got.status < 400 {
			t.Errorf("%s %s in live mode: %d %s; a missing one answers %+v", p.method, path, r.Status, r.Body, missing)
		}
		if r := c.do(p.method, path, p.body); r.Status != http.StatusBadRequest {
			t.Errorf("%s %s without mode: %d %s", p.method, path, r.Status, r.Body)
		}
	}
	for _, path := range lists() {
		r := c.do("GET", withMode(path, "live"), nil)
		for _, id := range f.dashIDs() {
			if strings.Contains(string(r.Body), id) {
				t.Errorf("GET %s shows test object %s", path, id)
			}
		}
	}
	// Every list but who the user is belongs to a mode.
	for _, path := range lists() {
		if path == "/v1/dashboard/me" {
			continue
		}
		if r := c.do("GET", withMode(path, ""), nil); r.Status != http.StatusBadRequest {
			t.Errorf("GET %s with an empty mode: %d", path, r.Status)
		}
	}
	f.untouched(t, c)
	// A live link is created live and only listed in live mode; a test product cannot get one.
	live := dashProductLink(t, c, "live", "live-1", map[string]any{"name": "Live", "price_type": "fixed", "amount": "25.00"})
	if live["livemode"] != true || strings.Contains(string(c.do("GET", "/v1/dashboard/links?mode=test", nil).Body), live["id"].(string)) ||
		strings.Contains(string(c.do("GET", "/v1/dashboard/products?mode=test", nil).Body), live["product_id"].(string)) {
		t.Fatalf("live link: %v", live)
	}
	if r := c.do("POST", "/v1/dashboard/links?mode=live", map[string]any{"product_id": f.product}, "Idempotency-Key", "cross"); r.Status != http.StatusNotFound {
		t.Fatalf("a live link of a test product: %d %s", r.Status, r.Body)
	}
}

func TestDashboardRequiresSessionAndCSRF(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	requireDashboardTable(t, app)
	owner := login(t, app, "owner@example.com")
	f := seedFixture(t, owner)
	var routes []probe
	for _, p := range dashboardRequests {
		routes = append(routes, probe{p.method, f.substitute(p.path), p.body})
	}
	anon := newClient(t, app)
	for _, p := range routes {
		r := anon.do(p.method, withMode(p.path, "test"), p.body, "Idempotency-Key", "anon")
		if r.Status != http.StatusUnauthorized || r.ErrorCode(t) != "unauthenticated" || r.Header.Get("Cache-Control") != "no-store" {
			t.Errorf("anonymous %s %s: %d %s", p.method, p.path, r.Status, r.Body)
		}
	}
	noCSRF := *owner
	noCSRF.csrf = ""
	for _, p := range routes {
		if p.method == "GET" {
			continue
		}
		r := noCSRF.do(p.method, withMode(p.path, "test"), p.body, "Idempotency-Key", "nocsrf")
		if r.Status != http.StatusForbidden || r.ErrorCode(t) != "csrf_failed" {
			t.Errorf("%s %s without CSRF: %d %s", p.method, p.path, r.Status, r.Body)
		}
	}
	f.untouched(t, owner)
	// The dashboard never answers cross-origin reads with CORS headers.
	r := owner.do("GET", "/v1/dashboard/me", nil, "Origin", "https://evil.example")
	if r.Header.Get("Access-Control-Allow-Origin") != "" || r.Header.Get("Access-Control-Allow-Credentials") != "" {
		t.Fatalf("CORS headers on the dashboard: %v", r.Header)
	}
	if r := anon.do("GET", "/v1/dashboard/nope", nil); r.Status != http.StatusNotFound {
		t.Fatalf("unknown dashboard route: %d", r.Status)
	}
}
