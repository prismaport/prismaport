package mail

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

func goldenMessages(t *testing.T) map[string]Message {
	t.Helper()
	code := CodeEmail{Code: "482193", Minutes: 15, DashboardURL: "https://pay.prismaport.test/dashboard/"}
	reset := ResetLinkEmail{Token: "k3Vq9_link-token_Zr7w", Minutes: 30, DashboardURL: "https://pay.prismaport.test/dashboard/"}
	notice := NoticeEmail{Email: "owner@example.com", DashboardURL: "https://pay.prismaport.test/dashboard/",
		At: time.Date(2026, 10, 26, 18, 4, 0, 0, time.UTC)}
	webhook := WebhookEmail{EndpointID: "we_01M5MY4JA0VHY826WNC5SSV6VN", Target: "https://shop.example", Livemode: true,
		FailingSince: time.Date(2026, 10, 26, 18, 4, 0, 0, time.UTC), LastStatusCode: 503,
		DisableFrom: time.Date(2026, 10, 31, 18, 4, 0, 0, time.UTC), DashboardURL: "https://pay.prismaport.test/dashboard/"}
	lost := TOTPLostLinkEmail{Token: "k3Vq9_link-token_Zr7w", Minutes: 30, DashboardURL: "https://pay.prismaport.test/dashboard/"}
	removal := TOTPRemovalEmail{NoticeEmail: notice, CancelToken: "c4nc3l_token_Zr7w", RemovalAt: time.Date(2026, 10, 29, 18, 4, 0, 0, time.UTC)}
	removed := TOTPRemovalEmail{NoticeEmail: notice, RemovalAt: time.Date(2026, 10, 29, 18, 4, 0, 0, time.UTC), LockEnds: time.Date(2026, 10, 31, 18, 4, 0, 0, time.UTC)}
	out := map[string]Message{}
	for kind, build := range map[string]func() (Message, error){
		KindTOTPDisabled:         func() (Message, error) { return TOTPDisabled("owner@example.com", notice) },
		KindTOTPReplaced:         func() (Message, error) { return TOTPReplaced("owner@example.com", notice) },
		KindTOTPRemovalLink:      func() (Message, error) { return TOTPRemovalLink("owner@example.com", lost) },
		KindTOTPRemovalScheduled: func() (Message, error) { return TOTPRemovalScheduled("owner@example.com", removal) },
		KindTOTPRemovalReminder:  func() (Message, error) { return TOTPRemovalReminder("owner@example.com", removal) },
		KindTOTPRemoved:          func() (Message, error) { return TOTPRemoved("owner@example.com", removed) },
		KindTOTPRemovalCancelled: func() (Message, error) { return TOTPRemovalCancelled("owner@example.com", notice) },
		KindVerifyEmail:          func() (Message, error) { return VerificationCode("owner@example.com", code) },
		KindConfirmationCode: func() (Message, error) {
			return ConfirmationCode("owner@example.com", ConfirmationCodeEmail{NoticeEmail: notice, Code: "482193", Minutes: 15, Action: "wallet_create"})
		},
		KindWalletAdded: func() (Message, error) {
			return WalletAdded("owner@example.com", WalletAddedEmail{NoticeEmail: notice,
				Networks: []WalletNetwork{{Chain: "base", Name: "Base", Assets: []string{"USDC", "USDT"}}}})
		},
		KindPasswordReset:   func() (Message, error) { return PasswordResetLink("owner@example.com", reset) },
		KindPasswordChanged: func() (Message, error) { return PasswordChanged("owner@example.com", notice) },
		KindAccountExists:   func() (Message, error) { return AccountExists("owner@example.com", notice) },
		KindTOTPFailures: func() (Message, error) {
			return TOTPFailures("owner@example.com", TOTPFailuresEmail{NoticeEmail: notice, Failures: 5})
		},
		KindPasswordFailures: func() (Message, error) {
			return PasswordFailures("owner@example.com", PasswordFailuresEmail{NoticeEmail: notice, Failures: 10})
		},
		KindWebhookFailing: func() (Message, error) {
			return WebhookFailing("owner@example.com", webhook)
		},
		KindWebhookDisabled: func() (Message, error) {
			d := webhook
			d.DisabledAt, d.LastStatusCode, d.LastError = time.Date(2026, 10, 31, 9, 30, 0, 0, time.UTC), 0, "timeout"
			return WebhookDisabled("owner@example.com", d)
		},
	} {
		m, err := build()
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		out[kind] = m
	}
	return out
}

