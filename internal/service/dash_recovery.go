package service

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"strings"
	"time"

	"github.com/prismaport/prismaport/internal/dashauth"
	"github.com/prismaport/prismaport/internal/mail"
	"github.com/prismaport/prismaport/internal/store"
)

// Account recovery (D56): recovery codes, the emailed path for a lost authenticator and the wait
// before it takes effect.
const (
	// CodeRecoveryLocked refuses adding a live wallet or a live API key while the lock after a
	// removal runs.
	CodeRecoveryLocked = "recovery_locked"

	// RecoveryCodeCount codes are issued at a time, in the form XXXXX-XXXXX (50 bits).
	RecoveryCodeCount = 10
	// TOTPLostLinkLifetime is how long the emailed link works (once).
	TOTPLostLinkLifetime = 30 * time.Minute
	// TOTPRemovalWait is the time between asking for a removal and the removal; the owner is
	// reminded TOTPRemovalReminder before it.
	TOTPRemovalWait     = 72 * time.Hour
	TOTPRemovalReminder = 24 * time.Hour
	// RecoveryLockDuration is how long after a removal's time live credentials stay locked.
	RecoveryLockDuration = 48 * time.Hour

	recoveryCodePurpose = "recovery_code"
	removalCancelLabel  = "dashboard_totp_removal_cancel"
	totpRemovalBatch    = 100
	// recoveryAlphabet is Crockford's base32: no I, L, O or U.
	recoveryAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
)

func lostLinkInvalid() *Error {
	return newErr(KindInvalid, CodeInvalidLink, "This link no longer works. It works for 30 minutes and only once. Ask for a new link.")
}

func cancelLinkInvalid() *Error {
	return newErr(KindInvalid, CodeInvalidLink, "This link no longer works. The removal was already cancelled or has happened.")
}

// newRecoveryCodes returns fresh codes and, per code, the hash to store for this user.
func (s *Service) newRecoveryCodes(userID string) ([]string, [][]byte, error) {
	codes, hashes := make([]string, RecoveryCodeCount), make([][]byte, RecoveryCodeCount)
	for i := range codes {
		var b [10]byte
		if _, err := rand.Read(b[:]); err != nil {
			return nil, nil, err
		}
		var c [10]byte
		for j, v := range b {
			// 32 symbols, so the low 5 bits of a random byte are uniform.
			c[j] = recoveryAlphabet[v&31]
		}
		code := string(c[:])
		codes[i] = code[:5] + "-" + code[5:]
		hashes[i] = s.recoveryCodeHash(userID, code)
	}
	return codes, hashes, nil
}

// normalizeRecoveryCode makes the entered form of a code comparable: case does not matter, the
// hyphen and spaces are optional, and Crockford's look-alikes read as the digit they resemble.
func normalizeRecoveryCode(code string) string {
	r := strings.NewReplacer("-", "", " ", "", "O", "0", "I", "1", "L", "1")
	return r.Replace(strings.ToUpper(strings.TrimSpace(code)))
}

func (s *Service) recoveryCodeHash(userID, normalized string) []byte {
	return s.codeHash(userID, recoveryCodePurpose, normalized)
}

// checkRecoveryCode spends a recovery code of the user, at most once. Every attempt counts in run
// like a TOTP code. Unlike a TOTP code, a recovery code is still checked while that run's wait
// runs: it has 50 bits, so the wait is not what keeps it from being guessed (the rate limits and
// the password in front of it are), and it is how the owner gets past wrong codes entered by
// someone who knows the password. A wrong one then answers the wait. The code is hashed and
// looked up whatever its shape, so a malformed one takes as long as a wrong one. err is an
// internal failure or a wait after too many wrong codes, not a wrong code.
func (s *Service) checkRecoveryCode(ctx context.Context, u store.DashboardUser, run store.TOTPRun, code string) (bool, error) {
	attempt, wait, err := s.store.TakeTOTPAttempt(ctx, u.ID, run, totpDelay)
	if err != nil {
		return false, err
	}
	ok, err := s.store.UseRecoveryCode(ctx, u, run, s.recoveryCodeHash(u.ID, normalizeRecoveryCode(code)), "dashboard:"+u.ID)
	if err != nil || ok {
		return ok, err
	}
	if wait > 0 {
		return false, totpWait(wait)
	}
	if failureNotice(attempt, TOTPFreeFailures) {
		s.sendTOTPFailures(ctx, u.Email, attempt)
	}
	return false, nil
}

