//go:build integration

package api_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prismaport/prismaport/internal/apptest"
)

// foreignIDs are the id placeholders merchantRequests uses, by the object they stand for.
var foreignIDs = map[string]string{
	"product":  "prod_01JBNOPE",
	"link":     "plink_01JBNOPE",
	"session":  "ses_01JBNOPE",
	"endpoint": "we_01JBNOPE",
	"wallet":   "wal_01JBNOPE",
	"address":  "caddr_01JBNOPE",
}

// referencesNothing are the merchant routes that name no existing object, so there is nothing
// of another merchant's to point them at. A new route must name a placeholder, be a list (GET),
// or be added here.
var referencesNothing = map[string]bool{
	"POST /v1/webhook_endpoints": true,
	"POST /v1/wallets":           true,
}

// ownProduct stands for a product of the intruder's own, in the mode of its key.
const ownProduct = "prod_01JBOWN"

// extraMerchantProbes are requests the route table does not cover on its own: list filters that
// name an object, the full event feed, and the references a write can carry besides its main
// object (a session opened on an address, the intruder's own product paying a wallet).
var extraMerchantProbes = []struct {
	method, path string
	body         any
}{
	{"GET", "/v1/payment_links?product_id=prod_01JBNOPE", nil},
	{"GET", "/v1/customer_addresses?customer_id=probe-test", nil},
	{"GET", "/v1/events", nil},
	{"POST", "/v1/sessions", map[string]any{"customer_address_id": "caddr_01JBNOPE"}},
	{"PATCH", "/v1/products/" + ownProduct, map[string]any{
		"payout": map[string]any{"base-sepolia": []map[string]any{{"wallet_id": "wal_01JBNOPE", "share": "100"}}}}},
}

// merchantObjects are one merchant's objects in one mode, keyed like foreignIDs.
type merchantObjects map[string]string

func seedMerchantObjects(t *testing.T, app *apptest.App, key, mode string) merchantObjects {
	t.Helper()
	consumer := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(consumer.Close)
	o := merchantObjects{}
	o["product"] = newProduct(t, app, key, nil)["id"].(string)
	o["link"] = app.PaymentLink(key, o["product"], map[string]any{"name": "Probe " + mode})
	s := app.Session(key, map[string]any{"product_id": o["product"]}, idem("authz-"+mode)...)
	if s.Status != http.StatusOK {
		t.Fatalf("%s session: %d %s", mode, s.Status, s.Body)
	}
	o["session"] = s.JSON(t)["id"].(string)
	ep := app.Do("POST", "/v1/webhook_endpoints", key, map[string]any{"url": consumer.URL + "/" + mode})
	if ep.Status != http.StatusOK {
		t.Fatalf("%s endpoint: %d %s", mode, ep.Status, ep.Body)
	}
	o["endpoint"] = ep.JSON(t)["id"].(string)
	o["wallet"] = walletID(t, app, key, apptest.EVMDestination)
	ca := app.Do("POST", "/v1/customer_addresses", key, map[string]any{"customer_id": "probe-" + mode, "product_id": o["product"]})
	if ca.Status != http.StatusCreated {
		t.Fatalf("%s customer address: %d %s", mode, ca.Status, ca.Body)
	}
	o["address"] = ca.JSON(t)["id"].(string)
	if _, err := app.Store.SequenceEvents(t.Context(), 100); err != nil {
		t.Fatal(err)
	}
	return o
}

// substitute points every placeholder in s at o's objects.
func (o merchantObjects) substitute(s string) string {
	for kind, placeholder := range foreignIDs {
		s = strings.ReplaceAll(s, placeholder, o[kind])
	}
	return s
}

// snapshot is what the owner reads of its objects, to prove a probe changed nothing.
func (o merchantObjects) snapshot(t *testing.T, app *apptest.App, key string) string {
	t.Helper()
	var b strings.Builder
	for _, path := range []string{"/v1/products/" + o["product"], "/v1/payment_links/" + o["link"], "/v1/sessions/" + o["session"],
		"/v1/wallets/" + o["wallet"], "/v1/webhook_endpoints", "/v1/payment_links?product_id=" + o["product"],
		"/v1/customer_addresses/" + o["address"], "/v1/customer_addresses/" + o["address"] + "/sessions"} {
		r := app.Do("GET", path, key, nil)
		if r.Status != http.StatusOK {
			t.Fatalf("owner GET %s: %d %s", path, r.Status, r.Body)
		}
		fmt.Fprintf(&b, "%s\n%s\n", path, r.Body)
	}
	return b.String()
}

type outcome struct {
	status      int
	code, param string
}

func outcomeOf(t *testing.T, r apptest.Response) outcome {
	t.Helper()
	if r.Status < 400 {
		return outcome{status: r.Status}
	}
	var body struct {
		Error struct{ Code, Param string } `json:"error"`
	}
	if err := json.Unmarshal(r.Body, &body); err != nil {
		t.Fatalf("decode %s: %v", r.Body, err)
	}
	return outcome{r.Status, body.Error.Code, body.Error.Param}
}

