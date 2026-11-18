//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/service"
	"github.com/prismaport/prismaport/internal/testutil"
	"github.com/prismaport/prismaport/internal/webhooksig"
)

// syncBuffer is the CLI's output, read while the command runs.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// localApp is the developer's local server receiving forwarded events.
type localApp struct {
	mu     sync.Mutex
	bodies [][]byte
	sigs   []string
	ids    []string
	srv    *httptest.Server
}

func newLocalApp(t *testing.T) *localApp {
	l := &localApp{}
	l.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		l.mu.Lock()
		l.bodies = append(l.bodies, raw)
		l.sigs = append(l.sigs, r.Header.Get("Signature"))
		l.ids = append(l.ids, r.Header.Get("Prismaport-Event-Id"))
		l.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(l.srv.Close)
	return l
}

func (l *localApp) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.bodies)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	testutil.Eventually(t, 10*time.Second, what, cond)
}

// sequencer stands in for the worker: it sequences new events until stopped.
func sequencer(t *testing.T, app *apptest.App) func() {
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for {
			_, _ = app.Store.SequenceEvents(ctx, 100)
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
	return func() { cancel(); <-done }
}

// feedProxy stands between listen and the API and counts the feed reads that pass through it,
// so a test can wait for listen to have read the feed again instead of sleeping.
type feedProxy struct {
	srv   *httptest.Server
	reads atomic.Int64
}

func newFeedProxy(t *testing.T, api string) *feedProxy {
	t.Helper()
	target, err := url.Parse(api)
	if err != nil {
		t.Fatal(err)
	}
	p := &feedProxy{}
	rp := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) { r.SetURL(target) }}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/events" {
			p.reads.Add(1)
		}
		rp.ServeHTTP(w, r)
	}))
	t.Cleanup(p.srv.Close)
	return p
}

// waitForFullRead waits until a feed read that started after the call has finished: listen reads
// the feed, forwards what it got and only then reads again, so the second read to start marks the
// end of the first.
func (p *feedProxy) waitForFullRead(t *testing.T) {
	t.Helper()
	from := p.reads.Load()
	waitFor(t, "listen to read the feed again", func() bool { return p.reads.Load() >= from+2 })
}

func simulateAPI(t *testing.T, app *apptest.App, id, action string) {
	t.Helper()
	if r := app.Do("POST", "/v1/test_helpers/sessions/"+id+"/simulate", app.TestKey, map[string]any{"action": action}); r.Status != 200 {
		t.Fatalf("simulate %s: %d %s", action, r.Status, r.Body)
	}
}

func newSession(t *testing.T, app *apptest.App, key string) string {
	t.Helper()
	r := app.Session(key, map[string]any{"amount": "25.00"}, "Idempotency-Key", time.Now().Format(time.RFC3339Nano))
	if r.Status != 200 {
		t.Fatalf("session: %d %s", r.Status, r.Body)
	}
	return r.JSON(t)["id"].(string)
}

