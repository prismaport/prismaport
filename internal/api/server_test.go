package api

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/service"
)

// A server error's cause reaches the log, while the client body stays the error's own message.
func TestServerErrorLogsItsCause(t *testing.T) {
	var buf bytes.Buffer
	s := &Server{log: slog.New(slog.NewTextHandler(&buf, nil))}
	r := httptest.NewRequest(http.MethodPost, "/v1/x", nil)
	w := httptest.NewRecorder()
	s.writeError(w, r, &service.Error{Kind: service.KindUnavailable, Code: service.CodeServiceUnavailable,
		Message: "temporarily unavailable", Cause: errors.New("gateway treasury mismatch")})
	if !strings.Contains(buf.String(), "gateway treasury mismatch") {
		t.Fatalf("log %q", buf.String())
	}
	if strings.Contains(w.Body.String(), "mismatch") || w.Code != http.StatusServiceUnavailable {
		t.Fatalf("response %d %s", w.Code, w.Body.String())
	}
}

// Every request runs under a deadline, so a handler stuck on a hung call ends.
func TestRequestsRunUnderADeadline(t *testing.T) {
	s := &Server{log: slog.New(slog.DiscardHandler), mux: http.NewServeMux(), requestTimeout: 20 * time.Millisecond}
	s.mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			w.WriteHeader(http.StatusGatewayTimeout)
		case <-time.After(5 * time.Second):
			w.WriteHeader(http.StatusOK)
		}
	})
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/slow", nil))
	if w.Code != http.StatusGatewayTimeout {
		t.Fatalf("status %d", w.Code)
	}
	if New(nil, nil, nil).requestTimeout != RequestTimeout {
		t.Fatal("the server has no request deadline")
	}
}

// Close waits for the background work and starts none after it.
func TestCloseWaitsForBackgroundWork(t *testing.T) {
	b := newBackground()
	started, finished := make(chan struct{}), make(chan struct{})
	b.Go(func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		close(finished)
	})
	<-started
	b.Close()
	select {
	case <-finished:
	default:
		t.Fatal("Close returned before the background work did")
	}
	ran := false
	b.Go(func(context.Context) { ran = true })
	b.Close()
	if ran {
		t.Fatal("work started after Close")
	}
}
