//go:build integration

package api_test

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/apptest"
)

// lastPaid reads the last_paid_at the session writer keeps on a product or link row.
func lastPaid(t *testing.T, app *apptest.App, table, id string) *time.Time {
	t.Helper()
	var at *time.Time
	if err := app.Store.Pool().QueryRow(t.Context(), `SELECT last_paid_at FROM `+table+` WHERE id = $1`, id).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at
}

func sessionPaidAt(t *testing.T, app *apptest.App, id string) time.Time {
	t.Helper()
	var at time.Time
	if err := app.Store.Pool().QueryRow(t.Context(), `SELECT paid_at FROM sessions WHERE id = $1`, id).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at
}

// The list order reads last_paid_at off the rows: a payment moves it later, a reorg that takes
// a session out of paid recomputes it from the sessions still paid.
func TestDashboardLastPaidFollowsReversal(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	prod := dashProduct(t, c, "test", "p", map[string]any{"name": "Founder", "price_type": "fixed", "amount": "10.00"})["id"].(string)
	app.Clock.Advance(time.Second)
	first := dashLink(t, c, "test", "first", map[string]any{"product_id": prod, "name": "First"})["id"].(string)
	app.Clock.Advance(time.Second)
	second := dashLink(t, c, "test", "second", map[string]any{"product_id": prod, "name": "Second"})["id"].(string)
	payLink := func(link, tx string) string {
		t.Helper()
		r := openLink(app, link, "k-"+tx, nil)
		if r.Status != http.StatusOK {
			t.Fatalf("open %s: %d %s", link, r.Status, r.Body)
		}
		ses := r.JSON(t)
		payFull(t, app, ses, "0x"+tx)
		return ses["id"].(string)
	}
	if lastPaid(t, app, "products", prod) != nil || lastPaid(t, app, "payment_links", first) != nil {
		t.Fatal("never paid rows hold a last payment")
	}
	early := payLink(first, fmt.Sprintf("%064x", 1))
	app.Clock.Advance(time.Minute)
	late := payLink(second, fmt.Sprintf("%064x", 2))
	same := func(got *time.Time, want time.Time) bool { return got != nil && got.Equal(want) }
	if p, l1, l2 := lastPaid(t, app, "products", prod), lastPaid(t, app, "payment_links", first), lastPaid(t, app, "payment_links", second); !same(p, sessionPaidAt(t, app, late)) ||
		!same(l1, sessionPaidAt(t, app, early)) || !same(l2, sessionPaidAt(t, app, late)) {
		t.Fatalf("after two payments: product %v, links %v %v", p, l1, l2)
	}
	ids := func(path string) string {
		var out []string
		for _, x := range c.get(path)["data"].([]any) {
			out = append(out, x.(map[string]any)["id"].(string))
		}
		return strings.Join(out, ",")
	}
	if got := ids("/v1/dashboard/links?mode=test"); got != second+","+first {
		t.Fatalf("order before the reorg: %v", got)
	}

	var payment string
	if err := app.Store.Pool().QueryRow(t.Context(), `SELECT id FROM payments WHERE session_id = $1`, late).Scan(&payment); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Svc.ReversePayment(t.Context(), late, payment); err != nil {
		t.Fatal(err)
	}
	if p, l2 := lastPaid(t, app, "products", prod), lastPaid(t, app, "payment_links", second); !same(p, sessionPaidAt(t, app, early)) || l2 != nil {
		t.Fatalf("after the reorg: product %v, second link %v", p, l2)
	}
	if got := ids("/v1/dashboard/links?mode=test"); got != first+","+second {
		t.Fatalf("order after the reorg: %v", got)
	}
}