// The rendered emails are compared byte for byte with testdata; run with -update after an
// intended copy or design change and review the diff.
func TestTemplatesGolden(t *testing.T) {
	for kind, m := range goldenMessages(t) {
		got := "Subject: " + m.Subject + "\n\n" + m.Text
		for ext, body := range map[string]string{".txt": got, ".html": m.HTML} {
			t.Run(ext, func(t *testing.T) {
				path := filepath.Join("testdata", kind+ext+".golden")
				if *update {
					if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
						t.Fatal(err)
					}
					return
				}
				want, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("%s: %v (run go test ./internal/mail -update)", path, err)
				}
				if string(want) != body {
					t.Errorf("%s differs from the rendered email; run with -update if the change is intended:\n%s", path, body)
				}
			})
		}
	}
}

func TestTemplatesFollowTheBrand(t *testing.T) {
	for kind, m := range goldenMessages(t) {
		all := m.Subject + m.Text + m.HTML
		for _, banned := range []string{"\u2014", "!", "{{", "<no value>", "%!"} {
			if strings.Contains(strings.ReplaceAll(all, "<!doctype", ""), banned) {
				t.Errorf("%s contains %q", kind, banned)
			}
		}
		if !strings.Contains(m.HTML, `src="cid:`+IconContentID+`"`) || len(m.Inline) == 0 || m.Inline[0].ContentID != IconContentID {
			t.Errorf("%s: the mark is not inline", kind)
		}
		for _, in := range m.Inline {
			if !bytes.HasPrefix(in.Data, []byte("\x89PNG\r\n\x1a\n")) || !strings.Contains(m.HTML, `src="cid:`+in.ContentID+`"`) {
				t.Errorf("%s: inline %s is not a PNG the HTML shows", kind, in.ContentID)
			}
		}
		if !strings.Contains(m.HTML, "https://pay.prismaport.test/dashboard/") || !strings.Contains(m.Text, "https://pay.prismaport.test/dashboard/") {
			t.Errorf("%s: no link to the dashboard", kind)
		}
		if strings.Contains(m.Subject, "482193") {
			t.Errorf("%s: the code is in the subject", kind)
		}
	}
	msgs := goldenMessages(t)
	if v := msgs[KindVerifyEmail]; !strings.Contains(v.Text, "    482193\n") || !strings.Contains(v.HTML, ">482193</span>") ||
		!strings.Contains(v.Text, "15 minutes") {
		t.Errorf("verify email: code or expiry missing")
	}
	link := "https://pay.prismaport.test/dashboard/reset-password#token=k3Vq9_link-token_Zr7w"
	if r := msgs[KindPasswordReset]; !strings.Contains(r.Text, "\n"+link+"\n") || !strings.Contains(r.HTML, `href="`+link+`"`) ||
		!strings.Contains(r.Text, "30 minutes, once") || strings.Contains(r.Subject, "k3Vq9") {
		t.Errorf("password reset: link or expiry missing: %s", r.Text)
	}
	if !strings.Contains(msgs[KindPasswordChanged].Text, "26 Oct 2026 at 18:04 UTC") || !strings.Contains(msgs[KindPasswordChanged].Text, "/dashboard/forgot-password") {
		t.Errorf("password changed: %s", msgs[KindPasswordChanged].Text)
	}
	if f := msgs[KindTOTPFailures]; !strings.Contains(f.Text, "entered 5 wrong two-factor codes in a row") ||
		!strings.Contains(f.Text, "(owner@example.com), the last one on 26 Oct 2026 at 18:04 UTC") ||
		!strings.Contains(f.HTML, `href="https://pay.prismaport.test/dashboard/forgot-password"`) {
		t.Errorf("two-factor failures: %s", f.Text)
	}
	if f := msgs[KindPasswordFailures]; !strings.Contains(f.Text, "entered 10 wrong passwords in a row") ||
		!strings.Contains(f.Text, "(owner@example.com), the last one on 26 Oct 2026 at 18:04 UTC") ||
		!strings.Contains(f.HTML, `href="https://pay.prismaport.test/dashboard/forgot-password"`) {
		t.Errorf("password failures: %s", f.Text)
	}
}

