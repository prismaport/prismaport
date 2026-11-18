package service

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/prismaport/prismaport/internal/dashauth"
	"github.com/prismaport/prismaport/internal/ids"
	"github.com/prismaport/prismaport/internal/mail"
	"github.com/prismaport/prismaport/internal/store"
)

// Error codes of sign-up, email confirmation and password checks.
const (
	// CodeEmailUnverified refuses every dashboard route but /me and logout until the user
	// confirmed the email.
	CodeEmailUnverified = "email_unverified"
	// CodeWeakPassword refuses a new password outside the length bounds or missing a character
	// class; CodeCommonPassword one that is common or repeated; CodeBreachedPassword one found
	// in a known data breach.
	CodeWeakPassword     = "weak_password"
	CodeCommonPassword   = "common_password"
	CodeBreachedPassword = "breached_password"
	// CodeLiveReview refuses changes in live mode while a merchant's live mode is closed.
	CodeLiveReview = "live_review_pending"
	// CodeInvalidCode answers every failed email code check alike.
	CodeInvalidCode = "invalid_code"
	// CodeInvalidLink answers every password reset link that does not work (unknown, expired,
	// spent or replaced) alike.
	CodeInvalidLink = "invalid_link"

	// EmailCodeLifetime is how long an emailed code works.
	EmailCodeLifetime = 15 * time.Minute
	// ResetLinkLifetime is how long a password reset link works (once).
	ResetLinkLifetime = 30 * time.Minute
	// MaxCodeAttempts is how often one code may be tried; MaxCodeFailures how many wrong codes
	// one account takes per CodeFailureWindow across every code, so codes cannot be guessed by
	// asking for new ones.
	MaxCodeAttempts   = 5
	MaxCodeFailures   = 20
	CodeFailureWindow = 24 * time.Hour

	maxBusinessName = 200
	codeKeyLabel    = "dashboard_email_code"
)

func codeInvalid() *Error {
	return newErr(KindInvalid, CodeInvalidCode,
		"That code is not valid or has expired. Check the newest email from Prismaport, or send a new code.")
}

func linkInvalid() *Error {
	return newErr(KindInvalid, CodeInvalidLink,
		"This link no longer works. A reset link works for 30 minutes and only once, and a newer one replaces it. Ask for a new link.")
}

// EmailUnverified refuses a dashboard route to a user who has not confirmed the email yet.
func EmailUnverified() *Error {
	return newErr(KindForbidden, CodeEmailUnverified, "Confirm your email to continue. Enter the code we sent you.")
}

// LiveReviewPending refuses a change in live mode for a merchant whose live mode is not open.
func LiveReviewPending() *Error {
	return newErr(KindForbidden, CodeLiveReview, "Live payments open after a review. Use Test mode until then.")
}

// SignupInput is a self-serve sign-up.
type SignupInput struct {
	BusinessName string
	Email        string
	Password     string
}

func (s *Service) requireMail() error {
	if s.cfg.Mail == nil || s.cfg.SecretBox == nil {
		return newErr(KindUnavailable, CodeServiceUnavailable,
			"Accounts cannot be created on this server yet: email is not set up.")
	}
	return nil
}

func (s *Service) dashboardURL() string {
	if s.cfg.DashboardURL != "" {
		return s.cfg.DashboardURL
	}
	return strings.TrimRight(s.cfg.CheckoutBaseURL, "/") + "/dashboard/"
}

// BusinessName checks the name a merchant signs up with.
func BusinessName(raw string) (string, error) {
	name := strings.Join(strings.Fields(raw), " ")
	if name == "" || utf8.RuneCountInString(name) > maxBusinessName {
		return "", invalid("Enter your business name, up to %d characters.", maxBusinessName)
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", invalid("The business name has characters that cannot be shown.")
		}
	}
	return name, nil
}

