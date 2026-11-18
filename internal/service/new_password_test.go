package service

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

type fakeBreaches struct {
	found bool
	err   error
	calls int
}

func (f *fakeBreaches) Breached(context.Context, string) (bool, error) {
	f.calls++
	return f.found, f.err
}

func TestCheckNewPassword(t *testing.T) {
	const ok = "Tr4vel-the-river"
	cases := []struct {
		name  string
		pw    string
		found bool
		err   error
		code  string
		calls int
	}{
		{name: "accepted", pw: ok, calls: 1},
		{name: "breached", pw: ok, found: true, code: CodeBreachedPassword, calls: 1},
		{name: "too short skips the breach check", pw: "Aa1!", code: CodeWeakPassword},
		{name: "no class skips the breach check", pw: "travel-the-river", code: CodeWeakPassword},
		{name: "common skips the breach check", pw: "Password123!", code: CodeCommonPassword},
		{name: "repeated skips the breach check", pw: "Aa1!Aa1!", code: CodeCommonPassword},
		{name: "a failed check accepts the password", pw: ok, err: errors.New("pwned: range answered 503"), calls: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := &fakeBreaches{found: tc.found, err: tc.err}
			s := &Service{cfg: Config{PasswordBreaches: b}}
			err := s.checkNewPassword(t.Context(), tc.pw, "new_password")
			if b.calls != tc.calls {
				t.Fatalf("breach check ran %d times, want %d", b.calls, tc.calls)
			}
			if tc.code == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			var e *Error
			if !errors.As(err, &e) || e.Code != tc.code || e.Kind != KindInvalid || e.Param != "new_password" || e.Message == "" {
				t.Fatalf("err = %#v, want %s", err, tc.code)
			}
		})
	}
}

func TestCheckNewPasswordWithoutAChecker(t *testing.T) {
	s := &Service{}
	if err := s.checkNewPassword(t.Context(), "Tr4vel-the-river", "password"); err != nil {
		t.Fatal(err)
	}
}

func TestFailedBreachCheckWarnsWithoutThePassword(t *testing.T) {
	const pw = "Tr4vel-the-river"
	var logs bytes.Buffer
	s := &Service{cfg: Config{
		PasswordBreaches: &fakeBreaches{err: errors.New("pwned: context deadline exceeded")},
		Log:              slog.New(slog.NewTextHandler(&logs, nil)),
	}}
	if err := s.checkNewPassword(t.Context(), pw, "password"); err != nil {
		t.Fatalf("a failed check must accept the password: %v", err)
	}
	out := logs.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "accepting the password") {
		t.Fatalf("no warning: %q", out)
	}
	if strings.Contains(out, pw) {
		t.Fatalf("the password was logged: %q", out)
	}
}