// signInRecoveryCode is checkRecoveryCode before a session (sign-in, password reset).
func (s *Service) signInRecoveryCode(ctx context.Context, u store.DashboardUser, code string) (bool, error) {
	return s.checkRecoveryCode(ctx, u, store.TOTPRunSignIn, code)
}

// stepUpOrRecovery confirms the changes that manage two-factor authentication itself (replace the
// authenticator, turn it off, new recovery codes): a current TOTP code or a recovery code, never
// the password or an emailed code (D58). Never used for a wallet or a live API key, where a
// recovery code must not pass. u has 2FA on.
func (s *Service) stepUpOrRecovery(ctx context.Context, u store.DashboardUser, code, recovery *string) error {
	hasCode, hasRecovery := code != nil && *code != "", recovery != nil && *recovery != ""
	if hasCode && hasRecovery {
		return &Error{Kind: KindInvalid, Code: CodeInvalidRequest, Param: "recovery_code", Message: "Send a code or a recovery code, not both."}
	}
	if !hasRecovery {
		if !hasCode {
			return newErr(KindForbidden, CodeTOTPRequired, "Enter the 6-digit code from your authenticator app, or a recovery code.")
		}
		return s.stepUp(ctx, u, code)
	}
	ok, err := s.checkRecoveryCode(ctx, u, store.TOTPRunStepUp, *recovery)
	if err != nil {
		return err
	}
	if !ok {
		if aerr := s.stepUpFailed(ctx, u, "recovery_code"); aerr != nil {
			return aerr
		}
		return newErr(KindForbidden, CodeTOTPRequired, "That recovery code is not valid or was already used.")
	}
	return nil
}

// notify emails a security notice; a server without email only logs it (the audit log has the
// change itself).
func (s *Service) notify(ctx context.Context, kind string, build func() (mail.Message, error)) {
	if s.cfg.Mail == nil {
		s.Logger().Warn("no email configured to tell the owner about a security change", "kind", kind)
		return
	}
	m, err := build()
	s.send(ctx, m, err)
}

func (s *Service) notice(to string) mail.NoticeEmail {
	return mail.NoticeEmail{Email: to, DashboardURL: s.dashboardURL(), At: s.store.Now()}
}

// ChangePassword sets a new password for the signed-in user. It takes the current password and,
// with 2FA on, a current code. The new password is checked before the code, so a refused change
// never spends it. Every other session ends and the owner is told by email.
func (s *Service) ChangePassword(ctx context.Context, ds DashSession, current, next string, totp *string) error {
	u, err := s.store.DashboardUser(ctx, ds.User.ID)
	if err != nil {
		return err
	}
	if err := s.stepUpPassword(ctx, u, &current); err != nil {
		return err
	}
	if next == current {
		return &Error{Kind: KindInvalid, Code: CodeInvalidRequest, Param: "new_password", Message: "Choose a password different from the current one."}
	}
	if err := s.checkNewPassword(ctx, next, "new_password"); err != nil {
		return err
	}
	if err := s.stepUp(ctx, u, totp); err != nil {
		return err
	}
	hash, err := s.cfg.Hasher.HashPassword(ctx, next, dashauth.DefaultParams)
	if err != nil {
		return err
	}
	err = s.store.ChangePassword(ctx, u.ID, u.PasswordHash, hash, ds.TokenHash, ds.User.actor())
	if errors.Is(err, store.ErrConflict) {
		return invalid("your password changed meanwhile; sign in again")
	}
	if err != nil {
		return err
	}
	s.notify(ctx, mail.KindPasswordChanged, func() (mail.Message, error) { return mail.PasswordChanged(u.Email, s.notice(u.Email)) })
	return nil
}

