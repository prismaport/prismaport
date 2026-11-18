//go:build integration

package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/alert"
	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/chainsettle"
	"github.com/prismaport/prismaport/internal/destination"
	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/service"
	"github.com/prismaport/prismaport/internal/session"
	"github.com/prismaport/prismaport/internal/store"
	"github.com/prismaport/prismaport/internal/testutil"
	"github.com/prismaport/prismaport/internal/webhooksig"
)

type delivery struct {
	Body      []byte
	Signature string
	EventID   string
}

type consumer struct {
	mu     sync.Mutex
	got    []delivery
	status int
	srv    *httptest.Server
}

func newConsumer(t *testing.T, status int) *consumer {
	c := &consumer{status: status}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.got = append(c.got, delivery{Body: body, Signature: r.Header.Get(webhooksig.HeaderName), EventID: r.Header.Get("Prismaport-Event-Id")})
		st := c.status
		c.mu.Unlock()
		w.WriteHeader(st)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *consumer) deliveries() []delivery {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]delivery(nil), c.got...)
}

func usdc(s string) money.Units {
	u, err := money.ParseDecimal(s, 6)
	if err != nil {
		panic(err)
	}
	return u
}

type sessionInfo struct {
	ID      string
	Address string
}

func createSession(t *testing.T, app *apptest.App, key, body, idemKey string) sessionInfo {
	t.Helper()
	r := app.RevealedSession(key, body, "Idempotency-Key", idemKey)
	if r.Status != http.StatusOK {
		t.Fatalf("create session: %d %s", r.Status, r.Body)
	}
	var s struct {
		ID    string `json:"id"`
		PayTo []struct {
			Chain   string `json:"chain"`
			Address string `json:"address"`
		} `json:"pay_to"`
	}
	_ = json.Unmarshal(r.Body, &s)
	for _, p := range s.PayTo {
		if p.Chain == "base-sepolia" || p.Chain == "base" {
			return sessionInfo{ID: s.ID, Address: p.Address}
		}
	}
	t.Fatalf("no EVM pay_to in %s", r.Body)
	return sessionInfo{}
}

func pay(t *testing.T, app *apptest.App, s sessionInfo, chainID, tx string, amount money.Units, confirm bool) string {
	t.Helper()
	ctx := t.Context()
	id, _, err := app.Svc.RecordPayment(ctx, service.Observation{SessionID: s.ID, Chain: chainID, Asset: "USDC", Address: s.Address, TxHash: tx, Amount: amount})
	if err != nil {
		t.Fatalf("record payment: %v", err)
	}
	if confirm {
		if _, err := app.Svc.ConfirmPayment(ctx, s.ID, id); err != nil {
			t.Fatalf("confirm: %v", err)
		}
	}
	return id
}

