package service

import (
	"context"
	"crypto/hmac"
	"errors"
	"strings"
	"time"

	"github.com/prismaport/prismaport/internal/mail"
	"github.com/prismaport/prismaport/internal/store"
	"github.com/prismaport/prismaport/internal/tuning"
)

// Confirmation of sensitive dashboard actions (D58): one code, either emailed to the account
// owner or, with 2FA on, from the authenticator app. No dashboard action takes the password but
// sign-in and changing it.
const (
	// CodeConfirmationRequired asks for a code; its details list the methods the user has.
	CodeConfirmationRequired = "confirmation_required"
	// CodeEmailSendFailed says the provider did not take the code's email this time; asking
	// again may work (unlike service_unavailable: no email on this server).
	CodeEmailSendFailed = "email_send_failed"

	// Actions an emailed code confirms. The code confirms only the action it was sent for.
	ConfirmWalletCreate    = "wallet_create"
	ConfirmWalletArchive   = "wallet_archive"
	ConfirmAPIKey          = "api_key"
	ConfirmWebhookEndpoint = "webhook_endpoint"
	ConfirmTOTPEnroll      = "totp_enroll"
	// ConfirmUnlock opens the confirmation window without an action (D61).
	ConfirmUnlock = "unlock"

	// ConfirmResendAfter is the wait before another code for the same action is sent.
	ConfirmResendAfter = 60 * time.Second

	// Methods of confirming, as the error details name them.
	MethodEmail = "email"
	MethodTOTP  = "totp"
)

// liveConfirmActions are the actions that exist only in Live mode; Test mode asks for nothing.
var liveConfirmActions = map[string]bool{ConfirmWalletCreate: true, ConfirmWalletArchive: true, ConfirmAPIKey: true, ConfirmWebhookEndpoint: true,
	ConfirmUnlock: true}

// StepUpProof is what a dashboard request carries to confirm a sensitive action: a code emailed
// for the action or, with 2FA on, a current TOTP code. At most one of them.
type StepUpProof struct {
	EmailCode *string
	TOTP      *string
}

// ConfirmationSent answers a request for an emailed code: where it went (masked), how long it
// works and when another may be asked for, in seconds.
type ConfirmationSent struct {
	SentTo      string `json:"sent_to"`
	ExpiresIn   int    `json:"expires_in"`
	ResendAfter int    `json:"resend_after"`
}

// MaskEmail shows the first character of the local part and the domain: "o•••@acme.studio".
func MaskEmail(email string) string {
	at := strings.LastIndexByte(email, '@')
	if at < 1 {
		return "•••"
	}
	return email[:1] + "•••" + email[at:]
}

func (s *Service) confirmMethods(u store.DashboardUser) []string {
	var m []string
	if s.cfg.Mail != nil && s.cfg.SecretBox != nil {
		m = append(m, MethodEmail)
	}
	if u.TOTPEnabled() {
		m = append(m, MethodTOTP)
	}
	return m
}

func confirmationRequired(methods []string) *Error {
	msg := "Confirm it's you with a 6-digit code we email you."
	if len(methods) == 2 {
		msg = "Confirm it's you with the code from your authenticator app or a code we email you."
	} else if len(methods) == 1 && methods[0] == MethodTOTP {
		msg = "Confirm it's you with the code from your authenticator app."
	}
	if methods == nil {
		methods = []string{}
	}
	return &Error{Kind: KindForbidden, Code: CodeConfirmationRequired, Message: msg, Details: map[string]any{"methods": methods}}
}

func confirmCodeInvalid() *Error {
	return newErr(KindForbidden, CodeInvalidCode,
		"That code is not valid or has expired. Check the newest email from Prismaport, or send a new code.")
}

func confirmTOTPInvalid() *Error {
	return newErr(KindForbidden, CodeInvalidCode, "That code is not valid. Enter the current code from your authenticator app.")
}

