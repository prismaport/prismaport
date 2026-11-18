package service

import (
	"fmt"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/dashauth"
	"github.com/prismaport/prismaport/internal/store"
)

func TestEnrollmentOpen(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	mine := [32]byte{1}
	other := [32]byte{2}
	pending := store.DashboardUser{TOTPPending: []byte{9}, TOTPPendingSession: mine[:], TOTPPendingExpiresAt: ptr(now.Add(time.Minute))}
	cases := []struct {
		name string
		u    store.DashboardUser
		hash [32]byte
		at   time.Time
		want bool
	}{
		{"started by this session, in time", pending, mine, now, true},
		{"another session", pending, other, now, false},
		{"at the expiry", pending, mine, now.Add(time.Minute), false},
		{"after the expiry", pending, mine, now.Add(time.Hour), false},
		{"nothing pending", store.DashboardUser{}, mine, now, false},
		{"pending without a session", store.DashboardUser{TOTPPending: []byte{9}, TOTPPendingExpiresAt: ptr(now.Add(time.Minute))}, mine, now, false},
		{"pending without an expiry", store.DashboardUser{TOTPPending: []byte{9}, TOTPPendingSession: mine[:]}, mine, now, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := enrollmentOpen(c.u, c.hash, c.at); got != c.want {
				t.Errorf("%s: %v, want %v", c.name, got, c.want)
			}
		})
	}
}

func TestTOTPDelay(t *testing.T) {
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{1, 0},
		{4, 0},
		{5, time.Minute},
		{6, 2 * time.Minute},
		{7, 4 * time.Minute},
		{8, 8 * time.Minute},
		{9, 15 * time.Minute},
		{40, 15 * time.Minute},
		{1 << 20, 15 * time.Minute},
	}
	for _, c := range cases {
		if got := totpDelay(c.attempt); got != c.want {
			t.Errorf("after %d wrong codes: wait %s, want %s", c.attempt, got, c.want)
		}
	}
}

// The owner hears of a run of failures at its start, again at five times that, then now and then.
func TestFailureNotice(t *testing.T) {
	var got []int
	for n := range 401 {
		if failureNotice(n, TOTPFreeFailures) {
			got = append(got, n)
		}
	}
	if fmt.Sprint(got) != "[5 25 100 200 300 400]" {
		t.Fatalf("two-factor notices at %v", got)
	}
	got = nil
	for n := range 401 {
		if failureNotice(n, PasswordNoticeFailures) {
			got = append(got, n)
		}
	}
	if fmt.Sprint(got) != "[10 50 200 400]" {
		t.Fatalf("password notices at %v", got)
	}
}

func TestTOTPWait(t *testing.T) {
	e := totpWait(61 * time.Second)
	if e.Kind != KindRateLimited || e.Code != CodeRateLimited || e.RetryAfter != 61*time.Second ||
		e.Message != "Too many wrong codes. Wait 2 minutes before entering another code." {
		t.Fatalf("wait: %+v", e)
	}
	if m := totpWait(30 * time.Second).Message; m != "Too many wrong codes. Wait 1 minute before entering another code." {
		t.Fatalf("one minute: %s", m)
	}
}

// ADR 0008 C-2: a password hash refused because every hashing slot is taken is a 503 with
// Retry-After, wrapped or not, never a 500.
func TestBusyPasswordHashIs503(t *testing.T) {
	for _, err := range []error{dashauth.ErrBusy, fmt.Errorf("login: %w", dashauth.ErrBusy)} {
		e := AsError(err)
		if e.Kind != KindUnavailable || e.Code != CodeServiceUnavailable || e.RetryAfter <= 0 {
			t.Fatalf("%v: %+v", err, e)
		}
	}
}
