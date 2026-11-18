//go:build integration

package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/apikey"
	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/service"
)

func createKey(t *testing.T, c *dashClient, mode, name string, scopes ...string) (id, secret string) {
	t.Helper()
	k := c.send("POST", "/v1/dashboard/api_keys", map[string]any{"name": name, "scopes": scopes, "mode": mode})
	return k["id"].(string), k["secret"].(string)
}

func keyRow(t *testing.T, c *dashClient, mode, id string) map[string]any {
	t.Helper()
	for _, x := range c.get("/v1/dashboard/api_keys?mode=" + mode)["data"].([]any) {
		if m := x.(map[string]any); m["id"] == id {
			return m
		}
	}
	t.Fatalf("key %s not listed in %s mode", id, mode)
	return nil
}

func rfc(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// auditDetails returns the details of every audit entry with the action, in order.
func auditDetails(t *testing.T, app *apptest.App, action string) []map[string]any {
	t.Helper()
	list, err := app.Store.AuditRecords(t.Context(), app.MerchantID)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, a := range list {
		if a.Action != action {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(a.Details, &m); err != nil {
			t.Fatal(err)
		}
		m["_actor"] = a.Actor
		out = append(out, m)
	}
	return out
}

func rowVersion(t *testing.T, app *apptest.App, id string) string {
	t.Helper()
	var v string
	if err := app.Store.Pool().QueryRow(t.Context(), `SELECT xmin::text FROM api_keys WHERE id = $1`, id).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestAPIKeyLastUsed(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	id, secret := createKey(t, c, "test", "Backend", "sessions:read")
	if k := keyRow(t, c, "test", id); k["last_used_at"] != nil || k["status"] != "active" || k["expires_at"] != nil {
		t.Fatalf("new key: %v", k)
	}
	use := func(path string, want int) {
		t.Helper()
		if r := app.Do("GET", path, secret, nil); r.Status != want {
			t.Fatalf("GET %s: %d %s", path, r.Status, r.Body)
		}
	}
	start := app.Clock.Now()
	use("/v1/sessions/ses_01JBNOPE", http.StatusNotFound)
	if got := keyRow(t, c, "test", id)["last_used_at"]; got != rfc(start) {
		t.Fatalf("last_used_at after the first use: %v", got)
	}
	// Within the minute nothing is written at all: the row version does not change.
	version := rowVersion(t, app, id)
	for range 5 {
		app.Clock.Advance(10 * time.Second)
		use("/v1/sessions/ses_01JBNOPE", http.StatusNotFound)
	}
	if got := keyRow(t, c, "test", id)["last_used_at"]; got != rfc(start) || rowVersion(t, app, id) != version {
		t.Fatalf("written within the minute: %v", got)
	}
	app.Clock.Advance(10 * time.Second)
	use("/v1/sessions/ses_01JBNOPE", http.StatusNotFound)
	if got := keyRow(t, c, "test", id)["last_used_at"]; got != rfc(start.Add(time.Minute)) {
		t.Fatalf("last_used_at after a minute: %v", got)
	}
	// A request the key's scopes refuse is still a use of the key.
	app.Clock.Advance(5 * time.Minute)
	use("/v1/products", http.StatusForbidden)
	if got := keyRow(t, c, "test", id)["last_used_at"]; got != rfc(app.Clock.Now()) {
		t.Fatalf("last_used_at after a refused request: %v", got)
	}
	// A wrong key touches nothing.
	app.Clock.Advance(5 * time.Minute)
	before := keyRow(t, c, "test", id)["last_used_at"]
	// Swap the last character for a different one: a secret that already ends in "x" stays valid.
	wrong := secret[:len(secret)-1] + "x"
	if strings.HasSuffix(secret, "x") {
		wrong = secret[:len(secret)-1] + "y"
	}
	if r := app.Do("GET", "/v1/sessions/ses_01JBNOPE", wrong, nil); r.Status != http.StatusUnauthorized {
		t.Fatalf("wrong key: %d", r.Status)
	}
	if got := keyRow(t, c, "test", id)["last_used_at"]; got != before {
		t.Fatalf("a wrong key recorded a use: %v", got)
	}
	// Many concurrent uses on many connections write once per window.
	app.Clock.Advance(time.Hour)
	var writes atomic.Int32
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			wrote, err := app.Store.TouchAPIKey(t.Context(), id, service.LastUsedResolution)
			if err != nil {
				t.Error(err)
			}
			if wrote {
				writes.Add(1)
			}
		})
	}
	wg.Wait()
	if writes.Load() != 1 {
		t.Fatalf("%d writes in one window", writes.Load())
	}
	// The same through the API: concurrent requests leave one recorded use.
	app.Clock.Advance(time.Hour)
	for range 16 {
		wg.Go(func() {
			if r := app.Do("GET", "/v1/sessions/ses_01JBNOPE", secret, nil); r.Status != http.StatusNotFound {
				t.Errorf("concurrent use: %d", r.Status)
			}
		})
	}
	wg.Wait()
	if got := keyRow(t, c, "test", id)["last_used_at"]; got != rfc(app.Clock.Now()) {
		t.Fatalf("last_used_at after concurrent uses: %v", got)
	}
}

