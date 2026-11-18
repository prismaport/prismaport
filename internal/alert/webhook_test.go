package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/prismaport/prismaport/internal/testutil"
	"github.com/prismaport/prismaport/internal/webhooksig"
)

type received struct {
	body      []byte
	signature string
}

type receiver struct {
	srv  *httptest.Server
	got  chan received
	mu   sync.Mutex
	wait time.Duration
}

func newReceiver(t *testing.T) *receiver {
	r := &receiver{got: make(chan received, 10)}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		wait := r.wait
		r.mu.Unlock()
		if wait > 0 {
			select {
			case <-time.After(wait):
			case <-req.Context().Done():
				return
			}
		}
		body, _ := io.ReadAll(req.Body)
		r.got <- received{body: body, signature: req.Header.Get(webhooksig.HeaderName)}
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *receiver) next(t *testing.T) received {
	t.Helper()
	select {
	case g := <-r.got:
		return g
	case <-time.After(5 * time.Second):
		t.Fatal("no alert posted")
		return received{}
	}
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func run(t *testing.T, w *Webhook) {
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
}

func TestWebhookPostsASignedSlackAndDiscordCompatibleBody(t *testing.T) {
	r := newReceiver(t)
	clk := &clock{t: time.Now()}
	w := NewWebhook(WebhookConfig{URL: r.srv.URL, Secret: "s3cret", Now: clk.now})
	run(t, w)

	w.Alert(t.Context(), Alert{Severity: Critical, Chain: "base", Kind: "chain_halted", Message: "providers disagree",
		Fields: map[string]string{"b": "2", "a": "1"}})
	got := r.next(t)

	if err := webhooksig.Verify(got.body, got.signature, "s3cret", webhooksig.DefaultTolerance, clk.now()); err != nil {
		t.Fatalf("signature does not verify: %v (%q)", err, got.signature)
	}
	if err := webhooksig.Verify(got.body, got.signature, "other", webhooksig.DefaultTolerance, clk.now()); err == nil {
		t.Fatal("signature verifies with the wrong secret")
	}
	var p payload
	if err := json.Unmarshal(got.body, &p); err != nil {
		t.Fatal(err)
	}
	want := "[critical] chain_halted on base: providers disagree a=1 b=2"
	if p.Text != want || p.Content != want {
		t.Fatalf("text %q content %q, want %q", p.Text, p.Content, want)
	}
	if p.Kind != "chain_halted" || p.Chain != "base" || p.Severity != Critical || p.Fields["a"] != "1" || p.Suppressed != 0 {
		t.Fatalf("payload %+v", p)
	}
}

func TestWebhookWithoutSecretSendsNoSignature(t *testing.T) {
	r := newReceiver(t)
	w := NewWebhook(WebhookConfig{URL: r.srv.URL})
	run(t, w)

	w.Alert(t.Context(), Alert{Severity: Warning, Kind: "webhook_dead_lettered"})

	if got := r.next(t); got.signature != "" {
		t.Fatalf("signature %q without a secret", got.signature)
	}
}

func TestWebhookHoldsBackRepeatsOfAKindAndChainWithinTheWindow(t *testing.T) {
	r := newReceiver(t)
	clk := &clock{t: time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)}
	w := NewWebhook(WebhookConfig{URL: r.srv.URL, Window: 10 * time.Minute, Now: clk.now})
	run(t, w)
	halted := func(chain string) Alert { return Alert{Severity: Critical, Chain: chain, Kind: "chain_halted"} }

	w.Alert(t.Context(), halted("base"))
	first := r.next(t)
	clk.advance(time.Minute)
	w.Alert(t.Context(), halted("base"))
	w.Alert(t.Context(), halted("base"))
	// One sender posts the queue in order: a base repeat let through would arrive before solana.
	w.Alert(t.Context(), halted("solana"))
	other := r.next(t)
	clk.advance(10 * time.Minute)
	w.Alert(t.Context(), halted("base"))
	after := r.next(t)

	var p1, p2, p3 payload
	_ = json.Unmarshal(first.body, &p1)
	_ = json.Unmarshal(other.body, &p2)
	_ = json.Unmarshal(after.body, &p3)
	if p1.Chain != "base" || p1.Suppressed != 0 {
		t.Fatalf("first %+v", p1)
	}
	if p2.Chain != "solana" || p2.Suppressed != 0 {
		t.Fatalf("another chain was held back with base: %+v", p2)
	}
	if p3.Chain != "base" || p3.Suppressed != 2 || !strings.Contains(p3.Text, "(2 more held back since the last one)") {
		t.Fatalf("after the window %+v", p3)
	}
}

