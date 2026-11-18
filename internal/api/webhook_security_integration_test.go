//go:build integration

package api_test

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/base58"
	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/service"
	"github.com/prismaport/prismaport/internal/webhooksig"
)

func TestWebhookEndpointURLRules(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	cases := []struct {
		name string
		key  string
		mode string
		url  string
		ok   bool
	}{
		{"live https", app.LiveKey, "live", "https://shop.example/webhooks", true},
		{"live plain http", app.LiveKey, "live", "http://shop.example/webhooks", false},
		{"live plain http to localhost", app.LiveKey, "live", "http://localhost:3000/webhooks", false},
		{"live plain http to the inbox address", app.LiveKey, "live", "http://pay.test/v1/sandbox/inbox/abc", false},
		{"test https", app.TestKey, "test", "https://shop.example/webhooks", true},
		{"test http to localhost", app.TestKey, "test", "http://localhost:3000/webhooks", true},
		{"test http to a .localhost name", app.TestKey, "test", "http://shop.localhost/webhooks", true},
		{"test http to a loopback IP", app.TestKey, "test", "http://127.0.0.1:3000/webhooks", true},
		{"test http to IPv6 loopback", app.TestKey, "test", "http://[::1]:3000/webhooks", true},
		{"test http to a public host", app.TestKey, "test", "http://shop.example/webhooks", false},
		{"test http to a private IP", app.TestKey, "test", "http://10.0.0.5/webhooks", false},
		{"test http to a look-alike of localhost", app.TestKey, "test", "http://localhost.evil.example/webhooks", false},
		{"credentials", app.TestKey, "test", "https://user:pw@shop.example/webhooks", false},
		{"other scheme", app.TestKey, "test", "ftp://shop.example/webhooks", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := app.Do("POST", "/v1/webhook_endpoints", tc.key, map[string]any{"url": tc.url})
			if tc.ok != (r.Status == http.StatusOK) {
				t.Errorf("API %s: %d %s", tc.name, r.Status, r.Body)
			}
			if !tc.ok && r.Status == http.StatusBadRequest && r.ErrorCode(t) != "invalid_request" {
				t.Errorf("API %s: code %s", tc.name, r.ErrorCode(t))
			}
			// A Live endpoint takes a confirmation code (ADR 0008 C-3, D58), asked after the URL is
			// checked; Test mode takes none.
			body := map[string]any{"url": tc.url}
			if tc.mode == "live" && tc.ok {
				body["email_code"] = c.emailCode(service.ConfirmWebhookEndpoint)
			}
			d := c.do("POST", "/v1/dashboard/webhook_endpoints?mode="+tc.mode, body)
			if tc.ok != (d.Status == http.StatusOK) {
				t.Errorf("dashboard %s: %d %s", tc.name, d.Status, d.Body)
			}
		})
	}
	r := app.Do("POST", "/v1/webhook_endpoints", app.LiveKey, map[string]any{"url": "http://shop.example/webhooks"})
	if !strings.Contains(string(r.Body), "live webhook endpoints must use https://") {
		t.Fatalf("live http message: %s", r.Body)
	}
	r = app.Do("POST", "/v1/webhook_endpoints", app.TestKey, map[string]any{"url": "http://shop.example/webhooks"})
	if !strings.Contains(string(r.Body), "only in test mode, for localhost or the sandbox inbox") {
		t.Fatalf("test http message: %s", r.Body)
	}
}

// The inbox exception follows the deployment's base URL, so it is exercised with a plain http
// base: the merchant may register its inbox address, never any other path on that host.
func TestWebhookURLInboxException(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	svc := service.New(app.Store, service.Config{Registry: app.Registry, SecretBox: app.Box, CheckoutBaseURL: "http://api.internal:8080"})
	a := service.Auth{KeyID: "test", MerchantID: app.MerchantID, Scopes: []string{"webhooks:write"}}
	token := base58.Encode(make([]byte, 32))
	for url, ok := range map[string]bool{
		"http://api.internal:8080/v1/sandbox/inbox/" + token:          true,
		"http://api.internal:8080/v1/sandbox/inbox/":                  false,
		"http://api.internal:8080/v1/sandbox/inbox/" + token + "/x":   false,
		"http://api.internal:8080/v1/sandbox/inbox/" + token + "?a=1": false,
		"http://api.internal:8080/webhooks":                           false,
		"http://other.internal:8080/v1/sandbox/inbox/" + token:        false,
	} {
		t.Run(url, func(t *testing.T) {
			_, err := svc.CreateWebhookEndpoint(t.Context(), a, service.EndpointInput{URL: &url})
			if ok != (err == nil) {
				t.Errorf("%s: %v", url, err)
			}
		})
	}
	a.Livemode = true
	inbox := "http://api.internal:8080/v1/sandbox/inbox/" + token
	if _, err := svc.CreateWebhookEndpoint(t.Context(), a, service.EndpointInput{URL: &inbox}); err == nil {
		t.Fatal("live endpoint on the inbox over http accepted")
	}
}

