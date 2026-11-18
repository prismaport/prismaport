package mail

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testResendKey = "re_test_123"

type resendCapture struct {
	auth, ctype string
	body        resendEmail
}

func resendServer(t *testing.T, status int, reply string) (*httptest.Server, *resendCapture) {
	t.Helper()
	got := &resendCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/emails" {
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
			return
		}
		got.auth, got.ctype = r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &got.body); err != nil {
			t.Errorf("body is not JSON: %v", err)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

func newTestResend(t *testing.T, baseURL string) *Resend {
	t.Helper()
	r, err := NewResend(ResendConfig{APIKey: testResendKey, From: "Prismaport (dev) <onboarding@resend.dev>", BaseURL: baseURL})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestResendSendsTheMessageWithItsInlineIcon(t *testing.T) {
	srv, got := resendServer(t, http.StatusOK, `{"id":"49a3999c-0ce1-4ea6-ab68-afcd6dc2e794"}`)
	m := sample()
	if err := newTestResend(t, srv.URL).Send(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	if got.auth != "Bearer "+testResendKey || got.ctype != "application/json" {
		t.Fatalf("headers: auth %q, content type %q", got.auth, got.ctype)
	}
	b := got.body
	if b.From != "Prismaport (dev) <onboarding@resend.dev>" || len(b.To) != 1 || b.To[0] != m.To || b.Subject != m.Subject ||
		b.Text != m.Text || b.HTML != m.HTML {
		t.Fatalf("body: %+v", b)
	}
	if len(b.Attachments) != 1 {
		t.Fatalf("attachments: %+v", b.Attachments)
	}
	a := b.Attachments[0]
	data, err := base64.StdEncoding.DecodeString(a.Content)
	if err != nil || string(data) != string(m.Inline[0].Data) || a.ContentID != IconContentID || a.ContentType != "image/png" ||
		a.Filename != "prismaport.png" {
		t.Fatalf("inline icon: %+v (%v)", a, err)
	}
}

func TestResendTextOnlyHasNoAttachments(t *testing.T) {
	srv, got := resendServer(t, http.StatusOK, `{"id":"x"}`)
	m := Message{To: "owner@example.com", Subject: "s", Text: "t", Kind: KindVerifyEmail}
	if err := newTestResend(t, srv.URL).Send(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	if got.body.HTML != "" || got.body.Attachments != nil {
		t.Fatalf("body: %+v", got.body)
	}
}

func TestResendReportsTheAPIErrorWithoutTheKey(t *testing.T) {
	srv, _ := resendServer(t, http.StatusForbidden,
		`{"statusCode":403,"name":"validation_error","message":"You can only send testing emails to your own email address."}`)
	err := newTestResend(t, srv.URL).Send(t.Context(), sample())
	if err == nil || !strings.Contains(err.Error(), "403 validation_error: You can only send testing emails") {
		t.Fatalf("error: %v", err)
	}
	if strings.Contains(err.Error(), testResendKey) {
		t.Fatalf("the error leaks the API key: %v", err)
	}
}

func TestResendRefusesAnInvalidMessageBeforeCallingTheAPI(t *testing.T) {
	srv, got := resendServer(t, http.StatusOK, `{"id":"x"}`)
	err := newTestResend(t, srv.URL).Send(t.Context(), Message{To: "a@example.com, b@example.com", Subject: "s", Text: "t"})
	if !errors.Is(err, ErrInvalidMessage) || got.auth != "" {
		t.Fatalf("error %v, request sent: %v", err, got.auth != "")
	}
}

func TestResendHonorsTheTimeout(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-block }))
	t.Cleanup(func() { close(block); srv.Close() })
	r, err := NewResend(ResendConfig{APIKey: testResendKey, From: "p@example.com", BaseURL: srv.URL, Timeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := r.Send(t.Context(), sample()); err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("want a timeout error soon, got %v after %s", err, time.Since(start))
	}
}

func TestResendConfigCheck(t *testing.T) {
	ok := ResendConfig{APIKey: testResendKey, From: "Prismaport <no-reply@example.com>", BaseURL: ResendBaseURL}
	if err := ok.Check(); err != nil {
		t.Fatalf("valid config: %v", err)
	}
	for name, mutate := range map[string]func(*ResendConfig){
		"no key":            func(c *ResendConfig) { c.APIKey = "" },
		"key with newline":  func(c *ResendConfig) { c.APIKey = "re_x\r\nX: y" },
		"bad sender":        func(c *ResendConfig) { c.From = "nobody" },
		"sender with CRLF":  func(c *ResendConfig) { c.From = "a@example.com\r\nBcc: x@example.com" },
		"plain http remote": func(c *ResendConfig) { c.BaseURL = "http://api.resend.com" },
		"not a URL":         func(c *ResendConfig) { c.BaseURL = "::" },
	} {
		t.Run(name, func(t *testing.T) {
			c := ok
			mutate(&c)
			if err := c.Check(); err == nil {
				t.Errorf("%s: want an error", name)
			}
		})
	}
	local := ok
	local.BaseURL = "http://127.0.0.1:8025"
	if err := local.Check(); err != nil {
		t.Fatalf("plain http to loopback: %v", err)
	}
}
