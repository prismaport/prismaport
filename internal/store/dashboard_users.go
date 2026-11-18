package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// DashboardUser is a dashboard login with its merchant, 2FA and recovery state.
type DashboardUser struct {
	ID           string
	MerchantID   string
	MerchantName string
	Email        string
	PasswordHash string
	// TOTPSecret and TOTPPending are sealed; nil when absent.
	TOTPSecret  []byte
	TOTPPending []byte
	// TOTPPendingSession is the token hash of the session that started the pending enrollment;
	// TOTPPendingExpiresAt is when that enrollment stops being accepted.
	TOTPPendingSession   []byte
	TOTPPendingExpiresAt *time.Time
	// TOTPLastStep is the last accepted TOTP time step (0 when none).
	TOTPLastStep int64
	CreatedAt    time.Time
	DisabledAt   *time.Time
	// EmailVerifiedAt is when the user proved the address (nil until then).
	EmailVerifiedAt *time.Time
	// MerchantLiveEnabled reports whether the user's merchant may use live mode.
	MerchantLiveEnabled bool
	// MerchantLogoID is the merchant's checkout logo (nil when it has none).
	MerchantLogoID *string
	// RecoveryCodesLeft counts the unused recovery codes. TOTPRemovalAt is when a pending removal
	// of two-factor authentication happens (nil when none is pending). RecoveryLockedUntil is the
	// end of the lock on live credentials after a removal (nil when it never had one).
	RecoveryCodesLeft   int
	TOTPRemovalAt       *time.Time
	RecoveryLockedUntil *time.Time
}

// TOTPEnabled reports whether the user has an active two-factor secret.
func (u DashboardUser) TOTPEnabled() bool { return u.TOTPSecret != nil }

// EmailVerified reports whether the user has verified their email address.
func (u DashboardUser) EmailVerified() bool { return u.EmailVerifiedAt != nil }

const userColumns = `u.id, u.merchant_id, m.name, u.email, u.password_hash, u.totp_secret_ciphertext,
	u.totp_pending_ciphertext, u.totp_pending_session, u.totp_pending_expires_at, coalesce(u.totp_last_step, 0), u.created_at,
	u.disabled_at, u.email_verified_at, m.live_enabled_at IS NOT NULL, m.logo_id,
	(SELECT count(*) FROM dashboard_recovery_codes rc WHERE rc.user_id = u.id AND rc.used_at IS NULL),
	(SELECT tr.removal_at FROM dashboard_totp_removals tr WHERE tr.user_id = u.id), u.recovery_locked_until`

func scanUser(r pgx.Row, extra ...any) (DashboardUser, error) {
	var u DashboardUser
	dest := append([]any{&u.ID, &u.MerchantID, &u.MerchantName, &u.Email, &u.PasswordHash, &u.TOTPSecret, &u.TOTPPending,
		&u.TOTPPendingSession, &u.TOTPPendingExpiresAt, &u.TOTPLastStep, &u.CreatedAt, &u.DisabledAt, &u.EmailVerifiedAt,
		&u.MerchantLiveEnabled, &u.MerchantLogoID, &u.RecoveryCodesLeft, &u.TOTPRemovalAt, &u.RecoveryLockedUntil}, extra...)
	return u, notFound(r.Scan(dest...))
}