// SendConfirmationCode emails the user a code for one action. The Live actions are asked for in
// Live mode only (Test mode needs no code); turning on 2FA takes no mode, and only while 2FA is
// off. Another code for the same action is sent at most every ConfirmResendAfter; it replaces
// the one before. The email goes out before the answer: when the provider refuses it, the code
// is deleted (so the wait for another does not apply) and the answer is 503 email_send_failed.
func (s *Service) SendConfirmationCode(ctx context.Context, du DashUser, livemode bool, action string) (ConfirmationSent, error) {
	if !liveConfirmActions[action] && action != ConfirmTOTPEnroll {
		return ConfirmationSent{}, at(invalid("purpose must be one of wallet_create, wallet_archive, api_key, webhook_endpoint, unlock or totp_enroll"), "purpose")
	}
	if liveConfirmActions[action] && !livemode {
		return ConfirmationSent{}, invalid("Test mode needs no confirmation: send the request without a code.")
	}
	if d, _ := s.confirmWindow(); action == ConfirmUnlock && d <= 0 {
		return ConfirmationSent{}, unlockOff()
	}
	if s.cfg.Mail == nil || s.cfg.SecretBox == nil {
		return ConfirmationSent{}, newErr(KindUnavailable, CodeServiceUnavailable,
			"Codes by email are not set up on this server. Use your authenticator app.")
	}
	u, err := s.store.DashboardUser(ctx, du.ID)
	if err != nil {
		return ConfirmationSent{}, err
	}
	if action == ConfirmTOTPEnroll && u.TOTPEnabled() {
		return ConfirmationSent{}, invalid("Two-factor authentication is on: confirm with your authenticator app or a recovery code.")
	}
	purpose := store.ConfirmPurpose(action)
	code, c, err := s.issue(u.ID, purpose)
	if err != nil {
		return ConfirmationSent{}, err
	}
	wait, err := s.store.IssueConfirmationCode(ctx, u, purpose, c, ConfirmResendAfter)
	if err != nil {
		return ConfirmationSent{}, err
	}
	if wait > 0 {
		e := newErr(KindRateLimited, CodeRateLimited, "A code was just sent. Wait %d seconds before asking for a new one.",
			int((wait+time.Second-1)/time.Second))
		e.RetryAfter = wait
		return ConfirmationSent{}, e
	}
	m, err := mail.ConfirmationCode(u.Email, mail.ConfirmationCodeEmail{NoticeEmail: s.notice(u.Email), Code: code,
		Minutes: int(EmailCodeLifetime / time.Minute), Action: action})
	if err == nil {
		err = mail.SendNow(ctx, s.cfg.Mail, m)
	}
	if err != nil {
		s.Logger().Error("confirmation code not sent", "purpose", purpose, "err", err)
		// The request's context may be what failed the send; the code goes all the same.
		if derr := s.store.DropConfirmationCode(context.WithoutCancel(ctx), u, purpose, c.Hash); derr != nil {
			return ConfirmationSent{}, derr
		}
		return ConfirmationSent{}, &Error{Kind: KindUnavailable, Code: CodeEmailSendFailed, Cause: err,
			Message: "We couldn't send the code. Try again in a moment."}
	}
	return ConfirmationSent{SentTo: MaskEmail(u.Email), ExpiresIn: int(EmailCodeLifetime / time.Second),
		ResendAfter: int(ConfirmResendAfter / time.Second)}, nil
}

// confirm checks the one code of a sensitive action, an emailed code sent for action or, with
// 2FA on, a current TOTP code, and runs write, the action, in the transaction that spends the
// code: a write that is refused leaves the code unspent, so it confirms the corrected request.
// Without a code the answer lists the methods the user has. A wrong code is audited as a failed
// step-up and counts like any other wrong code of its kind, committed whatever follows.
func (s *Service) confirm(ctx context.Context, u store.DashboardUser, action string, p StepUpProof, write func(b *Service) error) error {
	hasEmail, hasTOTP := p.EmailCode != nil && *p.EmailCode != "", p.TOTP != nil && *p.TOTP != ""
	if hasEmail && hasTOTP {
		return &Error{Kind: KindInvalid, Code: CodeInvalidRequest, Param: "email_code",
			Message: "Send the code from your email or from your authenticator app, not both."}
	}
	methods := s.confirmMethods(u)
	switch {
	case hasEmail && s.cfg.Mail != nil && s.cfg.SecretBox != nil:
		ok, err := s.useConfirmationCode(ctx, u, action, *p.EmailCode, write)
		if err != nil || ok {
			return err
		}
		if err := s.stepUpFailed(ctx, u, "email_code"); err != nil {
			return err
		}
		return confirmCodeInvalid()
	case hasTOTP && u.TOTPEnabled():
		ok, err := s.checkTOTPFor(ctx, u, store.TOTPRunStepUp, *p.TOTP, write)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		if err := s.stepUpFailed(ctx, u, "totp"); err != nil {
			return err
		}
		return confirmTOTPInvalid()
	}
	return confirmationRequired(methods)
}

