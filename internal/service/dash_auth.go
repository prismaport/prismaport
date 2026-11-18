package service

import (
	"bytes"
	"context"
	"errors"
	netmail "net/mail"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/prismaport/prismaport/internal/apikey"
	"github.com/prismaport/prismaport/internal/dashauth"
	"github.com/prismaport/prismaport/internal/ids"
	"github.com/prismaport/prismaport/internal/mail"
	"github.com/prismaport/prismaport/internal/store"
)

// Error codes of dashboard authentication.
const (
	CodeUnauthenticated = "unauthenticated"
	CodeCSRFFailed      = "csrf_failed"
	CodeTOTPRequired    = "totp_required"
	CodeInvalidPassword = "invalid_password"
)

// Dashboard session lifetimes: a session ends after DashboardIdle without a request or
// DashboardLifetime after login, whichever comes first.
const (
	DashboardIdle     = 12 * time.Hour
	DashboardLifetime = 7 * 24 * time.Hour
	dashboardTouch    = time.Minute
	totpIssuer        = "Prismaport"
)

var (
	loginFailed = newErr(KindUnauthorized, CodeUnauthenticated, "Email or password is incorrect.")
	// With the right password and 2FA on, a sign-in asks for the second factor by name.
	loginCodeMissing     = newErr(KindUnauthorized, CodeTOTPRequired, "Enter the 6-digit code from your authenticator app.")
	loginCodeWrong       = newErr(KindUnauthorized, CodeTOTPRequired, "That code is not valid. Enter the current code from your authenticator app.")
	loginRecoveryWrong   = newErr(KindUnauthorized, CodeTOTPRequired, "That recovery code is not valid or was already used.")
	loginCodeAndRecovery = newErr(KindInvalid, CodeInvalidRequest, "Send the authenticator code or a recovery code, not both.")
)

func unauthenticated() *Error {
	return newErr(KindUnauthorized, CodeUnauthenticated, "Sign in to continue.")
}

// DashUser is the signed-in dashboard user.
type DashUser struct {
	ID           string
	MerchantID   string
	MerchantName string
	Email        string
	TOTPEnabled  bool
	// EmailVerified is false until the user entered the emailed code: the dashboard then opens
	// only the confirmation screen.
	EmailVerified bool
	// LiveEnabled is false while the merchant's live mode is closed: live mode is read-only.
	LiveEnabled bool
	// LogoID is the merchant's checkout logo (nil when it has none).
	LogoID *string
	// RecoveryCodesLeft counts the unused recovery codes; TOTPRemovalAt is the time of a pending
	// removal of 2FA; RecoveryLockedUntil is the end of the lock on live credentials after one.
	RecoveryCodesLeft   int
	TOTPRemovalAt       *time.Time
	RecoveryLockedUntil *time.Time
	// session is the hash of the dashboard session acting, whose confirmation window (D59) a
	// covered Live action may use; zero outside a session.
	session [32]byte
}

func (u DashUser) actor() string { return "dashboard:" + u.ID }

// Auth acts for the user's merchant in one mode with every scope.
func (u DashUser) Auth(livemode bool) Auth {
	scopes := make([]string, 0, len(apikey.AllScopes))
	for _, sc := range apikey.AllScopes {
		scopes = append(scopes, string(sc))
	}
	return Auth{KeyID: "dashboard", MerchantID: u.MerchantID, Livemode: livemode, Scopes: scopes, Actor: u.actor()}
}

// DashSession is an authenticated dashboard session: its user, CSRF token, token hash and expiry.
type DashSession struct {
	User      DashUser
	CSRFToken string
	TokenHash [32]byte
	ExpiresAt time.Time
	// UsedRecoveryCode is true on the session of a sign-in that spent a recovery code.
	UsedRecoveryCode bool
	// ConfirmWindow is the session's open confirmation window (D59), nil when there is none.
	ConfirmWindow *ConfirmWindowView
}