// CreateDashboardUser inserts a user whose email counts as confirmed (a self-serve user confirms
// it with a code); a taken email is ErrConflict.
func (s *Store) CreateDashboardUser(ctx context.Context, u DashboardUser, actor string) (DashboardUser, error) {
	u.CreatedAt = s.now()
	u.EmailVerifiedAt = &u.CreatedAt
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO dashboard_users (id, merchant_id, email, password_hash, created_at, updated_at, email_verified_at)
			VALUES ($1, $2, $3, $4, $5, $5, $5)`, u.ID, u.MerchantID, u.Email, u.PasswordHash, u.CreatedAt); err != nil {
			if isUniqueViolation(err) {
				return ErrConflict
			}
			return err
		}
		return audit(ctx, tx, u.MerchantID, actor, "user.created", map[string]any{"user_id": u.ID, "email": u.Email})
	})
	return u, opErr("create dashboard user", err)
}

// DashboardUserByEmail returns the user with the given email, or ErrNotFound when none exists.
func (s *Store) DashboardUserByEmail(ctx context.Context, email string) (DashboardUser, error) {
	out, err := scanUser(s.db.QueryRow(ctx, `SELECT `+userColumns+` FROM dashboard_users u JOIN merchants m ON m.id = u.merchant_id
		WHERE u.email = $1`, email))
	return out, opErr("dashboard user by email", err)
}

// DashboardUser returns the user with the given id, or ErrNotFound when none exists.
func (s *Store) DashboardUser(ctx context.Context, id string) (DashboardUser, error) {
	out, err := scanUser(s.db.QueryRow(ctx, `SELECT `+userColumns+` FROM dashboard_users u JOIN merchants m ON m.id = u.merchant_id
		WHERE u.id = $1`, id))
	return out, opErr("dashboard user", err)
}

// DashboardSession is a logged-in dashboard session together with its user.
type DashboardSession struct {
	TokenHash  [32]byte
	CSRFToken  string
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time
	User       DashboardUser
	// ConfirmedUntil ends the session's confirmation window (D59), nil when none was opened;
	// ConfirmedWallets counts the Live wallets added in it.
	ConfirmedUntil   *time.Time
	ConfirmedWallets int
}

// CreateDashboardSession stores a new session (and drops the caller's previous one, if any, so a
// login never reuses a session id) and deletes sessions that ended.
func (s *Store) CreateDashboardSession(ctx context.Context, userID string, tokenHash [32]byte, csrf string, lifetime, idle time.Duration,
	previous *[32]byte) (DashboardSession, error) {
	now := s.now()
	ds := DashboardSession{TokenHash: tokenHash, CSRFToken: csrf, CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(lifetime)}
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if previous != nil {
			if _, err := tx.Exec(ctx, `DELETE FROM dashboard_sessions WHERE token_hash = $1`, previous[:]); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM dashboard_sessions WHERE expires_at <= $1 OR last_seen_at <= $2`, now, now.Add(-idle)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO dashboard_sessions (token_hash, user_id, csrf_token, created_at, last_seen_at, expires_at)
			VALUES ($1, $2, $3, $4, $4, $5)`, tokenHash[:], userID, csrf, now, ds.ExpiresAt)
		return err
	})
	return ds, opErr("create dashboard session", err)
}

// DashboardSessionByToken returns the live session with this token hash: not past its absolute
// expiry, seen within idle, and its user not disabled. last_seen_at moves forward at most once
// per touchEvery so reads do not all become writes.
func (s *Store) DashboardSessionByToken(ctx context.Context, tokenHash [32]byte, idle, touchEvery time.Duration) (DashboardSession, error) {
	now := s.now()
	var ds DashboardSession
	var hash []byte
	u, err := scanUser(s.db.QueryRow(ctx, `SELECT `+userColumns+`, ds.token_hash, ds.csrf_token, ds.created_at, ds.last_seen_at, ds.expires_at,
		ds.confirmed_until, ds.confirmed_wallets
		FROM dashboard_sessions ds JOIN dashboard_users u ON u.id = ds.user_id JOIN merchants m ON m.id = u.merchant_id
		WHERE ds.token_hash = $1 AND ds.expires_at > $2 AND ds.last_seen_at > $3 AND u.disabled_at IS NULL`,
		tokenHash[:], now, now.Add(-idle)), &hash, &ds.CSRFToken, &ds.CreatedAt, &ds.LastSeenAt, &ds.ExpiresAt,
		&ds.ConfirmedUntil, &ds.ConfirmedWallets)
	if err != nil {
		return DashboardSession{}, opErr("dashboard session by token", err)
	}
	copy(ds.TokenHash[:], hash)
	ds.User = u
	if now.Sub(ds.LastSeenAt) >= touchEvery {
		if _, err := s.db.Exec(ctx, `UPDATE dashboard_sessions SET last_seen_at = $2 WHERE token_hash = $1 AND last_seen_at < $2`,
			tokenHash[:], now); err != nil {
			return DashboardSession{}, opErr("dashboard session by token", err)
		}
		ds.LastSeenAt = now
	}
	return ds, nil
}

// DeleteDashboardSession removes the dashboard session with the given token hash.
func (s *Store) DeleteDashboardSession(ctx context.Context, tokenHash [32]byte) error {
	_, err := s.db.Exec(ctx, `DELETE FROM dashboard_sessions WHERE token_hash = $1`, tokenHash[:])
	return opErr("delete dashboard session", err)
}

// SetTOTPPending stores a sealed secret awaiting its first code, bound to the session that asked
// for it until expiresAt, and audits the start. active is the active secret the caller checked
// (nil when 2FA is off): if it changed meanwhile nothing is stored and the result is ErrConflict.
func (s *Store) SetTOTPPending(ctx context.Context, userID string, sealed []byte, sessionHash [32]byte, expiresAt time.Time,
	active []byte, actor string) error {
	return opErr("set totp pending", s.inTx(ctx, func(tx pgx.Tx) error {
		var merchantID string
		err := tx.QueryRow(ctx, `UPDATE dashboard_users SET totp_pending_ciphertext = $2, totp_pending_session = $3,
			totp_pending_expires_at = $4, updated_at = $5
			WHERE id = $1 AND totp_secret_ciphertext IS NOT DISTINCT FROM $6 RETURNING merchant_id`,
			userID, sealed, sessionHash[:], expiresAt, s.now(), active).Scan(&merchantID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrConflict
			}
			return err
		}
		return audit(ctx, tx, merchantID, actor, "user.totp_enrollment_started", map[string]any{"user_id": userID, "replacing": active != nil})
	}))
}

// EnableTOTP promotes the pending secret once a code for it was verified at step. The pending
// ciphertext must still be the one verified, started by sessionHash and not expired, and the
// active secret must still be active (nil when 2FA was off), so a concurrent re-enrollment or
// change wins nothing. Either way every other session of the user ends, so a session opened
// without the second factor does not outlive it. Turning 2FA on for the first time stores
// recoveryHashes as the account's recovery codes. Replacing an active secret is audited as
// user.totp_replaced, keeps the recovery codes, cancels a pending removal and drops an unused
// lost link.
func (s *Store) EnableTOTP(ctx context.Context, userID string, sealed []byte, sessionHash [32]byte, active []byte, step int64, recoveryHashes [][]byte, actor string) error {
	return opErr("enable totp", s.inTx(ctx, func(tx pgx.Tx) error {
		var merchantID string
		now := s.now()
		err := tx.QueryRow(ctx, `UPDATE dashboard_users SET totp_secret_ciphertext = totp_pending_ciphertext,
			totp_pending_ciphertext = NULL, totp_pending_session = NULL, totp_pending_expires_at = NULL, totp_last_step = $3, updated_at = $4
			WHERE id = $1 AND totp_pending_ciphertext = $2 AND totp_pending_session = $5 AND totp_pending_expires_at > $4
				AND totp_secret_ciphertext IS NOT DISTINCT FROM $6 RETURNING merchant_id`,
			userID, sealed, step, now, sessionHash[:], active).Scan(&merchantID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrConflict
			}
			return err
		}
		ended, err := endOtherSessions(ctx, tx, userID, sessionHash)
		if err != nil {
			return err
		}
		if active == nil {
			if err := replaceRecoveryCodes(ctx, tx, userID, recoveryHashes, now); err != nil {
				return err
			}
			return audit(ctx, tx, merchantID, actor, "user.totp_enabled", map[string]any{"user_id": userID, "recovery_codes": len(recoveryHashes),
				"sessions_ended": ended})
		}
		if _, err := cancelRemoval(ctx, tx, userID, merchantID, actor, "replaced"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM dashboard_totp_lost_links WHERE user_id = $1`, userID); err != nil {
			return err
		}
		return audit(ctx, tx, merchantID, actor, "user.totp_replaced", map[string]any{"user_id": userID, "sessions_ended": ended})
	}))
}

