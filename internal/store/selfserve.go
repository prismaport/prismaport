package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Purposes of a dashboard email code.
const (
	CodeVerifyEmail   = "verify_email"
	CodePasswordReset = "password_reset"
)

// ErrCodeInvalid is every failed code check: no such user, no live code, expired, out of
// attempts, the daily budget spent, or simply the wrong code. Callers answer them all alike.
var ErrCodeInvalid = errors.New("store: code not valid")

// EmailCode is a code to store: only its hash.
type EmailCode struct {
	Hash      []byte
	ExpiresAt time.Time
}

// NewAccount is a self-serve sign-up: a merchant with live mode open, its first user (email not
// confirmed yet, so nothing opens before the code), the assets it accepts per mode and the
// confirmation code.
type NewAccount struct {
	MerchantID   string
	MerchantName string
	UserID       string
	Email        string
	PasswordHash string
	TestAssets   []ChainAsset
	LiveAssets   []ChainAsset
	Code         EmailCode
}

func upsertCode(ctx context.Context, tx pgx.Tx, userID, purpose string, c EmailCode, now time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO dashboard_email_codes (user_id, purpose, code_hash, attempts, created_at, expires_at)
		VALUES ($1, $2, $3, 0, $4, $5)
		ON CONFLICT (user_id, purpose) DO UPDATE SET code_hash = EXCLUDED.code_hash, attempts = 0,
			created_at = EXCLUDED.created_at, expires_at = EXCLUDED.expires_at`, userID, purpose, c.Hash, now, c.ExpiresAt)
	return err
}

// CreateAccount stores a sign-up in one transaction. A taken email is ErrConflict.
func (s *Store) CreateAccount(ctx context.Context, a NewAccount) error {
	now := s.now()
	actor := "signup:" + a.UserID
	return opErr("create account", s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO merchants (id, name, created_at, live_enabled_at) VALUES ($1, $2, $3, $3)`, a.MerchantID, a.MerchantName, now); err != nil {
			return err
		}
		if err := audit(ctx, tx, a.MerchantID, actor, "merchant.created", map[string]any{"name": a.MerchantName, "self_serve": true}); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO dashboard_users (id, merchant_id, email, password_hash, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $5)`, a.UserID, a.MerchantID, a.Email, a.PasswordHash, now); err != nil {
			if isUniqueViolation(err) {
				return ErrConflict
			}
			return err
		}
		if err := audit(ctx, tx, a.MerchantID, actor, "user.signed_up", map[string]any{"user_id": a.UserID, "email": a.Email}); err != nil {
			return err
		}
		if err := s.Bind(tx).SetAcceptedAssets(ctx, a.MerchantID, false, a.TestAssets, actor); err != nil {
			return err
		}
		if len(a.LiveAssets) > 0 {
			if err := s.Bind(tx).SetAcceptedAssets(ctx, a.MerchantID, true, a.LiveAssets, actor); err != nil {
				return err
			}
		}
		if err := audit(ctx, tx, a.MerchantID, actor, "merchant.live_enabled", nil); err != nil {
			return err
		}
		if err := upsertCode(ctx, tx, a.UserID, CodeVerifyEmail, a.Code, now); err != nil {
			return err
		}
		return audit(ctx, tx, a.MerchantID, actor, "user.verification_sent", map[string]any{"user_id": a.UserID})
	}))
}

// RestartSignup is a new sign-up with the email of an account that never confirmed it. Only a
// fresh code replaces the old one: the password, the business name and the sessions stay as
// they are, because anyone can send a sign-up with any email and a replaced password would be
// confirmed by the owner's next code (ADR 0008 C-1). Whoever enters the code sets the password
// then (VerifyEmail). ErrConflict when the email was confirmed meanwhile.
func (s *Store) RestartSignup(ctx context.Context, userID string, code EmailCode) error {
	now := s.now()
	return opErr("restart signup", s.inTx(ctx, func(tx pgx.Tx) error {
		var merchantID string
		err := tx.QueryRow(ctx, `SELECT merchant_id FROM dashboard_users
			WHERE id = $1 AND email_verified_at IS NULL AND disabled_at IS NULL FOR UPDATE`, userID).Scan(&merchantID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrConflict
		}
		if err != nil {
			return err
		}
		if err := upsertCode(ctx, tx, userID, CodeVerifyEmail, code, now); err != nil {
			return err
		}
		return audit(ctx, tx, merchantID, "signup:"+userID, "user.signup_restarted", map[string]any{"user_id": userID})
	}))
}

// CodeBudgetSpent reports whether the user took max wrong codes in the current window, so no
// code would be checked until it ends.
func (s *Store) CodeBudgetSpent(ctx context.Context, userID string, max int, window time.Duration) (bool, error) {
	var failures int
	var start *time.Time
	err := s.db.QueryRow(ctx, `SELECT code_failures, code_window_start FROM dashboard_users WHERE id = $1`, userID).Scan(&failures, &start)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, opErr("code budget spent", err)
	}
	return start != nil && s.now().Sub(*start) < window && failures >= max, nil
}

// IssueCode stores a new code for the user (replacing any live one of the purpose) and audits
// action.
func (s *Store) IssueCode(ctx context.Context, u DashboardUser, purpose string, code EmailCode, action string) error {
	return opErr("issue code", s.inTx(ctx, func(tx pgx.Tx) error {
		if err := upsertCode(ctx, tx, u.ID, purpose, code, s.now()); err != nil {
			return err
		}
		return audit(ctx, tx, u.MerchantID, "dashboard:"+u.ID, action, map[string]any{"user_id": u.ID})
	}))
}

// CodeCheck is one attempt at a code.
type CodeCheck struct {
	Email   string
	Purpose string
	// Matches reports whether the stored hash is the hash of the entered code for this user.
	Matches func(userID string, stored []byte) bool
	// MaxAttempts is how often one code may be tried; MaxFailures how many wrong codes a user
	// may enter per Window across every code.
	MaxAttempts int
	MaxFailures int
	Window      time.Duration
}

// consumeCode checks a code under the user's row lock. A wrong code spends one of the code's
// attempts and one of the user's failures, and that is committed even though the check fails.
// A right one deletes the code and runs apply in the same transaction. Every failure is
// ErrCodeInvalid.
func (s *Store) consumeCode(ctx context.Context, c CodeCheck, apply func(tx pgx.Tx, u DashboardUser, now time.Time) error) (DashboardUser, error) {
	now := s.now()
	var user DashboardUser
	valid := false
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var failures int
		var window *time.Time
		u, err := scanUser(tx.QueryRow(ctx, `SELECT `+userColumns+`, u.code_failures, u.code_window_start
			FROM dashboard_users u JOIN merchants m ON m.id = u.merchant_id WHERE u.email = $1 FOR UPDATE OF u`, c.Email), &failures, &window)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if u.DisabledAt != nil {
			return nil
		}
		if window == nil || now.Sub(*window) >= c.Window {
			failures, window = 0, &now
		}
		if failures >= c.MaxFailures {
			return nil
		}
		var stored []byte
		var attempts int
		var expires time.Time
		err = tx.QueryRow(ctx, `SELECT code_hash, attempts, expires_at FROM dashboard_email_codes WHERE user_id = $1 AND purpose = $2 FOR UPDATE`,
			u.ID, c.Purpose).Scan(&stored, &attempts, &expires)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if !now.Before(expires) || attempts >= c.MaxAttempts {
			return nil
		}
		if !c.Matches(u.ID, stored) {
			if _, err := tx.Exec(ctx, `UPDATE dashboard_email_codes SET attempts = attempts + 1 WHERE user_id = $1 AND purpose = $2`, u.ID, c.Purpose); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE dashboard_users SET code_failures = $2, code_window_start = $3 WHERE id = $1`, u.ID, failures+1, *window); err != nil {
				return err
			}
			return audit(ctx, tx, u.MerchantID, "dashboard:"+u.ID, "user.code_failed", map[string]any{"user_id": u.ID, "purpose": c.Purpose,
				"attempt": attempts + 1})
		}
		if _, err := tx.Exec(ctx, `DELETE FROM dashboard_email_codes WHERE user_id = $1 AND purpose = $2`, u.ID, c.Purpose); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE dashboard_users SET code_failures = 0, code_window_start = NULL WHERE id = $1`, u.ID); err != nil {
			return err
		}
		if err := apply(tx, u, now); err != nil {
			return err
		}
		user, valid = u, true
		return nil
	})
	if err != nil {
		return DashboardUser{}, err
	}
	if !valid {
		return DashboardUser{}, ErrCodeInvalid
	}
	return user, nil
}

// VerifyEmail confirms the user's email with a verify_email code. passwordHash, when set,
// becomes the password in the same transaction: the code proved the email, and the password
// is the one of whoever entered it. Every session of the user ends except keep (the browser
// that entered the code, when it is signed in as this user), so nobody who signed in before
// the address was proven stays signed in.
func (s *Store) VerifyEmail(ctx context.Context, c CodeCheck, keep *[32]byte, passwordHash *string) (DashboardUser, error) {
	c.Purpose = CodeVerifyEmail
	out, err := s.consumeCode(ctx, c, func(tx pgx.Tx, u DashboardUser, now time.Time) error {
		if _, err := tx.Exec(ctx, `UPDATE dashboard_users SET email_verified_at = coalesce(email_verified_at, $2), updated_at = $2,
			password_hash = coalesce($3, password_hash) WHERE id = $1`, u.ID, now, passwordHash); err != nil {
			return err
		}
		var keepHash []byte
		if keep != nil {
			keepHash = keep[:]
		}
		if _, err := tx.Exec(ctx, `DELETE FROM dashboard_sessions WHERE user_id = $1 AND token_hash IS DISTINCT FROM $2`, u.ID, keepHash); err != nil {
			return err
		}
		return audit(ctx, tx, u.MerchantID, "dashboard:"+u.ID, "user.email_verified", map[string]any{"user_id": u.ID,
			"password_set": passwordHash != nil})
	})
	return out, opErr("verify email", err)
}

// resetLinkQuery finds the enabled user a live password_reset link belongs to, by the link's
// token hash ($1) at time $2.
const resetLinkQuery = `SELECT ` + userColumns + ` FROM dashboard_email_codes c
	JOIN dashboard_users u ON u.id = c.user_id JOIN merchants m ON m.id = u.merchant_id
	WHERE c.purpose = '` + CodePasswordReset + `' AND c.code_hash = $1 AND c.expires_at > $2 AND u.disabled_at IS NULL`

// ResetLinkUser is the user a password reset link belongs to, without spending the link.
// ErrCodeInvalid when no live link has this hash.
func (s *Store) ResetLinkUser(ctx context.Context, tokenHash []byte) (DashboardUser, error) {
	u, err := scanUser(s.db.QueryRow(ctx, resetLinkQuery, tokenHash, s.now()))
	if errors.Is(err, ErrNotFound) {
		return DashboardUser{}, ErrCodeInvalid
	}
	return u, opErr("reset link user", err)
}

// ResetPassword spends a password reset link: it sets the new password, confirms the email (the
// link proved it, so a pending verify_email code is deleted) and ends every session of the user.
// ErrCodeInvalid when no live link has this hash.
func (s *Store) ResetPassword(ctx context.Context, tokenHash []byte, passwordHash string) (DashboardUser, error) {
	now := s.now()
	var user DashboardUser
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		u, err := scanUser(tx.QueryRow(ctx, resetLinkQuery+` FOR UPDATE OF c, u`, tokenHash, now))
		if errors.Is(err, ErrNotFound) {
			return ErrCodeInvalid
		}
		if err != nil {
			return err
		}
		// A verify_email code goes too: it could otherwise still set the password (VerifyEmail),
		// and the link already proved the email.
		if _, err := tx.Exec(ctx, `DELETE FROM dashboard_email_codes WHERE user_id = $1 AND purpose IN ($2, $3)`,
			u.ID, CodePasswordReset, CodeVerifyEmail); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE dashboard_users SET password_hash = $2, email_verified_at = coalesce(email_verified_at, $3),
			updated_at = $3 WHERE id = $1`, u.ID, passwordHash, now); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `DELETE FROM dashboard_sessions WHERE user_id = $1`, u.ID)
		if err != nil {
			return err
		}
		user = u
		return audit(ctx, tx, u.MerchantID, "dashboard:"+u.ID, "user.password_reset", map[string]any{"user_id": u.ID,
			"sessions_ended": tag.RowsAffected()})
	})
	return user, opErr("reset password", err)
}