func TestAPIKeyRollOverlap(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	works := func(key string) int { return app.Do("GET", "/v1/products", key, nil).Status }
	for _, tc := range []struct {
		overlap string
		d       time.Duration
	}{{"1h", time.Hour}, {"24h", 24 * time.Hour}, {"7d", 7 * 24 * time.Hour}} {
		t.Run(tc.overlap, func(t *testing.T) {
			id, old := createKey(t, c, "test", "Server "+tc.overlap, "products:read", "sessions:read")
			rolledAt := app.Clock.Now()
			res := c.send("POST", "/v1/dashboard/api_keys/"+id+"/roll?mode=test", map[string]any{"overlap": tc.overlap})
			fresh, prev := res["api_key"].(map[string]any), res["previous"].(map[string]any)
			secret, _ := fresh["secret"].(string)
			if !strings.HasPrefix(secret, "sk_test_") || secret == old || !strings.HasPrefix(secret, fresh["prefix"].(string)) ||
				fresh["name"] != "Server "+tc.overlap || fresh["status"] != "active" || fresh["id"] == id {
				t.Fatalf("%s: new key %v", tc.overlap, fresh)
			}
			if got := strings.Join(toStrings(fresh["scopes"]), ","); got != "products:read,sessions:read" {
				t.Fatalf("%s: scopes not kept: %s", tc.overlap, got)
			}
			if _, leaked := prev["secret"]; leaked || prev["id"] != id || prev["status"] != "expiring" || prev["replaced_by"] != fresh["id"] ||
				prev["expires_at"] != rfc(rolledAt.Add(tc.d)) || prev["revoked_at"] != nil {
				t.Fatalf("%s: previous key %v", tc.overlap, prev)
			}
			if works(old) != http.StatusOK || works(secret) != http.StatusOK {
				t.Fatalf("%s: both keys must work during the overlap", tc.overlap)
			}
			app.Clock.Set(rolledAt.Add(tc.d - time.Second))
			if works(old) != http.StatusOK {
				t.Fatalf("%s: old key refused one second before the deadline", tc.overlap)
			}
			app.Clock.Set(rolledAt.Add(tc.d))
			// Dashboard sessions end after 12 idle hours: sign in again after the jump.
			c = login(t, app, "owner@example.com")
			if r := app.Do("GET", "/v1/products", old, nil); r.Status != http.StatusUnauthorized || r.ErrorCode(t) != "invalid_api_key" {
				t.Fatalf("%s: old key at the deadline: %d %s", tc.overlap, r.Status, r.Body)
			}
			if works(secret) != http.StatusOK {
				t.Fatalf("%s: new key refused", tc.overlap)
			}
			if k := keyRow(t, c, "test", id); k["status"] != "expired" || k["revoked_at"] != nil {
				t.Fatalf("%s: expired key before the sweep: %v", tc.overlap, k)
			}
			if n, err := app.Svc.ExpireRolledAPIKeys(t.Context()); err != nil || n != 1 {
				t.Fatalf("%s: sweep: %d %v", tc.overlap, n, err)
			}
			if n, err := app.Svc.ExpireRolledAPIKeys(t.Context()); err != nil || n != 0 {
				t.Fatalf("%s: second sweep: %d %v", tc.overlap, n, err)
			}
			if k := keyRow(t, c, "test", id); k["status"] != "expired" || k["revoked_at"] != rfc(rolledAt.Add(tc.d)) {
				t.Fatalf("%s: expired key after the sweep: %v", tc.overlap, k)
			}
			if works(old) != http.StatusUnauthorized {
				t.Fatalf("%s: old key works after the sweep", tc.overlap)
			}
		})
	}
	// Both steps of every roll are audited, the second by the system at the deadline.
	rolled, expired := auditDetails(t, app, "api_key.rolled"), auditDetails(t, app, "api_key.expired")
	if len(rolled) != 3 || len(expired) != 3 {
		t.Fatalf("audit: %d rolled, %d expired", len(rolled), len(expired))
	}
	for i, secs := range []float64{3600, 86400, 604800} {
		if rolled[i]["overlap_seconds"] != secs || rolled[i]["replaced_by"] == nil || !strings.HasPrefix(rolled[i]["_actor"].(string), "dashboard:") {
			t.Errorf("rolled audit %d: %v", i, rolled[i])
		}
		if expired[i]["key_id"] != rolled[i]["key_id"] || expired[i]["expired_at"] != rolled[i]["expires_at"] || expired[i]["_actor"] != "system" {
			t.Errorf("expired audit %d: %v vs %v", i, expired[i], rolled[i])
		}
	}
	created := 0
	for _, a := range auditDetails(t, app, "api_key.created") {
		if a["rolled_from"] != nil {
			created++
		}
	}
	if created != 3 {
		t.Fatalf("%d created entries name the rolled key", created)
	}
}

func toStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

func TestAPIKeyRollNow(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	// The seeded key was issued by the CLI: no name, every scope.
	var seeded map[string]any
	for _, x := range c.get("/v1/dashboard/api_keys?mode=test")["data"].([]any) {
		if m := x.(map[string]any); m["name"] == nil {
			seeded = m
		}
	}
	if seeded == nil {
		t.Fatal("no CLI key listed")
	}
	res := c.send("POST", "/v1/dashboard/api_keys/"+seeded["id"].(string)+"/roll?mode=test", map[string]any{"overlap": "now"})
	fresh, prev := res["api_key"].(map[string]any), res["previous"].(map[string]any)
	if fresh["name"] != nil || len(toStrings(fresh["scopes"])) != len(apikey.AllScopes) {
		t.Fatalf("roll of a CLI key: %v", fresh)
	}
	if prev["status"] != "expired" || prev["revoked_at"] != rfc(app.Clock.Now()) || prev["expires_at"] != rfc(app.Clock.Now()) {
		t.Fatalf("previous after an immediate roll: %v", prev)
	}
	if r := app.Do("GET", "/v1/products", app.TestKey, nil); r.Status != http.StatusUnauthorized {
		t.Fatalf("old key still works: %d", r.Status)
	}
	if r := app.Do("GET", "/v1/products", fresh["secret"].(string), nil); r.Status != http.StatusOK {
		t.Fatalf("new key: %d %s", r.Status, r.Body)
	}
	if n, err := app.Svc.ExpireRolledAPIKeys(t.Context()); err != nil || n != 0 {
		t.Fatalf("sweep after an immediate roll: %d %v", n, err)
	}
	if got := auditActions(t, app, app.MerchantID); !hasAll(got, "api_key.created", "api_key.rolled", "api_key.expired") {
		t.Fatalf("audit: %v", got)
	}
	if e := auditDetails(t, app, "api_key.expired"); len(e) != 1 || e[0]["key_id"] != seeded["id"] {
		t.Fatalf("expired audit: %v", e)
	}
}