func dashUser(u store.DashboardUser) DashUser {
	return DashUser{ID: u.ID, MerchantID: u.MerchantID, MerchantName: u.MerchantName, Email: u.Email, TOTPEnabled: u.TOTPEnabled(),
		EmailVerified: u.EmailVerified(), LiveEnabled: u.MerchantLiveEnabled, LogoID: u.MerchantLogoID,
		RecoveryCodesLeft: u.RecoveryCodesLeft, TOTPRemovalAt: u.TOTPRemovalAt, RecoveryLockedUntil: u.RecoveryLockedUntil}
}

// NormalizeEmail lowercases and checks an email address.
func NormalizeEmail(email string) (string, error) {
	e := strings.ToLower(strings.TrimSpace(email))
	a, err := netmail.ParseAddress(e)
	if err != nil || a.Address != e || len(e) > 254 || !strings.Contains(e, "@") {
		return "", invalid("email must be a plain address such as owner@example.com")
	}
	return e, nil
}

// CreateDashboardUser adds a user with a confirmed email to a merchant. The password is hashed
// with argon2id; the plaintext is never stored.
func (s *Service) CreateDashboardUser(ctx context.Context, merchantID, email, password, actor string) (store.DashboardUser, error) {
	e, err := NormalizeEmail(email)
	if err != nil {
		return store.DashboardUser{}, err
	}
	if err := s.checkNewPassword(ctx, password, "password"); err != nil {
		return store.DashboardUser{}, err
	}
	if _, err := s.store.GetMerchant(ctx, merchantID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.DashboardUser{}, invalid("no merchant %s", merchantID)
		}
		return store.DashboardUser{}, err
	}
	hash, err := s.cfg.Hasher.HashPassword(ctx, password, dashauth.DefaultParams)
	if err != nil {
		return store.DashboardUser{}, err
	}
	u, err := s.store.CreateDashboardUser(ctx, store.DashboardUser{ID: ids.NewAt(ids.User, s.store.Now()), MerchantID: merchantID, Email: e, PasswordHash: hash}, actor)
	if errors.Is(err, store.ErrConflict) {
		return store.DashboardUser{}, invalid("a user with this email already exists")
	}
	return u, err
}

// Login checks the password (and the TOTP code when 2FA is on) and opens a session. It returns
// the session and the cookie token, which is shown only here. previous is the token the browser
// still holds, if any; that session is ended so a login always gets a fresh one.
//
// An unknown email and a wrong password answer the same 401 unauthenticated. With the right
// password and 2FA on, a missing or wrong code answers 401 totp_required, so the page asks for
// the code only for accounts that have it. That confirms the password to whoever typed it; the
// per-email limit and the code wait still stand between them and the account (the owner chose
// this over the uniform answer of ADR 0008 C-5 on 2026-11-16).
//
// A recovery code may stand in for the TOTP code. It is looked at only with the right password
// and 2FA on, counts in the same sign-in run, and is still checked during its wait. Every
// password check counts toward the run of wrong passwords that emails the owner. A sign-in that
// passed a second factor cancels a pending removal.
func (s *Service) Login(ctx context.Context, email, password, totp, recoveryCode, previous string) (DashSession, string, error) {
	u, found, err := s.loginUser(ctx, email, password)
	if err != nil {
		return DashSession{}, "", err
	}
	if !found {
		return DashSession{}, "", loginFailed
	}
	reason, refusal, err := s.loginFailure(ctx, u, password, totp, recoveryCode)
	if err != nil {
		return DashSession{}, "", err
	}
	if reason != "" {
		if err := s.store.Audit(ctx, u.MerchantID, "dashboard:"+u.ID, "user.login_failed", map[string]any{"user_id": u.ID, "reason": reason}); err != nil {
			return DashSession{}, "", err
		}
		return DashSession{}, "", refusal
	}
	return s.openDashboardSession(ctx, u, u.TOTPEnabled() && strings.TrimSpace(recoveryCode) != "", previous)
}

// loginUser finds the account of a sign-in. An invalid or unknown email still spends a password
// check, so it takes as long as a real account's; found is false then.
func (s *Service) loginUser(ctx context.Context, email, password string) (u store.DashboardUser, found bool, err error) {
	e, err := NormalizeEmail(email)
	if err == nil {
		u, err = s.store.DashboardUserByEmail(ctx, e)
		if err == nil {
			return u, true, nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return u, false, err
		}
	}
	return u, false, s.cfg.Hasher.BurnPasswordCheck(ctx, password)
}

