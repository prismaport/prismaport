package mail

import (
	"bytes"
	"embed"
	"fmt"
	htmltemplate "html/template"
	"strings"
	texttemplate "text/template"
	"time"
)

//go:embed templates/*.tmpl assets/*.png
var files embed.FS

// IconContentID is the inline image the HTML templates show as the Prismaport mark.
const IconContentID = "prismaport-icon"

// Template kinds.
const (
	KindVerifyEmail      = "verify_email"
	KindPasswordReset    = "password_reset"
	KindPasswordChanged  = "password_changed"
	KindAccountExists    = "account_exists"
	KindTOTPFailures     = "totp_failures"
	KindPasswordFailures = "password_failures"
	KindWebhookFailing   = "webhook_failing"
	KindWebhookDisabled  = "webhook_disabled"

	KindTOTPDisabled         = "totp_disabled"
	KindTOTPReplaced         = "totp_replaced"
	KindTOTPRemovalLink      = "totp_removal_link"
	KindTOTPRemovalScheduled = "totp_removal_scheduled"
	KindTOTPRemovalReminder  = "totp_removal_reminder"
	KindTOTPRemoved          = "totp_removed"
	KindTOTPRemovalCancelled = "totp_removal_cancelled"

	KindConfirmationCode = "confirmation_code"
	KindWalletAdded      = "wallet_added"
)

var subjects = map[string]string{
	KindVerifyEmail:      "Confirm your email for Prismaport",
	KindPasswordReset:    "Reset your Prismaport password",
	KindPasswordChanged:  "Your Prismaport password was changed",
	KindAccountExists:    "You already have a Prismaport account",
	KindTOTPFailures:     "Wrong two-factor codes on your Prismaport account",
	KindPasswordFailures: "Wrong passwords on your Prismaport account",
	KindWebhookFailing:   "A Prismaport webhook endpoint keeps failing",
	KindWebhookDisabled:  "A Prismaport webhook endpoint was disabled",

	KindTOTPDisabled:         "Two-factor authentication was turned off on your Prismaport account",
	KindTOTPReplaced:         "Your Prismaport authenticator app was replaced",
	KindTOTPRemovalLink:      "Remove two-factor authentication from your Prismaport account",
	KindTOTPRemovalScheduled: "Two-factor authentication will be removed from your Prismaport account",
	KindTOTPRemovalReminder:  "Two-factor authentication will be removed from your Prismaport account in 24 hours",
	KindTOTPRemoved:          "Two-factor authentication was removed from your Prismaport account",
	KindTOTPRemovalCancelled: "The removal of two-factor authentication was cancelled",

	KindWalletAdded: "A new way to get paid was added to your account",
}

// confirmationCopy is the subject, title and first line of the confirmation code email per
// action (D58); the rest of the email is the same for every action.
var confirmationCopy = map[string]struct{ Subject, Title, Line string }{
	"wallet_create": {"Your code to add a Live wallet", "Confirm a new Live wallet",
		"Enter this code in the Prismaport dashboard to add a wallet that receives Live payments."},
	"wallet_archive": {"Your code to archive a Live wallet", "Confirm archiving a Live wallet",
		"Enter this code in the Prismaport dashboard to archive a wallet that receives Live payments."},
	"api_key": {"Your code for a Live API key", "Confirm a Live API key",
		"Enter this code in the Prismaport dashboard to create or roll a Live API key."},
	"webhook_endpoint": {"Your code to change a Live webhook", "Confirm a Live webhook change",
		"Enter this code in the Prismaport dashboard to add or change a Live webhook endpoint, or roll its secret."},
	"unlock": {"Your code to unlock Live changes", "Unlock Live changes",
		"Enter this code in the Prismaport dashboard to unlock Live changes on your device for a few minutes."},
	"totp_enroll": {"Your code to turn on two-factor authentication", "Confirm two-factor authentication",
		"Enter this code in the Prismaport dashboard to turn on two-factor authentication."},
}