// checkNewPassword applies the password policy to a password being created or changed; param
// is the request field it came from. Login and step-up never come here: they only compare.
//
// The breach check runs only for a password the rules accept, and fails open: a third party
// being down must not stop anyone from signing up or resetting a password (D55).
func (s *Service) checkNewPassword(ctx context.Context, pw, param string) error {
	if err := dashauth.CheckPassword(pw); err != nil {
		if errors.Is(err, dashauth.ErrCommonPassword) {
			return &Error{Kind: KindInvalid, Code: CodeCommonPassword, Param: param,
				Message: "This password is too easy to guess. Choose one that is harder to predict."}
		}
		return &Error{Kind: KindInvalid, Code: CodeWeakPassword, Param: param,
			Message: fmt.Sprintf("Use %d to %d characters with an uppercase letter, a lowercase letter, a number and a special character.", dashauth.MinPasswordLen, dashauth.MaxPasswordLen)}
	}
	if s.cfg.PasswordBreaches == nil {
		return nil
	}
	breached, err := s.cfg.PasswordBreaches.Breached(ctx, pw)
	if err != nil {
		s.Logger().Warn("password breach check failed, accepting the password", "err", err)
		return nil
	}
	if breached {
		return &Error{Kind: KindInvalid, Code: CodeBreachedPassword, Param: param,
			Message: "This password appeared in a data breach. Choose one you have not used anywhere else."}
	}
	return nil
}

func newCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

// codeHash keys the stored code to the user and its purpose, with a key derived from the
// service secret: a database read alone cannot test the million possible codes.
func (s *Service) codeHash(userID, purpose, code string) []byte {
	m := hmac.New(sha256.New, s.cfg.SecretBox.DeriveKey(codeKeyLabel))
	m.Write([]byte(userID + "\x00" + purpose + "\x00" + code))
	return m.Sum(nil)
}

func (s *Service) issue(userID, purpose string) (string, store.EmailCode, error) {
	code, err := newCode()
	if err != nil {
		return "", store.EmailCode{}, err
	}
	return code, store.EmailCode{Hash: s.codeHash(userID, purpose, code), ExpiresAt: s.store.Now().Add(EmailCodeLifetime)}, nil
}

// normalizeCode accepts "482 193" and "482-193"; anything else that is not 6 digits never matches.
func normalizeCode(code string) (string, bool) {
	c := strings.NewReplacer(" ", "", "-", "").Replace(strings.TrimSpace(code))
	if len(c) != 6 {
		return "", false
	}
	for _, r := range c {
		if r < '0' || r > '9' {
			return "", false
		}
	}
	return c, true
}

func (s *Service) send(ctx context.Context, m mail.Message, err error) {
	if err == nil {
		err = s.cfg.Mail.Send(ctx, m)
	}
	if err != nil {
		s.Logger().Error("email not queued", "kind", m.Kind, "err", err)
	}
}

func (s *Service) sendCode(ctx context.Context, to, code string) {
	m, err := mail.VerificationCode(to, mail.CodeEmail{Code: code, Minutes: int(EmailCodeLifetime / time.Minute), DashboardURL: s.dashboardURL()})
	s.send(ctx, m, err)
}

func (s *Service) sendNotice(ctx context.Context, kind, to string) {
	d := mail.NoticeEmail{Email: to, DashboardURL: s.dashboardURL(), At: s.store.Now()}
	if kind == mail.KindPasswordChanged {
		m, err := mail.PasswordChanged(to, d)
		s.send(ctx, m, err)
		return
	}
	m, err := mail.AccountExists(to, d)
	s.send(ctx, m, err)
}

func emailDigest(email string) string {
	sum := sha256.Sum256([]byte(email))
	return hex.EncodeToString(sum[:])
}