// Every action has its own subject, title and first line around the same code block; the code
// never reaches the subject, and the preview spaces it.
func TestConfirmationCodeEmails(t *testing.T) {
	notice := NoticeEmail{Email: "owner@example.com", DashboardURL: "https://pay.prismaport.test/dashboard/", At: time.Now()}
	subjects := map[string]bool{}
	for _, action := range ConfirmationActions() {
		m, err := ConfirmationCode("owner@example.com", ConfirmationCodeEmail{NoticeEmail: notice, Code: "482193", Minutes: 15, Action: action})
		if err != nil {
			t.Fatalf("%s: %v", action, err)
		}
		c := confirmationCopy[action]
		if m.Subject != c.Subject || subjects[m.Subject] || !strings.HasPrefix(m.Text, c.Title+"\n\n"+c.Line+"\n\n    482193\n") ||
			!strings.Contains(m.HTML, ">482193</span>") || !strings.Contains(m.HTML, ">Your code is 482 193. It works for 15 minutes.<") ||
			!strings.Contains(m.Text, "15 minutes, once") || !strings.Contains(m.Text, "/dashboard/forgot-password") {
			t.Errorf("%s:\n%s\n%s", action, m.Subject, m.Text)
		}
		subjects[m.Subject] = true
	}
	if len(subjects) != 6 {
		t.Errorf("%d actions", len(subjects))
	}
	if _, err := ConfirmationCode("owner@example.com", ConfirmationCodeEmail{NoticeEmail: notice, Code: "482193", Minutes: 15, Action: "login"}); err == nil {
		t.Error("an email for an unknown action")
	}
}