// CreateRecoveryCodes replaces all the user's recovery codes. Like every change to 2FA itself it
// takes a current TOTP code or a recovery code (D58); the codes it replaces include that one.
func (s *Service) CreateRecoveryCodes(ctx context.Context, du DashUser, code, recoveryCode *string) ([]string, error) {
	u, err := s.store.DashboardUser(ctx, du.ID)
	if err != nil {
		return nil, err
	}
	if !u.TOTPEnabled() {
		return nil, invalid("two-factor authentication is off")
	}
	if err := s.stepUpOrRecovery(ctx, u, code, recoveryCode); err != nil {
		return nil, err
	}
	codes, hashes, err := s.newRecoveryCodes(u.ID)
	if err != nil {
		return nil, err
	}
	if err := s.store.ReplaceRecoveryCodes(ctx, u.ID, hashes, du.actor()); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, invalid("two-factor authentication is off")
		}
		return nil, err
	}
	return codes, nil
}

// CurrentDashUser re-reads the signed-in user after a change, for the answer's counts and dates.
func (s *Service) CurrentDashUser(ctx context.Context, id string) (DashUser, error) {
	u, err := s.store.DashboardUser(ctx, id)
	if err != nil {
		return DashUser{}, err
	}
	return dashUser(u), nil
}

// TOTPLost emails the link that asks to remove a lost authenticator. It answers the same whatever
// the email: a link goes out only to a confirmed account with 2FA on and no removal pending.
func (s *Service) TOTPLost(ctx context.Context, email string) error {
	if err := s.requireMail(); err != nil {
		return err
	}
	e, err := NormalizeEmail(email)
	if err != nil {
		return nil
	}
	u, err := s.store.DashboardUserByEmail(ctx, e)
	if errors.Is(err, store.ErrNotFound) {
		return s.store.AuditAnonymous(ctx, "signup", "auth.unknown_email", map[string]any{"route": "totp_lost", "email_sha256": emailDigest(e)})
	}
	if err != nil || u.DisabledAt != nil || !u.EmailVerified() || !u.TOTPEnabled() || u.TOTPRemovalAt != nil {
		return err
	}
	token, hash, err := dashauth.NewToken()
	if err != nil {
		return err
	}
	if err := s.store.IssueTOTPLostLink(ctx, u, hash[:], s.store.Now().Add(TOTPLostLinkLifetime)); err != nil {
		return err
	}
	m, err := mail.TOTPRemovalLink(e, mail.TOTPLostLinkEmail{Token: token, Minutes: int(TOTPLostLinkLifetime / time.Minute), DashboardURL: s.dashboardURL()})
	s.send(ctx, m, err)
	return nil
}

// TOTPLostLink is what the page behind a working lost link shows.
type TOTPLostLink struct {
	Email string `json:"email"`
}

func (s *Service) lostLinkUser(ctx context.Context, token string) (store.DashboardUser, []byte, error) {
	hash, err := dashauth.HashToken(token)
	if err != nil {
		return store.DashboardUser{}, nil, lostLinkInvalid()
	}
	u, err := s.store.TOTPLostLinkUser(ctx, hash[:])
	if errors.Is(err, store.ErrCodeInvalid) {
		return store.DashboardUser{}, nil, lostLinkInvalid()
	}
	return u, hash[:], err
}