// Signup creates a merchant with live mode open (D52) and its owner, unconfirmed, and emails the
// confirmation code. The answer never says whether the email already had an account: an
// existing confirmed account gets a notice instead of a code, and an unconfirmed one only gets
// a fresh code (sendConfirmation). Its password and name stay: anyone can sign up with any
// email, so a later sign-up must not set the password the owner's code then confirms (ADR 0008
// C-1). Entering the code sets the password of whoever entered it (VerifyEmail).
func (s *Service) Signup(ctx context.Context, in SignupInput) error {
	if err := s.requireMail(); err != nil {
		return err
	}
	name, err := BusinessName(in.BusinessName)
	if err != nil {
		return err
	}
	email, err := NormalizeEmail(in.Email)
	if err != nil {
		return err
	}
	if err := s.checkNewPassword(ctx, in.Password, "password"); err != nil {
		return err
	}
	// Hashed whatever happens next, so the answer takes as long for a taken email.
	hash, err := s.cfg.Hasher.HashPassword(ctx, in.Password, dashauth.DefaultParams)
	if err != nil {
		return err
	}
	u, err := s.store.DashboardUserByEmail(ctx, email)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return s.createAccount(ctx, name, email, hash)
	case err != nil:
		return err
	case u.DisabledAt != nil:
		return s.store.Audit(ctx, u.MerchantID, "signup:"+u.ID, "user.signup_refused", map[string]any{"user_id": u.ID, "reason": "disabled"})
	case u.EmailVerified():
		if err := s.store.Audit(ctx, u.MerchantID, "signup:"+u.ID, "user.signup_existing", map[string]any{"user_id": u.ID}); err != nil {
			return err
		}
		s.sendNotice(ctx, mail.KindAccountExists, email)
		return nil
	}
	err = s.sendConfirmation(ctx, u, email, func(c store.EmailCode) error { return s.store.RestartSignup(ctx, u.ID, c) })
	if errors.Is(err, store.ErrConflict) {
		s.sendNotice(ctx, mail.KindAccountExists, email)
		return nil
	}
	return err
}

// sendConfirmation emails an unconfirmed account the means to confirm it: a fresh code, stored
// by save. When the account took MaxCodeFailures wrong codes in the window, no code would be
// checked, and anyone who knows the email can spend that budget; the owner then gets a reset
// link instead, which confirms the email when used and is not counted against codes (ADR 0008
// C-4). The budget itself stays, so asking for new codes never buys more guesses.
func (s *Service) sendConfirmation(ctx context.Context, u store.DashboardUser, email string, save func(store.EmailCode) error) error {
	spent, err := s.store.CodeBudgetSpent(ctx, u.ID, MaxCodeFailures, CodeFailureWindow)
	if err != nil {
		return err
	}
	if spent {
		return s.sendResetLink(ctx, u, email)
	}
	code, c, err := s.issue(u.ID, store.CodeVerifyEmail)
	if err != nil {
		return err
	}
	if err := save(c); err != nil {
		return err
	}
	s.sendCode(ctx, email, code)
	return nil
}

func (s *Service) createAccount(ctx context.Context, name, email, hash string) error {
	now := s.store.Now()
	userID := ids.NewAt(ids.User, now)
	code, c, err := s.issue(userID, store.CodeVerifyEmail)
	if err != nil {
		return err
	}
	err = s.store.CreateAccount(ctx, store.NewAccount{MerchantID: ids.NewAt(ids.Merchant, now), MerchantName: name, UserID: userID, Email: email,
		PasswordHash: hash, TestAssets: s.offeredAssets(false), LiveAssets: s.offeredAssets(true), Code: c})
	if errors.Is(err, store.ErrConflict) {
		// Another sign-up with this email won the race; its owner gets that one's code.
		return nil
	}
	if err != nil {
		return err
	}
	s.sendCode(ctx, email, code)
	return nil
}

// ResendVerification emails a new confirmation code to an account that has not confirmed its
// email. It answers the same whatever the email.
func (s *Service) ResendVerification(ctx context.Context, email string) error {
	if err := s.requireMail(); err != nil {
		return err
	}
	e, err := NormalizeEmail(email)
	if err != nil {
		return nil
	}
	u, err := s.store.DashboardUserByEmail(ctx, e)
	if errors.Is(err, store.ErrNotFound) {
		return s.store.AuditAnonymous(ctx, "signup", "auth.unknown_email", map[string]any{"route": "resend_code", "email_sha256": emailDigest(e)})
	}
	if err != nil || u.DisabledAt != nil || u.EmailVerified() {
		return err
	}
	return s.sendConfirmation(ctx, u, e, func(c store.EmailCode) error {
		return s.store.IssueCode(ctx, u, store.CodeVerifyEmail, c, "user.verification_sent")
	})
}