func TestListenForwardsNewEventsAndResumes(t *testing.T) {
	app := apptest.New(t, apptest.Options{})
	stop := sequencer(t, app)
	defer stop()
	local := newLocalApp(t)

	// History before listen starts is not forwarded.
	old := newSession(t, app, app.TestKey)
	simulateAPI(t, app, old, "pay_exact")
	waitFor(t, "the old events to be sequenced", func() bool { n, _ := app.Store.UnsequencedCount(t.Context()); return n == 0 })

	feed := newFeedProxy(t, app.Server.URL)
	ctx, cancel := context.WithCancel(t.Context())
	out := &syncBuffer{}
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"listen", "--api-key", app.TestKey, "--forward-to", local.srv.URL + "/webhooks",
			"--api-url", feed.srv.URL, "--events", "session.paid,payment.reversed", "--interval", "50ms"}, out)
	}()
	waitFor(t, "listen to start", func() bool { return strings.Contains(out.String(), "Press Ctrl+C") })
	secret := regexp.MustCompile(`Signing secret for this session: (whsec_\S+)`).FindStringSubmatch(out.String())
	if len(secret) != 2 {
		t.Fatalf("no secret printed:\n%s", out)
	}

	ses := newSession(t, app, app.TestKey)
	simulateAPI(t, app, ses, "pay_exact")            // payment_detected (filtered out) + paid
	simulateAPI(t, app, ses, "reverse_last_payment") // payment.reversed
	waitFor(t, "two forwarded events", func() bool { return local.count() == 2 })
	feed.waitForFullRead(t) // nothing else (the filtered payment_detected, a repeat) is forwarded
	if local.count() != 2 {
		t.Fatalf("forwarded %d events", local.count())
	}

	all := app.Do("GET", "/v1/events?limit=100", app.TestKey, nil)
	var page struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(all.Body, &page); err != nil {
		t.Fatal(err)
	}
	byID := map[string]string{}
	for _, raw := range page.Data {
		var e struct{ ID string }
		_ = json.Unmarshal(raw, &e)
		byID[e.ID] = string(raw)
	}
	local.mu.Lock()
	for i, body := range local.bodies {
		if err := webhooksig.Verify(body, local.sigs[i], secret[1], 0, time.Now()); err != nil {
			t.Fatalf("event %d: %v", i, err)
		}
		if byID[local.ids[i]] != string(body) {
			t.Fatalf("forwarded body differs from the feed for %s", local.ids[i])
		}
		if strings.Contains(string(body), old) {
			t.Fatal("an event from before listen started was forwarded")
		}
	}
	local.mu.Unlock()
	text := out.String()
	if strings.Count(text, "-> 200 OK") != 2 || !strings.Contains(text, "session.paid") || !strings.Contains(text, "payment.reversed") ||
		strings.Contains(text, "session.payment_detected") {
		t.Fatalf("output:\n%s", text)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	resume := regexp.MustCompile(`Resume with --after (\d+)`).FindStringSubmatch(out.String())
	if len(resume) != 2 {
		t.Fatalf("no resume cursor:\n%s", out)
	}

	// Events while it was stopped are forwarded when it resumes from its cursor.
	missed := newSession(t, app, app.TestKey)
	simulateAPI(t, app, missed, "pay_exact")
	ctx2, cancel2 := context.WithCancel(t.Context())
	out2 := &syncBuffer{}
	done2 := make(chan error, 1)
	go func() {
		done2 <- run(ctx2, []string{"listen", "--api-key", app.TestKey, "--forward-to", local.srv.URL, "--api-url", app.Server.URL,
			"--after", resume[1], "--interval", "50ms", "--secret", secret[1]}, out2)
	}()
	waitFor(t, "the missed events", func() bool { return local.count() == 4 })
	cancel2()
	if err := <-done2; err != nil {
		t.Fatal(err)
	}
	local.mu.Lock()
	for i := 2; i < 4; i++ {
		if !strings.Contains(string(local.bodies[i]), missed) || webhooksig.Verify(local.bodies[i], local.sigs[i], secret[1], 0, time.Now()) != nil {
			t.Fatalf("resumed event %d: %s", i, local.bodies[i])
		}
	}
	local.mu.Unlock()

	// A failing local app is reported and listen keeps going.
	local.srv.Close()
	ctx3, cancel3 := context.WithCancel(t.Context())
	out3 := &syncBuffer{}
	done3 := make(chan error, 1)
	go func() {
		done3 <- run(ctx3, []string{"listen", "--api-key", app.TestKey, "--forward-to", local.srv.URL, "--api-url", app.Server.URL, "--interval", "50ms"}, out3)
	}()
	waitFor(t, "listen to start", func() bool { return strings.Contains(out3.String(), "Press Ctrl+C") })
	simulateAPI(t, app, newSession(t, app, app.TestKey), "under_review")
	waitFor(t, "the failure line", func() bool { return strings.Contains(out3.String(), "-> error:") })
	cancel3()
	if err := <-done3; err != nil {
		t.Fatal(err)
	}
}