// DisableTOTP turns 2FA off, removes the recovery codes, any pending removal and any lost link,
// and ends every session of the user except keep.
func (s *Store) DisableTOTP(ctx context.Context, userID string, keep [32]byte, actor string) error {
	now := s.now()
	return opErr("disable totp", s.inTx(ctx, func(tx pgx.Tx) error {
		var merchantID string
		err := tx.QueryRow(ctx, `UPDATE dashboard_users SET totp_secret_ciphertext = NULL, totp_pending_ciphertext = NULL,
			totp_pending_session = NULL, totp_pending_expires_at = NULL, totp_last_step = NULL, totp_failures = 0, totp_retry_at = NULL,
			totp_step_failures = 0, totp_step_retry_at = NULL, updated_at = $2
			WHERE id = $1 AND totp_secret_ciphertext IS NOT NULL RETURNING merchant_id`,
			userID, now).Scan(&merchantID)
		if err != nil {
			return notFound(err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM dashboard_recovery_codes WHERE user_id = $1`, userID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM dashboard_totp_removals WHERE user_id = $1`, userID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM dashboard_totp_lost_links WHERE user_id = $1`, userID); err != nil {
			return err
		}
		ended, err := endOtherSessions(ctx, tx, userID, keep)
		if err != nil {
			return err
		}
		return audit(ctx, tx, merchantID, actor, "user.totp_disabled", map[string]any{"user_id": userID, "sessions_ended": ended})
	}))
}

// TOTPRun names one of a user's two runs of wrong second-factor codes. Sign-in and password reset
// (before a session) count apart from the step-ups of a signed-in session, so wrong codes from
// someone who only knows the password never make the signed-in owner wait (migration 00119).
type TOTPRun int

const (
	// TOTPRunSignIn counts the codes entered before a session: sign-in and password reset.
	TOTPRunSignIn TOTPRun = iota
	// TOTPRunStepUp counts the codes entered inside a session: step-ups and confirmations.
	TOTPRunStepUp
)

// columns are the run's failure count and retry time in dashboard_users.
func (r TOTPRun) columns() (failures, retryAt string) {
	if r == TOTPRunStepUp {
		return "totp_step_failures", "totp_step_retry_at"
	}
	return "totp_failures", "totp_retry_at"
}

// endRun is the SET list that ends the run.
func (r TOTPRun) endRun() string {
	failures, retryAt := r.columns()
	return failures + " = 0, " + retryAt + " = NULL"
}

// TakeTOTPAttempt counts a TOTP attempt in the user's run before its code is checked, so parallel
// guesses cannot all slip in before the first failure is recorded. It returns the attempt's
// number in the current run of failures; delay(attempt) is how long the next attempt must wait
// should this one fail. While an earlier attempt's wait runs, it counts nothing and returns
// what is left of it.
func (s *Store) TakeTOTPAttempt(ctx context.Context, userID string, run TOTPRun, delay func(attempt int) time.Duration) (attempt int, wait time.Duration, err error) {
	failures, retryCol := run.columns()
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		now := s.now()
		var retryAt *time.Time
		if err := tx.QueryRow(ctx, `SELECT `+failures+`, `+retryCol+` FROM dashboard_users WHERE id = $1 FOR UPDATE`, userID).
			Scan(&attempt, &retryAt); err != nil {
			return notFound(err)
		}
		if retryAt != nil && retryAt.After(now) {
			wait = retryAt.Sub(now)
			return nil
		}
		attempt++
		var next *time.Time
		if d := delay(attempt); d > 0 {
			t := now.Add(d)
			next = &t
		}
		_, err := tx.Exec(ctx, `UPDATE dashboard_users SET `+failures+` = $2, `+retryCol+` = $3 WHERE id = $1`, userID, attempt, next)
		return err
	})
	if err != nil || wait > 0 {
		return 0, wait, opErr("take totp attempt", err)
	}
	return attempt, 0, nil
}

// UseTOTPStep records step as used and reports whether it was newer than every step used
// before. Two requests racing with one code cannot both succeed. An accepted code ends the
// run it was counted in.
func (s *Store) UseTOTPStep(ctx context.Context, userID string, run TOTPRun, step int64) (bool, error) {
	tag, err := s.db.Exec(ctx, `UPDATE dashboard_users SET totp_last_step = $2, `+run.endRun()+`
		WHERE id = $1 AND totp_secret_ciphertext IS NOT NULL AND coalesce(totp_last_step, 0) < $2`, userID, step)
	if err != nil {
		return false, opErr("use totp step", err)
	}
	return tag.RowsAffected() == 1, nil
}

// EndTOTPFailures ends the user's run of failed TOTP attempts without using a step: a right code
// whose action was refused (UseTOTPStep was rolled back with it) is not a failed attempt.
func (s *Store) EndTOTPFailures(ctx context.Context, userID string, run TOTPRun) error {
	_, err := s.db.Exec(ctx, `UPDATE dashboard_users SET `+run.endRun()+` WHERE id = $1`, userID)
	return opErr("end totp failures", err)
}

// RecordPasswordFailure counts a wrong password for the user and returns how many came in a row.
// The count slows nothing down (the rate limits do that); it decides when the owner is told.
func (s *Store) RecordPasswordFailure(ctx context.Context, userID string) (int, error) {
	var n int
	err := s.db.QueryRow(ctx, `UPDATE dashboard_users SET password_failures = password_failures + 1 WHERE id = $1 RETURNING password_failures`, userID).Scan(&n)
	return n, opErr("record password failure", notFound(err))
}

// EndPasswordFailures ends the user's run of wrong passwords (a right password was entered). It
// writes nothing when there is no run.
func (s *Store) EndPasswordFailures(ctx context.Context, userID string) error {
	_, err := s.db.Exec(ctx, `UPDATE dashboard_users SET password_failures = 0 WHERE id = $1 AND password_failures > 0`, userID)
	return opErr("end password failures", err)
}

// AuditAnonymous writes an audit entry that belongs to no merchant (an email with no account).
func (s *Store) AuditAnonymous(ctx context.Context, actor, action string, details map[string]any) error {
	_, err := s.db.Exec(ctx, `INSERT INTO audit_log (merchant_id, actor, action, details) VALUES (NULL, $1, $2, $3)`, actor, action, details)
	return opErr("audit anonymous", err)
}

// Audit writes one audit entry outside any other change (logins, failed attempts).
func (s *Store) Audit(ctx context.Context, merchantID, actor, action string, details map[string]any) error {
	return opErr("audit", s.inTx(ctx, func(tx pgx.Tx) error { return audit(ctx, tx, merchantID, actor, action, details) }))
}