// The link lists filter liveness in SQL and render it with Liveness: every rule must agree,
// on the dashboard and on the merchant API.
func TestLinkListLivenessAgrees(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	prod := dashProduct(t, c, "test", "p", map[string]any{"name": "Founder", "price_type": "fixed", "amount": "10.00"})["id"].(string)
	gone := dashProduct(t, c, "test", "g", map[string]any{"name": "Retired", "price_type": "fixed", "amount": "10.00"})["id"].(string)
	mk := func(label, product string, extra map[string]any) string {
		app.Clock.Advance(time.Second)
		body := map[string]any{"product_id": product, "name": label}
		for k, v := range extra {
			body[k] = v
		}
		return dashLink(t, c, "test", label, body)["id"].(string)
	}
	plain := mk("plain", prod, nil)
	reserved := mk("reserved", prod, map[string]any{"max_payments": 1})
	room := mk("room", prod, map[string]any{"max_payments": 2})
	expiring := mk("expiring", prod, map[string]any{"expires_at": app.Clock.Now().Add(time.Hour).Format(time.RFC3339)})
	retired := mk("retired", gone, nil)
	off := mk("off", prod, nil)
	c.send("PATCH", "/v1/dashboard/links/"+off+"?mode=test", map[string]any{"active": false})
	c.send("PATCH", "/v1/dashboard/products/"+gone+"?mode=test", map[string]any{"active": false})
	for _, link := range []string{reserved, room} {
		if r := openLink(app, link, "held-"+link, nil); r.Status != http.StatusOK {
			t.Fatalf("open %s: %d %s", link, r.Status, r.Body)
		}
	}

	check := func(step string, wantLive ...string) {
		t.Helper()
		sort.Strings(wantLive)
		for _, list := range []struct {
			path string
			key  string
		}{{"/v1/dashboard/links?mode=test", ""}, {"/v1/payment_links?limit=100", app.TestKey}} {
			get := func(status string) map[string]bool {
				var page map[string]any
				if list.key == "" {
					page = c.get(list.path + "&status=" + status)
				} else {
					page = app.Do("GET", list.path+"&status="+status, list.key, nil).JSON(t)
				}
				out := map[string]bool{}
				for _, x := range page["data"].([]any) {
					l := x.(map[string]any)
					out[l["id"].(string)] = l["live"].(bool)
				}
				return out
			}
			live, inactive := get("live"), get("inactive")
			var got []string
			for id, rendered := range live {
				if !rendered {
					t.Errorf("%s %s: %s filtered live, rendered not live", step, list.path, id)
				}
				got = append(got, id)
			}
			for id, rendered := range inactive {
				if rendered {
					t.Errorf("%s %s: %s filtered inactive, rendered live", step, list.path, id)
				}
			}
			sort.Strings(got)
			if strings.Join(got, ",") != strings.Join(wantLive, ",") || len(live)+len(inactive) != 6 {
				t.Errorf("%s %s: live %v inactive %v, want live %v", step, list.path, got, inactive, wantLive)
			}
		}
	}
	check("held", plain, room, expiring)
	if l := c.get("/v1/dashboard/links/" + reserved + "?mode=test"); l["unavailable_reason"] != "reserved" {
		t.Fatalf("reserved: %v", l["unavailable_reason"])
	}
	if l := c.get("/v1/dashboard/links/" + retired + "?mode=test"); l["unavailable_reason"] != "product_inactive" {
		t.Fatalf("retired: %v", l["unavailable_reason"])
	}
	// Past the link's expiry, and past the held sessions' own: the expired sessions free the slots.
	app.Clock.Advance(2 * time.Hour)
	check("expired link", plain, room)
	if _, err := app.Svc.ExpireDue(t.Context(), 100); err != nil {
		t.Fatal(err)
	}
	check("slots released", plain, reserved, room)
}

// A link that expires between two pages of the activity order is not returned twice: the first
// page pins the instant liveness is evaluated at in the cursor.
func TestLinkPagesPinLiveness(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	prod := dashProduct(t, c, "test", "p", map[string]any{"name": "Founder", "price_type": "fixed", "amount": "10.00"})["id"].(string)
	for i := 0; i < 3; i++ {
		app.Clock.Advance(time.Second)
		dashLink(t, c, "test", fmt.Sprint("l", i), map[string]any{"product_id": prod, "name": fmt.Sprint("l", i),
			"expires_at": app.Clock.Now().Add(time.Hour).Format(time.RFC3339)})
	}
	page := c.get("/v1/dashboard/links?mode=test&limit=1")
	seen := map[string]bool{}
	take := func(page map[string]any) {
		t.Helper()
		for _, x := range page["data"].([]any) {
			id := x.(map[string]any)["id"].(string)
			if seen[id] {
				t.Errorf("%s returned twice", id)
			}
			seen[id] = true
		}
	}
	take(page)
	app.Clock.Advance(2 * time.Hour) // every link expires between the pages
	for page["next_cursor"] != nil {
		page = c.get("/v1/dashboard/links?mode=test&limit=1&cursor=" + page["next_cursor"].(string))
		take(page)
	}
	if len(seen) != 3 {
		t.Fatalf("saw %d links, want 3", len(seen))
	}
}

// A forged cursor with a time the database cannot encode is a 400, not a 500.
func TestForgedCursorsAreBadRequests(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	for _, path := range []string{"/v1/dashboard/links?mode=test&cursor=", "/v1/dashboard/products?mode=test&cursor="} {
		for _, raw := range []string{
			`{"l":1,"a":1,"p":9223372036854775807,"i":"x"}`,
			`{"l":1,"a":1,"p":-5,"i":"x"}`,
			`{"l":1,"a":1,"p":-1,"i":"x","n":9223372036854775807}`, // products have no pinned time
			`{"l":7,"a":7,"p":-1,"i":"x"}`,
		} {
			if strings.Contains(raw, `"n"`) && strings.Contains(path, "products") {
				continue
			}
			if r := c.do("GET", path+enc(raw), nil); r.Status != http.StatusBadRequest {
				t.Errorf("%s%s: %d %s", path, raw, r.Status, r.Body)
			}
		}
	}
}