// loginFailure checks the password and, with 2FA on, the TOTP or recovery code. It returns why
// the sign-in fails, for the audit entry, and the answer to give, or "" when it passes.
func (s *Service) loginFailure(ctx context.Context, u store.DashboardUser, password, totp, recoveryCode string) (string, *Error, error) {
	hasTOTP, hasRecovery := strings.TrimSpace(totp) != "", strings.TrimSpace(recoveryCode) != ""
	ok, err := s.cfg.Hasher.VerifyPassword(ctx, u.PasswordHash, password)
	if err != nil {
		return "", nil, err
	}
	if u.DisabledAt == nil {
		if err := s.passwordChecked(ctx, u, ok); err != nil {
			return "", nil, err
		}
	}
	switch {
	case !ok || u.DisabledAt != nil:
		return "password", loginFailed, nil
	case !u.TOTPEnabled():
		return "", nil, nil
	case !hasTOTP && !hasRecovery:
		return "totp_missing", loginCodeMissing, nil
	case hasTOTP && hasRecovery:
		return "totp_and_recovery_code", loginCodeAndRecovery, nil
	case hasRecovery:
		return s.secondFactorFailure(ctx, u, "recovery_code", loginRecoveryWrong, s.signInRecoveryCode, recoveryCode)
	}
	return s.secondFactorFailure(ctx, u, "totp", loginCodeWrong, s.signInTOTP, totp)
}

// secondFactorFailure checks a TOTP or recovery code with check. A wrong code returns wrong
// (its audit reason) and refusal; while wrong codes make the user wait, "totp_wait" and the
// wait itself. A right code returns "".
func (s *Service) secondFactorFailure(ctx context.Context, u store.DashboardUser, wrong string, refusal *Error,
	check func(context.Context, store.DashboardUser, string) (bool, error), code string) (string, *Error, error) {
	ok, err := check(ctx, u, code)
	var wait *Error
	switch {
	case errors.As(err, &wait) && wait.Code == CodeRateLimited:
		return "totp_wait", wait, nil
	case err != nil:
		return "", nil, err
	case !ok:
		return wrong, refusal, nil
	}
	return "", nil, nil
}

// openDashboardSession starts the session of a sign-in that passed, replacing previous (the
// browser's earlier session token, if any). A sign-in that passed a second factor cancels a
// pending removal of 2FA.
func (s *Service) openDashboardSession(ctx context.Context, u store.DashboardUser, usedRecovery bool, previous string) (DashSession, string, error) {
	token, hash, err := dashauth.NewToken()
	if err != nil {
		return DashSession{}, "", err
	}
	csrf, _, err := dashauth.NewToken()
	if err != nil {
		return DashSession{}, "", err
	}
	var prev *[32]byte
	if previous != "" {
		if h, err := dashauth.HashToken(previous); err == nil {
			prev = &h
		}
	}
	ds, err := s.store.CreateDashboardSession(ctx, u.ID, hash, csrf, DashboardLifetime, DashboardIdle, prev)
	if err != nil {
		return DashSession{}, "", err
	}
	if err := s.store.Audit(ctx, u.MerchantID, "dashboard:"+u.ID, "user.login", map[string]any{"user_id": u.ID, "recovery_code": usedRecovery}); err != nil {
		return DashSession{}, "", err
	}
	du := dashUser(u)
	if u.TOTPEnabled() {
		cancelled, err := s.store.CancelTOTPRemoval(ctx, u, "dashboard:"+u.ID, "sign_in")
		if err != nil {
			return DashSession{}, "", err
		}
		if cancelled {
			du.TOTPRemovalAt = nil
			s.sendRemovalCancelled(ctx, u.Email)
		}
		if usedRecovery {
			du.RecoveryCodesLeft = max(du.RecoveryCodesLeft-1, 0)
		}
	}
	return DashSession{User: du, CSRFToken: csrf, TokenHash: hash, ExpiresAt: ds.ExpiresAt, UsedRecoveryCode: usedRecovery}, token, nil
}