// ConfirmationSubject is the subject of the confirmation code email for action.
func ConfirmationSubject(action string) string { return confirmationCopy[action].Subject }

// ConfirmationActions lists the actions a confirmation code email exists for.
func ConfirmationActions() []string {
	out := make([]string, 0, len(confirmationCopy))
	for a := range confirmationCopy {
		out = append(out, a)
	}
	return out
}

// networkLogos are the inline logos of the networks a wallet email names, by chain id.
var networkLogos = map[string]string{
	"base":         "assets/network-base.png",
	"base-sepolia": "assets/network-base.png",
}

type linkData struct{ URL, Label string }

var funcs = map[string]any{"link": func(url, label string) linkData { return linkData{URL: url, Label: label} }}

var (
	htmlTemplates = htmltemplate.Must(htmltemplate.New("mail").Funcs(funcs).ParseFS(files, "templates/*.html.tmpl"))
	textTemplates = texttemplate.Must(texttemplate.New("mail").Funcs(funcs).ParseFS(files, "templates/*.txt.tmpl"))
	icon          = asset("assets/prismaport-icon.png")
)

func asset(name string) []byte {
	b, err := files.ReadFile(name)
	if err != nil {
		panic(err)
	}
	return b
}

// CodeEmail fills the verification email.
type CodeEmail struct {
	Code    string
	Minutes int
	// DashboardURL is where the code is entered ("https://…/dashboard/").
	DashboardURL string
}

// ResetLinkEmail fills the password reset email: a link that carries a one-time token.
type ResetLinkEmail struct {
	Token   string
	Minutes int
	// DashboardURL is the dashboard the link opens ("https://…/dashboard/").
	DashboardURL string
}

// ResetURL is the link that opens the page for a new password. The token rides in the fragment,
// which browsers never send to a server, so it stays out of access logs and Referer headers.
func (r ResetLinkEmail) ResetURL() string {
	return strings.TrimRight(r.DashboardURL, "/") + "/reset-password#token=" + r.Token
}

// NoticeEmail fills the security notices (password changed, account exists).
type NoticeEmail struct {
	Email        string
	DashboardURL string
	At           time.Time
}

// ConfirmationCodeEmail fills the code that confirms one sensitive action (D58). Action is one
// of ConfirmationActions.
type ConfirmationCodeEmail struct {
	NoticeEmail
	Code    string
	Minutes int
	Action  string
}

// Title returns the email heading for the confirmation's action.
func (c ConfirmationCodeEmail) Title() string { return confirmationCopy[c.Action].Title }

// Line returns the sentence explaining what the code confirms.
func (c ConfirmationCodeEmail) Line() string { return confirmationCopy[c.Action].Line }

// SpacedCode is the code as "482 193", for the preview line.
func (c ConfirmationCodeEmail) SpacedCode() string {
	if len(c.Code) != 6 {
		return c.Code
	}
	return c.Code[:3] + " " + c.Code[3:]
}

// WalletAddedEmail fills the notice about a new Live wallet. It names the networks the wallet
// can be paid on, never the wallet's address or name.
type WalletAddedEmail struct {
	NoticeEmail
	Networks []WalletNetwork
}

// WalletNetwork is one network a new wallet receives payments on: its chain id (for the logo),
// its name and the stablecoins payers can send there.
type WalletNetwork struct {
	Chain  string
	Name   string
	Assets []string
}

// Logo is the content id of the network's inline logo, or "" without one.
func (n WalletNetwork) Logo() string {
	if _, ok := networkLogos[n.Chain]; !ok {
		return ""
	}
	return "network-" + n.Chain
}

// Detail is the line under the network's name: "Live mode · payers can now send USDC and USDT to it".
func (n WalletNetwork) Detail() string {
	if len(n.Assets) == 0 {
		return "Live mode"
	}
	list := n.Assets[0]
	if k := len(n.Assets); k > 1 {
		list = strings.Join(n.Assets[:k-1], ", ") + " and " + n.Assets[k-1]
	}
	return "Live mode · payers can now send " + list + " to it"
}