// The new wallet notice names each network with its logo, the mode and the time, and never the
// wallet; a network without a logo is named without one.
func TestWalletAddedEmail(t *testing.T) {
	m := goldenMessages(t)[KindWalletAdded]
	for _, want := range []string{"(owner@example.com) on 26 Oct 2026 at 18:04 UTC", "    Base: Live mode \u00b7 payers can now send USDC and USDT to it\n"} {
		if !strings.Contains(m.Text, want) {
			t.Errorf("lacks %q:\n%s", want, m.Text)
		}
	}
	if len(m.Inline) != 2 || m.Inline[1].ContentID != "network-base" || !strings.Contains(m.HTML, `src="cid:network-base"`) {
		t.Errorf("no Base logo: %+v", m.Inline)
	}
	notice := NoticeEmail{Email: "owner@example.com", DashboardURL: "https://pay.prismaport.test/dashboard/", At: time.Now()}
	two, err := WalletAdded("owner@example.com", WalletAddedEmail{NoticeEmail: notice,
		Networks: []WalletNetwork{{Chain: "solana", Name: "Solana", Assets: []string{"USDC"}}, {Chain: "arbitrum", Name: "Arbitrum"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(two.Inline) != 1 || strings.Contains(two.HTML, "cid:network-") || !strings.Contains(two.Text, "Solana: Live mode \u00b7 payers can now send USDC to it") ||
		!strings.Contains(two.Text, "    Arbitrum: Live mode\n") || !strings.Contains(two.HTML, "Solana and Arbitrum was added") {
		t.Errorf("networks without logos:\n%s", two.Text)
	}
}

// The recovery emails carry the links in the fragment, the removal time and the end of the lock.
func TestAccountRecoveryEmails(t *testing.T) {
	msgs := goldenMessages(t)
	for _, c := range []struct {
		kind string
		want []string
	}{
		{KindTOTPRemovalLink, []string{"\nhttps://pay.prismaport.test/dashboard/lost-authenticator/confirm#token=k3Vq9_link-token_Zr7w\n", "30 minutes, once"}},
		{KindTOTPRemovalScheduled, []string{"\nhttps://pay.prismaport.test/dashboard/cancel-2fa-removal#token=c4nc3l_token_Zr7w\n", "in 72 hours, on 29 Oct 2026 at 18:04 UTC", "locked for 48 hours"}},
		{KindTOTPRemovalReminder, []string{"\nhttps://pay.prismaport.test/dashboard/cancel-2fa-removal#token=c4nc3l_token_Zr7w\n", "in 24 hours, on 29 Oct 2026 at 18:04 UTC"}},
		{KindTOTPRemoved, []string{"removed on 29 Oct 2026 at 18:04 UTC", "locked until 31 Oct 2026 at 18:04 UTC"}},
		{KindTOTPRemovalCancelled, []string{"was cancelled on 26 Oct 2026 at 18:04 UTC", "/dashboard/forgot-password"}},
		{KindTOTPDisabled, []string{"turned off on 26 Oct 2026 at 18:04 UTC"}},
		{KindTOTPReplaced, []string{"replaced on 26 Oct 2026 at 18:04 UTC", "recovery codes keep working"}},
	} {
		t.Run(c.kind, func(t *testing.T) {
			m := msgs[c.kind]
			for _, want := range c.want {
				if !strings.Contains(m.Text, want) {
					t.Errorf("%s lacks %q:\n%s", c.kind, want, m.Text)
				}
			}
			if strings.Contains(m.Subject, "token") {
				t.Errorf("%s: a token in the subject", c.kind)
			}
		})
	}
}

// The failing-endpoint emails name the endpoint by scheme and host, its id and mode, the streak
// start, the last failure and what happens next, and link to the delivery log in its mode.
func TestWebhookEmails(t *testing.T) {
	msgs := goldenMessages(t)
	log := "https://pay.prismaport.test/dashboard/developers/webhooks/we_01M5MY4JA0VHY826WNC5SSV6VN?mode=live"
	w := msgs[KindWebhookFailing]
	for _, want := range []string{"Live mode webhook endpoint https://shop.example (we_01M5MY4JA0VHY826WNC5SSV6VN)",
		"failed since 26 Oct 2026 at 18:04 UTC", "got: HTTP 503.", "from 31 Oct 2026 at 18:04 UTC on", "\n" + log + "\n"} {
		if !strings.Contains(w.Text, want) {
			t.Errorf("warning lacks %q:\n%s", want, w.Text)
		}
	}
	d := msgs[KindWebhookDisabled]
	for _, want := range []string{"on 31 Oct 2026 at 09:30 UTC", "got: timeout.", "\n" + log + "\n"} {
		if !strings.Contains(d.Text, want) {
			t.Errorf("disabled notice lacks %q:\n%s", want, d.Text)
		}
	}
	if !strings.Contains(w.HTML, `href="`+log+`"`) {
		t.Errorf("warning: no link to the log")
	}
	if got := (WebhookEmail{LastStatusCode: 302, LastError: "redirect"}).LastFailure(); got != "HTTP 302 (redirect)" {
		t.Errorf("redirect: %s", got)
	}
}

func TestTemplatesEscapeHTML(t *testing.T) {
	m, err := PasswordChanged("owner@example.com", NoticeEmail{Email: `<script>alert(1)</script>@example.com`, DashboardURL: "javascript:alert(1)//",
		At: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(m.HTML, "<script>") || strings.Contains(m.HTML, `href="javascript:`) {
		t.Fatalf("unescaped: %s", m.HTML)
	}
}