// DashboardSession resolves a cookie token to its live session.
func (s *Service) DashboardSession(ctx context.Context, token string) (DashSession, error) {
	hash, err := dashauth.HashToken(token)
	if err != nil {
		return DashSession{}, unauthenticated()
	}
	ds, err := s.store.DashboardSessionByToken(ctx, hash, DashboardIdle, dashboardTouch)
	if errors.Is(err, store.ErrNotFound) {
		return DashSession{}, unauthenticated()
	}
	if err != nil {
		return DashSession{}, err
	}
	du := dashUser(ds.User)
	du.session = ds.TokenHash
	return DashSession{User: du, CSRFToken: ds.CSRFToken, TokenHash: ds.TokenHash, ExpiresAt: ds.ExpiresAt,
		ConfirmWindow: s.confirmWindowView(ds.ConfirmedUntil, ds.ConfirmedWallets)}, nil
}

// Logout deletes the dashboard session so its cookie no longer authenticates.
func (s *Service) Logout(ctx context.Context, ds DashSession) error {
	if err := s.store.DeleteDashboardSession(ctx, ds.TokenHash); err != nil {
		return err
	}
	return s.store.Audit(ctx, ds.User.MerchantID, ds.User.actor(), "user.logout", map[string]any{"user_id": ds.User.ID})
}

func (s *Service) totpAAD(userID string) []byte { return []byte("dashboard_totp/" + userID) }

func (s *Service) openTOTP(userID string, sealed []byte) ([]byte, error) {
	if s.cfg.SecretBox == nil {
		return nil, errors.New("service: no secret box configured")
	}
	return s.cfg.SecretBox.Open(sealed, s.totpAAD(userID))
}

// TOTP attempts per user, in two runs (store.TOTPRun): one for the codes entered before a session
// (sign-in, password reset) and one for the step-ups inside a session, so someone who knows the
// password and keeps entering wrong codes never makes the signed-in owner wait. In each run the
// first TOTPFreeFailures wrong codes in a row cost nothing, then each one makes the next attempt
// wait twice as long as the one before, from TOTPBaseDelay up to TOTPMaxDelay. A growing delay
// rather than a long lock, and a recovery code is checked even while a wait runs (it has 50
// bits, so guessing one is not the concern), so the owner can always sign in or reset the
// password with one. The owner is emailed when a run reaches TOTPFreeFailures, again at five
// times that and then at every twenty times (failureNotice). An accepted code starts its run over.
const (
	TOTPFreeFailures = 5
	TOTPBaseDelay    = time.Minute
	TOTPMaxDelay     = 15 * time.Minute
)

// totpDelay is how long the next TOTP attempt waits after attempt wrong codes in a row.
func totpDelay(attempt int) time.Duration {
	if attempt < TOTPFreeFailures {
		return 0
	}
	return min(TOTPBaseDelay<<min(attempt-TOTPFreeFailures, 10), TOTPMaxDelay)
}

// failureNotice reports whether the n-th failure in a row emails the owner: at first, at five
// times first, then at every twenty times first, so a long attack is still reported now and then
// without one email per failure.
func failureNotice(n, first int) bool {
	return n == first || n == 5*first || n > 0 && n%(20*first) == 0
}

func totpWait(wait time.Duration) *Error {
	minutes := int((wait + time.Minute - 1) / time.Minute)
	unit := "minutes"
	if minutes == 1 {
		unit = "minute"
	}
	e := newErr(KindRateLimited, CodeRateLimited, "Too many wrong codes. Wait %d %s before entering another code.", minutes, unit)
	e.RetryAfter = wait
	return e
}

// checkTOTP accepts a code for the user's active secret at most once, counted in run. err is an
// internal failure or a wait after too many wrong codes, not a wrong code.
func (s *Service) checkTOTP(ctx context.Context, u store.DashboardUser, run store.TOTPRun, code string) (bool, error) {
	return s.checkTOTPFor(ctx, u, run, code, nil)
}