func TestAPIKeyRollRefusals(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	id, old := createKey(t, c, "test", "Backend", "sessions:read")
	roll := func(mode, keyID string, body map[string]any) (int, string) {
		r := c.do("POST", "/v1/dashboard/api_keys/"+keyID+"/roll?mode="+mode, body)
		if r.Status == http.StatusOK {
			return r.Status, ""
		}
		return r.Status, r.ErrorCode(t)
	}
	for _, bad := range []map[string]any{{}, {"overlap": "2h"}, {"overlap": 3600}, {"overlap": "1h", "extra": true}} {
		if st, code := roll("test", id, bad); st != http.StatusBadRequest || code != "invalid_request" {
			t.Errorf("%v: %d %s", bad, st, code)
		}
	}
	if st, _ := roll("test", "key_01JBNOPE0000000000000000", map[string]any{"overlap": "1h"}); st != http.StatusNotFound {
		t.Fatalf("unknown key: %d", st)
	}
	if st, _ := roll("live", id, map[string]any{"overlap": "1h"}); st != http.StatusNotFound {
		t.Fatalf("key of the other mode: %d", st)
	}
	if st, _ := roll("test", id, map[string]any{"overlap": "24h"}); st != http.StatusOK {
		t.Fatalf("first roll: %d", st)
	}
	if st, code := roll("test", id, map[string]any{"overlap": "24h"}); st != http.StatusConflict || code != "invalid_state" {
		t.Fatalf("second roll of the same key: %d %s", st, code)
	}
	// Revoking a key during its overlap ends the overlap at once.
	rev := c.send("POST", "/v1/dashboard/api_keys/"+id+"/revoke?mode=test", nil)
	if rev["status"] != "revoked" || app.Do("GET", "/v1/sessions/ses_01JBNOPE", old, nil).Status != http.StatusUnauthorized {
		t.Fatalf("revoke during the overlap: %v", rev)
	}
	other, _ := createKey(t, c, "test", "Other", "sessions:read")
	c.send("POST", "/v1/dashboard/api_keys/"+other+"/revoke?mode=test", nil)
	if st, code := roll("test", other, map[string]any{"overlap": "1h"}); st != http.StatusConflict || code != "invalid_state" {
		t.Fatalf("roll of a revoked key: %d %s", st, code)
	}

	// Live keys: a confirmation code (here the app's, with 2FA on), checked after the key so a
	// refused roll never spends a code.
	totpSecret := c.enable2FA()
	app.Clock.Advance(30 * time.Second)
	liveA := c.send("POST", "/v1/dashboard/api_keys", map[string]any{"name": "Live A", "scopes": []string{"sessions:read"}, "mode": "live",
		"totp": totpCode(t, app, totpSecret)})["id"].(string)
	app.Clock.Advance(30 * time.Second)
	liveB := c.send("POST", "/v1/dashboard/api_keys", map[string]any{"name": "Live B", "scopes": []string{"sessions:read"}, "mode": "live",
		"totp": totpCode(t, app, totpSecret)})["id"].(string)
	app.Clock.Advance(30 * time.Second)
	if st, code := roll("live", liveA, map[string]any{"overlap": "1h", "password": ownerPassword}); st != http.StatusForbidden || code != "confirmation_required" {
		t.Fatalf("live roll without a code: %d %s", st, code)
	}
	code := totpCode(t, app, totpSecret)
	if st, _ := roll("live", liveA, map[string]any{"overlap": "1h", "totp": code}); st != http.StatusOK {
		t.Fatalf("live roll: %d", st)
	}
	app.Clock.Advance(30 * time.Second)
	code = totpCode(t, app, totpSecret)
	if st, c2 := roll("live", liveA, map[string]any{"overlap": "1h", "totp": code}); st != http.StatusConflict || c2 != "invalid_state" {
		t.Fatalf("refused live roll: %d %s", st, c2)
	}
	if st, _ := roll("live", liveB, map[string]any{"overlap": "7d", "totp": code}); st != http.StatusOK {
		t.Fatalf("the code of a refused roll was spent: %d", st)
	}
}
