package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// TOTPRemoval is a pending removal of a user's two-factor authentication (D56).
type TOTPRemoval struct {
	UserID      string
	MerchantID  string
	Email       string
	RequestedAt time.Time
	RemovalAt   time.Time
	// LockedUntil is set once the removal happened.
	LockedUntil time.Time
}

// replaceRecoveryCodes swaps all the user's recovery codes for hashes in the caller's
// transaction.
func replaceRecoveryCodes(ctx context.Context, tx pgx.Tx, userID string, hashes [][]byte, now time.Time) error {
	if _, err := tx.Exec(ctx, `DELETE FROM dashboard_recovery_codes WHERE user_id = $1`, userID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO dashboard_recovery_codes (user_id, code_hash, created_at)
		SELECT $1::text, h, $3::timestamptz FROM unnest($2::bytea[]) AS h`, userID, hashes, now)
	return err
}

// endOtherSessions deletes every session of the user except keep, and reports how many ended.
func endOtherSessions(ctx context.Context, tx pgx.Tx, userID string, keep [32]byte) (int64, error) {
	tag, err := tx.Exec(ctx, `DELETE FROM dashboard_sessions WHERE user_id = $1 AND token_hash <> $2`, userID, keep[:])
	return tag.RowsAffected(), err
}

// ChangePassword sets a new password for a signed-in user and ends every other session. oldHash
// is the hash the caller checked the current password against: if it changed meanwhile nothing
// is stored and the result is ErrConflict.
func (s *Store) ChangePassword(ctx context.Context, userID, oldHash, newHash string, keep [32]byte, actor string) error {
	now := s.now()
	return opErr("change password", s.inTx(ctx, func(tx pgx.Tx) error {
		var merchantID string
		err := tx.QueryRow(ctx, `UPDATE dashboard_users SET password_hash = $2, updated_at = $3 WHERE id = $1 AND password_hash = $4 RETURNING merchant_id`,
			userID, newHash, now, oldHash).Scan(&merchantID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrConflict
		}
		if err != nil {
			return err
		}
		ended, err := endOtherSessions(ctx, tx, userID, keep)
		if err != nil {
			return err
		}
		return audit(ctx, tx, merchantID, actor, "user.password_changed", map[string]any{"user_id": userID, "sessions_ended": ended})
	}))
}

// ReplaceRecoveryCodes swaps all the user's recovery codes atomically. ErrNotFound when
// two-factor authentication is off.
func (s *Store) ReplaceRecoveryCodes(ctx context.Context, userID string, hashes [][]byte, actor string) error {
	now := s.now()
	return opErr("replace recovery codes", s.inTx(ctx, func(tx pgx.Tx) error {
		var merchantID string
		if err := tx.QueryRow(ctx, `SELECT merchant_id FROM dashboard_users WHERE id = $1 AND totp_secret_ciphertext IS NOT NULL FOR UPDATE`, userID).
			Scan(&merchantID); err != nil {
			return notFound(err)
		}
		if err := replaceRecoveryCodes(ctx, tx, userID, hashes, now); err != nil {
			return err
		}
		return audit(ctx, tx, merchantID, actor, "user.recovery_codes_created", map[string]any{"user_id": userID, "count": len(hashes)})
	}))
}

// UseRecoveryCode spends the unused recovery code with this hash and reports whether there was
// one: the single UPDATE is what stops two requests from spending one code. An accepted code ends
// the run it was entered in, like an accepted TOTP code.
func (s *Store) UseRecoveryCode(ctx context.Context, u DashboardUser, run TOTPRun, hash []byte, actor string) (bool, error) {
	now := s.now()
	used := false
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE dashboard_recovery_codes SET used_at = $3 WHERE user_id = $1 AND code_hash = $2 AND used_at IS NULL`, u.ID, hash, now)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		used = true
		if _, err := tx.Exec(ctx, `UPDATE dashboard_users SET `+run.endRun()+` WHERE id = $1`, u.ID); err != nil {
			return err
		}
		return audit(ctx, tx, u.MerchantID, actor, "user.recovery_code_used", map[string]any{"user_id": u.ID})
	})
	return used, opErr("use recovery code", err)
}

// lostLinkQuery finds the enabled user with two-factor authentication on that a live lost link
// belongs to, by the link's token hash ($1) at time $2.
const lostLinkQuery = `SELECT ` + userColumns + ` FROM dashboard_totp_lost_links l
	JOIN dashboard_users u ON u.id = l.user_id JOIN merchants m ON m.id = u.merchant_id
	WHERE l.token_hash = $1 AND l.expires_at > $2 AND u.disabled_at IS NULL AND u.totp_secret_ciphertext IS NOT NULL`