func TestWebhookAlertNeverWaitsForASlowReceiver(t *testing.T) {
	r := newReceiver(t)
	r.wait = 2 * time.Second
	w := NewWebhook(WebhookConfig{URL: r.srv.URL, Timeout: 100 * time.Millisecond, Queue: 1})

	start := time.Now()
	for i := range 50 {
		w.Alert(t.Context(), Alert{Severity: Warning, Kind: "k" + string(rune('a'+i%26)) + string(rune('a'+i/26))})
	}
	queued := time.Since(start)
	sent := time.Now()
	err := w.post(t.Context(), <-w.queue)

	if queued > time.Second {
		t.Fatalf("Alert blocked for %v", queued)
	}
	if err == nil || time.Since(sent) > time.Second {
		t.Fatalf("post to a slow receiver: err %v after %v, want a timeout near 100ms", err, time.Since(sent))
	}
}

func TestWebhookReportsANon2xxAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer srv.Close()
	w := NewWebhook(WebhookConfig{URL: srv.URL})

	err := w.post(t.Context(), payload{Kind: "x"})

	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err %v, want HTTP 403", err)
	}
}

// The webhook URL carries the receiver's token (Discord and Slack put it in the path): a failed
// POST must not write it to the log.
func TestWebhookFailureDoesNotLogTheURL(t *testing.T) {
	// The receiver hangs up without an answer, so the client fails with a *url.Error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			conn.Close()
		}
	}))
	t.Cleanup(srv.Close)
	hook := srv.URL + "/api/webhooks/123/s3cr3t-t0ken"
	logs := &syncBuffer{}
	w := NewWebhook(WebhookConfig{URL: hook, Attempts: 1, Log: slog.New(slog.NewTextHandler(logs, nil))})

	// The queued alert is handled here, as Run would, so no wait on a background loop can time
	// out on a loaded machine.
	w.Alert(t.Context(), Alert{Severity: Critical, Kind: "chain_halted"})
	select {
	case p := <-w.queue:
		w.handle(t.Context(), p)
	default:
		t.Fatal("the alert was not queued")
	}
	logged := logs.String()

	if !strings.Contains(logged, "alert webhook failed") || strings.Contains(logged, "s3cr3t-t0ken") || !strings.Contains(logged, "err=\"Post: ") {
		t.Fatalf("log %s", logged)
	}
	bad := NewWebhook(WebhookConfig{URL: "http://h/s3cr3t-t0ken\x7f", Attempts: 1})
	if err := bad.post(t.Context(), payload{Kind: "x"}); err == nil || strings.Contains(err.Error(), "s3cr3t-t0ken") {
		t.Fatalf("invalid URL err %v", err)
	}
}

// syncBuffer is a log destination the test reads while Run writes to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// String returns what was logged so far.
func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitFor returns the log once it contains s.
func (b *syncBuffer) waitFor(t *testing.T, s string) string {
	t.Helper()
	var out string
	defer func() {
		if t.Failed() {
			t.Logf("log: %s", out)
		}
	}()
	testutil.Eventually(t, 10*time.Second, fmt.Sprintf("%q in the log", s), func() bool {
		b.mu.Lock()
		out = b.buf.String()
		b.mu.Unlock()
		return strings.Contains(out, s)
	})
	return out
}

// failing answers 500 to the first fails posts and 200 after, recording every body it got.
type failing struct {
	srv   *httptest.Server
	fails atomic.Int32
	got   chan payload
}

func newFailing(t *testing.T, fails int32) *failing {
	f := &failing{got: make(chan payload, 10)}
	f.fails.Store(fails)
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if f.fails.Add(-1) >= 0 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var p payload
		_ = json.NewDecoder(req.Body).Decode(&p)
		f.got <- p
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *failing) next(t *testing.T) payload {
	t.Helper()
	select {
	case p := <-f.got:
		return p
	case <-time.After(5 * time.Second):
		t.Fatal("no alert delivered")
		return payload{}
	}
}

// A POST that fails is retried; the alert counts as sent once one attempt succeeds.
func TestWebhookRetriesAFailedPost(t *testing.T) {
	f := newFailing(t, 2)
	w := NewWebhook(WebhookConfig{URL: f.srv.URL, RetryWait: time.Millisecond})
	run(t, w)

	w.Alert(t.Context(), Alert{Severity: Critical, Chain: "base", Kind: "gas_wallet_empty"})

	if p := f.next(t); p.Kind != "gas_wallet_empty" {
		t.Fatalf("delivered %+v", p)
	}
}

