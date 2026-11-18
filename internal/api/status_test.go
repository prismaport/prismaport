package api

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prismaport/prismaport/internal/service"
)

// Every service error kind has its HTTP status; an unknown one is a server error.
func TestKindStatuses(t *testing.T) {
	want := map[service.Kind]int{
		service.KindInternal: 500, service.KindInvalid: 400, service.KindUnauthorized: 401, service.KindForbidden: 403,
		service.KindNotFound: 404, service.KindConflict: 409, service.KindGone: 410, service.KindRateLimited: 429,
		service.KindUnavailable: 503, service.Kind("bogus"): 500,
	}
	for k, status := range want {
		if got := httpStatus(k); got != status {
			t.Errorf("kind %q: %d, want %d", k, got, status)
		}
	}
}

// A transport refusal keeps its own status and answers its wrapped error's code, also when
// wrapped again.
func TestTransportRefusal(t *testing.T) {
	s := &Server{log: slog.New(slog.DiscardHandler)}
	err := fmt.Errorf("reading: %w", refuse(http.StatusRequestEntityTooLarge, "too big"))
	status, e := s.errorOf(httptest.NewRecorder(), httptest.NewRequest("POST", "/", nil), err)
	if status != http.StatusRequestEntityTooLarge || e.Code != service.CodeInvalidRequest || e.Message != "too big" {
		t.Fatalf("refusal: %d %+v", status, e)
	}
	if status, _ := s.errorOf(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil), errors.New("boom")); status != 500 {
		t.Fatalf("unknown error: %d", status)
	}
}