// IssueTOTPLostLink stores a new lost link for the user (replacing any live one).
func (s *Store) IssueTOTPLostLink(ctx context.Context, u DashboardUser, tokenHash []byte, expiresAt time.Time) error {
	now := s.now()
	return opErr("issue totp lost link", s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO dashboard_totp_lost_links (user_id, token_hash, created_at, expires_at) VALUES ($1, $2, $3, $4)
			ON CONFLICT (user_id) DO UPDATE SET token_hash = EXCLUDED.token_hash, created_at = EXCLUDED.created_at, expires_at = EXCLUDED.expires_at,
				password_failures = 0`,
			u.ID, tokenHash, now, expiresAt); err != nil {
			return err
		}
		return audit(ctx, tx, u.MerchantID, "dashboard:"+u.ID, "user.totp_lost_requested", map[string]any{"user_id": u.ID})
	}))
}

// TOTPLostLinkUser is the user a lost link belongs to, without spending the link. ErrCodeInvalid
// when no live link has this hash.
func (s *Store) TOTPLostLinkUser(ctx context.Context, tokenHash []byte) (DashboardUser, error) {
	u, err := scanUser(s.db.QueryRow(ctx, lostLinkQuery, tokenHash, s.now()))
	if errors.Is(err, ErrNotFound) {
		return DashboardUser{}, ErrCodeInvalid
	}
	return u, opErr("totp lost link user", err)
}

// FailTOTPLostLink counts a wrong password entered on a lost link; the maxTries-th one deletes
// the link, so its password cannot be guessed for as long as it lives. ended reports that
// deletion; a link that is already gone counts nothing.
func (s *Store) FailTOTPLostLink(ctx context.Context, linkHash []byte, maxTries int) (ended bool, err error) {
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var userID string
		var tries int
		err := tx.QueryRow(ctx, `UPDATE dashboard_totp_lost_links SET password_failures = password_failures + 1 WHERE token_hash = $1
			RETURNING user_id, password_failures`, linkHash).Scan(&userID, &tries)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && tries < maxTries {
			return nil
		}
		if err != nil {
			return err
		}
		ended = true
		_, err = tx.Exec(ctx, `DELETE FROM dashboard_totp_lost_links WHERE user_id = $1`, userID)
		return err
	})
	return ended, opErr("fail totp lost link", err)
}

// RequestTOTPRemoval spends a lost link and schedules the removal wait from now. cancelHash gives
// the stored hash of the cancel token of a removal requested at a time. A removal already pending
// keeps its time: it is returned with created false. ErrCodeInvalid when no live link has this
// hash.
func (s *Store) RequestTOTPRemoval(ctx context.Context, linkHash []byte, wait time.Duration,
	cancelHash func(userID string, requestedAt time.Time) []byte) (TOTPRemoval, bool, error) {
	now := s.now()
	var r TOTPRemoval
	created := false
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		u, err := scanUser(tx.QueryRow(ctx, lostLinkQuery+` FOR UPDATE OF l, u`, linkHash, now))
		if errors.Is(err, ErrNotFound) {
			return ErrCodeInvalid
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM dashboard_totp_lost_links WHERE user_id = $1`, u.ID); err != nil {
			return err
		}
		r = TOTPRemoval{UserID: u.ID, MerchantID: u.MerchantID, Email: u.Email}
		err = tx.QueryRow(ctx, `SELECT requested_at, removal_at FROM dashboard_totp_removals WHERE user_id = $1 FOR UPDATE`, u.ID).
			Scan(&r.RequestedAt, &r.RemovalAt)
		if err == nil {
			return audit(ctx, tx, u.MerchantID, "dashboard:"+u.ID, "user.totp_removal_repeated", map[string]any{"user_id": u.ID})
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		r.RequestedAt, r.RemovalAt = now, now.Add(wait)
		if _, err := tx.Exec(ctx, `INSERT INTO dashboard_totp_removals (user_id, requested_at, removal_at, cancel_token_hash) VALUES ($1, $2, $3, $4)`,
			u.ID, r.RequestedAt, r.RemovalAt, cancelHash(u.ID, r.RequestedAt)); err != nil {
			return err
		}
		created = true
		return audit(ctx, tx, u.MerchantID, "dashboard:"+u.ID, "user.totp_removal_requested", map[string]any{"user_id": u.ID, "removal_at": r.RemovalAt})
	})
	return r, created, opErr("request totp removal", err)
}

// cancelRemoval deletes the user's pending removal in the caller's transaction and audits how it
// ended; false when none was pending.
func cancelRemoval(ctx context.Context, tx pgx.Tx, userID, merchantID, actor, via string) (bool, error) {
	tag, err := tx.Exec(ctx, `DELETE FROM dashboard_totp_removals WHERE user_id = $1`, userID)
	if err != nil || tag.RowsAffected() == 0 {
		return false, err
	}
	return true, audit(ctx, tx, merchantID, actor, "user.totp_removal_cancelled", map[string]any{"user_id": userID, "via": via})
}

