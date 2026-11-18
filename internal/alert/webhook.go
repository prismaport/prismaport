package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/prismaport/prismaport/internal/webhooksig"
)

// WebhookConfig configures the HTTP alert sink.
type WebhookConfig struct {
	URL string
	// Secret, when set, signs every POST with the same Signature header as merchant webhooks.
	Secret string
	// Window is how long further alerts of one kind on one chain are held back after one was
	// sent; the next one sent reports how many were held back (default 15 minutes).
	Window time.Duration
	// Timeout bounds one POST (default 5 seconds).
	Timeout time.Duration
	// Queue bounds the alerts waiting to be sent; an alert that finds it full is dropped and
	// logged (default 100).
	Queue int
	// Attempts is how many times one alert is posted before it is given up (default 3), waiting
	// RetryWait (default 2 seconds), then twice that, and so on, between attempts.
	Attempts  int
	RetryWait time.Duration
	Client    *http.Client
	Log       *slog.Logger
	Now       func() time.Time
}

// Webhook posts alerts as signed JSON to one URL. Alert never waits on the network: it queues
// the alert and Run sends it, so a slow or unreachable receiver cannot hold up a chain loop.
type Webhook struct {
	cfg   WebhookConfig
	queue chan payload

	mu sync.Mutex
	// last is when an alert of the key was last delivered; sending holds the keys with an alert
	// queued or being posted. Repeats are held back only behind an alert that is on its way or
	// was delivered, so a dropped or failed alert never mutes the ones after it.
	last    map[dedupKey]time.Time
	sending map[dedupKey]bool
	held    map[dedupKey]int
}

type dedupKey struct{ kind, chain string }

type payload struct {
	// Text is read by Slack, Mattermost and Discord's /slack endpoint; Content by Discord.
	Text       string            `json:"text"`
	Content    string            `json:"content"`
	Severity   Severity          `json:"severity"`
	Kind       string            `json:"kind"`
	Chain      string            `json:"chain,omitempty"`
	Message    string            `json:"message"`
	Fields     map[string]string `json:"fields,omitempty"`
	Suppressed int               `json:"suppressed"`
	At         time.Time         `json:"at"`
}

const maxText = 1900

// NewWebhook returns a webhook sink, filling unset config fields with their defaults.
func NewWebhook(cfg WebhookConfig) *Webhook {
	if cfg.Window == 0 {
		cfg.Window = 15 * time.Minute
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Second
	}
	if cfg.Queue == 0 {
		cfg.Queue = 100
	}
	if cfg.Attempts == 0 {
		cfg.Attempts = 3
	}
	if cfg.RetryWait == 0 {
		cfg.RetryWait = 2 * time.Second
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Webhook{cfg: cfg, queue: make(chan payload, cfg.Queue), last: map[dedupKey]time.Time{}, sending: map[dedupKey]bool{},
		held: map[dedupKey]int{}}
}

// Alert queues the alert for posting, holding back repeats of the same kind and chain inside the
// window; the next post counts them.
func (w *Webhook) Alert(_ context.Context, a Alert) {
	now := w.cfg.Now()
	k := dedupKey{a.Kind, a.Chain}
	w.mu.Lock()
	if t, ok := w.last[k]; w.sending[k] || ok && now.Sub(t) < w.cfg.Window {
		w.held[k]++
		w.mu.Unlock()
		return
	}
	held := w.held[k]
	w.sending[k], w.held[k] = true, 0
	w.mu.Unlock()
	p := payload{Severity: a.Severity, Kind: a.Kind, Chain: a.Chain, Message: a.Message, Fields: a.Fields, Suppressed: held, At: now.UTC()}
	p.Text = text(p)
	p.Content = p.Text
	select {
	case w.queue <- p:
	default:
		w.cfg.Log.Warn("alert webhook queue full; alert dropped", "alert", a.Kind, "chain", a.Chain)
		w.done(p, false)
	}
}

// done ends the sending of p. Delivered, further alerts of its kind and chain are held back for
// Window from now; not, p and the alerts it reported count as held back, and the next alert of
// its kind and chain goes out at once.
func (w *Webhook) done(p payload, delivered bool) {
	k := dedupKey{p.Kind, p.Chain}
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.sending, k)
	if delivered {
		w.last[k] = w.cfg.Now()
		return
	}
	w.held[k] += p.Suppressed + 1
}

// Run sends queued alerts until ctx ends.
func (w *Webhook) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case p := <-w.queue:
			w.handle(ctx, p)
		}
	}
}

// handle sends one queued alert and logs it when every attempt failed.
func (w *Webhook) handle(ctx context.Context, p payload) {
	err := w.send(ctx, p)
	w.done(p, err == nil)
	if err != nil && ctx.Err() == nil {
		w.cfg.Log.Warn("alert webhook failed", "alert", p.Kind, "chain", p.Chain, "attempts", w.cfg.Attempts, "err", err)
	}
}

// send posts p up to Attempts times. A panic while posting fails the alert, not the worker.
func (w *Webhook) send(ctx context.Context, p payload) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	wait := w.cfg.RetryWait
	for attempt := 1; ; attempt++ {
		if err = w.post(ctx, p); err == nil || attempt >= w.cfg.Attempts {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(wait):
		}
		wait *= 2
	}
}

func (w *Webhook) post(ctx context.Context, p payload) error {
	body, err := json.Marshal(p)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, w.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return withoutURL(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Prismaport-Alerts/1")
	if w.cfg.Secret != "" {
		req.Header.Set(webhooksig.HeaderName, webhooksig.Header(w.cfg.Now(), body, w.cfg.Secret))
	}
	resp, err := w.cfg.Client.Do(req)
	if err != nil {
		return withoutURL(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// withoutURL drops the URL a *url.Error names: a chat webhook's URL carries its token.
func withoutURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s: %w", ue.Op, ue.Err)
	}
	return err
}

func text(p payload) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[%s] %s", p.Severity, p.Kind)
	if p.Chain != "" {
		fmt.Fprintf(&b, " on %s", p.Chain)
	}
	if p.Message != "" {
		fmt.Fprintf(&b, ": %s", p.Message)
	}
	keys := make([]string, 0, len(p.Fields))
	for k := range p.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, " %s=%s", k, p.Fields[k])
	}
	if p.Suppressed > 0 {
		fmt.Fprintf(&b, " (%d more held back since the last one)", p.Suppressed)
	}
	s := b.String()
	if len(s) > maxText {
		i := maxText
		for i > 0 && !utf8.RuneStart(s[i]) {
			i--
		}
		s = s[:i] + "…"
	}
	return s
}
