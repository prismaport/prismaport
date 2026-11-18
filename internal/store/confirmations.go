package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// ConfirmPurpose is the stored purpose of a confirmation code (D58) for an action such as
// "wallet_create": one live code per user and action, which confirms only that action.
func ConfirmPurpose(action string) string { return "confirm_" + action }

// IssueConfirmationCode stores a new confirmation code for the user, replacing any earlier one
// of the purpose, and audits it. A code of the purpose stored less than minGap ago stays and the
// answer is the time left until a new one may be sent (nothing is stored then).
func (s *Store) IssueConfirmationCode(ctx context.Context, u DashboardUser, purpose string, code EmailCode, minGap time.Duration) (time.Duration, error) {
	var wait time.Duration
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		now := s.now()
		// The user's row lock orders two requests for the same purpose.
		var id string
		if err := tx.QueryRow(ctx, `SELECT id FROM dashboard_users WHERE id = $1 FOR UPDATE`, u.ID).Scan(&id); err != nil {
			return notFound(err)
		}
		var created time.Time
		err := tx.QueryRow(ctx, `SELECT created_at FROM dashboard_email_codes WHERE user_id = $1 AND purpose = $2`, u.ID, purpose).Scan(&created)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err == nil && now.Sub(created) < minGap {
			wait = minGap - now.Sub(created)
			return nil
		}
		if err := upsertCode(ctx, tx, u.ID, purpose, code, now); err != nil {
			return err
		}
		return audit(ctx, tx, u.MerchantID, "dashboard:"+u.ID, "user.confirmation_sent", map[string]any{"user_id": u.ID, "purpose": purpose})
	})
	return wait, opErr("issue confirmation code", err)
}

// UseConfirmationCode spends the user's confirmation code of c.Purpose when c.Matches it, under
// the rules of every emailed code: a wrong one spends one of the code's attempts and one of the
// user's failures, committed at once. Every failure is ErrCodeInvalid. write, the action the code
// confirms, runs on a store bound to the same transaction: when it fails, the code is not spent
// and stays good for the corrected request.
func (s *Store) UseConfirmationCode(ctx context.Context, c CodeCheck, write func(st *Store) error) error {
	_, err := s.consumeCode(ctx, c, func(tx pgx.Tx, u DashboardUser, _ time.Time) error {
		if err := audit(ctx, tx, u.MerchantID, "dashboard:"+u.ID, "user.confirmation_used", map[string]any{"user_id": u.ID, "purpose": c.Purpose}); err != nil {
			return err
		}
		return write(s.Bind(tx))
	})
	return opErr("use confirmation code", err)
}

// DropConfirmationCode deletes the user's code of purpose when it is still the one with hash: a
// code that could not be emailed. With it gone, a new one may be asked for at once.
func (s *Store) DropConfirmationCode(ctx context.Context, u DashboardUser, purpose string, hash []byte) error {
	return opErr("drop confirmation code", s.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM dashboard_email_codes WHERE user_id = $1 AND purpose = $2 AND code_hash = $3`, u.ID, purpose, hash)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		return audit(ctx, tx, u.MerchantID, "dashboard:"+u.ID, "user.confirmation_not_sent", map[string]any{"user_id": u.ID, "purpose": purpose})
	}))
}

// OpenConfirmWindow opens the confirmation window of the dashboard session (D59) until until, with
// wallets Live wallets already added in it. It runs in the transaction of the action the code
// confirmed; a session that ended opens nothing.
func (s *Store) OpenConfirmWindow(ctx context.Context, sessionHash [32]byte, until time.Time, wallets int) error {
	_, err := s.db.Exec(ctx, `UPDATE dashboard_sessions SET confirmed_until = $2, confirmed_wallets = $3 WHERE token_hash = $1`,
		sessionHash[:], until, wallets)
	return opErr("open confirm window", err)
}

// UseConfirmWindow runs write, an action the session's confirmation window covers, in one
// transaction with the session row locked. It answers false and runs nothing when the window is
// closed or, for a Live wallet (wallet true), when maxWallets were already added in it; a wallet
// counts once write succeeds. Using the window never extends it.
func (s *Store) UseConfirmWindow(ctx context.Context, sessionHash [32]byte, wallet bool, maxWallets int, write func(st *Store) error) (bool, error) {
	used := false
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var until *time.Time
		var wallets int
		err := tx.QueryRow(ctx, `SELECT confirmed_until, confirmed_wallets FROM dashboard_sessions WHERE token_hash = $1 FOR UPDATE`,
			sessionHash[:]).Scan(&until, &wallets)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if until == nil || !until.After(s.now()) || (wallet && wallets >= maxWallets) {
			return nil
		}
		if err := write(s.Bind(tx)); err != nil {
			return err
		}
		if wallet {
			if _, err := tx.Exec(ctx, `UPDATE dashboard_sessions SET confirmed_wallets = confirmed_wallets + 1 WHERE token_hash = $1`,
				sessionHash[:]); err != nil {
				return err
			}
		}
		used = true
		return nil
	})
	return used, opErr("use confirm window", err)
}

// CloseConfirmWindow ends the session's open confirmation window ("Lock now", D61). It answers
// whether one was open.
func (s *Store) CloseConfirmWindow(ctx context.Context, sessionHash [32]byte) (bool, error) {
	tag, err := s.db.Exec(ctx, `UPDATE dashboard_sessions SET confirmed_until = NULL, confirmed_wallets = 0
		WHERE token_hash = $1 AND confirmed_until > $2`, sessionHash[:], s.now())
	if err != nil {
		return false, opErr("close confirm window", err)
	}
	return tag.RowsAffected() == 1, nil
}