// An endpoint registered before the rules (plain http where they now refuse it) gets no
// delivery: the attempt fails with the rule, and nothing is sent.
func TestLegacyPlainHTTPEndpointsGetNoDeliveries(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	rc := newReceiver(t, http.StatusOK, "ok")
	liveID, _ := createEndpoint(t, app, app.LiveKey, "https://shop.example/webhooks")
	testID, _ := createEndpoint(t, app, app.TestKey, "https://shop.example/webhooks")
	for id, url := range map[string]string{liveID: rc.srv.URL + "/live", testID: "http://shop.example/webhooks"} {
		t.Run(id, func(t *testing.T) {
			if _, err := app.Store.Pool().Exec(t.Context(), `UPDATE webhook_endpoints SET url = $2 WHERE id = $1`, id, url); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, key := range []string{app.LiveKey, app.TestKey} {
		r := app.RevealedSession(key, map[string]any{"amount": "25.00"}, "Idempotency-Key", "legacy-"+key[:8])
		if r.Status != http.StatusOK {
			t.Fatalf("session: %d %s", r.Status, r.Body)
		}
		ses := r.JSON(t)
		p := ses["pay_to"].([]any)[0].(map[string]any)
		amount, err := money.ParseDecimal(p["amount"].(string), 6)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := app.Svc.RecordPayment(t.Context(), service.Observation{SessionID: ses["id"].(string), Chain: p["chain"].(string),
			Asset: p["asset"].(string), Address: p["address"].(string), TxHash: fmt.Sprintf("0x%064x", len(key)), Amount: amount}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := app.Store.SequenceEvents(t.Context(), 100); err != nil {
		t.Fatal(err)
	}
	if n, err := app.Dispatcher.RunOnce(t.Context()); err != nil || n != 2 {
		t.Fatalf("dispatch: %d %v", n, err)
	}
	if got := rc.all(); len(got) != 0 {
		t.Fatalf("a live event went out over plain http: %d requests", len(got))
	}
	for id, want := range map[string]string{liveID: "live webhook endpoints must use https://", testID: "only in test mode, for localhost or the sandbox inbox"} {
		t.Run(id, func(t *testing.T) {
			var lastErr string
			if err := app.Store.Pool().QueryRow(t.Context(), `SELECT coalesce(last_error, '') FROM webhook_deliveries WHERE endpoint_id = $1`, id).Scan(&lastErr); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(lastErr, want) {
				t.Errorf("%s: last error %q", id, lastErr)
			}
		})
	}
	// A test send is refused the same way.
	if r := app.Do("POST", "/v1/webhook_endpoints/"+testID+"/test", app.TestKey, map[string]any{"type": "session.paid"}); r.Status != http.StatusBadRequest {
		t.Fatalf("test send to a refused URL: %d %s", r.Status, r.Body)
	}
}

func TestWebhookSecretsHaveThePrefix(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	rc := newReceiver(t, http.StatusOK, "ok")
	check := func(what, secret string) {
		t.Helper()
		body, ok := strings.CutPrefix(secret, "whsec_")
		raw, err := base58.Decode(body)
		if !ok || err != nil || len(raw) != 32 {
			t.Errorf("%s: %q", what, secret)
		}
	}
	id, secret := createEndpoint(t, app, app.TestKey, rc.srv.URL)
	check("merchant API create", secret)
	rot := app.Do("POST", "/v1/webhook_endpoints/"+id+"/rotate_secret", app.TestKey, nil).JSON(t)
	check("merchant API rotate", rot["secret"].(string))
	if rot["previous_secret_expires_at"] != rfc(app.Clock.Now().Add(24*time.Hour)) {
		t.Fatalf("rotation overlap: %v", rot["previous_secret_expires_at"])
	}
	if list := app.Do("GET", "/v1/webhook_endpoints", app.TestKey, nil); !strings.Contains(string(list.Body), `"previous_secret_expires_at":"`) ||
		strings.Contains(string(list.Body), "whsec_") {
		t.Fatalf("list: %s", list.Body)
	}
	d := c.send("POST", "/v1/dashboard/webhook_endpoints?mode=test", map[string]any{"url": rc.srv.URL + "/dash"})
	check("dashboard create", d["secret"].(string))
	if d["previous_secret_expires_at"] != nil {
		t.Fatalf("new endpoint has an overlap: %v", d)
	}
	check("dashboard rotate", c.send("POST", "/v1/dashboard/webhook_endpoints/"+d["id"].(string)+"/rotate_secret?mode=test", nil)["secret"].(string))
	inbox := c.send("POST", "/v1/dashboard/sandbox/inbox?mode=test", nil)["inbox"].(map[string]any)
	check("sandbox inbox", inbox["secret"].(string))
	// After the window the overlap is gone from the view.
	app.Clock.Advance(24 * time.Hour)
	for _, e := range app.Do("GET", "/v1/webhook_endpoints", app.TestKey, nil).JSON(t)["data"].([]any) {
		if m := e.(map[string]any); m["previous_secret_expires_at"] != nil {
			t.Fatalf("overlap after the window: %v", m)
		}
	}
}

// A second rotation inside the window is refused, so the secret a consumer that has not switched
// yet verifies with keeps signing; once the window ends, rotating works again.
func TestSecondRotationWaitsForTheWindow(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	rc := newReceiver(t, http.StatusOK, "ok")
	id, _ := createEndpoint(t, app, app.TestKey, rc.srv.URL)
	path := "/v1/webhook_endpoints/" + id + "/rotate_secret"
	first := app.Do("POST", path, app.TestKey, nil).JSON(t)
	app.Clock.Advance(23 * time.Hour)

	again := app.Do("POST", path, app.TestKey, nil)

	if again.Status != http.StatusConflict || again.ErrorCode(t) != service.CodeSecretRotationPending ||
		!strings.Contains(string(again.Body), app.Clock.Now().Add(time.Hour).UTC().Format("2006-01-02 15:04 UTC")) {
		t.Fatalf("rotation inside the window: %d %s", again.Status, again.Body)
	}
	for _, e := range app.Do("GET", "/v1/webhook_endpoints", app.TestKey, nil).JSON(t)["data"].([]any) {
		if m := e.(map[string]any); m["id"] == id && m["previous_secret_expires_at"] != first["previous_secret_expires_at"] {
			t.Fatalf("the refused rotation changed the endpoint: %v", m)
		}
	}
	app.Clock.Advance(time.Hour)
	if r := app.Do("POST", path, app.TestKey, nil); r.Status != http.StatusOK {
		t.Fatalf("rotation after the window: %d %s", r.Status, r.Body)
	}
}

// A secret issued without the whsec_ prefix keeps signing and verifying, through a rotation too.
func TestLegacyWebhookSecretStillVerifies(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	rc := newReceiver(t, http.StatusOK, "ok")
	id, _ := createEndpoint(t, app, app.TestKey, rc.srv.URL)
	const legacy = "3f9c1b0e7a5d42e8b6c0f1a2d3e4b5c6"
	sealed, err := app.Box.Seal([]byte(legacy), []byte(id))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.Store.Pool().Exec(t.Context(), `UPDATE webhook_endpoints SET secret_ciphertext = $2 WHERE id = $1`, id, sealed); err != nil {
		t.Fatal(err)
	}
	deliver := func(n int) received {
		t.Helper()
		r := app.Session(app.TestKey, map[string]any{"amount": "25.00"}, "Idempotency-Key", fmt.Sprintf("legacy-%d", n))
		if r.Status != http.StatusOK {
			t.Fatalf("session: %d %s", r.Status, r.Body)
		}
		payFull(t, app, r.JSON(t), fmt.Sprintf("0x%064x", 1000+n))
		if _, err := app.Store.SequenceEvents(t.Context(), 100); err != nil {
			t.Fatal(err)
		}
		for {
			n, err := app.Dispatcher.RunOnce(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if n == 0 {
				break
			}
		}
		got := rc.all()
		if len(got) == 0 {
			t.Fatal("nothing delivered")
		}
		return got[len(got)-1]
	}
	verify := func(d received, secret string) error {
		return webhooksig.Verify(d.body, d.header.Get(webhooksig.HeaderName), secret, 0, app.Clock.Now())
	}
	d := deliver(1)
	if err := verify(d, legacy); err != nil {
		t.Fatalf("legacy secret: %v (%s)", err, d.header.Get("Signature"))
	}
	fresh := app.Do("POST", "/v1/webhook_endpoints/"+id+"/rotate_secret", app.TestKey, nil).JSON(t)["secret"].(string)
	if !strings.HasPrefix(fresh, "whsec_") {
		t.Fatalf("rotated secret %q", fresh)
	}
	d = deliver(2)
	if strings.Count(d.header.Get("Signature"), "v1=") != 2 || verify(d, legacy) != nil || verify(d, fresh) != nil {
		t.Fatalf("during the rotation window: %s", d.header.Get("Signature"))
	}
	app.Clock.Advance(24 * time.Hour)
	d = deliver(3)
	if verify(d, fresh) != nil || !errors.Is(verify(d, legacy), webhooksig.ErrMismatch) {
		t.Fatalf("after the window: %s", d.header.Get("Signature"))
	}
}