// CheckTOTPLostLink tells the page whether its link works, without spending it.
func (s *Service) CheckTOTPLostLink(ctx context.Context, token string) (TOTPLostLink, error) {
	if err := s.requireMail(); err != nil {
		return TOTPLostLink{}, err
	}
	u, _, err := s.lostLinkUser(ctx, token)
	if err != nil {
		return TOTPLostLink{}, err
	}
	return TOTPLostLink{Email: u.Email}, nil
}

// TOTPRemovalRequest answers a confirmed lost link.
type TOTPRemovalRequest struct {
	RemovalAt   time.Time `json:"removal_at"`
	CancelToken string    `json:"cancel_token"`
}

// removalCancelToken is the cancel token of a removal: an HMAC of the user and the moment it was
// requested, with a key derived from the service secret. Only its hash is stored, so a database
// read cannot cancel a removal, and the worker and a repeated request can name the same link the
// first email carried without keeping the token anywhere.
func (s *Service) removalCancelToken(userID string, requestedAt time.Time) string {
	m := hmac.New(sha256.New, s.cfg.SecretBox.DeriveKey(removalCancelLabel))
	var micros [8]byte
	binary.BigEndian.PutUint64(micros[:], uint64(requestedAt.UnixMicro()))
	m.Write([]byte(userID + "\x00"))
	m.Write(micros[:])
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (s *Service) removalCancelHash(userID string, requestedAt time.Time) []byte {
	sum := sha256.Sum256([]byte(s.removalCancelToken(userID, requestedAt)))
	return sum[:]
}

// TOTPLostLinkPasswordTries is how many wrong passwords a lost link takes before it is deleted.
const TOTPLostLinkPasswordTries = 5

// ConfirmTOTPLost spends a lost link once the password is right and schedules the removal
// TOTPRemovalWait away. A wrong password does not spend the link, but the
// TOTPLostLinkPasswordTries-th one deletes it, and each counts toward the password failures that
// email the owner. A removal already pending keeps
// its time and sends no new email: asking again never shortens or restarts the wait.
func (s *Service) ConfirmTOTPLost(ctx context.Context, token, password string) (TOTPRemovalRequest, error) {
	if err := s.requireMail(); err != nil {
		return TOTPRemovalRequest{}, err
	}
	u, hash, err := s.lostLinkUser(ctx, token)
	if err != nil {
		return TOTPRemovalRequest{}, err
	}
	ok, err := s.cfg.Hasher.VerifyPassword(ctx, u.PasswordHash, password)
	if err != nil {
		return TOTPRemovalRequest{}, err
	}
	if err := s.passwordChecked(ctx, u, ok); err != nil {
		return TOTPRemovalRequest{}, err
	}
	if !ok {
		ended, err := s.store.FailTOTPLostLink(ctx, hash, TOTPLostLinkPasswordTries)
		if err != nil {
			return TOTPRemovalRequest{}, err
		}
		if err := s.store.Audit(ctx, u.MerchantID, "dashboard:"+u.ID, "user.totp_removal_failed",
			map[string]any{"user_id": u.ID, "reason": "password", "link_ended": ended}); err != nil {
			return TOTPRemovalRequest{}, err
		}
		if ended {
			return TOTPRemovalRequest{}, newErr(KindInvalid, CodeInvalidLink, "Too many wrong passwords, so this link no longer works. Ask for a new link.")
		}
		return TOTPRemovalRequest{}, newErr(KindForbidden, CodeInvalidPassword, "That password is not correct.")
	}
	r, created, err := s.store.RequestTOTPRemoval(ctx, hash, TOTPRemovalWait, s.removalCancelHash)
	if errors.Is(err, store.ErrCodeInvalid) {
		return TOTPRemovalRequest{}, lostLinkInvalid()
	}
	if err != nil {
		return TOTPRemovalRequest{}, err
	}
	cancel := s.removalCancelToken(r.UserID, r.RequestedAt)
	if created {
		m, err := mail.TOTPRemovalScheduled(r.Email, s.removalEmail(r, cancel))
		s.send(ctx, m, err)
	}
	return TOTPRemovalRequest{RemovalAt: r.RemovalAt, CancelToken: cancel}, nil
}

func (s *Service) removalEmail(r store.TOTPRemoval, cancelToken string) mail.TOTPRemovalEmail {
	return mail.TOTPRemovalEmail{NoticeEmail: s.notice(r.Email), CancelToken: cancelToken, RemovalAt: r.RemovalAt, LockEnds: r.LockedUntil}
}

// CancelTOTPRemovalLink cancels a pending removal from the emailed link's page: the POST of its
// button, so a mail scanner opening the link cancels nothing.
func (s *Service) CancelTOTPRemovalLink(ctx context.Context, token string) error {
	if err := s.requireMail(); err != nil {
		return err
	}
	hash, err := dashauth.HashToken(token)
	if err != nil {
		return cancelLinkInvalid()
	}
	u, err := s.store.CancelTOTPRemovalByToken(ctx, hash[:])
	if errors.Is(err, store.ErrCodeInvalid) {
		return cancelLinkInvalid()
	}
	if err != nil {
		return err
	}
	s.sendRemovalCancelled(ctx, u.Email)
	return nil
}

func (s *Service) sendRemovalCancelled(ctx context.Context, email string) {
	s.notify(ctx, mail.KindTOTPRemovalCancelled, func() (mail.Message, error) { return mail.TOTPRemovalCancelled(email, s.notice(email)) })
}

// CancelTOTPRemoval cancels the signed-in user's pending removal, if any.
func (s *Service) CancelTOTPRemoval(ctx context.Context, du DashUser) error {
	u, err := s.store.DashboardUser(ctx, du.ID)
	if err != nil {
		return err
	}
	cancelled, err := s.store.CancelTOTPRemoval(ctx, u, du.actor(), "session")
	if err != nil {
		return err
	}
	if cancelled {
		s.sendRemovalCancelled(ctx, u.Email)
	}
	return nil
}

// recoveryLock refuses a live wallet or live API key while the lock after a removal runs. It is
// checked before the step-up, so no code is spent on a change that cannot happen.
func (s *Service) recoveryLock(du DashUser, livemode bool) error {
	if !livemode || du.RecoveryLockedUntil == nil {
		return nil
	}
	wait := du.RecoveryLockedUntil.Sub(s.store.Now())
	if wait <= 0 {
		return nil
	}
	e := newErr(KindForbidden, CodeRecoveryLocked, "Adding a wallet and creating or rolling live API keys are locked for 48 hours after two-factor authentication is removed.")
	e.RetryAfter = wait
	e.Details = map[string]any{"recovery_locked_until": du.RecoveryLockedUntil.UTC()}
	return e
}

// SweepTOTPRemovals is one pass of the worker: reminders that are due, then one removal whose
// time has come. A pass that did something runs again at once.
func (s *Service) SweepTOTPRemovals(ctx context.Context) (int, error) {
	if s.cfg.SecretBox == nil {
		return 0, nil
	}
	reminders, err := s.store.ClaimTOTPRemovalReminders(ctx, TOTPRemovalReminder, totpRemovalBatch)
	if err != nil {
		return 0, err
	}
	for _, r := range reminders {
		s.notify(ctx, mail.KindTOTPRemovalReminder, func() (mail.Message, error) {
			return mail.TOTPRemovalReminder(r.Email, s.removalEmail(r, s.removalCancelToken(r.UserID, r.RequestedAt)))
		})
	}
	r, found, err := s.store.RemoveDueTOTP(ctx, RecoveryLockDuration)
	if err != nil {
		return len(reminders), err
	}
	if !found {
		return len(reminders), nil
	}
	s.notify(ctx, mail.KindTOTPRemoved, func() (mail.Message, error) { return mail.TOTPRemoved(r.Email, s.removalEmail(r, "")) })
	return len(reminders) + 1, nil
}