// NetworkNames is "Base" or "Base and Solana", for the preview line.
func (w WalletAddedEmail) NetworkNames() string {
	names := make([]string, len(w.Networks))
	for i, n := range w.Networks {
		names[i] = n.Name
	}
	if k := len(names); k > 1 {
		return strings.Join(names[:k-1], ", ") + " and " + names[k-1]
	}
	return strings.Join(names, "")
}

// TOTPFailuresEmail fills the notice about wrong two-factor codes in a row.
type TOTPFailuresEmail struct {
	NoticeEmail
	Failures int
}

// PasswordFailuresEmail fills the notice about wrong passwords in a row.
type PasswordFailuresEmail struct {
	NoticeEmail
	Failures int
}

// TOTPLostLinkEmail fills the link that asks to remove a lost authenticator. Like a reset link,
// the token rides in the fragment.
type TOTPLostLinkEmail struct {
	Token        string
	Minutes      int
	DashboardURL string
}

// ConfirmURL is the page that asks for the password.
func (t TOTPLostLinkEmail) ConfirmURL() string {
	return strings.TrimRight(t.DashboardURL, "/") + "/lost-authenticator/confirm#token=" + t.Token
}

// TOTPRemovalEmail fills the emails about a pending or finished removal of two-factor
// authentication. CancelToken is empty once the removal is done; LockEnds is set only then.
type TOTPRemovalEmail struct {
	NoticeEmail
	CancelToken string
	RemovalAt   time.Time
	LockEnds    time.Time
}

// CancelURL opens the page with the cancel button; opening it cancels nothing, so a mail scanner
// cannot.
func (t TOTPRemovalEmail) CancelURL() string {
	return strings.TrimRight(t.DashboardURL, "/") + "/cancel-2fa-removal#token=" + t.CancelToken
}

// RemovalText returns when the authenticator removal takes effect, in words.
func (t TOTPRemovalEmail) RemovalText() string { return humanTime(t.RemovalAt) }

// LockEndsText returns when the account lock ends, in words.
func (t TOTPRemovalEmail) LockEndsText() string { return humanTime(t.LockEnds) }

// WebhookEmail fills the emails about a failing webhook endpoint. It names the endpoint by
// Target, its scheme and host only (a path or query can carry a token), and never carries the
// secret or anything the target answered.
type WebhookEmail struct {
	EndpointID string
	Target     string
	Livemode   bool
	// FailingSince is the start of the failure streak.
	FailingSince time.Time
	// LastStatusCode is the HTTP status of the failed attempt that queued the email (0 without
	// an answer); LastError names the failure from a fixed list ("timeout", "redirect", ...).
	LastStatusCode int
	LastError      string
	// DisableFrom (the warning) is when a failed attempt starts disabling the endpoint;
	// DisabledAt (the disabled notice) is when it was disabled.
	DisableFrom  time.Time
	DisabledAt   time.Time
	DashboardURL string
}

func humanTime(t time.Time) string { return t.UTC().Format("2 Jan 2006 at 15:04 UTC") }

// Mode is "Live" or "Test".
func (w WebhookEmail) Mode() string {
	if w.Livemode {
		return "Live"
	}
	return "Test"
}

// Since returns when the endpoint started failing, in words.
func (w WebhookEmail) Since() string { return humanTime(w.FailingSince) }

// DisableFromText returns when the endpoint will be disabled, in words.
func (w WebhookEmail) DisableFromText() string { return humanTime(w.DisableFrom) }

// DisabledText returns when the endpoint was disabled, in words.
func (w WebhookEmail) DisabledText() string { return humanTime(w.DisabledAt) }

// LastFailure is the status code or the error of the last failed attempt.
func (w WebhookEmail) LastFailure() string {
	switch {
	case w.LastStatusCode != 0 && w.LastError != "":
		return fmt.Sprintf("HTTP %d (%s)", w.LastStatusCode, w.LastError)
	case w.LastStatusCode != 0:
		return fmt.Sprintf("HTTP %d", w.LastStatusCode)
	case w.LastError != "":
		return w.LastError
	}
	return "no answer"
}

