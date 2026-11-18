package config

import (
	"strings"
	"testing"

	"github.com/prismaport/prismaport/internal/mail"
)

func mailEnv(t *testing.T, vars map[string]string) {
	t.Helper()
	t.Setenv("PRISMAPORT_DATABASE_URL", "postgres://x")
	for _, k := range []string{"PRISMAPORT_SMTP_HOST", "PRISMAPORT_SMTP_PORT", "PRISMAPORT_SMTP_TLS", "PRISMAPORT_SMTP_USERNAME",
		"PRISMAPORT_SMTP_PASSWORD", "PRISMAPORT_MAIL_FROM", "PRISMAPORT_RESEND_API_KEY", "PRISMAPORT_RESEND_BASE_URL"} {
		t.Setenv(k, vars[k])
	}
}

func TestLoadPicksResend(t *testing.T) {
	mailEnv(t, map[string]string{"PRISMAPORT_RESEND_API_KEY": "re_test_123", "PRISMAPORT_MAIL_FROM": "Prismaport <no-reply@example.com>"})
	e, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if e.SMTP != nil || e.Resend == nil || e.Resend.APIKey != "re_test_123" || e.Resend.BaseURL != mail.ResendBaseURL {
		t.Fatalf("smtp %+v, resend %+v", e.SMTP, e.Resend)
	}
}

func TestLoadMailErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		vars map[string]string
		want string
	}{
		"resend without a sender": {map[string]string{"PRISMAPORT_RESEND_API_KEY": "re_test_123"}, "PRISMAPORT_MAIL_FROM is required with PRISMAPORT_RESEND_API_KEY"},
		"resend over plain http": {map[string]string{"PRISMAPORT_RESEND_API_KEY": "re_test_123", "PRISMAPORT_MAIL_FROM": "p@example.com",
			"PRISMAPORT_RESEND_BASE_URL": "http://api.resend.com"}, "must be https"},
		"both transports": {map[string]string{"PRISMAPORT_RESEND_API_KEY": "re_test_123", "PRISMAPORT_MAIL_FROM": "p@example.com",
			"PRISMAPORT_SMTP_HOST": "smtp.example.com"}, "not both"},
	} {
		t.Run(name, func(t *testing.T) {
			mailEnv(t, tc.vars)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
}

func TestLoadWithoutMail(t *testing.T) {
	mailEnv(t, nil)
	e, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if e.SMTP != nil || e.Resend != nil {
		t.Fatalf("smtp %+v, resend %+v", e.SMTP, e.Resend)
	}
}

func TestRequireMail(t *testing.T) {
	mailEnv(t, nil)
	t.Setenv("PRISMAPORT_REQUIRE_MAIL", "")
	e, err := Load()
	if err != nil || e.RequireMail || e.CheckMail() != nil {
		t.Fatalf("default: %v %v %v", e.RequireMail, err, e.CheckMail())
	}
	t.Setenv("PRISMAPORT_REQUIRE_MAIL", "true")
	if e, err = Load(); err != nil || !e.RequireMail {
		t.Fatalf("on: %v %v", e.RequireMail, err)
	}
	if err := e.CheckMail(); err == nil || !strings.Contains(err.Error(), "PRISMAPORT_RESEND_API_KEY") {
		t.Fatalf("required without a transport: %v", err)
	}
	mailEnv(t, map[string]string{"PRISMAPORT_RESEND_API_KEY": "re_test_123", "PRISMAPORT_MAIL_FROM": "Prismaport <no-reply@example.com>"})
	if e, err = Load(); err != nil || e.CheckMail() != nil {
		t.Fatalf("required with Resend: %v %v", err, e.CheckMail())
	}
}