// A full-access API key of one merchant can never read, change or reference another merchant's
// objects, in either mode: every merchant route that names an object answers for another
// merchant's real object exactly as for one that does not exist (404 on the by-id routes),
// the owner's objects stay as they were, and no list shows them.
func TestMerchantAPIAcrossMerchants(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	requireMerchantTable(t, app)
	otherID, otherTest := secondMerchant(t, app, "other@example.com")
	_, otherLive, err := app.Svc.CreateAPIKey(t.Context(), otherID, true, nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	newWallet(t, app, otherLive, "Other live", "0x2222222222222222222222222222222222222222")
	owner := map[string]string{"test": app.TestKey, "live": app.LiveKey}
	victim := map[string]merchantObjects{}
	before := map[string]string{}
	for mode, key := range owner {
		victim[mode] = seedMerchantObjects(t, app, key, mode)
		before[mode] = victim[mode].snapshot(t, app, key)
	}
	audit := len(auditActions(t, app, app.MerchantID))

	type request struct{ pattern, method, path, body string }
	var named, listed []request
	for pattern, rq := range merchantRequests {
		body := bodyJSON(t, rq.body)
		r := request{pattern, rq.method, rq.path, body}
		names := merchantObjects{}.substitute(rq.path+body) != rq.path+body
		switch {
		case names:
			named = append(named, r)
		case rq.method == "GET":
			listed = append(listed, r)
		case !referencesNothing[pattern]:
			t.Errorf("%s names no placeholder: point its request at an object or add it to referencesNothing", pattern)
		}
	}
	for _, p := range extraMerchantProbes {
		r := request{p.method + " " + p.path, p.method, p.path, bodyJSON(t, p.body)}
		if (merchantObjects{}).substitute(r.path+r.body) != r.path+r.body {
			named = append(named, r)
		} else {
			listed = append(listed, r)
		}
	}
	sent := 0
	do := func(r request, path, body, key string) apptest.Response {
		var b any
		if body != "" {
			b = body
		}
		sent++
		return app.Do(r.method, path, key, b, "Idempotency-Key", fmt.Sprintf("authz-%d", sent))
	}

	// leaks names what of the owner's a successful answer shows.
	leaks := func(resp apptest.Response) []string {
		var out []string
		for _, o := range victim {
			for kind, id := range o {
				if strings.Contains(string(resp.Body), id) {
					out = append(out, kind+" "+id)
				}
			}
		}
		for _, s := range []string{app.MerchantID, apptest.EVMDestination, "Probe test", "Probe live"} {
			if strings.Contains(string(resp.Body), s) {
				out = append(out, s)
			}
		}
		return out
	}
	for intruderMode, key := range map[string]string{"test": otherTest, "live": otherLive} {
		t.Run(intruderMode, func(t *testing.T) {
			own := newProduct(t, app, key, nil)["id"].(string)
			for _, r := range named {
				if intruderMode == "live" {
					// The table's payout names a Test network; a live key needs a live one to reach the
					// wallet lookup.
					r.body = strings.ReplaceAll(r.body, `"base-sepolia"`, `"base"`)
				}
				r.path = strings.ReplaceAll(r.path, ownProduct, own)
				missing := outcomeOf(t, do(r, r.path, r.body, key))
				if strings.Contains(r.pattern, "{id}") && intruderMode == "test" && missing.status != http.StatusNotFound {
					t.Errorf("%s with a test key answers %+v for a missing id, want 404", r.pattern, missing)
				}
				if r.method != "GET" && missing.status < 400 {
					t.Errorf("%s with a %s key succeeds for a missing object: the probe no longer reaches its lookup", r.pattern, intruderMode)
				}
				for objectMode, o := range victim {
					resp := do(r, o.substitute(r.path), o.substitute(r.body), key)
					got := outcomeOf(t, resp)
					if got != missing || (strings.Contains(r.pattern, "{id}") && got.status < 400) {
						t.Errorf("%s with a %s key on a %s object: %d %s; a missing id answers %+v", r.pattern, intruderMode, objectMode, resp.Status, resp.Body, missing)
					}
					if l := leaks(resp); got.status < 400 && len(l) > 0 {
						t.Errorf("%s with a %s key on a %s object shows %v", r.pattern, intruderMode, objectMode, l)
					}
				}
			}
			for _, r := range listed {
				resp := do(r, r.path, "", key)
				if resp.Status != http.StatusOK {
					t.Errorf("GET %s with a %s key: %d %s", r.path, intruderMode, resp.Status, resp.Body)
				}
				if l := leaks(resp); len(l) > 0 {
					t.Errorf("GET %s with a %s key shows %v", r.path, intruderMode, l)
				}
			}
		})
	}

	for mode, key := range owner {
		t.Run(mode, func(t *testing.T) {
			if after := victim[mode].snapshot(t, app, key); after != before[mode] {
				t.Errorf("the owner's %s objects changed:\nbefore %s\nafter %s", mode, before[mode], after)
			}
		})
	}
	if after := len(auditActions(t, app, app.MerchantID)); after != audit {
		t.Errorf("the intruder wrote %d audit entries for the owner", after-audit)
	}
}

// Postgres itself refuses a session, a customer address, a payout snapshot or a webhook delivery
// that names another merchant's object, should a query ever skip the merchant scope.
func TestOwnerForeignKeys(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	otherID, otherTest := secondMerchant(t, app, "other@example.com")
	mine := seedMerchantObjects(t, app, app.TestKey, "test")
	product := newProduct(t, app, otherTest, nil)["id"].(string)
	theirWallet := walletID(t, app, otherTest, "0x1111111111111111111111111111111111111111")
	consumer := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(consumer.Close)
	ep := app.Do("POST", "/v1/webhook_endpoints", otherTest, map[string]any{"url": consumer.URL})
	if ep.Status != http.StatusOK {
		t.Fatalf("other endpoint: %d %s", ep.Status, ep.Body)
	}
	theirEndpoint := ep.JSON(t)["id"].(string)
	s := app.Session(otherTest, map[string]any{"product_id": product}, idem("fk-theirs")...)
	if s.Status != http.StatusOK {
		t.Fatalf("other session: %d %s", s.Status, s.Body)
	}
	// A paid session emits an event: theirs gives the other merchant one, ours a delivery to our
	// endpoint.
	for key, id := range map[string]string{otherTest: s.JSON(t)["id"].(string), app.TestKey: mine["session"]} {
		t.Run(key, func(t *testing.T) {
			if r := app.Do("POST", "/v1/test_helpers/sessions/"+id+"/simulate", key, map[string]any{"action": "pay_exact"}); r.Status != http.StatusOK {
				t.Fatalf("simulate %s: %d %s", id, r.Status, r.Body)
			}
		})
	}
	if _, err := app.Store.SequenceEvents(t.Context(), 100); err != nil {
		t.Fatal(err)
	}
	var theirEvent, delivery string
	if err := app.Store.Pool().QueryRow(t.Context(), `SELECT id FROM events WHERE merchant_id = $1 LIMIT 1`, otherID).Scan(&theirEvent); err != nil {
		t.Fatal(err)
	}
	if err := app.Store.Pool().QueryRow(t.Context(), `SELECT id FROM webhook_deliveries WHERE endpoint_id = $1 LIMIT 1`,
		mine["endpoint"]).Scan(&delivery); err != nil {
		t.Fatal(err)
	}
	ca := app.Do("POST", "/v1/customer_addresses", otherTest, map[string]any{"customer_id": "theirs", "product_id": product})
	if ca.Status != http.StatusCreated {
		t.Fatalf("other customer address: %d %s", ca.Status, ca.Body)
	}
	theirs := ca.JSON(t)["id"].(string)
	var salt []byte
	if err := app.Store.Pool().QueryRow(t.Context(), `SELECT salt FROM customer_addresses WHERE id = $1`, theirs).Scan(&salt); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		constraint, sql string
		args            []any
	}{
		{"sessions_product_owner", `UPDATE sessions SET product_id = $2 WHERE id = $1`, []any{mine["session"], product}},
		{"sessions_customer_address_owner", `UPDATE sessions SET customer_address_id = $2, salt = $3 WHERE id = $1`,
			[]any{mine["session"], theirs, salt}},
		{"customer_addresses_product_owner", `UPDATE customer_addresses SET product_id = $2 WHERE id = $1`, []any{mine["address"], product}},
		{"customer_addresses_replaced_by_owner", `UPDATE customer_addresses SET status = 'rotated', rotated_at = now(), replaced_by = $2
			WHERE id = $1`, []any{mine["address"], theirs}},
		{"session_payouts_wallet_owner", `UPDATE session_payouts SET wallet_id = $2 WHERE session_id = $1`,
			[]any{mine["session"], theirWallet}},
		{"session_payouts_session_owner", `UPDATE session_payouts SET merchant_id = $2, wallet_id = NULL WHERE session_id = $1`,
			[]any{mine["session"], otherID}},
		{"customer_address_payouts_wallet_owner", `UPDATE customer_address_payouts SET wallet_id = $2 WHERE customer_address_id = $1`,
			[]any{mine["address"], theirWallet}},
		{"customer_address_payouts_address_owner", `UPDATE customer_address_payouts SET merchant_id = $2, wallet_id = NULL
			WHERE customer_address_id = $1`, []any{mine["address"], otherID}},
		{"webhook_deliveries_endpoint_owner", `UPDATE webhook_deliveries SET endpoint_id = $2 WHERE id = $1`,
			[]any{delivery, theirEndpoint}},
		{"webhook_deliveries_event_owner", `UPDATE webhook_deliveries SET event_id = $2 WHERE id = $1`, []any{delivery, theirEvent}},
	} {
		t.Run(c.constraint, func(t *testing.T) {
			_, err := app.Store.Pool().Exec(t.Context(), c.sql, c.args...)
			var pg *pgconn.PgError
			if !errors.As(err, &pg) || pg.Code != "23503" || pg.ConstraintName != c.constraint {
				t.Errorf("%s: %v, want a violation of %s", c.sql, err, c.constraint)
			}
		})
	}
}