// An alert whose every attempt failed does not mute its repeats for the window: the next one
// goes out at once and reports the lost one as held back.
func TestWebhookFailedAlertDoesNotMuteItsRepeats(t *testing.T) {
	f := newFailing(t, 1)
	clk := &clock{t: time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)}
	logs := &syncBuffer{}
	w := NewWebhook(WebhookConfig{URL: f.srv.URL, Window: 15 * time.Minute, Attempts: 1, Now: clk.now,
		Log: slog.New(slog.NewTextHandler(logs, nil))})
	run(t, w)
	empty := Alert{Severity: Critical, Chain: "base", Kind: "gas_wallet_empty"}

	w.Alert(t.Context(), empty)
	logs.waitFor(t, "alert webhook failed")
	clk.advance(time.Minute)
	w.Alert(t.Context(), empty)

	if p := f.next(t); p.Kind != "gas_wallet_empty" || p.Suppressed != 1 {
		t.Fatalf("repeat after a failed post %+v", p)
	}
	clk.advance(time.Minute)
	w.Alert(t.Context(), empty) // delivered a minute ago: held back now
	// One sender posts the queue in order: a repeat let through would arrive before this one.
	w.Alert(t.Context(), Alert{Severity: Warning, Chain: "base", Kind: "loop_failing"})
	if p := f.next(t); p.Kind != "loop_failing" {
		t.Fatalf("a repeat inside the window was sent: %+v", p)
	}
}

// An alert dropped because the queue is full does not mute its repeats either.
func TestWebhookDroppedAlertDoesNotMuteItsRepeats(t *testing.T) {
	w := NewWebhook(WebhookConfig{URL: "http://unused", Queue: 1})
	w.Alert(t.Context(), Alert{Kind: "loop_failing", Chain: "solana"})   // fills the queue
	w.Alert(t.Context(), Alert{Kind: "gas_wallet_empty", Chain: "base"}) // dropped
	if p := <-w.queue; p.Kind != "loop_failing" {
		t.Fatalf("queued %+v", p)
	}

	w.Alert(t.Context(), Alert{Kind: "gas_wallet_empty", Chain: "base"})

	select {
	case p := <-w.queue:
		if p.Kind != "gas_wallet_empty" || p.Suppressed != 1 {
			t.Fatalf("queued %+v", p)
		}
	default:
		t.Fatal("the repeat of a dropped alert was held back")
	}
}

func TestWebhookTextIsCutOnARuneBoundary(t *testing.T) {
	p := payload{Severity: Info, Kind: "k", Message: strings.Repeat("é", maxText)}

	got := text(p)

	if !strings.HasSuffix(got, "…") || len(got) > maxText+len("…") || !strings.HasPrefix(got, "[info] k: é") {
		t.Fatalf("text %d bytes: %q...", len(got), got[:20])
	}
	if !utf8.ValidString(got) {
		t.Fatal("text was cut inside a rune")
	}
}

func TestFanoutSendsToEverySink(t *testing.T) {
	a, b := &Recorder{}, &Recorder{}

	Fanout{a, b}.Alert(t.Context(), Alert{Kind: "gas_wallet_empty"})

	if !a.Has("gas_wallet_empty") || !b.Has("gas_wallet_empty") {
		t.Fatalf("a %v b %v", a.Kinds(), b.Kinds())
	}
}

func TestSustainedRaisesOnceAfterForAndClearsWhenItEnds(t *testing.T) {
	s := Sustained{For: 5 * time.Minute}
	t0 := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	type step struct {
		active         bool
		at             time.Duration
		raise, cleared bool
	}
	steps := []step{
		{active: false, at: 0},
		{active: true, at: time.Minute},
		{active: true, at: 5 * time.Minute},
		{active: true, at: 6 * time.Minute, raise: true},
		{active: true, at: 20 * time.Minute},
		{active: false, at: 21 * time.Minute, cleared: true},
		{active: false, at: 22 * time.Minute},
		{active: true, at: 23 * time.Minute},
		{active: false, at: 24 * time.Minute},
		{active: true, at: 25 * time.Minute},
		{active: true, at: 30 * time.Minute, raise: true},
	}
	for i, st := range steps {
		raise, cleared := s.Update(st.active, t0.Add(st.at))
		if raise != st.raise || cleared != st.cleared {
			t.Fatalf("step %d (%+v): raise %v cleared %v", i, st, raise, cleared)
		}
	}
}