func TestListenRefusesLiveAndBadKeys(t *testing.T) {
	app := apptest.New(t, apptest.Options{})
	for _, key := range []string{app.LiveKey, "sk_test_" + strings.Repeat("1", 44)} {
		var out bytes.Buffer
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		err := run(ctx, []string{"listen", "--api-key", key, "--forward-to", "http://localhost:9/x", "--api-url", app.Server.URL}, &out)
		cancel()
		if err == nil {
			t.Fatalf("listen accepted %s", key[:8])
		}
	}
	// A test key without events:read is refused by the feed.
	_, noFeed, err := app.Svc.CreateAPIKey(t.Context(), app.MerchantID, false, []string{"sessions:read"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run(t.Context(), []string{"listen", "--api-key", noFeed, "--forward-to", "http://localhost:9/x", "--api-url", app.Server.URL}, &out); err == nil ||
		!strings.Contains(err.Error(), "insufficient_scope") {
		t.Fatalf("scope: %v", err)
	}
}

func TestTriggerProducesEachEvent(t *testing.T) {
	app := apptest.New(t, apptest.Options{})
	sessionID := regexp.MustCompile(`Created test session (ses_[0-9A-Z]{26})`)
	for event, status := range map[string]string{"session.payment_detected": "paid", "session.partially_paid": "partially_paid",
		"session.paid": "paid", "session.overpaid": "overpaid", "session.expired": "expired", "session.late_payment": "late_payment",
		"session.under_review": "under_review", "payment.reversed": "open"} {
		t.Run(event, func(t *testing.T) {
			var out bytes.Buffer
			if err := run(t.Context(), []string{"trigger", event, "--api-key", app.TestKey, "--api-url", app.Server.URL}, &out); err != nil {
				t.Fatalf("%s: %v\n%s", event, err, out.String())
			}
			m := sessionID.FindStringSubmatch(out.String())
			if len(m) != 2 {
				t.Fatalf("%s output:\n%s", event, out.String())
			}
			ses, err := app.Store.LoadSession(t.Context(), m[1])
			if err != nil {
				t.Fatal(err)
			}
			evs, _ := app.Store.SessionEvents(t.Context(), m[1])
			found := false
			for _, e := range evs {
				found = found || e.Type == event
			}
			if !found || string(ses.State.Status) != status || ses.Livemode || ses.Metadata["source"] != "prismaport trigger" ||
				!strings.Contains(out.String(), event) {
				t.Errorf("%s: status %s events %v\n%s", event, ses.State.Status, evs, out.String())
			}
			for _, p := range ses.State.Payments {
				if !p.Simulated || !strings.HasPrefix(p.TxHash, service.SimulatedHashPrefix) {
					t.Errorf("%s: payment %+v", event, p)
				}
			}
		})
	}
	var settled bytes.Buffer
	if err := run(t.Context(), []string{"trigger", "session.paid", "--settle", "--api-key", app.TestKey, "--api-url", app.Server.URL}, &settled); err != nil {
		t.Fatalf("trigger --settle: %v\n%s", err, settled.String())
	}
	for _, want := range []string{`"gross": "25.000000"`, `"fee": "0.225000"`, `"payouts": [`, `"amount": "24.775000"`, `"simulated": true`,
		`"address": "` + apptest.EVMDestination + `"`} {
		if !strings.Contains(settled.String(), want) {
			t.Fatalf("trigger --settle output lacks %s:\n%s", want, settled.String())
		}
	}
	var out bytes.Buffer
	if err := run(t.Context(), []string{"trigger", "session.paid", "--api-key", app.LiveKey, "--api-url", app.Server.URL}, &out); err == nil {
		t.Fatal("trigger accepted a live key")
	}
	var live int
	if err := app.Store.Pool().QueryRow(t.Context(), `SELECT count(*) FROM sessions WHERE livemode`).Scan(&live); err != nil || live != 0 {
		t.Fatalf("live sessions: %d %v", live, err)
	}
}

// The full developer loop: listen running, trigger an event, the local app gets it signed.
func TestTriggerReachesListen(t *testing.T) {
	app := apptest.New(t, apptest.Options{})
	stop := sequencer(t, app)
	defer stop()
	local := newLocalApp(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	out := &syncBuffer{}
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"listen", "--api-key", app.TestKey, "--forward-to", local.srv.URL, "--api-url", app.Server.URL,
			"--events", "session.overpaid", "--interval", "50ms", "--secret", "whsec_stable"}, out)
	}()
	waitFor(t, "listen to start", func() bool { return strings.Contains(out.String(), "Press Ctrl+C") })
	var trig bytes.Buffer
	if err := run(t.Context(), []string{"trigger", "session.overpaid", "--api-key", app.TestKey, "--api-url", app.Server.URL}, &trig); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the overpaid event", func() bool { return local.count() == 1 })
	local.mu.Lock()
	body, sig := local.bodies[0], local.sigs[0]
	local.mu.Unlock()
	if !strings.Contains(string(body), `"type":"session.overpaid"`) || webhooksig.Verify(body, sig, "whsec_stable", 0, time.Now()) != nil {
		t.Fatalf("forwarded: %s", body)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