// LogURL is the endpoint's delivery log in the dashboard, in the endpoint's mode.
func (w WebhookEmail) LogURL() string {
	mode := "test"
	if w.Livemode {
		mode = "live"
	}
	return strings.TrimRight(w.DashboardURL, "/") + "/developers/webhooks/" + w.EndpointID + "?mode=" + mode
}

type page struct {
	Kind    string
	Subject string
	Preview string
	Data    any
}

// When formats a notice time for people: "26 Oct 2026 at 18:04 UTC".
func (n NoticeEmail) When() string { return n.At.UTC().Format("2 Jan 2006 at 15:04 UTC") }

// ForgotURL is the dashboard's password reset page.
func (n NoticeEmail) ForgotURL() string {
	return strings.TrimRight(n.DashboardURL, "/") + "/forgot-password"
}

// VerifyURL is the dashboard's confirmation page.
func (c CodeEmail) VerifyURL() string {
	return strings.TrimRight(c.DashboardURL, "/") + "/verify-email"
}

func render(kind, to, preview string, data any) (Message, error) {
	return renderAs(kind, subjects[kind], to, preview, data)
}

func renderAs(kind, subject, to, preview string, data any) (Message, error) {
	p := page{Kind: kind, Subject: subject, Preview: preview, Data: data}
	var html, text bytes.Buffer
	if err := htmlTemplates.ExecuteTemplate(&html, kind+".html.tmpl", p); err != nil {
		return Message{}, err
	}
	if err := textTemplates.ExecuteTemplate(&text, kind+".txt.tmpl", p); err != nil {
		return Message{}, err
	}
	m := Message{To: to, Subject: p.Subject, Kind: kind, Text: tidy(text.String()), HTML: html.String(),
		Inline: []Inline{{ContentID: IconContentID, ContentType: "image/png", Filename: "prismaport.png", Data: icon}}}
	return m, m.Validate()
}

// tidy trims the blank lines templates leave behind, keeping single empty lines between
// paragraphs.
func tidy(s string) string {
	var out []string
	blank := true
	for _, line := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		line = strings.TrimRight(line, " \t")
		if line == "" {
			if !blank {
				out = append(out, "")
			}
			blank = true
			continue
		}
		out = append(out, line)
		blank = false
	}
	return strings.TrimSpace(strings.Join(out, "\n")) + "\n"
}

// VerificationCode is the code that confirms a new account's email.
func VerificationCode(to string, d CodeEmail) (Message, error) {
	return render(KindVerifyEmail, to, fmt.Sprintf("Enter the code in the dashboard. It works for %d minutes.", d.Minutes), d)
}

// PasswordResetLink is the link that lets the owner of the email choose a new password.
func PasswordResetLink(to string, d ResetLinkEmail) (Message, error) {
	return render(KindPasswordReset, to, fmt.Sprintf("Open the link to choose a new password. It works for %d minutes, once.", d.Minutes), d)
}

// PasswordChanged tells the owner that the password changed and other sessions ended.
func PasswordChanged(to string, d NoticeEmail) (Message, error) {
	return render(KindPasswordChanged, to, "If this was not you, reset your password now.", d)
}

// AccountExists answers a sign-up with an email that already has an account.
func AccountExists(to string, d NoticeEmail) (Message, error) {
	return render(KindAccountExists, to, "Nothing was created. Sign in or reset your password.", d)
}

// TOTPFailures tells the owner that someone entered several wrong two-factor codes in a row.
func TOTPFailures(to string, d TOTPFailuresEmail) (Message, error) {
	return render(KindTOTPFailures, to, "If this was not you, someone knows your password. Reset it now.", d)
}