// check prepares one attempt at a code of purpose; false when the email or the code cannot be
// one (nothing is looked up then).
func (s *Service) check(email, code, purpose string) (store.CodeCheck, bool) {
	e, err := NormalizeEmail(email)
	c, ok := normalizeCode(code)
	if err != nil || !ok {
		return store.CodeCheck{}, false
	}
	return store.CodeCheck{Email: e, MaxAttempts: MaxCodeAttempts, MaxFailures: MaxCodeFailures, Window: CodeFailureWindow,
		Matches: func(userID string, stored []byte) bool { return hmac.Equal(stored, s.codeHash(userID, purpose, c)) }}, true
}

// VerifyEmail confirms the email with the emailed code. keep is the session of the browser that
// entered it, when it belongs to this user: it stays signed in, every other session ends.
//
// The code proves the email; it must also be tied to the password, or whoever set the password
// owns the account once the owner enters the code (ADR 0008 C-1). A browser signed in as this
// user proved the password when it signed in (the password never changes before confirmation
// without ending sessions). Any other browser sends password: it becomes the account's password
// when the code is right, so the password always belongs to whoever holds the email.
func (s *Service) VerifyEmail(ctx context.Context, email, code string, password *string, keep *DashSession) error {
	if err := s.requireMail(); err != nil {
		return err
	}
	e, _ := NormalizeEmail(email)
	signedIn := keep != nil && e != "" && keep.User.Email == e
	var pwHash *string
	if !signedIn {
		if password == nil || *password == "" {
			return &Error{Kind: KindInvalid, Code: CodeInvalidRequest, Param: "password",
				Message: "Enter the password for this account, or sign in first."}
		}
		if err := s.checkNewPassword(ctx, *password, "password"); err != nil {
			return err
		}
	}
	chk, ok := s.check(email, code, store.CodeVerifyEmail)
	if !ok {
		return codeInvalid()
	}
	var hash *[32]byte
	if signedIn {
		hash = &keep.TokenHash
	} else {
		h, err := s.cfg.Hasher.HashPassword(ctx, *password, dashauth.DefaultParams)
		if err != nil {
			return err
		}
		pwHash = &h
	}
	if _, err := s.store.VerifyEmail(ctx, chk, hash, pwHash); err != nil {
		if errors.Is(err, store.ErrCodeInvalid) {
			return codeInvalid()
		}
		return err
	}
	return nil
}

// ForgotPassword emails a reset link to any account with this email. It answers the same
// whatever the email. The link carries a random token; only its SHA-256 is stored, and a new
// link replaces the one before.
func (s *Service) ForgotPassword(ctx context.Context, email string) error {
	if err := s.requireMail(); err != nil {
		return err
	}
	e, err := NormalizeEmail(email)
	if err != nil {
		return nil
	}
	u, err := s.store.DashboardUserByEmail(ctx, e)
	if errors.Is(err, store.ErrNotFound) {
		return s.store.AuditAnonymous(ctx, "signup", "auth.unknown_email", map[string]any{"route": "password_forgot", "email_sha256": emailDigest(e)})
	}
	if err != nil || u.DisabledAt != nil {
		return err
	}
	return s.sendResetLink(ctx, u, e)
}

// sendResetLink stores a new password reset link for u (replacing the one before) and emails it.
func (s *Service) sendResetLink(ctx context.Context, u store.DashboardUser, email string) error {
	token, hash, err := dashauth.NewToken()
	if err != nil {
		return err
	}
	c := store.EmailCode{Hash: hash[:], ExpiresAt: s.store.Now().Add(ResetLinkLifetime)}
	if err := s.store.IssueCode(ctx, u, store.CodePasswordReset, c, "user.password_reset_requested"); err != nil {
		return err
	}
	m, err := mail.PasswordResetLink(email, mail.ResetLinkEmail{Token: token, Minutes: int(ResetLinkLifetime / time.Minute), DashboardURL: s.dashboardURL()})
	s.send(ctx, m, err)
	return nil
}