// useConfirmationCode spends the user's emailed code for action when code is it, running write in
// the same transaction. A code that cannot be one (not 6 digits) is refused without being looked
// up.
func (s *Service) useConfirmationCode(ctx context.Context, u store.DashboardUser, action, code string, write func(b *Service) error) (bool, error) {
	c, ok := normalizeCode(code)
	if !ok {
		return false, nil
	}
	purpose := store.ConfirmPurpose(action)
	err := s.store.UseConfirmationCode(ctx, store.CodeCheck{Email: u.Email, Purpose: purpose, MaxAttempts: MaxCodeAttempts,
		MaxFailures: MaxCodeFailures, Window: CodeFailureWindow,
		Matches: func(userID string, stored []byte) bool { return hmac.Equal(stored, s.codeHash(userID, purpose, c)) }},
		func(st *store.Store) error { return write(s.withStore(st)) })
	if errors.Is(err, store.ErrCodeInvalid) {
		return false, nil
	}
	return err == nil, err
}

func (s *Service) stepUpFailed(ctx context.Context, u store.DashboardUser, reason string) error {
	return s.store.Audit(ctx, u.MerchantID, "dashboard:"+u.ID, "user.step_up_failed", map[string]any{"user_id": u.ID, "reason": reason})
}

// liveConfirm guards a Live mode action that decides where money goes or issues a credential
// that outlives the session (a wallet, an API key, a webhook endpoint): write, the action, runs
// once the code is right, in the transaction that spends it (confirm). Test mode asks for
// nothing and runs write as is.
func (s *Service) liveConfirm(ctx context.Context, du DashUser, livemode bool, action string, p StepUpProof, write func(b *Service) error) error {
	if !livemode {
		return write(s)
	}
	u, err := s.store.DashboardUser(ctx, du.ID)
	if err != nil {
		return err
	}
	return s.confirm(ctx, u, action, p, write)
}

// confirmTOTPEnroll confirms turning 2FA on with the code emailed for it and runs write, the
// pending enrollment, with it; the authenticator's first code then proves the enrollment
// (VerifyTOTP).
func (s *Service) confirmTOTPEnroll(ctx context.Context, u store.DashboardUser, emailCode *string, write func(b *Service) error) error {
	return s.confirm(ctx, u, ConfirmTOTPEnroll, StepUpProof{EmailCode: emailCode}, write)
}

// The confirmation window (D59): a right code for a covered Live action lets the session's next
// covered actions run without another, for a while. Rolling an API key and rotating a webhook
// secret are not covered and always take a code.
const (
	DefaultConfirmWindow        = tuning.DefaultConfirmWindow
	DefaultConfirmWindowWallets = tuning.DefaultConfirmWindowWallets
)

// ConfirmWindowView is a session's open confirmation window, as GET /v1/dashboard/me shows it.
type ConfirmWindowView struct {
	ExpiresAt   time.Time `json:"expires_at"`
	WalletsLeft int       `json:"wallets_left"`
}

// confirmWindow is the window's length (zero: off) and its cap on Live wallets.
func (s *Service) confirmWindow() (time.Duration, int) {
	d, n := DefaultConfirmWindow, DefaultConfirmWindowWallets
	if s.cfg.ConfirmWindow != nil {
		d = *s.cfg.ConfirmWindow
	}
	if s.cfg.ConfirmWindowWallets > 0 {
		n = s.cfg.ConfirmWindowWallets
	}
	return d, n
}

func (s *Service) confirmWindowView(until *time.Time, wallets int) *ConfirmWindowView {
	d, n := s.confirmWindow()
	if d <= 0 || until == nil || !until.After(s.store.Now()) {
		return nil
	}
	return &ConfirmWindowView{ExpiresAt: until.UTC(), WalletsLeft: max(n-wallets, 0)}
}