// PasswordFailures tells the owner that someone entered several wrong passwords in a row.
func PasswordFailures(to string, d PasswordFailuresEmail) (Message, error) {
	return render(KindPasswordFailures, to, "If this was not you, someone is trying to guess your password.", d)
}

// TOTPDisabled tells the owner that two-factor authentication was turned off.
func TOTPDisabled(to string, d NoticeEmail) (Message, error) {
	return render(KindTOTPDisabled, to, "If this was not you, reset your password now.", d)
}

// TOTPReplaced tells the owner that the authenticator app was replaced.
func TOTPReplaced(to string, d NoticeEmail) (Message, error) {
	return render(KindTOTPReplaced, to, "If this was not you, reset your password now.", d)
}

// TOTPRemovalLink is the link that asks to remove a lost authenticator.
func TOTPRemovalLink(to string, d TOTPLostLinkEmail) (Message, error) {
	return render(KindTOTPRemovalLink, to, fmt.Sprintf("Open the link and enter your password. It works for %d minutes, once.", d.Minutes), d)
}

// TOTPRemovalScheduled tells the owner a removal was asked for and how to cancel it.
func TOTPRemovalScheduled(to string, d TOTPRemovalEmail) (Message, error) {
	return render(KindTOTPRemovalScheduled, to, "It will be removed in 72 hours. If this was not you, cancel now.", d)
}

// TOTPRemovalReminder repeats the notice 24 hours before the removal.
func TOTPRemovalReminder(to string, d TOTPRemovalEmail) (Message, error) {
	return render(KindTOTPRemovalReminder, to, "It will be removed in 24 hours. If this was not you, cancel now.", d)
}

// TOTPRemoved tells the owner the removal happened and when the lock on live credentials ends.
func TOTPRemoved(to string, d TOTPRemovalEmail) (Message, error) {
	return render(KindTOTPRemoved, to, "Your password now signs you in. If this was not you, reset it now.", d)
}

// TOTPRemovalCancelled tells the owner that a pending removal was cancelled.
func TOTPRemovalCancelled(to string, d NoticeEmail) (Message, error) {
	return render(KindTOTPRemovalCancelled, to, "Two-factor authentication stays on.", d)
}

// ConfirmationCode is the code that confirms one sensitive action; its subject names the action.
func ConfirmationCode(to string, d ConfirmationCodeEmail) (Message, error) {
	c, ok := confirmationCopy[d.Action]
	if !ok {
		return Message{}, fmt.Errorf("%w: no confirmation email for %q", ErrInvalidMessage, d.Action)
	}
	return renderAs(KindConfirmationCode, c.Subject, to, fmt.Sprintf("Your code is %s. It works for %d minutes.", d.SpacedCode(), d.Minutes), d)
}

// WalletAdded tells the owner a wallet that receives Live payments was added, with the logo of
// each network it receives on.
func WalletAdded(to string, d WalletAddedEmail) (Message, error) {
	m, err := render(KindWalletAdded, to, d.NetworkNames()+" was added in Live mode. If this was not you, reset your password now.", d)
	if err != nil {
		return m, err
	}
	seen := map[string]bool{}
	for _, n := range d.Networks {
		if id := n.Logo(); id != "" && !seen[id] {
			seen[id] = true
			m.Inline = append(m.Inline, Inline{ContentID: id, ContentType: "image/png", Filename: id + ".png", Data: asset(networkLogos[n.Chain])})
		}
	}
	return m, nil
}

// WebhookFailing warns that an endpoint has failed for a day and when it will be disabled.
func WebhookFailing(to string, d WebhookEmail) (Message, error) {
	return render(KindWebhookFailing, to, "Deliveries to "+d.Target+" have failed for a day. Fix it before the endpoint is disabled.", d)
}

// WebhookDisabled says Prismaport disabled an endpoint that kept failing.
func WebhookDisabled(to string, d WebhookEmail) (Message, error) {
	return render(KindWebhookDisabled, to, "Deliveries to "+d.Target+" failed for days. Enable the endpoint again once it works.", d)
}