// signInTOTP is checkTOTP before a session (sign-in, password reset).
func (s *Service) signInTOTP(ctx context.Context, u store.DashboardUser, code string) (bool, error) {
	return s.checkTOTP(ctx, u, store.TOTPRunSignIn, code)
}

// checkTOTPFor is checkTOTP for an action: write, when set, runs in the transaction that uses
// the code, so a refused action leaves the code unused (useTOTPStep); its error is returned.
func (s *Service) checkTOTPFor(ctx context.Context, u store.DashboardUser, run store.TOTPRun, code string, write func(b *Service) error) (bool, error) {
	secret, err := s.openTOTP(u.ID, u.TOTPSecret)
	if err != nil {
		return false, err
	}
	attempt, wait, err := s.store.TakeTOTPAttempt(ctx, u.ID, run, totpDelay)
	if err != nil {
		return false, err
	}
	if wait > 0 {
		return false, totpWait(wait)
	}
	step, ok := dashauth.VerifyTOTP(secret, strings.TrimSpace(code), s.store.Now(), u.TOTPLastStep)
	if ok {
		if ok, err = s.useTOTPStep(ctx, u.ID, run, step, write); err != nil || ok {
			return ok, err
		}
	}
	if failureNotice(attempt, TOTPFreeFailures) {
		s.sendTOTPFailures(ctx, u.Email, attempt)
	}
	return false, nil
}

// useTOTPStep records a verified step as used and runs write, when set, in the same
// transaction. A refused write rolls the step back with it, and the right code still ends the
// run of failed attempts (TakeTOTPAttempt counted it before it was checked).
func (s *Service) useTOTPStep(ctx context.Context, userID string, run store.TOTPRun, step int64, write func(b *Service) error) (bool, error) {
	if write == nil {
		return s.store.UseTOTPStep(ctx, userID, run, step)
	}
	used := false
	err := pgx.BeginFunc(ctx, s.store.Pool(), func(tx pgx.Tx) error {
		b := s.bind(tx)
		var err error
		if used, err = b.store.UseTOTPStep(ctx, userID, run, step); err != nil || !used {
			return err
		}
		return write(b)
	})
	if err != nil {
		if used {
			if rerr := s.store.EndTOTPFailures(context.WithoutCancel(ctx), userID, run); rerr != nil {
				s.Logger().Error("totp failures not reset after a refused action", "err", rerr)
			}
		}
		return false, err
	}
	return used, nil
}

// PasswordNoticeFailures is how many wrong passwords in a row (sign-in and the lost authenticator
// link) first email the owner; failureNotice says when again. Wrong passwords slow nothing down
// per account, so they cannot be used to lock the owner out: the per (email, client IP) and per
// email rate limits bound the guessing.
const PasswordNoticeFailures = 10

// passwordChecked records the outcome of a password check of u: a right password ends the run of
// wrong ones, a wrong one extends it and may email the owner.
func (s *Service) passwordChecked(ctx context.Context, u store.DashboardUser, ok bool) error {
	if ok {
		return s.store.EndPasswordFailures(ctx, u.ID)
	}
	n, err := s.store.RecordPasswordFailure(ctx, u.ID)
	if err != nil {
		return err
	}
	if failureNotice(n, PasswordNoticeFailures) {
		s.sendPasswordFailures(ctx, u.Email, n)
	}
	return nil
}

// sendPasswordFailures emails the owner about a run of wrong passwords; a server without email
// only logs it (the audit log has every failure).
func (s *Service) sendPasswordFailures(ctx context.Context, to string, failures int) {
	if s.cfg.Mail == nil {
		s.Logger().Warn("wrong passwords in a row; no email configured to tell the owner", "failures", failures)
		return
	}
	m, err := mail.PasswordFailures(to, mail.PasswordFailuresEmail{NoticeEmail: mail.NoticeEmail{Email: to, DashboardURL: s.dashboardURL(), At: s.store.Now()},
		Failures: failures})
	s.send(ctx, m, err)
}