func sequence(t *testing.T, app *apptest.App) int {
	t.Helper()
	n, err := app.Store.SequenceEvents(t.Context(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func registerEndpoint(t *testing.T, app *apptest.App, url string, events ...string) (string, string) {
	t.Helper()
	body := map[string]any{"url": url}
	if events != nil {
		body["enabled_events"] = events
	}
	r := app.Do("POST", "/v1/webhook_endpoints", app.TestKey, body)
	if r.Status != http.StatusOK {
		t.Fatalf("endpoint: %d %s", r.Status, r.Body)
	}
	m := r.JSON(t)
	secret := m["secret"].(string)
	if !strings.HasPrefix(secret, "whsec_") {
		t.Fatalf("secret %q", secret)
	}
	return m["id"].(string), secret
}

type feedPage struct {
	Data       []json.RawMessage `json:"data"`
	HasMore    bool              `json:"has_more"`
	NextCursor string            `json:"next_cursor"`
}

type envelope struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	CreatedAt string `json:"created_at"`
	Data      struct {
		Session struct {
			ID              string `json:"id"`
			Version         int64  `json:"version"`
			Status          string `json:"status"`
			AmountReceived  string `json:"amount_received"`
			AmountRemaining string `json:"amount_remaining"`
			PaidAt          *string
		} `json:"session"`
	} `json:"data"`
}

func feed(t *testing.T, app *apptest.App, key, after string) feedPage {
	t.Helper()
	r := app.Do("GET", "/v1/events?limit=200&after="+after, key, nil)
	if r.Status != http.StatusOK {
		t.Fatalf("feed: %d %s", r.Status, r.Body)
	}
	var p feedPage
	if err := json.Unmarshal(r.Body, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPaymentFlowFeedAndWebhook(t *testing.T) {
	app := apptest.New(t, apptest.Options{})
	c := newConsumer(t, http.StatusNoContent)
	_, secret := registerEndpoint(t, app, c.srv.URL)
	s := createSession(t, app, app.TestKey, `{"amount":"25.00"}`, "flow")

	p1 := pay(t, app, s, "base-sepolia", "0xaaa", usdc("10"), true)
	again, _, err := app.Svc.RecordPayment(t.Context(), service.Observation{SessionID: s.ID, Chain: "base-sepolia", Asset: "USDC", Address: s.Address, TxHash: "0xaaa", Amount: usdc("10")})
	if err != nil || again != p1 {
		t.Fatalf("re-reported log must be a no-op: %s %v", again, err)
	}
	pay(t, app, s, "base-sepolia", "0xbbb", usdc("15"), true)

	if r := feed(t, app, app.TestKey, "0"); len(r.Data) != 0 {
		t.Fatalf("unsequenced events visible in the feed: %d", len(r.Data))
	}
	if n := sequence(t, app); n != 4 {
		t.Fatalf("sequenced %d events, want 4", n)
	}
	page := feed(t, app, app.TestKey, "0")
	wantTypes := []string{"session.payment_detected", "session.partially_paid", "session.payment_detected", "session.paid"}
	if len(page.Data) != 4 || page.HasMore {
		t.Fatalf("feed: %+v", page)
	}
	var lastVersion int64
	for i, raw := range page.Data {
		var e envelope
		if err := json.Unmarshal(raw, &e); err != nil {
			t.Fatal(err)
		}
		if e.Type != wantTypes[i] || e.Data.Session.ID != s.ID || e.Data.Session.Version <= lastVersion {
			t.Fatalf("event %d: %s v%d", i, e.Type, e.Data.Session.Version)
		}
		lastVersion = e.Data.Session.Version
	}
	var last envelope
	_ = json.Unmarshal(page.Data[3], &last)
	if last.Data.Session.Status != "paid" || last.Data.Session.AmountReceived != "25.00" || last.Data.Session.AmountRemaining != "0.00" {
		t.Fatalf("paid snapshot: %+v", last.Data.Session)
	}
	if rest := feed(t, app, app.TestKey, page.NextCursor); len(rest.Data) != 0 || rest.NextCursor != page.NextCursor {
		t.Fatalf("after cursor: %+v", rest)
	}
	if other := feed(t, app, app.LiveKey, "0"); len(other.Data) != 0 {
		t.Fatal("live feed shows test events")
	}

	n, err := app.Dispatcher.RunOnce(t.Context())
	if err != nil || n != 4 {
		t.Fatalf("dispatched %d: %v", n, err)
	}
	got := c.deliveries()
	if len(got) != 4 {
		t.Fatalf("consumer got %d", len(got))
	}
	for i, d := range got {
		if err := webhooksig.Verify(d.Body, d.Signature, secret, 0, app.Clock.Now()); err != nil {
			t.Fatalf("delivery %d signature: %v", i, err)
		}
		if string(d.Body) != string(page.Data[i]) {
			t.Fatalf("webhook body differs from feed:\n%s\n%s", d.Body, page.Data[i])
		}
		var e envelope
		_ = json.Unmarshal(d.Body, &e)
		if e.ID != d.EventID {
			t.Fatalf("event id header %s vs %s", d.EventID, e.ID)
		}
	}
	if n, _ := app.Dispatcher.RunOnce(t.Context()); n != 0 {
		t.Fatalf("succeeded deliveries re-sent: %d", n)
	}

	r := app.Do("GET", "/v1/sessions/"+s.ID, app.TestKey, nil).JSON(t)
	if r["status"] != "paid" || r["paid_at"] == nil || r["version"].(float64) != 5 || len(r["payments"].([]any)) != 2 {
		t.Fatalf("final session: %v", r)
	}
}

func TestEventFilterPerEndpoint(t *testing.T) {
	app := apptest.New(t, apptest.Options{})
	c := newConsumer(t, http.StatusOK)
	registerEndpoint(t, app, c.srv.URL, "session.paid")
	if r := app.Do("POST", "/v1/webhook_endpoints", app.TestKey, map[string]any{"url": c.srv.URL, "enabled_events": []string{"session.refunded"}}); r.ErrorCode(t) != "invalid_request" {
		t.Fatalf("unknown event type accepted: %s", r.Body)
	}
	s := createSession(t, app, app.TestKey, `{"amount":"5.00"}`, "f")
	pay(t, app, s, "base-sepolia", "0x1", usdc("5"), true)
	sequence(t, app)
	if _, err := app.Dispatcher.RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	got := c.deliveries()
	if len(got) != 1 || !strings.Contains(string(got[0].Body), `"type":"session.paid"`) {
		t.Fatalf("filtered deliveries: %d", len(got))
	}
}

func TestWebhookRetryAndDeadLetter(t *testing.T) {
	alerts := &alert.Recorder{}
	app := apptest.New(t, apptest.Options{WebhookAttempts: 3, WebhookBaseDelay: time.Second, WebhookAlerts: alerts})
	c := newConsumer(t, http.StatusInternalServerError)
	registerEndpoint(t, app, c.srv.URL)
	s := createSession(t, app, app.TestKey, `{"amount":"5.00"}`, "r")
	pay(t, app, s, "base-sepolia", "0x1", usdc("1"), false)
	sequence(t, app)
	ctx := t.Context()
	for attempt := 1; attempt <= 3; attempt++ {
		n, err := app.Dispatcher.RunOnce(ctx)
		if err != nil || n != 1 {
			t.Fatalf("attempt %d: dispatched %d (%v)", attempt, n, err)
		}
		if n, _ := app.Dispatcher.RunOnce(ctx); n != 0 {
			t.Fatalf("retried before backoff elapsed")
		}
		if attempt < 3 && alerts.Has("webhook_dead_lettered") {
			t.Fatalf("dead-letter alert after attempt %d of 3", attempt)
		}
		app.Clock.Advance(10 * time.Second)
	}
	if all := alerts.All(); len(all) != 1 || all[0].Kind != "webhook_dead_lettered" || all[0].Severity != alert.Warning ||
		all[0].Fields["merchant"] != app.MerchantID || all[0].Fields["err"] != "HTTP 500" || all[0].Fields["endpoint"] == "" {
		t.Fatalf("alerts %+v, want one webhook_dead_lettered", all)
	}
	if n, _ := app.Dispatcher.RunOnce(ctx); n != 0 {
		t.Fatal("dead delivery retried")
	}
	page := feed(t, app, app.TestKey, "0")
	var e envelope
	_ = json.Unmarshal(page.Data[0], &e)
	ds, err := app.Store.DeliveriesForEvent(ctx, app.MerchantID, e.ID)
	if err != nil || len(ds) != 1 {
		t.Fatalf("deliveries %v %v", ds, err)
	}
	if d := ds[0]; d.Status != "dead" || d.Attempts != 3 || d.LastStatusCode == nil || *d.LastStatusCode != 500 {
		t.Fatalf("dead letter: %+v", d)
	}
	if len(c.deliveries()) != 3 {
		t.Fatalf("consumer saw %d attempts", len(c.deliveries()))
	}
}

func TestDeadLetterAlertLeavesOutTheEndpointURL(t *testing.T) {
	alerts := &alert.Recorder{}
	app := apptest.New(t, apptest.Options{WebhookAttempts: 1, WebhookAlerts: alerts})
	gone := httptest.NewServer(http.NotFoundHandler())
	url := gone.URL + "/hook?token=merchant-secret-token"
	gone.Close()
	registerEndpoint(t, app, url)
	s := createSession(t, app, app.TestKey, `{"amount":"5.00"}`, "u")
	pay(t, app, s, "base-sepolia", "0x1", usdc("1"), false)
	sequence(t, app)

	if n, err := app.Dispatcher.RunOnce(t.Context()); n != 1 || err != nil {
		t.Fatalf("dispatched %d (%v)", n, err)
	}

	all := alerts.All()
	if len(all) != 1 || all[0].Kind != "webhook_dead_lettered" || all[0].Fields["err"] == "" {
		t.Fatalf("alerts %+v", all)
	}
	for k, v := range all[0].Fields {
		if strings.Contains(v, "merchant-secret-token") || strings.Contains(v, gone.URL) {
			t.Fatalf("alert field %s carries the endpoint URL: %q", k, v)
		}
	}
	if !strings.HasPrefix(all[0].Fields["err"], "dial tcp ") {
		t.Fatalf("alert lost the cause: %+v", all[0])
	}
}

func TestWebhookRecoversAfterFailure(t *testing.T) {
	app := apptest.New(t, apptest.Options{WebhookBaseDelay: time.Second})
	c := newConsumer(t, http.StatusBadGateway)
	registerEndpoint(t, app, c.srv.URL)
	s := createSession(t, app, app.TestKey, `{"amount":"5.00"}`, "x")
	pay(t, app, s, "base-sepolia", "0x1", usdc("1"), false)
	sequence(t, app)
	_, _ = app.Dispatcher.RunOnce(t.Context())
	c.mu.Lock()
	c.status = http.StatusOK
	c.mu.Unlock()
	app.Clock.Advance(time.Minute)
	if n, err := app.Dispatcher.RunOnce(t.Context()); n != 1 || err != nil {
		t.Fatalf("retry: %d %v", n, err)
	}
	page := feed(t, app, app.TestKey, "0")
	var e envelope
	_ = json.Unmarshal(page.Data[0], &e)
	ds, _ := app.Store.DeliveriesForEvent(t.Context(), app.MerchantID, e.ID)
	if ds[0].Status != "succeeded" || ds[0].Attempts != 2 {
		t.Fatalf("delivery %+v", ds[0])
	}
}

func TestSecretRotationSignsWithBoth(t *testing.T) {
	app := apptest.New(t, apptest.Options{})
	c := newConsumer(t, http.StatusOK)
	id, oldSecret := registerEndpoint(t, app, c.srv.URL)
	r := app.Do("POST", "/v1/webhook_endpoints/"+id+"/rotate_secret", app.TestKey, nil)
	if r.Status != http.StatusOK {
		t.Fatalf("rotate: %d %s", r.Status, r.Body)
	}
	newSecret := r.JSON(t)["secret"].(string)
	if newSecret == oldSecret {
		t.Fatal("rotation kept the secret")
	}
	if list := app.Do("GET", "/v1/webhook_endpoints", app.TestKey, nil); strings.Contains(string(list.Body), "whsec_") {
		t.Fatalf("list leaks secrets: %s", list.Body)
	}
	s := createSession(t, app, app.TestKey, `{"amount":"5.00"}`, "rot")
	pay(t, app, s, "base-sepolia", "0x1", usdc("1"), false)
	sequence(t, app)
	_, _ = app.Dispatcher.RunOnce(t.Context())
	d := c.deliveries()[0]
	for _, sec := range []string{oldSecret, newSecret} {
		if err := webhooksig.Verify(d.Body, d.Signature, sec, 0, app.Clock.Now()); err != nil {
			t.Fatalf("verify with %s: %v", sec[:10], err)
		}
	}
	// After the rotation window only the new secret signs.
	app.Clock.Advance(25 * time.Hour)
	pay(t, app, s, "base-sepolia", "0x2", usdc("1"), false)
	sequence(t, app)
	_, _ = app.Dispatcher.RunOnce(t.Context())
	d = c.deliveries()[1]
	if err := webhooksig.Verify(d.Body, d.Signature, oldSecret, 0, app.Clock.Now()); !errors.Is(err, webhooksig.ErrMismatch) {
		t.Fatalf("old secret still valid after window: %v", err)
	}
	if err := webhooksig.Verify(d.Body, d.Signature, newSecret, 0, app.Clock.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestDisabledEndpointDeadLettersPending(t *testing.T) {
	app := apptest.New(t, apptest.Options{})
	c := newConsumer(t, http.StatusOK)
	id, _ := registerEndpoint(t, app, c.srv.URL)
	s := createSession(t, app, app.TestKey, `{"amount":"5.00"}`, "dis")
	pay(t, app, s, "base-sepolia", "0x1", usdc("1"), false)
	sequence(t, app)
	if r := app.Do("DELETE", "/v1/webhook_endpoints/"+id, app.TestKey, nil); r.Status != http.StatusOK {
		t.Fatalf("delete: %s", r.Body)
	}
	if r := app.Do("DELETE", "/v1/webhook_endpoints/"+id, app.TestKey, nil); r.ErrorCode(t) != "not_found" {
		t.Fatalf("second delete: %s", r.Body)
	}
	if n, _ := app.Dispatcher.RunOnce(t.Context()); n != 0 || len(c.deliveries()) != 0 {
		t.Fatal("delivered to a disabled endpoint")
	}
}

func TestWebhookURLValidation(t *testing.T) {
	app := apptest.New(t, apptest.Options{})
	for _, u := range []string{"ftp://x", "not a url", "https://user:pw@host/x", ""} {
		if r := app.Do("POST", "/v1/webhook_endpoints", app.TestKey, map[string]any{"url": u}); r.Status != http.StatusBadRequest {
			t.Errorf("%q accepted: %s", u, r.Body)
		}
	}
}

func sessionJSON(t *testing.T, app *apptest.App, id string) map[string]any {
	t.Helper()
	return app.Do("GET", "/v1/sessions/"+id, app.TestKey, nil).JSON(t)
}

func TestExpiryLatePaymentAndPendingBlock(t *testing.T) {
	app := apptest.New(t, apptest.Options{})
	ctx := t.Context()
	s := createSession(t, app, app.TestKey, `{"amount":"10.00"}`, "e1")
	blocked := createSession(t, app, app.TestKey, `{"amount":"10.00"}`, "e2")
	pending := pay(t, app, blocked, "base-sepolia", "0xp", usdc("10"), false)

	if n, err := app.Svc.ExpireDue(ctx, 100); err != nil || n != 0 {
		t.Fatalf("expired early: %d %v", n, err)
	}
	app.Clock.Advance(2 * time.Hour)
	if n, err := app.Svc.ExpireDue(ctx, 100); err != nil || n != 1 {
		t.Fatalf("expired %d: %v", n, err)
	}
	if st := sessionJSON(t, app, s.ID)["status"]; st != "expired" {
		t.Fatalf("status %v", st)
	}
	if st := sessionJSON(t, app, blocked.ID)["status"]; st != "open" {
		t.Fatalf("pending payment session expired: %v", st)
	}
	if _, err := app.Svc.ConfirmPayment(ctx, blocked.ID, pending); err != nil {
		t.Fatal(err)
	}
	if st := sessionJSON(t, app, blocked.ID)["status"]; st != "paid" {
		t.Fatalf("on-time payment confirmed late should be paid: %v", st)
	}

	pay(t, app, s, "base-sepolia", "0xlate", usdc("10"), true)
	m := sessionJSON(t, app, s.ID)
	if m["status"] != "late_payment" || m["paid_at"] != nil || m["amount_received"] != "10.00" {
		t.Fatalf("late: %v", m)
	}
	sequence(t, app)
	page := feed(t, app, app.TestKey, "0")
	var types []string
	for _, raw := range page.Data {
		var e envelope
		_ = json.Unmarshal(raw, &e)
		if e.Data.Session.ID == s.ID {
			types = append(types, e.Type)
		}
	}
	want := "session.expired,session.payment_detected,session.late_payment"
	if strings.Join(types, ",") != want {
		t.Fatalf("events %v", types)
	}
}

func TestReversalAndReview(t *testing.T) {
	app := apptest.New(t, apptest.Options{})
	ctx := t.Context()
	s := createSession(t, app, app.TestKey, `{"amount":"25.00"}`, "rev")
	p1 := pay(t, app, s, "base-sepolia", "0x1", usdc("25"), true)
	extra := pay(t, app, s, "base-sepolia", "0x2", usdc("0.000001"), true)
	m := sessionJSON(t, app, s.ID)
	if m["status"] != "overpaid" || m["amount_received"] != "25.000001" {
		t.Fatalf("overpaid: %v", m)
	}
	if _, err := app.Svc.ReversePayment(ctx, s.ID, p1); err != nil {
		t.Fatal(err)
	}
	m = sessionJSON(t, app, s.ID)
	if m["status"] != "partially_paid" || m["paid_at"] != nil || m["amount_received"] != "0.000001" || m["amount_remaining"] != "24.999999" {
		t.Fatalf("after reversal: %v", m)
	}
	// The same log may be mined again after the reorg.
	again := pay(t, app, s, "base-sepolia", "0x1", usdc("25"), true)
	if again == p1 {
		t.Fatal("re-mined log reused the reversed payment")
	}
	if st := sessionJSON(t, app, s.ID)["status"]; st != "overpaid" {
		t.Fatalf("re-mined: %v", st)
	}
	if _, err := app.Svc.FinalizePayment(ctx, s.ID, extra); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Svc.ReversePayment(ctx, s.ID, extra); !errors.Is(err, session.ErrInvalidTransition) {
		t.Fatalf("finalized payment reversed: %v", err)
	}
	if _, err := app.Svc.FlagForReview(ctx, s.ID, "screening hit"); err != nil {
		t.Fatal(err)
	}
	if st := sessionJSON(t, app, s.ID)["status"]; st != "under_review" {
		t.Fatalf("review: %v", st)
	}
	if _, err := app.Svc.ClearReview(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	if st := sessionJSON(t, app, s.ID)["status"]; st != "overpaid" {
		t.Fatalf("cleared: %v", st)
	}
	sequence(t, app)
	page := feed(t, app, app.TestKey, "0")
	found := false
	for _, raw := range page.Data {
		var e envelope
		_ = json.Unmarshal(raw, &e)
		found = found || e.Type == "payment.reversed"
	}
	if !found {
		t.Fatal("no payment.reversed event")
	}
	// A payment for an address that is not in the session is refused.
	_, _, err := app.Svc.RecordPayment(ctx, service.Observation{SessionID: s.ID, Chain: "base-sepolia", Asset: "USDC", Address: "0xother", TxHash: "0x9", Amount: usdc("1")})
	if !errors.Is(err, session.ErrUnknownPayTo) {
		t.Fatalf("foreign address: %v", err)
	}
}

func TestSettlementMustMatchFeeRule(t *testing.T) {
	app := apptest.New(t, apptest.Options{})
	ctx := t.Context()
	s := createSession(t, app, app.TestKey, `{"amount":"25.00"}`, "stl")
	pay(t, app, s, "base-sepolia", "0x1", usdc("25"), true)
	base := store.Settlement{Chain: "base-sepolia", Asset: "USDC", Gross: usdc("25"), TxHash: "0xs", SettledAt: app.Clock.Now()}

	bad := base
	bad.Fee, bad.Net = usdc("0.2"), usdc("24.8")
	if _, err := app.Store.RecordSettlement(ctx, s.ID, bad, chainsettle.Verify); !errors.Is(err, store.ErrSettlementMismatch) {
		t.Fatalf("wrong fee accepted: %v", err)
	}
	dust := base
	dust.Gross, dust.Fee, dust.Net = usdc("0.1"), usdc("0.05"), usdc("0.05")
	if _, err := app.Store.RecordSettlement(ctx, s.ID, dust, chainsettle.Verify); !errors.Is(err, store.ErrSettlementMismatch) {
		t.Fatalf("dust settlement accepted: %v", err)
	}
	other := base
	other.Chain = "solana-devnet"
	other.Fee, other.Net, other.Shares = usdc("0.225"), usdc("24.775"), []money.Units{usdc("24.775")}
	other.Asset = "USDT"
	if _, err := app.Store.RecordSettlement(ctx, s.ID, other, chainsettle.Verify); !errors.Is(err, store.ErrSettlementMismatch) {
		t.Fatalf("settlement for an asset without pay_to accepted: %v", err)
	}
	short := base
	short.Fee, short.Net, short.Shares = usdc("0.225"), usdc("24.775"), []money.Units{usdc("24.774")}
	if _, err := app.Store.RecordSettlement(ctx, s.ID, short, chainsettle.Verify); !errors.Is(err, store.ErrSettlementMismatch) {
		t.Fatalf("a payout below the rule accepted: %v", err)
	}
	missing := base
	missing.Fee, missing.Net = usdc("0.225"), usdc("24.775")
	if _, err := app.Store.RecordSettlement(ctx, s.ID, missing, chainsettle.Verify); !errors.Is(err, store.ErrSettlementMismatch) {
		t.Fatalf("a settlement without payouts accepted: %v", err)
	}
	// The rule is the caller's; whatever it accepts, the store refuses payouts that do not add up
	// to gross - fee, and a write without a rule.
	accept := func(store.PayToRecord, store.Settlement) error { return nil }
	if _, err := app.Store.RecordSettlement(ctx, s.ID, short, accept); !errors.Is(err, store.ErrSettlementMismatch) {
		t.Fatalf("payouts that do not add up accepted: %v", err)
	}
	if _, err := app.Store.RecordSettlement(ctx, s.ID, missing, accept); !errors.Is(err, store.ErrSettlementMismatch) {
		t.Fatalf("a payout count other than the snapshot's accepted: %v", err)
	}
	good := base
	good.Fee, good.Net, good.Shares = usdc("0.225"), usdc("24.775"), []money.Units{usdc("24.775")}
	if _, err := app.Store.RecordSettlement(ctx, s.ID, good, nil); err == nil {
		t.Fatal("a settlement written without a rule")
	}
	if _, err := app.Store.RecordSettlement(ctx, s.ID, good, chainsettle.Verify); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Store.RecordSettlement(ctx, s.ID, good, chainsettle.Verify); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate settlement: %v", err)
	}
	m := sessionJSON(t, app, s.ID)
	st, ok := m["settlement"].(map[string]any)
	if !ok || st["gross"] != "25.000000" || st["fee"] != "0.225000" || st["net"] != "24.775000" || st["tx_hash"] != "0xs" {
		t.Fatalf("settlement view: %v", m["settlement"])
	}
	if _, many := m["settlements"]; many {
		t.Fatal("settlements list shown for a single settlement")
	}
}

// TestSequencerUnderConcurrency writes events from many goroutines while several sequencers and
// a feed poller run. The poller must end with every event exactly once, in seq order: a seq is
// never visible before every lower seq.
func TestSequencerUnderConcurrency(t *testing.T) {
	app := apptest.New(t, apptest.Options{})
	ctx := t.Context()
	const writers, perWriter = 6, 5
	var sessions []sessionInfo
	for i := 0; i < writers; i++ {
		sessions = append(sessions, createSession(t, app, app.TestKey, `{"amount":"100.00"}`, fmt.Sprint("c", i)))
	}
	done := make(chan struct{})
	var seqWG sync.WaitGroup
	for i := 0; i < 3; i++ {
		seqWG.Add(1)
		go func() {
			defer seqWG.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				if _, err := app.Store.SequenceEvents(ctx, 7); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	var polled []string
	pollDone := make(chan struct{})
	go func() {
		defer close(pollDone)
		cursor := int64(0)
		for {
			// done is read before the query: an empty page only ends the poll when it was read
			// after every event was sequenced.
			finished := false
			select {
			case <-done:
				finished = true
			default:
			}
			list, _, err := app.Store.ListEvents(ctx, app.MerchantID, false, cursor, 50)
			if err != nil {
				t.Error(err)
				return
			}
			for _, e := range list {
				if e.Seq <= cursor {
					t.Errorf("seq %d after cursor %d", e.Seq, cursor)
				}
				cursor = e.Seq
				polled = append(polled, e.ID)
			}
			if finished && len(list) == 0 {
				return
			}
		}
	}()
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				_, _, err := app.Svc.RecordPayment(ctx, service.Observation{SessionID: sessions[w].ID, Chain: "base-sepolia",
					Asset: "USDC", Address: sessions[w].Address, TxHash: fmt.Sprintf("0x%d-%d", w, i), Amount: usdc("1")})
				if err != nil {
					t.Error(err)
				}
			}
		}(w)
	}
	wg.Wait()
	testutil.Eventually(t, 30*time.Second, "every event to be sequenced", func() bool {
		n, err := app.Store.UnsequencedCount(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return n == 0
	})
	close(done)
	seqWG.Wait()
	<-pollDone

	var seqs []int64
	rows, err := app.Store.Pool().Query(ctx, `SELECT seq FROM events ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s int64
		_ = rows.Scan(&s)
		seqs = append(seqs, s)
	}
	rows.Close()
	if len(seqs) != writers*perWriter {
		t.Fatalf("%d events", len(seqs))
	}
	for i, s := range seqs {
		if s != int64(i+1) {
			t.Fatalf("seq gap: position %d has %d", i, s)
		}
	}
	if len(polled) != len(seqs) {
		t.Fatalf("poller saw %d of %d events", len(polled), len(seqs))
	}
	for _, ses := range sessions {
		evs, _ := app.Store.SessionEvents(ctx, ses.ID)
		var prevSeq int64
		for _, e := range evs {
			var seq int64
			_ = app.Store.Pool().QueryRow(ctx, `SELECT seq FROM events WHERE id = $1`, e.ID).Scan(&seq)
			if seq <= prevSeq {
				t.Fatalf("session %s: version order and seq order disagree", ses.ID)
			}
			prevSeq = seq
		}
	}
}

// missingATA reports a Solana wallet with no token account for the asset, as the RPC check
// would for a wallet that never received it.
type missingATA struct{ asset string }

func (missingATA) Inspect(context.Context, bool, string) (destination.SolanaAccountKind, error) {
	return destination.SolanaWallet, nil
}

func (m missingATA) MissingTokenAccounts(_ context.Context, _ bool, w string) ([]destination.MissingTokenAccount, error) {
	if w == apptest.SolanaDestination || m.asset == "" {
		return nil, nil
	}
	return []destination.MissingTokenAccount{{Chain: "solana", Asset: m.asset, Mint: "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB", Account: "x"}}, nil
}

// Onboarding refuses a Solana destination without a token account for an enabled mint, even
// with allowUnverified: the flusher never pays to create it.
func TestSolanaDestinationNeedsTokenAccounts(t *testing.T) {
	app := apptest.New(t, apptest.Options{SolanaInspector: missingATA{asset: "USDT"}})
	ctx := t.Context()
	wallet := "DEb5yphxEaPc5BN118svVN4R3GFu9jKs31Gcv5yekjZx"
	for _, allow := range []bool{false, true} {
		_, err := app.Svc.SetDestination(ctx, app.MerchantID, true, chain.Solana, wallet, allow, "test")
		if !errors.Is(err, destination.ErrTokenAccountMissing) || !strings.Contains(err.Error(), "USDT on solana") || service.AsError(err).Kind != service.KindInvalid {
			t.Fatalf("allowUnverified=%v: %v", allow, err)
		}
	}
	wallets, err := app.Store.ListWallets(ctx, app.MerchantID, true, store.WalletFilter{Family: chain.Solana, IncludeArchived: true})
	if err != nil || len(wallets) != 1 {
		t.Fatalf("a refused address became a wallet: %+v %v", wallets, err)
	}
	_, err = app.Svc.SeedMerchant(ctx, service.SeedInput{Name: "x", Livemode: true, SolanaDestination: wallet, Actor: "test"})
	if !errors.Is(err, destination.ErrTokenAccountMissing) {
		t.Fatalf("seed: %v", err)
	}
}
