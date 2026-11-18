//go:build integration

package service_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/mail"
	"github.com/prismaport/prismaport/internal/service"
)

// switchable answers its status, 500 until set otherwise.
type switchable struct {
	status atomic.Int32
	srv    *httptest.Server
}

func newSwitchable(t *testing.T) *switchable {
	s := &switchable{}
	s.status.Store(http.StatusInternalServerError)
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(int(s.status.Load())) }))
	t.Cleanup(s.srv.Close)
	return s
}

type streakState struct {
	Status         string
	DisabledReason *string
	FailingSince   *time.Time
	WarnedAt       *time.Time
}

func streakOf(t *testing.T, app *apptest.App, endpointID string) streakState {
	t.Helper()
	var s streakState
	if err := app.Store.Pool().QueryRow(t.Context(), `SELECT status, disabled_reason, failing_since, failing_warned_at FROM webhook_endpoints
		WHERE id = $1`, endpointID).Scan(&s.Status, &s.DisabledReason, &s.FailingSince, &s.WarnedAt); err != nil {
		t.Fatal(err)
	}
	return s
}

func noticeCount(t *testing.T, app *apptest.App, endpointID string) int {
	t.Helper()
	var n int
	if err := app.Store.Pool().QueryRow(t.Context(), `SELECT count(*) FROM webhook_endpoint_notices WHERE endpoint_id = $1`, endpointID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// streakApp is an app whose deliveries are retried long enough to span days, with a verified
// dashboard user to warn (unless noUser), a failing endpoint and one pending delivery to it.
func streakApp(t *testing.T, noUser bool) (*apptest.App, *switchable, string) {
	t.Helper()
	app := apptest.New(t, apptest.Options{WebhookAttempts: 200})
	if !noUser {
		if _, err := app.Svc.CreateDashboardUser(t.Context(), app.MerchantID, "owner@example.com", "Correct-horse-battery-1", "test"); err != nil {
			t.Fatal(err)
		}
	}
	c := newSwitchable(t)
	id, _ := registerEndpoint(t, app, c.srv.URL+"/hooks/prismaport?token=s3cret")
	newEvent(t, app, "first")
	return app, c, id
}

var eventN atomic.Int32

func newEvent(t *testing.T, app *apptest.App, tag string) {
	t.Helper()
	s := createSession(t, app, app.TestKey, `{"amount":"5.00"}`, fmt.Sprintf("streak-%s-%d", tag, eventN.Add(1)))
	pay(t, app, s, "base-sepolia", fmt.Sprintf("0x%064x", eventN.Add(1)), usdc("1"), false)
	sequence(t, app)
}

// attemptAfter moves the clock and runs the dispatcher once; every pending delivery is due.
func attemptAfter(t *testing.T, app *apptest.App, d time.Duration) {
	t.Helper()
	app.Clock.Advance(d)
	if _, err := app.Dispatcher.RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func sendNotices(t *testing.T, app *apptest.App) {
	t.Helper()
	if _, err := app.Svc.SendWebhookNotices(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func mailsOf(app *apptest.App, kind string) []mail.Message {
	var out []mail.Message
	for _, m := range app.Mail.To("owner@example.com") {
		if m.Kind == kind {
			out = append(out, m)
		}
	}
	return out
}

func auditOf(t *testing.T, app *apptest.App, action string) []string {
	t.Helper()
	recs, err := app.Store.AuditRecords(t.Context(), app.MerchantID)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range recs {
		if r.Action == action {
			out = append(out, r.Actor+" "+string(r.Details))
		}
	}
	return out
}

// A day of failures warns every verified user once, by scheme and host only; five days (and a
// day after the warning) disable the endpoint, dead-letter what is pending and say so; only an
// enable turns it back on, and the disabled period's events are never sent.
func TestWebhookFailureStreakWarnsThenDisables(t *testing.T) {
	t.Parallel()
	app, c, id := streakApp(t, false)
	start := app.Clock.Now()
	attemptAfter(t, app, 0)
	if s := streakOf(t, app, id); s.FailingSince == nil || !s.FailingSince.Equal(start) || s.Status != "enabled" {
		t.Fatalf("first failure: %+v", s)
	}
	attemptAfter(t, app, 23*time.Hour)
	if n := noticeCount(t, app, id); n != 0 {
		t.Fatalf("warned before a day: %d", n)
	}
	attemptAfter(t, app, 2*time.Hour)
	attemptAfter(t, app, time.Hour)
	if n := noticeCount(t, app, id); n != 1 {
		t.Fatalf("one warning queued, got %d", n)
	}
	sendNotices(t, app)
	warned := mailsOf(app, mail.KindWebhookFailing)
	if len(warned) != 1 {
		t.Fatalf("warning emails: %d", len(warned))
	}
	w := warned[0].Text
	if !strings.Contains(w, c.srv.URL+" ("+id+")") || strings.Contains(w, "s3cret") || strings.Contains(w, "/hooks") ||
		!strings.Contains(w, "HTTP 500") || !strings.Contains(w, "Test mode") || !strings.Contains(w, "from "+start.Add(5*24*time.Hour).Format("2 Jan 2006 at 15:04 UTC")) {
		t.Fatalf("warning: %s", w)
	}
	s := streakOf(t, app, id)
	if s.WarnedAt == nil || noticeCount(t, app, id) != 0 || len(auditOf(t, app, "webhook_endpoint.failing_warned")) != 1 {
		t.Fatalf("warning did not count: %+v %v", s, auditOf(t, app, "webhook_endpoint.failing_warned"))
	}
	// More failures inside the streak never warn again.
	attemptAfter(t, app, 6*time.Hour)
	sendNotices(t, app)
	if len(mailsOf(app, mail.KindWebhookFailing)) != 1 {
		t.Fatal("a second warning in one streak")
	}

	// Day five. Steps of 7 hours outlast the longest backoff (6 hours, plus jitter).
	for app.Clock.Now().Before(start.Add(5*24*time.Hour - 7*time.Hour)) {
		attemptAfter(t, app, 7*time.Hour)
	}
	if s := streakOf(t, app, id); s.Status != "enabled" {
		t.Fatalf("disabled before 5 days: %+v", s)
	}
	attemptAfter(t, app, 7*time.Hour)
	s = streakOf(t, app, id)
	if s.Status != "disabled" || s.DisabledReason == nil || *s.DisabledReason != "failing" || s.FailingSince == nil {
		t.Fatalf("not disabled after 5 days: %+v", s)
	}
	if d := endpointDelivery(t, app, id); d.Status != "dead" || d.LastError == nil || *d.LastError != "endpoint kept failing" {
		t.Fatalf("pending delivery after the disable: %+v", d)
	}
	if a := auditOf(t, app, "webhook_endpoint.disabled"); len(a) != 1 || !strings.HasPrefix(a[0], "system ") || !strings.Contains(a[0], `"disabled_reason": "failing"`) {
		t.Fatalf("disable audit: %v", a)
	}
	sendNotices(t, app)
	if d := mailsOf(app, mail.KindWebhookDisabled); len(d) != 1 || !strings.Contains(d[0].Text, c.srv.URL+" ("+id+")") {
		t.Fatalf("disabled email: %v", d)
	}

	// Nothing from the disabled period goes out, not even after enable.
	newEvent(t, app, "while-disabled")
	var n int
	if err := app.Store.Pool().QueryRow(t.Context(), `SELECT count(*) FROM webhook_deliveries WHERE endpoint_id = $1`, id).Scan(&n); err != nil || n != 1 {
		t.Fatalf("deliveries while disabled: %d %v", n, err)
	}
	c.status.Store(http.StatusOK)
	if r := app.Do("PATCH", "/v1/webhook_endpoints/"+id, app.TestKey, map[string]any{"status": "enabled"}); r.Status != http.StatusOK ||
		r.JSON(t)["failing_since"] != nil {
		t.Fatalf("enable: %d %s", r.Status, r.Body)
	}
	if s := streakOf(t, app, id); s.WarnedAt != nil || s.FailingSince != nil {
		t.Fatalf("enable kept the streak: %+v", s)
	}
	newEvent(t, app, "after-enable")
	attemptAfter(t, app, time.Second)
	if err := app.Store.Pool().QueryRow(t.Context(), `SELECT count(*) FROM webhook_deliveries WHERE endpoint_id = $1 AND status = 'succeeded'`, id).Scan(&n); err != nil || n != 1 {
		t.Fatalf("delivered after enable: %d %v", n, err)
	}
}

// An attempt past both limits at once (a long quiet gap) only warns: the endpoint is disabled no
// sooner than a day after the warning.
func TestWebhookStreakCrossingBothLimitsOnlyWarns(t *testing.T) {
	t.Parallel()
	app, _, id := streakApp(t, false)
	attemptAfter(t, app, 0)
	attemptAfter(t, app, 6*24*time.Hour)
	if s := streakOf(t, app, id); s.Status != "enabled" || noticeCount(t, app, id) != 1 {
		t.Fatalf("crossing both limits: %+v, %d notices", s, noticeCount(t, app, id))
	}
	sendNotices(t, app)
	attemptAfter(t, app, 23*time.Hour)
	if s := streakOf(t, app, id); s.Status != "enabled" {
		t.Fatalf("disabled less than a day after the warning: %+v", s)
	}
	attemptAfter(t, app, 2*time.Hour)
	if s := streakOf(t, app, id); s.Status != "disabled" {
		t.Fatalf("not disabled a day after the warning: %+v", s)
	}
}

// A warning the provider refuses for every recipient does not count; the next failed attempt
// queues it again.
func TestWebhookWarningCountsOnlyOnceAccepted(t *testing.T) {
	t.Parallel()
	app, _, id := streakApp(t, false)
	attemptAfter(t, app, 0)
	attemptAfter(t, app, 25*time.Hour)
	app.Mail.Fail = errors.New("provider refused")
	sendNotices(t, app)
	if s := streakOf(t, app, id); s.WarnedAt != nil || noticeCount(t, app, id) != 0 {
		t.Fatalf("a refused warning counted: %+v", s)
	}
	attemptAfter(t, app, time.Hour)
	if noticeCount(t, app, id) != 1 {
		t.Fatal("the next failure did not queue the warning again")
	}
	app.Mail.Fail = nil
	sendNotices(t, app)
	if s := streakOf(t, app, id); s.WarnedAt == nil || len(mailsOf(app, mail.KindWebhookFailing)) != 1 {
		t.Fatalf("accepted warning: %+v", s)
	}
}

// Without a verified dashboard user the warning counts at once, audited.
func TestWebhookWarningWithoutUsersCountsAtOnce(t *testing.T) {
	t.Parallel()
	alone, _, lone := streakApp(t, true)
	attemptAfter(t, alone, 0)
	attemptAfter(t, alone, 25*time.Hour)
	if s := streakOf(t, alone, lone); s.WarnedAt == nil || noticeCount(t, alone, lone) != 0 {
		t.Fatalf("no verified user: %+v", s)
	}
	if a := auditOf(t, alone, "webhook_endpoint.failing_warned"); len(a) != 1 || !strings.Contains(a[0], `"recipients": 0`) {
		t.Fatalf("audit: %v", a)
	}
}

// A success ends the streak and drops a queued warning; pausing keeps failing_since and stops the
// streak's timers; a test event never touches the streak.
func TestWebhookStreakEndsOnSuccess(t *testing.T) {
	t.Parallel()
	app, c, id := streakApp(t, false)
	attemptAfter(t, app, 0)
	attemptAfter(t, app, 25*time.Hour)
	if noticeCount(t, app, id) != 1 {
		t.Fatal("no warning queued")
	}
	if r := app.Do("POST", "/v1/webhook_endpoints/"+id+"/test", app.TestKey, map[string]any{"type": "session.paid"}); r.Status != http.StatusOK {
		t.Fatalf("test event: %d %s", r.Status, r.Body)
	}
	if s := streakOf(t, app, id); s.FailingSince == nil {
		t.Fatal("a test event changed the streak")
	}
	c.status.Store(http.StatusOK)
	newEvent(t, app, "ok")
	attemptAfter(t, app, time.Hour)
	if s := streakOf(t, app, id); s.FailingSince != nil || s.WarnedAt != nil || noticeCount(t, app, id) != 0 {
		t.Fatalf("a success kept the streak: %+v", s)
	}
	sendNotices(t, app)
	if len(app.Mail.Sent()) != 0 {
		t.Fatal("a dropped warning was sent")
	}

	c.status.Store(http.StatusInternalServerError)
	newEvent(t, app, "fail-again")
	attemptAfter(t, app, time.Minute)
	since := streakOf(t, app, id).FailingSince
	if r := app.Do("PATCH", "/v1/webhook_endpoints/"+id, app.TestKey, map[string]any{"status": "disabled"}); r.Status != http.StatusOK ||
		r.JSON(t)["failing_since"] == nil {
		t.Fatalf("pause: %d %s", r.Status, r.Body)
	}
	if s := streakOf(t, app, id); s.FailingSince == nil || !s.FailingSince.Equal(*since) || *s.DisabledReason != "merchant" {
		t.Fatalf("pause dropped failing_since: %+v", s)
	}
}

// The sandbox inbox's endpoint never has a failure streak.
func TestWebhookInboxNeverFails(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{WebhookAttempts: 200, APIBaseURL: "https://api.prismaport.test"})
	c := newSwitchable(t)
	if _, err := app.Svc.CreateSandboxInbox(t.Context(), service.Auth{MerchantID: app.MerchantID, KeyID: "dashboard", Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	var id string
	if err := app.Store.Pool().QueryRow(t.Context(), `UPDATE webhook_endpoints SET url = $1
		WHERE id = (SELECT endpoint_id FROM sandbox_inboxes WHERE merchant_id = $2) RETURNING id`, c.srv.URL, app.MerchantID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	newEvent(t, app, "inbox")
	for range 8 {
		attemptAfter(t, app, 24*time.Hour)
	}
	if s := streakOf(t, app, id); s.FailingSince != nil || s.Status != "enabled" || noticeCount(t, app, id) != 0 {
		t.Fatalf("inbox streak: %+v", s)
	}
}