// sendTOTPFailures emails the owner about a run of wrong codes; a server without email only
// logs it (the audit log has every failure).
func (s *Service) sendTOTPFailures(ctx context.Context, to string, failures int) {
	if s.cfg.Mail == nil {
		s.Logger().Warn("wrong two-factor codes in a row; no email configured to tell the owner", "failures", failures)
		return
	}
	m, err := mail.TOTPFailures(to, mail.TOTPFailuresEmail{NoticeEmail: mail.NoticeEmail{Email: to, DashboardURL: s.dashboardURL(), At: s.store.Now()},
		Failures: failures})
	s.send(ctx, m, err)
}

// TOTPEnrollment is a new authenticator secret and its otpauth URL, shown while the user sets up
// 2FA.
type TOTPEnrollment struct {
	Secret     string `json:"secret"`
	OTPAuthURL string `json:"otpauth_url"`
}

// TOTPEnrollmentWindow is how long a started enrollment waits for its first code.
const TOTPEnrollmentWindow = 10 * time.Minute

// EnrollTOTP starts 2FA, or replaces the authenticator when 2FA is already on: a new secret waits
// until VerifyTOTP proves the app holds it. Turning 2FA on takes a code emailed for it, so a
// stolen session cannot put its own second factor on the account; replacing the authenticator
// takes a current code or a recovery code (the way to a new phone), never the emailed code
// (D58). The enrollment belongs to this session and expires after TOTPEnrollmentWindow.
func (s *Service) EnrollTOTP(ctx context.Context, ds DashSession, emailCode, code, recoveryCode *string) (TOTPEnrollment, error) {
	if s.cfg.SecretBox == nil {
		return TOTPEnrollment{}, errors.New("service: no secret box configured")
	}
	u, err := s.store.DashboardUser(ctx, ds.User.ID)
	if err != nil {
		return TOTPEnrollment{}, err
	}
	// Two-factor sign-in needs a confirmed email: 2FA would otherwise lock the email's owner out
	// of the reset link of an account someone else set up (ADR 0008 C-1).
	if !u.EmailVerified() {
		return TOTPEnrollment{}, EmailUnverified()
	}
	secret, err := dashauth.NewTOTPSecret()
	if err != nil {
		return TOTPEnrollment{}, err
	}
	sealed, err := s.cfg.SecretBox.Seal(secret, s.totpAAD(u.ID))
	if err != nil {
		return TOTPEnrollment{}, err
	}
	expires := s.store.Now().Add(TOTPEnrollmentWindow)
	pending := func(b *Service) error {
		err := b.store.SetTOTPPending(ctx, u.ID, sealed, ds.TokenHash, expires, u.TOTPSecret, ds.User.actor())
		if errors.Is(err, store.ErrConflict) {
			return invalid("two-factor authentication changed meanwhile; reload and start again")
		}
		return err
	}
	if u.TOTPEnabled() {
		if err := s.stepUpOrRecovery(ctx, u, code, recoveryCode); err != nil {
			return TOTPEnrollment{}, err
		}
		if err := pending(s); err != nil {
			return TOTPEnrollment{}, err
		}
	} else if err := s.confirmTOTPEnroll(ctx, u, emailCode, pending); err != nil {
		return TOTPEnrollment{}, err
	}
	return TOTPEnrollment{Secret: dashauth.EncodeSecret(secret), OTPAuthURL: dashauth.OTPAuthURL(totpIssuer, u.Email, secret)}, nil
}

// enrollmentOpen reports whether the user's pending TOTP secret was started by the session with
// this token hash and has not expired.
func enrollmentOpen(u store.DashboardUser, tokenHash [32]byte, now time.Time) bool {
	return u.TOTPPending != nil && u.TOTPPendingExpiresAt != nil && bytes.Equal(u.TOTPPendingSession, tokenHash[:]) &&
		now.Before(*u.TOTPPendingExpiresAt)
}