// CancelTOTPRemoval cancels the user's pending removal, if any, and reports whether there was
// one. via says what cancelled it ("session", "sign_in").
func (s *Store) CancelTOTPRemoval(ctx context.Context, u DashboardUser, actor, via string) (bool, error) {
	cancelled := false
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		cancelled, err = cancelRemoval(ctx, tx, u.ID, u.MerchantID, actor, via)
		return err
	})
	return cancelled, opErr("cancel totp removal", err)
}

// CancelTOTPRemovalByToken cancels the removal the emailed cancel link belongs to and returns its
// user. ErrCodeInvalid when no pending removal has this hash.
func (s *Store) CancelTOTPRemovalByToken(ctx context.Context, cancelHash []byte) (DashboardUser, error) {
	var user DashboardUser
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		u, err := scanUser(tx.QueryRow(ctx, `SELECT `+userColumns+` FROM dashboard_totp_removals r
			JOIN dashboard_users u ON u.id = r.user_id JOIN merchants m ON m.id = u.merchant_id
			WHERE r.cancel_token_hash = $1 FOR UPDATE OF r`, cancelHash))
		if errors.Is(err, ErrNotFound) {
			return ErrCodeInvalid
		}
		if err != nil {
			return err
		}
		user = u
		_, err = cancelRemoval(ctx, tx, u.ID, u.MerchantID, "dashboard:"+u.ID, "link")
		return err
	})
	return user, opErr("cancel totp removal by token", err)
}

// ClaimTOTPRemovalReminders marks up to batch due reminders as sent and returns them: removals
// not yet reminded of, at most within ahead of their time and not yet past it. Each is reminded
// once; the email goes out after this commits.
func (s *Store) ClaimTOTPRemovalReminders(ctx context.Context, within time.Duration, batch int) ([]TOTPRemoval, error) {
	now := s.now()
	var out []TOTPRemoval
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		out = out[:0]
		rows, err := tx.Query(ctx, `SELECT r.user_id, u.merchant_id, u.email, r.requested_at, r.removal_at
			FROM dashboard_totp_removals r JOIN dashboard_users u ON u.id = r.user_id
			WHERE r.reminded_at IS NULL AND r.removal_at > $1 AND r.removal_at <= $2
			ORDER BY r.removal_at LIMIT $3 FOR UPDATE OF r SKIP LOCKED`, now, now.Add(within), batch)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r TOTPRemoval
			if err := rows.Scan(&r.UserID, &r.MerchantID, &r.Email, &r.RequestedAt, &r.RemovalAt); err != nil {
				return err
			}
			out = append(out, r)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		rows.Close()
		for _, r := range out {
			if _, err := tx.Exec(ctx, `UPDATE dashboard_totp_removals SET reminded_at = $2 WHERE user_id = $1`, r.UserID, now); err != nil {
				return err
			}
			if err := audit(ctx, tx, r.MerchantID, "worker", "user.totp_removal_reminded", map[string]any{"user_id": r.UserID}); err != nil {
				return err
			}
		}
		return nil
	})
	return out, opErr("claim totp removal reminders", err)
}

// RemoveDueTOTP carries out one removal whose time has come, in one transaction: the secret and
// every recovery code go, the pending row goes, live credentials lock for lock after removal_at,
// and every session of the user ends. false when none is due. A row locked by another request is
// skipped and taken at the next pass, so the removal can be late but never early.
func (s *Store) RemoveDueTOTP(ctx context.Context, lock time.Duration) (TOTPRemoval, bool, error) {
	now := s.now()
	var r TOTPRemoval
	found := false
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT r.user_id, u.merchant_id, u.email, r.requested_at, r.removal_at
			FROM dashboard_totp_removals r JOIN dashboard_users u ON u.id = r.user_id
			WHERE r.removal_at <= $1 ORDER BY r.removal_at LIMIT 1 FOR UPDATE OF r, u SKIP LOCKED`, now).
			Scan(&r.UserID, &r.MerchantID, &r.Email, &r.RequestedAt, &r.RemovalAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		r.LockedUntil = r.RemovalAt.Add(lock)
		if _, err := tx.Exec(ctx, `UPDATE dashboard_users SET totp_secret_ciphertext = NULL, totp_pending_ciphertext = NULL, totp_pending_session = NULL,
			totp_pending_expires_at = NULL, totp_last_step = NULL, totp_failures = 0, totp_retry_at = NULL, totp_step_failures = 0, totp_step_retry_at = NULL,
			recovery_locked_until = $2, updated_at = $3
			WHERE id = $1`, r.UserID, r.LockedUntil, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM dashboard_recovery_codes WHERE user_id = $1`, r.UserID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM dashboard_totp_removals WHERE user_id = $1`, r.UserID); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `DELETE FROM dashboard_sessions WHERE user_id = $1`, r.UserID)
		if err != nil {
			return err
		}
		return audit(ctx, tx, r.MerchantID, "worker", "user.totp_removed_by_recovery", map[string]any{"user_id": r.UserID,
			"locked_until": r.LockedUntil, "sessions_ended": tag.RowsAffected()})
	})
	return r, found, opErr("remove due totp", err)
}