// windowConfirm guards a Live action the confirmation window covers. Without a code, inside the
// session's open window, write runs as is (a wallet only while the window's cap is not reached).
// Otherwise the action takes a code (liveConfirm), and a right one opens a new window in the
// action's transaction. Test mode asks for nothing.
func (s *Service) windowConfirm(ctx context.Context, du DashUser, livemode bool, action string, p StepUpProof, write func(b *Service) error) error {
	d, maxWallets := s.confirmWindow()
	if !livemode || d <= 0 || du.session == ([32]byte{}) {
		return s.liveConfirm(ctx, du, livemode, action, p, write)
	}
	wallet := action == ConfirmWalletCreate
	if (p.EmailCode == nil || *p.EmailCode == "") && (p.TOTP == nil || *p.TOTP == "") {
		used, err := s.store.UseConfirmWindow(ctx, du.session, wallet, maxWallets, func(st *store.Store) error {
			b := s.withStore(st)
			if err := b.store.Audit(ctx, du.MerchantID, du.actor(), "user.confirmation_window_used",
				map[string]any{"user_id": du.ID, "action": action}); err != nil {
				return err
			}
			return write(b)
		})
		if err != nil || used {
			return err
		}
	}
	return s.liveConfirm(ctx, du, livemode, action, p, func(b *Service) error {
		wallets := 0
		if wallet {
			wallets = 1
		}
		// The session row before the write, as the window path takes it, so the two never wait on
		// each other's locks; a refused write rolls the window back with it.
		if err := b.openWindow(ctx, du, action, d, wallets); err != nil {
			return err
		}
		return write(b)
	})
}

func unlockOff() *Error {
	return newErr(KindConflict, CodeInvalidState, "Unlocking is off on this server: each Live change asks for its own code.")
}

// openWindow opens the session's confirmation window for d, with wallets already added in it, and
// audits it with the action whose code opened it.
func (s *Service) openWindow(ctx context.Context, du DashUser, action string, d time.Duration, wallets int) error {
	if err := s.store.OpenConfirmWindow(ctx, du.session, s.store.Now().Add(d), wallets); err != nil {
		return err
	}
	return s.store.Audit(ctx, du.MerchantID, du.actor(), "user.confirmation_window_opened", map[string]any{"user_id": du.ID, "action": action})
}

// ConfirmWindowSeconds is the length of a confirmation window, 0 when the window is off; the
// dashboard names it in the Unlock dialog.
func (s *Service) ConfirmWindowSeconds() int {
	d, _ := s.confirmWindow()
	return int(max(d, 0) / time.Second)
}

// Unlock opens the session's confirmation window without an action (D61): a right emailed code
// sent for "unlock", or a current TOTP code, and the session's covered Live actions take no
// other code until the window ends. A recovery code never unlocks. Live mode only; an open
// window is replaced by a new one with its own wallet cap.
func (s *Service) Unlock(ctx context.Context, du DashUser, livemode bool, p StepUpProof) (*ConfirmWindowView, error) {
	d, _ := s.confirmWindow()
	switch {
	case !livemode:
		return nil, invalid("Test mode needs no code: there is nothing to unlock.")
	case d <= 0:
		return nil, unlockOff()
	case du.session == [32]byte{}:
		return nil, unauthenticated()
	}
	u, err := s.store.DashboardUser(ctx, du.ID)
	if err != nil {
		return nil, err
	}
	var until time.Time
	if err := s.confirm(ctx, u, ConfirmUnlock, p, func(b *Service) error {
		until = b.store.Now().Add(d)
		return b.openWindow(ctx, du, ConfirmUnlock, d, 0)
	}); err != nil {
		return nil, err
	}
	_, maxWallets := s.confirmWindow()
	return &ConfirmWindowView{ExpiresAt: until.UTC(), WalletsLeft: maxWallets}, nil
}

// Lock ends the session's confirmation window at once ("Lock now", D61); nothing to end is fine.
func (s *Service) Lock(ctx context.Context, du DashUser) error {
	if du.session == [32]byte{} {
		return unauthenticated()
	}
	closed, err := s.store.CloseConfirmWindow(ctx, du.session)
	if err != nil || !closed {
		return err
	}
	return s.store.Audit(ctx, du.MerchantID, du.actor(), "user.confirmation_window_closed", map[string]any{"user_id": du.ID})
}