// VerifyTOTP turns 2FA on (or switches to the new authenticator) with the first valid code for
// the pending secret. Only the session that started the enrollment can finish it, before it
// expires. Turning it on for the first time returns the recovery codes, shown only here; a
// replace returns none, keeps the codes, ends every other session and emails the owner.
func (s *Service) VerifyTOTP(ctx context.Context, ds DashSession, code string) ([]string, error) {
	u, err := s.store.DashboardUser(ctx, ds.User.ID)
	if err != nil {
		return nil, err
	}
	if !u.EmailVerified() {
		return nil, EmailUnverified()
	}
	if !enrollmentOpen(u, ds.TokenHash, s.store.Now()) {
		return nil, invalid("start the two-factor setup again")
	}
	secret, err := s.openTOTP(u.ID, u.TOTPPending)
	if err != nil {
		return nil, err
	}
	step, ok := dashauth.VerifyTOTP(secret, strings.TrimSpace(code), s.store.Now(), 0)
	if !ok {
		return nil, newErr(KindInvalid, CodeTOTPRequired, "That code is not valid. Enter the current code from your authenticator app.")
	}
	var codes []string
	var hashes [][]byte
	if !u.TOTPEnabled() {
		if codes, hashes, err = s.newRecoveryCodes(u.ID); err != nil {
			return nil, err
		}
	}
	if err := s.store.EnableTOTP(ctx, u.ID, u.TOTPPending, ds.TokenHash, u.TOTPSecret, step, hashes, ds.User.actor()); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil, invalid("the two-factor setup changed; start again")
		}
		return nil, err
	}
	if u.TOTPEnabled() {
		s.notify(ctx, mail.KindTOTPReplaced, func() (mail.Message, error) { return mail.TOTPReplaced(u.Email, s.notice(u.Email)) })
	}
	return codes, nil
}

// DisableTOTP turns 2FA off; it takes exactly one of a current code or a recovery code. Every
// other session ends and the owner is told by email.
func (s *Service) DisableTOTP(ctx context.Context, ds DashSession, code, recoveryCode *string) error {
	u, err := s.store.DashboardUser(ctx, ds.User.ID)
	if err != nil {
		return err
	}
	if !u.TOTPEnabled() {
		return invalid("two-factor authentication is off")
	}
	if err := s.stepUpOrRecovery(ctx, u, code, recoveryCode); err != nil {
		return err
	}
	if err := s.store.DisableTOTP(ctx, u.ID, ds.TokenHash, ds.User.actor()); err != nil {
		return err
	}
	s.notify(ctx, mail.KindTOTPDisabled, func() (mail.Message, error) { return mail.TOTPDisabled(u.Email, s.notice(u.Email)) })
	return nil
}

// stepUpPassword checks the current password, which only changing the password asks for (D58).
func (s *Service) stepUpPassword(ctx context.Context, u store.DashboardUser, password *string) error {
	if password == nil || *password == "" {
		return newErr(KindForbidden, CodeInvalidPassword, "Enter your password to confirm.")
	}
	ok, err := s.cfg.Hasher.VerifyPassword(ctx, u.PasswordHash, *password)
	if err != nil {
		return err
	}
	if !ok {
		if err := s.store.Audit(ctx, u.MerchantID, "dashboard:"+u.ID, "user.step_up_failed", map[string]any{"user_id": u.ID, "reason": "password"}); err != nil {
			return err
		}
		return newErr(KindForbidden, CodeInvalidPassword, "That password is not correct.")
	}
	return nil
}

// stepUp asks for a current TOTP code when 2FA is on.
func (s *Service) stepUp(ctx context.Context, u store.DashboardUser, code *string) error {
	if !u.TOTPEnabled() {
		return nil
	}
	if code == nil || *code == "" {
		return newErr(KindForbidden, CodeTOTPRequired, "Enter the 6-digit code from your authenticator app.")
	}
	ok, err := s.checkTOTP(ctx, u, store.TOTPRunStepUp, *code)
	if err != nil {
		return err
	}
	if !ok {
		if aerr := s.stepUpFailed(ctx, u, "totp"); aerr != nil {
			return aerr
		}
		return newErr(KindForbidden, CodeTOTPRequired, "That code is not valid. Enter the current code from your authenticator app.")
	}
	return nil
}
