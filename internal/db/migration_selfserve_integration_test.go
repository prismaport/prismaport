//go:build integration

package db_test

import (
	"bytes"
	"testing"

	"github.com/prismaport/prismaport/migrations"
)

// 00111: recovery codes are one row per code, one pending removal per user, and every hash has
// its size. Deleting a user takes them along.
func TestMigration00111AccountRecovery(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool, m := chainOn(t, migrations.FS)
	if err := m.UpTo(ctx, 111); err != nil {
		t.Fatal(err)
	}
	seedMerchant(t, pool)
	exec(t, pool, `INSERT INTO dashboard_users (id, merchant_id, email, password_hash, created_at, updated_at, recovery_locked_until)
		VALUES ('usr_1', 'mer_1', 'a@example.com', '$argon2id$x', now(), now(), now() + interval '48 hours')`)
	hash := bytes.Repeat([]byte{7}, 32)
	short := []byte{1}
	exec(t, pool, `INSERT INTO dashboard_recovery_codes (user_id, code_hash, created_at) VALUES ('usr_1', $1, now())`, hash)
	mustFail(t, pool, "a repeated code", `INSERT INTO dashboard_recovery_codes (user_id, code_hash, created_at) VALUES ('usr_1', $1, now())`, hash)
	mustFail(t, pool, "a short hash", `INSERT INTO dashboard_recovery_codes (user_id, code_hash, created_at) VALUES ('usr_1', $1, now())`, short)

	exec(t, pool, `INSERT INTO dashboard_totp_lost_links (user_id, token_hash, created_at, expires_at) VALUES ('usr_1', $1, now(), now() + interval '30 minutes')`, hash)
	mustFail(t, pool, "a second link", `INSERT INTO dashboard_totp_lost_links (user_id, token_hash, created_at, expires_at) VALUES ('usr_1', $1, now(), now())`, hash)

	exec(t, pool, `INSERT INTO dashboard_totp_removals (user_id, requested_at, removal_at, cancel_token_hash) VALUES ('usr_1', now(), now() + interval '72 hours', $1)`, hash)
	mustFail(t, pool, "a second removal", `INSERT INTO dashboard_totp_removals (user_id, requested_at, removal_at, cancel_token_hash) VALUES ('usr_1', now(), now() + interval '72 hours', $1)`, hash)
	mustFail(t, pool, "a removal not after the request", `UPDATE dashboard_totp_removals SET removal_at = requested_at WHERE user_id = 'usr_1'`)

	exec(t, pool, `DELETE FROM dashboard_users WHERE id = 'usr_1'`)
	if got := scanText(t, pool, `SELECT (SELECT count(*) FROM dashboard_recovery_codes) + (SELECT count(*) FROM dashboard_totp_lost_links) + (SELECT count(*) FROM dashboard_totp_removals)`); got != "0" {
		t.Fatalf("rows left after deleting the user: %s", got)
	}
}

// 00112: confirmation codes are emailed codes of their own purposes, and each family's oldest
// active wallet becomes its default; at most one active default per merchant, mode and family.
func TestMigration00112ConfirmationCodesDefaultWallet(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool, m := chainOn(t, migrations.FS)
	if err := m.UpTo(ctx, 111); err != nil {
		t.Fatal(err)
	}
	seedMerchant(t, pool)
	exec(t, pool, `INSERT INTO dashboard_users (id, merchant_id, email, password_hash, created_at, updated_at)
		VALUES ('usr_1', 'mer_1', 'a@example.com', '$argon2id$x', now(), now())`)
	wallet := `INSERT INTO wallets (id, merchant_id, livemode, family, label, address, verified, created_at, archived_at)
		VALUES ($1, 'mer_1', $2, $3, 'W', $4, true, now() - $5::interval, $6)`
	exec(t, pool, wallet, "wal_old_archived", true, "evm", "0xA", "3 days", "now")
	exec(t, pool, wallet, "wal_first", true, "evm", "0xB", "2 days", nil)
	exec(t, pool, wallet, "wal_second", true, "evm", "0xC", "1 day", nil)
	exec(t, pool, wallet, "wal_test", false, "evm", "0xB", "1 day", nil)
	exec(t, pool, wallet, "wal_sol", true, "solana", "So1", "1 day", nil)
	if err := m.UpTo(ctx, 112); err != nil {
		t.Fatal(err)
	}
	if got := scanText(t, pool, `SELECT string_agg(id, ',' ORDER BY id) FROM wallets WHERE is_default`); got != "wal_first,wal_sol,wal_test" {
		t.Fatalf("defaults: %s", got)
	}
	mustFail(t, pool, "two defaults", `UPDATE wallets SET is_default = true WHERE id = 'wal_second'`)
	mustFail(t, pool, "an archived default", `UPDATE wallets SET archived_at = now() WHERE id = 'wal_first'`)
	exec(t, pool, `INSERT INTO wallets (id, merchant_id, livemode, family, label, address, verified, created_at)
		VALUES ('wal_new', 'mer_1', true, 'evm', 'W', '0xD', true, now())`)
	if got := scanText(t, pool, `SELECT is_default::text FROM wallets WHERE id = 'wal_new'`); got != "false" {
		t.Fatalf("a new wallet's default: %s", got)
	}

	code := `INSERT INTO dashboard_email_codes (user_id, purpose, code_hash, attempts, created_at, expires_at) VALUES ('usr_1', $1, $2, 0, now(), now() + interval '15 minutes')`
	for _, p := range []string{"confirm_wallet_create", "confirm_wallet_archive", "confirm_api_key", "confirm_webhook_endpoint", "confirm_totp_enroll"} {
		exec(t, pool, code, p, make([]byte, 32))
	}
	mustFail(t, pool, "unknown purpose", code, "confirm_login", make([]byte, 32))

	if err := m.DownTo(ctx, 111); err != nil {
		t.Fatal(err)
	}
	if got := scanText(t, pool, `SELECT count(*)::text FROM dashboard_email_codes`); got != "0" {
		t.Fatalf("confirmation codes left after down: %s", got)
	}
}