// ResetLink is what the page behind a working reset link shows: the account, and whether it
// must also ask for the authenticator code.
type ResetLink struct {
	Email        string `json:"email"`
	TOTPRequired bool   `json:"totp_required"`
}

// resetLinkUser is the user of a working reset link and the link's stored hash; every other
// link is linkInvalid.
func (s *Service) resetLinkUser(ctx context.Context, token string) (store.DashboardUser, []byte, error) {
	hash, err := dashauth.HashToken(token)
	if err != nil {
		return store.DashboardUser{}, nil, linkInvalid()
	}
	u, err := s.store.ResetLinkUser(ctx, hash[:])
	if errors.Is(err, store.ErrCodeInvalid) {
		return store.DashboardUser{}, nil, linkInvalid()
	}
	return u, hash[:], err
}

// CheckResetLink tells the reset page whether its link works, without spending it: mail
// scanners and link previews open links, and only choosing the password spends one.
func (s *Service) CheckResetLink(ctx context.Context, token string) (ResetLink, error) {
	if err := s.requireMail(); err != nil {
		return ResetLink{}, err
	}
	u, _, err := s.resetLinkUser(ctx, token)
	if err != nil {
		return ResetLink{}, err
	}
	return ResetLink{Email: u.Email, TOTPRequired: u.TOTPEnabled()}, nil
}

// ResetPassword sets a new password with the emailed reset link. With 2FA on it also takes a
// current authenticator code, so the email alone cannot take the account. Every session of the
// account ends, the email counts as confirmed (the link proved it) and the owner is told by
// email. Two-factor authentication stays as it was. A recovery code may stand in for the
// authenticator code.
func (s *Service) ResetPassword(ctx context.Context, token, newPassword, totp, recoveryCode string) error {
	if err := s.requireMail(); err != nil {
		return err
	}
	u, tokenHash, err := s.resetLinkUser(ctx, token)
	if err != nil {
		return err
	}
	if err := s.checkNewPassword(ctx, newPassword, "new_password"); err != nil {
		return err
	}
	if u.TOTPEnabled() {
		hasTOTP, hasRecovery := strings.TrimSpace(totp) != "", strings.TrimSpace(recoveryCode) != ""
		if hasTOTP && hasRecovery {
			return &Error{Kind: KindInvalid, Code: CodeInvalidRequest, Param: "recovery_code", Message: "Send a code or a recovery code, not both."}
		}
		if !hasTOTP && !hasRecovery {
			return newErr(KindUnauthorized, CodeTOTPRequired, "Enter the 6-digit code from your authenticator app.")
		}
		reason, check, code, wrong := "totp", s.signInTOTP, totp, "That code is not valid. Enter the current code from your authenticator app."
		if hasRecovery {
			reason, check, code, wrong = "recovery_code", s.signInRecoveryCode, recoveryCode, "That recovery code is not valid or was already used."
		}
		ok, err := check(ctx, u, code)
		if err != nil {
			return err
		}
		if !ok {
			if err := s.store.Audit(ctx, u.MerchantID, "dashboard:"+u.ID, "user.password_reset_failed", map[string]any{"user_id": u.ID, "reason": reason}); err != nil {
				return err
			}
			return newErr(KindUnauthorized, CodeTOTPRequired, "%s", wrong)
		}
	}
	hash, err := s.cfg.Hasher.HashPassword(ctx, newPassword, dashauth.DefaultParams)
	if err != nil {
		return err
	}
	u, err = s.store.ResetPassword(ctx, tokenHash, hash)
	if errors.Is(err, store.ErrCodeInvalid) {
		return linkInvalid()
	}
	if err != nil {
		return err
	}
	s.sendNotice(ctx, mail.KindPasswordChanged, u.Email)
	return nil
}

// offeredAssets is every asset the pilot allows in a mode, as a new account accepts them.
func (s *Service) offeredAssets(livemode bool) []store.ChainAsset {
	var assets []store.ChainAsset
	for _, a := range s.OfferedAssets(livemode) {
		assets = append(assets, store.ChainAsset{Chain: a.Chain, Asset: a.Asset})
	}
	return assets
}
