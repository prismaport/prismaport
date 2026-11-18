// Package alert is the alerting hook used by the chain loops and the worker. Log writes
// alerts to the process log; Webhook posts them to a chat or pager endpoint.
package alert

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Severity ranks how urgent an alert is.
type Severity string

// The alert severities, from least to most urgent.
const (
	Info     Severity = "info"
	Warning  Severity = "warning"
	Critical Severity = "critical"
)

// Alert is one condition that needs attention. Kind is a stable machine-readable name.
type Alert struct {
	Severity Severity
	Chain    string
	Kind     string
	Message  string
	Fields   map[string]string
}

// Sink receives alerts; implementations must not block the caller for long.
type Sink interface {
	Alert(ctx context.Context, a Alert)
}

// Log writes alerts to a structured logger; a nil Logger discards them (the process passes its
// own, never the global default).
type Log struct{ Logger *slog.Logger }

// Alert writes the alert to the logger at a level matching its severity.
func (l Log) Alert(ctx context.Context, a Alert) {
	log := l.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	level := slog.LevelInfo
	switch a.Severity {
	case Warning:
		level = slog.LevelWarn
	case Critical:
		level = slog.LevelError
	}
	attrs := []any{"alert", a.Kind, "chain", a.Chain}
	for k, v := range a.Fields {
		attrs = append(attrs, k, v)
	}
	log.Log(ctx, level, a.Message, attrs...)
}

// Fanout sends every alert to each of its sinks in order.
type Fanout []Sink

// Alert passes the alert to every sink of the fanout in order.
func (f Fanout) Alert(ctx context.Context, a Alert) {
	for _, s := range f {
		s.Alert(ctx, a)
	}
}

// Sustained tracks a condition that is worth an alert only once it has held for For: Update
// reports raise the first time it has, and cleared when it ends after that.
type Sustained struct {
	For    time.Duration
	since  time.Time
	raised bool
}

// Update records whether the condition holds at now and reports whether to raise the alert or
// announce that it cleared.
func (s *Sustained) Update(active bool, now time.Time) (raise, cleared bool) {
	if !active {
		cleared = s.raised
		s.since, s.raised = time.Time{}, false
		return false, cleared
	}
	if s.since.IsZero() {
		s.since = now
	}
	if !s.raised && now.Sub(s.since) >= s.For {
		s.raised = true
		return true, false
	}
	return false, false
}

// Recorder keeps alerts in memory; tests use it.
type Recorder struct {
	mu     sync.Mutex
	alerts []Alert
}

// Alert records the alert in memory.
func (r *Recorder) Alert(_ context.Context, a Alert) {
	r.mu.Lock()
	r.alerts = append(r.alerts, a)
	r.mu.Unlock()
}

// All returns a copy of the alerts recorded so far, in order.
func (r *Recorder) All() []Alert {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Alert(nil), r.alerts...)
}

// Kinds lists the kinds raised so far, in order.
func (r *Recorder) Kinds() []string {
	var out []string
	for _, a := range r.All() {
		out = append(out, a.Kind)
	}
	return out
}

// Has reports whether an alert of the given kind was recorded.
func (r *Recorder) Has(kind string) bool {
	for _, a := range r.All() {
		if a.Kind == kind {
			return true
		}
	}
	return false
}
