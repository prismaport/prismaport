//go:build integration

package api_test

import (
	"bytes"
	"crypto/sha256"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/prismaport/prismaport/internal/api"
	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/service"
)

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// The API logs failures with the method, the path and the key id, never the key itself.
func TestAPILogsNeverCarryKeys(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	var logs syncBuffer
	log := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	// No secret box: creating a webhook endpoint fails inside the server, which logs it.
	broken := service.New(app.Store, service.Config{Registry: app.Registry, CheckoutBaseURL: apptest.CheckoutBase})
	h := api.New(broken, app.Store, log)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	send := func(key, body string) int {
		t.Helper()
		req, err := http.NewRequest("POST", srv.URL+"/v1/webhook_endpoints", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := send(app.TestKey, `{"url":"https://example.com/hooks"}`); got != http.StatusInternalServerError {
		t.Fatalf("broken create: %d", got)
	}
	if got := send(app.TestKey[:len(app.TestKey)-2]+"zz", `{}`); got != http.StatusUnauthorized {
		t.Fatalf("wrong key: %d", got)
	}
	if got := send("sk_live_"+strings.Repeat("1", 44), `{}`); got != http.StatusUnauthorized {
		t.Fatalf("malformed key: %d", got)
	}
	out := logs.String()
	if !strings.Contains(out, "request failed") {
		t.Fatalf("expected a logged failure, got %q", out)
	}
	body := strings.TrimPrefix(app.TestKey, "sk_test_")
	for _, secret := range []string{app.TestKey, body, body[:16], body[len(body)-16:]} {
		if strings.Contains(out, secret) {
			t.Fatalf("log carries the key: %s", out)
		}
	}
}

// Only the SHA-256 of a key is stored: no table holds the plaintext, rolled keys included.
func TestAPIKeysStoredAsHashesOnly(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	id, secret := createKey(t, c, "test", "Backend", "sessions:read")
	rolled := c.send("POST", "/v1/dashboard/api_keys/"+id+"/roll?mode=test", map[string]any{"overlap": "1h"})["api_key"].(map[string]any)
	fresh := rolled["secret"].(string)
	for _, plaintext := range []string{secret, fresh, app.TestKey, app.LiveKey} {
		want := sha256.Sum256([]byte(plaintext))
		var n int
		if err := app.Store.Pool().QueryRow(t.Context(), `SELECT count(*) FROM api_keys WHERE key_hash = $1`, want[:]).Scan(&n); err != nil || n != 1 {
			t.Fatalf("hash of %s…: %d %v", plaintext[:12], n, err)
		}
		body := plaintext[len("sk_test_"):]
		for _, table := range []string{"api_keys", "audit_log", "idempotency_keys", "rate_limits"} {
			var leaks int
			err := app.Store.Pool().QueryRow(t.Context(), `SELECT count(*) FROM `+table+` t WHERE strpos(row_to_json(t)::text, $1) > 0`, body).Scan(&leaks)
			if err != nil {
				t.Fatalf("%s: %v", table, err)
			}
			if leaks != 0 {
				t.Fatalf("%s holds a plaintext key", table)
			}
		}
	}
}
