//go:build integration

package db_test

import (
	"context"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prismaport/prismaport/internal/db"
	"github.com/prismaport/prismaport/internal/testdb"
	"github.com/prismaport/prismaport/migrations"
)

const maxUint256 = "115792089237316195423570985008687907853269984665640564039457584007913129639935"

func chainOn(t *testing.T, fsys fs.FS) (*pgxpool.Pool, *db.Chain) {
	t.Helper()
	pool := testdb.Open(t, testdb.Empty(t))
	c, err := db.NewChain(pool, fsys)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return pool, c
}

func readSchema(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	s, err := db.ReadSchema(t.Context(), pool)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestEveryMigrationRoundtrips applies each migration, rolls it back and applies it again, then
// checks that a full down/up cycle reproduces the same schema. Each migration after the baseline
// must also leave exactly the schema it found when rolled back, and the same schema again when
// applied a second time.
func TestEveryMigrationRoundtrips(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool, m := chainOn(t, migrations.FS)
	versions := m.Versions()
	if len(versions) == 0 {
		t.Fatal("no migrations embedded")
	}
	prev := int64(0)
	for _, v := range versions {
		// Rolling the baseline back drops everything; only the migrations after it are held to an
		// exact rollback.
		exact := v > migrations.BaselineVersion
		schema := func() string {
			if !exact {
				return ""
			}
			return readSchema(t, pool)
		}
		before := schema()
		if err := m.UpTo(ctx, v); err != nil {
			t.Fatalf("up %d: %v", v, err)
		}
		after := schema()
		if err := m.DownTo(ctx, prev); err != nil {
			t.Fatalf("down %d -> %d: %v", v, prev, err)
		}
		if diff := db.DiffSchemas(before, schema()); diff != "" {
			t.Fatalf("down %d leaves a schema (+) other than %d's (-):\n%s", v, prev, diff)
		}
		if err := m.UpTo(ctx, v); err != nil {
			t.Fatalf("re-up %d: %v", v, err)
		}
		if diff := db.DiffSchemas(after, schema()); diff != "" {
			t.Fatalf("up %d again builds a schema (+) other than its first up (-):\n%s", v, diff)
		}
		prev = v
	}
	first := readSchema(t, pool)
	if err := m.DownTo(ctx, 0); err != nil {
		t.Fatalf("down to 0: %v", err)
	}
	if s := readSchema(t, pool); s != "" {
		t.Fatalf("objects left after full down:\n%s", s)
	}
	if err := m.Up(ctx); err != nil {
		t.Fatal(err)
	}
	if diff := db.DiffSchemas(first, readSchema(t, pool)); diff != "" {
		t.Fatalf("schema differs after down/up cycle:\n%s", diff)
	}
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func mustFail(t *testing.T, pool *pgxpool.Pool, why, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), sql, args...); err == nil {
		t.Fatalf("%s: accepted: %s", why, sql)
	}
}

func scanText(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(t.Context(), sql, args...).Scan(&s); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return s
}

func seedMerchant(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	exec(t, pool, `INSERT INTO merchants (id, name) VALUES ('mer_1', 'Acme Store')`)
}

// insertProduct inserts a live product of mer_1 paying a live EVM wallet 100% on base, in the
// same transaction.
func insertProduct(t *testing.T, pool *pgxpool.Pool, id, sql string, args ...any) {
	t.Helper()
	exec(t, pool, `INSERT INTO wallets (id, merchant_id, livemode, family, label, address, verified, created_at)
		VALUES ('wal_t', 'mer_1', true, 'evm', 'Main', '0xT', true, now()) ON CONFLICT DO NOTHING`)
	err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(t.Context(), sql, args...); err != nil {
			return err
		}
		// Since 00113 a payout needs its network row; the binary writes it first (00116 dropped
		// the trigger that filled it in for the 0.4.0 binary).
		if hasColumn(t, pool, "product_networks", "chain_id") {
			if _, err := tx.Exec(t.Context(), `INSERT INTO product_networks (product_id, merchant_id, livemode, chain_id, accept)
				VALUES ($1, 'mer_1', true, 'base', 'all')`, id); err != nil {
				return err
			}
		}
		_, err := tx.Exec(t.Context(), `INSERT INTO product_payout_items (product_id, merchant_id, livemode, chain_id, family, position, wallet_id, bps)
			VALUES ($1, 'mer_1', true, 'base', 'evm', 0, 'wal_t', 10000)`, id)
		return err
	})
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func TestMigration00001Merchants(t *testing.T) {
	t.Parallel()
	pool := testdb.Pool(t)
	seedMerchant(t, pool)

	exec(t, pool, `CREATE TEMP TABLE money_probe (v base_units)`)
	exec(t, pool, `INSERT INTO money_probe VALUES ($1::numeric)`, maxUint256)
	if got := scanText(t, pool, `SELECT v::text FROM money_probe`); got != maxUint256 {
		t.Fatalf("2^256-1 roundtrip: %s", got)
	}
	for _, bad := range []string{"1.5", "0.000001", "-1", "115792089237316195423570985008687907853269984665640564039457584007913129639936"} {
		mustFail(t, pool, "base_units "+bad, `INSERT INTO money_probe VALUES ($1::numeric)`, bad)
	}

	key := make([]byte, 32)
	exec(t, pool, `INSERT INTO api_keys (id, merchant_id, livemode, key_hash, key_prefix, scopes)
		VALUES ('key_1', 'mer_1', false, $1, 'sk_test_abcd', $2)`, key, []string{"sessions:write"})
	var scopes []string
	if err := pool.QueryRow(t.Context(), `SELECT scopes FROM api_keys WHERE id = 'key_1'`).Scan(&scopes); err != nil || len(scopes) != 1 {
		t.Fatalf("scopes roundtrip: %v %v", scopes, err)
	}
	mustFail(t, pool, "empty scopes", `INSERT INTO api_keys (id, merchant_id, livemode, key_hash, key_prefix, scopes)
		VALUES ('key_2', 'mer_1', false, $1, 'p', '{}')`, make([]byte, 32))
	mustFail(t, pool, "short hash", `INSERT INTO api_keys (id, merchant_id, livemode, key_hash, key_prefix, scopes)
		VALUES ('key_3', 'mer_1', false, $1, 'p', '{x}')`, make([]byte, 31))

	exec(t, pool, `INSERT INTO merchant_assets VALUES ('mer_1', true, 'base', 'USDC')`)
	mustFail(t, pool, "duplicate asset", `INSERT INTO merchant_assets VALUES ('mer_1', true, 'base', 'USDC')`)
	exec(t, pool, `INSERT INTO audit_log (merchant_id, actor, action, details) VALUES ('mer_1', 'cli', 'x', '{"a":1}')`)
}

func TestMigration00002Products(t *testing.T) {
	t.Parallel()
	pool := testdb.Pool(t)
	seedMerchant(t, pool)
	insertProduct(t, pool, "prod_1", `INSERT INTO products (id, merchant_id, livemode, name, price_type, amount_cents)
		VALUES ('prod_1', 'mer_1', true, 'Founder', 'fixed', $1::numeric)`, "2500")
	if got := scanText(t, pool, `SELECT amount_cents::text FROM products WHERE id = 'prod_1'`); got != "2500" {
		t.Fatalf("amount roundtrip %s", got)
	}
	insertProduct(t, pool, "prod_2", `INSERT INTO products (id, merchant_id, livemode, name, price_type, min_amount_cents, max_amount_cents, suggested_amounts_cents)
		VALUES ('prod_2', 'mer_1', true, 'Donate', 'open', 100, 10000, $1::text[]::base_units[])`, []string{"500", "1000"})
	if got := scanText(t, pool, `SELECT suggested_amounts_cents::text FROM products WHERE id = 'prod_2'`); got != "{500,1000}" {
		t.Fatalf("suggested roundtrip %s", got)
	}
	cases := map[string]string{
		"fixed without amount":   `INSERT INTO products (id, merchant_id, livemode, name, price_type) VALUES ('p', 'mer_1', true, 'x', 'fixed')`,
		"fixed with min":         `INSERT INTO products (id, merchant_id, livemode, name, price_type, amount_cents, min_amount_cents) VALUES ('p', 'mer_1', true, 'x', 'fixed', 1, 1)`,
		"open without min":       `INSERT INTO products (id, merchant_id, livemode, name, price_type) VALUES ('p', 'mer_1', true, 'x', 'open')`,
		"open max below min":     `INSERT INTO products (id, merchant_id, livemode, name, price_type, min_amount_cents, max_amount_cents) VALUES ('p', 'mer_1', true, 'x', 'open', 10, 9)`,
		"four suggested":         `INSERT INTO products (id, merchant_id, livemode, name, price_type, min_amount_cents, suggested_amounts_cents) VALUES ('p', 'mer_1', true, 'x', 'open', 10, '{10,20,30,40}')`,
		"fractional suggested":   `INSERT INTO products (id, merchant_id, livemode, name, price_type, min_amount_cents, suggested_amounts_cents) VALUES ('p', 'mer_1', true, 'x', 'open', 10, '{10.5}')`,
		"fractional amount":      `INSERT INTO products (id, merchant_id, livemode, name, price_type, amount_cents) VALUES ('p', 'mer_1', true, 'x', 'fixed', 2500.5)`,
		"zero amount":            `INSERT INTO products (id, merchant_id, livemode, name, price_type, amount_cents) VALUES ('p', 'mer_1', true, 'x', 'fixed', 0)`,
		"metadata not an object": `INSERT INTO products (id, merchant_id, livemode, name, price_type, amount_cents, metadata) VALUES ('p', 'mer_1', true, 'x', 'fixed', 1, '[]')`,
		"empty name":             `INSERT INTO products (id, merchant_id, livemode, name, price_type, amount_cents) VALUES ('p', 'mer_1', true, '', 'fixed', 1)`,
	}
	for why, sql := range cases {
		mustFail(t, pool, why, sql)
	}
}

func hasColumn(t *testing.T, pool *pgxpool.Pool, table, column string) bool {
	t.Helper()
	return scanText(t, pool, `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = 'public'
		AND table_name = $1 AND column_name = $2)::text`, table, column) == "true"
}

// seedSession adds a session paying 25 USDC on Base to 0xdst: a one-recipient payout snapshot.
func seedSession(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	seedMerchant(t, pool)
	exec(t, pool, `INSERT INTO sessions (id, merchant_id, livemode, amount_cents, status, version, salt, expires_at)
		VALUES ('ses_1', 'mer_1', true, 2500, 'open', 1, $1, now() + interval '1 hour')`, make([]byte, 32))
	exec(t, pool, `WITH p AS (INSERT INTO session_pay_to (session_id, position, chain, asset, family, token, decimals, address, amount, treasury, fee_bps, fixed_fee_micros, bound_tokens)
			VALUES ('ses_1', 0, 'base', 'USDC', 'evm', '0xtoken', 6, '0xfwd', $1::numeric, '0xtre', 100, 100000, '{0xtoken}'))
		INSERT INTO session_payouts (session_id, chain_id, family, position, address, bps, merchant_id, livemode) VALUES ('ses_1', 'base', 'evm', 0, '0xdst', 10000, 'mer_1', true)`, "25000000")
}

// settle records a settlement and its single payout in one statement (one transaction), as the
// commit-time check wants them together.
func settle(t *testing.T, pool *pgxpool.Pool, id, gross, fee, net, tx string) error {
	t.Helper()
	_, err := pool.Exec(t.Context(), `WITH s AS (INSERT INTO settlements (id, session_id, chain, asset, gross, fee, net, tx_hash, log_index, settled_at)
			VALUES ($1, 'ses_1', 'base', 'USDC', $2::numeric, $3::numeric, $4::numeric, $5, 0, now()) RETURNING id, net)
		INSERT INTO settlement_payouts (settlement_id, position, address, amount) SELECT id, 0, '0xdst', net FROM s`, id, gross, fee, net, tx)
	return err
}

func TestMigration00003Sessions(t *testing.T) {
	t.Parallel()
	pool := testdb.Pool(t)
	seedSession(t, pool)
	if got := scanText(t, pool, `SELECT amount::text FROM session_pay_to WHERE session_id = 'ses_1'`); got != "25000000" {
		t.Fatalf("pay_to amount %s", got)
	}
	payment := `INSERT INTO payments (id, session_id, chain, asset, address, tx_hash, log_index, amount, decimals, status, late, detected_at)
		VALUES ($1, 'ses_1', 'base', 'USDC', '0xfwd', '0xtx', 3, $2::numeric, 6, $3, false, now())`
	exec(t, pool, payment, "pay_1", maxUint256, "reversed")
	exec(t, pool, payment, "pay_2", "1", "confirmed")
	mustFail(t, pool, "same log twice while live", payment, "pay_3", "1", "detected")
	if got := scanText(t, pool, `SELECT amount::text FROM payments WHERE id = 'pay_1'`); got != maxUint256 {
		t.Fatalf("payment amount %s", got)
	}
	mustFail(t, pool, "payment on a chain without pay_to", `INSERT INTO payments (id, session_id, chain, asset, address, tx_hash, log_index, amount, decimals, status, late, detected_at)
		VALUES ('pay_9', 'ses_1', 'solana', 'USDC', 'x', 'tx', 0, 1, 6, 'detected', false, now())`)
	mustFail(t, pool, "fractional payment", payment, "pay_4", "1.5", "detected")
	mustFail(t, pool, "bad status", `UPDATE sessions SET status = 'refunded' WHERE id = 'ses_1'`)
	mustFail(t, pool, "expiry before creation", `INSERT INTO sessions (id, merchant_id, livemode, amount_cents, status, version, salt, expires_at)
		VALUES ('ses_2', 'mer_1', true, 1, 'open', 1, $1, now() - interval '1 second')`, append(make([]byte, 31), 1))
	mustFail(t, pool, "fee above cap", `UPDATE session_pay_to SET fee_bps = 301`)

	if err := settle(t, pool, "stl_1", "25000000", "250000", "24750000", "0xs1"); err != nil {
		t.Fatal(err)
	}
	if settle(t, pool, "stl_2", "25000000", "250000", "24750001", "0xs2") == nil {
		t.Fatal("fee+net != gross accepted")
	}
	var gross, fee, net string
	if err := pool.QueryRow(t.Context(), `SELECT gross::text, fee::text, net::text FROM settlements WHERE id = 'stl_1'`).Scan(&gross, &fee, &net); err != nil {
		t.Fatal(err)
	}
	if gross != "25000000" || fee != "250000" || net != "24750000" {
		t.Fatalf("settlement roundtrip %s %s %s", gross, fee, net)
	}
}

func TestMigration00004EventsWebhooks(t *testing.T) {
	t.Parallel()
	pool := testdb.Pool(t)
	seedSession(t, pool)
	payload := `{"id":"evt_1","type":"session.paid","data":{"session":{"z":1,"a":"25.00"}}}`
	exec(t, pool, `INSERT INTO events (id, merchant_id, livemode, type, session_id, session_version, payload, created_at)
		VALUES ('evt_1', 'mer_1', true, 'session.paid', 'ses_1', 2, $1, $2)`, payload, time.Now())
	if got := scanText(t, pool, `SELECT payload::text FROM events WHERE id = 'evt_1'`); got != payload {
		t.Fatalf("payload not byte-exact:\n%s\n%s", got, payload)
	}
	mustFail(t, pool, "two events for one version", `INSERT INTO events (id, merchant_id, livemode, type, session_id, session_version, payload, created_at)
		VALUES ('evt_2', 'mer_1', true, 'session.paid', 'ses_1', 2, '{}', now())`)
	exec(t, pool, `UPDATE events SET seq = 1 WHERE id = 'evt_1'`)
	exec(t, pool, `INSERT INTO events (id, merchant_id, livemode, type, session_id, session_version, payload, created_at, seq)
		VALUES ('evt_3', 'mer_1', true, 'session.paid', 'ses_1', 3, '{}', now(), 2)`)
	mustFail(t, pool, "duplicate seq", `UPDATE events SET seq = 1 WHERE id = 'evt_3'`)

	exec(t, pool, `INSERT INTO webhook_endpoints (id, merchant_id, livemode, url, enabled_events, secret_ciphertext)
		VALUES ('we_1', 'mer_1', true, 'https://x', $1, $2)`, []string{}, []byte{1})
	var n int
	if err := pool.QueryRow(t.Context(), `SELECT cardinality(enabled_events) FROM webhook_endpoints WHERE id = 'we_1'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("empty enabled_events must stay an empty array, not NULL: %d %v", n, err)
	}
	mustFail(t, pool, "previous secret without expiry", `UPDATE webhook_endpoints SET previous_secret_ciphertext = '\x01'`)
	ownerCol, ownerVal := "", ""
	if hasColumn(t, pool, "webhook_deliveries", "merchant_id") {
		ownerCol, ownerVal = ", merchant_id, livemode", ", 'mer_1', true"
	}
	exec(t, pool, `INSERT INTO webhook_deliveries (id, endpoint_id, event_id, status, next_attempt_at`+ownerCol+`) VALUES ('whd_1', 'we_1', 'evt_1', 'pending', now()`+ownerVal+`)`)
	mustFail(t, pool, "duplicate delivery", `INSERT INTO webhook_deliveries (id, endpoint_id, event_id, status, next_attempt_at`+ownerCol+`) VALUES ('whd_2', 'we_1', 'evt_1', 'pending', now()`+ownerVal+`)`)
	mustFail(t, pool, "bad delivery status", `UPDATE webhook_deliveries SET status = 'lost'`)
}

func TestMigration00005Idempotency(t *testing.T) {
	t.Parallel()
	pool := testdb.Pool(t)
	seedMerchant(t, pool)
	body := []byte(`{"id":"ses_1"}`)
	exec(t, pool, `INSERT INTO idempotency_keys (merchant_id, livemode, key, request_hash, response_status, response_body)
		VALUES ('mer_1', true, 'k1', $1, 200, $2)`, make([]byte, 32), body)
	var got []byte
	if err := pool.QueryRow(t.Context(), `SELECT response_body FROM idempotency_keys WHERE key = 'k1'`).Scan(&got); err != nil || string(got) != string(body) {
		t.Fatalf("body roundtrip %q %v", got, err)
	}
	mustFail(t, pool, "duplicate key", `INSERT INTO idempotency_keys (merchant_id, livemode, key, request_hash, response_status, response_body)
		VALUES ('mer_1', true, 'k1', $1, 200, '\x00')`, make([]byte, 32))
	exec(t, pool, `INSERT INTO idempotency_keys (merchant_id, livemode, key, request_hash, response_status, response_body)
		VALUES ('mer_1', false, 'k1', $1, 200, '\x00')`, make([]byte, 32))
	mustFail(t, pool, "empty key", `INSERT INTO idempotency_keys (merchant_id, livemode, key, request_hash, response_status, response_body)
		VALUES ('mer_1', true, '', $1, 200, '\x00')`, make([]byte, 32))
}

func TestMigration00008FlushBlocked(t *testing.T) {
	t.Parallel()
	pool := testdb.Pool(t)
	seedSession(t, pool)
	job := `INSERT INTO chain_tx_jobs (id, chain, kind, ref, session_id, asset, address, planned_total, to_address, calldata, sender,
		status, next_attempt_at, created_at, updated_at) VALUES ($1, 'solana', 'flush', $1, 'ses_1', 'USDC', 'ata', 25000000, 'prog', '\x00', 'gas',
		$2, now(), now(), now())`
	exec(t, pool, job, "txj_1", "blocked")
	mustFail(t, pool, "unknown status", job, "txj_2", "stuck")
	exec(t, pool, `UPDATE chain_tx_jobs SET status = 'pending' WHERE id = 'txj_1'`)
}

func TestMigration00009RateLimits(t *testing.T) {
	t.Parallel()
	pool := testdb.Pool(t)
	exec(t, pool, `INSERT INTO rate_limits (key, tokens, allowed, updated_at) VALUES ('link_open|203.0.113.7', 0, false, now())`)
	mustFail(t, pool, "duplicate key", `INSERT INTO rate_limits (key, tokens, allowed, updated_at) VALUES ('link_open|203.0.113.7', 0, true, now())`)
	mustFail(t, pool, "negative tokens", `INSERT INTO rate_limits (key, tokens, allowed, updated_at) VALUES ('k', -1, true, now())`)
	mustFail(t, pool, "empty key", `INSERT INTO rate_limits (key, tokens, allowed, updated_at) VALUES ('', 0, true, now())`)
	if got := scanText(t, pool, `SELECT relpersistence::text FROM pg_class WHERE relname = 'rate_limits'`); got != "u" {
		t.Fatalf("rate_limits persistence %q, want unlogged", got)
	}
}

func TestMigration00011Dashboard(t *testing.T) {
	t.Parallel()
	pool := testdb.Pool(t)
	seedMerchant(t, pool)
	hash := "$argon2id$v=19$m=65536,t=2,p=1$c2FsdA$a2V5"
	user := `INSERT INTO dashboard_users (id, merchant_id, email, password_hash, created_at, updated_at)
		VALUES ($1, 'mer_1', $2, $3, now(), now())`
	exec(t, pool, user, "usr_1", "owner@example.com", hash)
	if got := scanText(t, pool, `SELECT password_hash FROM dashboard_users WHERE id = 'usr_1'`); got != hash {
		t.Fatalf("hash roundtrip %s", got)
	}
	mustFail(t, pool, "duplicate email", user, "usr_2", "owner@example.com", hash)
	mustFail(t, pool, "mixed-case email", user, "usr_3", "Owner@example.com", hash)
	mustFail(t, pool, "email without @", user, "usr_4", "owner", hash)
	mustFail(t, pool, "not an argon2id hash", user, "usr_5", "x@example.com", "$2b$10$bcrypt")
	sealed := []byte{1, 2, 3}
	exec(t, pool, `UPDATE dashboard_users SET totp_secret_ciphertext = $1, totp_last_step = 59000000 WHERE id = 'usr_1'`, sealed)
	var got []byte
	if err := pool.QueryRow(t.Context(), `SELECT totp_secret_ciphertext FROM dashboard_users WHERE id = 'usr_1'`).Scan(&got); err != nil || string(got) != string(sealed) {
		t.Fatalf("sealed secret roundtrip %v %v", got, err)
	}

	session := `INSERT INTO dashboard_sessions (token_hash, user_id, csrf_token, created_at, last_seen_at, expires_at)
		VALUES ($1, 'usr_1', $2, now(), now(), now() + interval '7 days')`
	exec(t, pool, session, make([]byte, 32), strings.Repeat("c", 43))
	mustFail(t, pool, "duplicate token", session, make([]byte, 32), strings.Repeat("c", 43))
	mustFail(t, pool, "short token hash", session, make([]byte, 31), strings.Repeat("c", 43))
	mustFail(t, pool, "short CSRF token", session, append(make([]byte, 31), 1), "short")
	mustFail(t, pool, "expiry before creation", `INSERT INTO dashboard_sessions (token_hash, user_id, csrf_token, created_at, last_seen_at, expires_at)
		VALUES ($1, 'usr_1', $2, now(), now(), now())`, append(make([]byte, 31), 2), strings.Repeat("c", 43))
	exec(t, pool, `DELETE FROM dashboard_users WHERE id = 'usr_1'`)
	if got := scanText(t, pool, `SELECT count(*)::text FROM dashboard_sessions`); got != "0" {
		t.Fatalf("sessions outlive their user: %s", got)
	}

	exec(t, pool, `INSERT INTO api_keys (id, merchant_id, livemode, key_hash, key_prefix, scopes, name)
		VALUES ('key_1', 'mer_1', false, $1, 'sk_test_abcd', '{sessions:read}', 'Backend')`, make([]byte, 32))
	exec(t, pool, `INSERT INTO api_keys (id, merchant_id, livemode, key_hash, key_prefix, scopes)
		VALUES ('key_2', 'mer_1', false, $1, 'sk_test_efgh', '{sessions:read}')`, append(make([]byte, 31), 1))
	mustFail(t, pool, "empty key name", `UPDATE api_keys SET name = '' WHERE id = 'key_1'`)
}

// The search's prefix and amount conditions must be served by their indexes even in a generic
// plan, where the bound values are not known to the planner.
func TestMigration00013SessionSearch(t *testing.T) {
	t.Parallel()
	pool := testdb.Pool(t)
	ctx := t.Context()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	// Enough rows, spread over two merchants, that each condition is selective.
	for _, sql := range []string{
		`INSERT INTO merchants (id, name) VALUES ('mer_1', 'A'), ('mer_2', 'B')`,
		`INSERT INTO sessions (id, merchant_id, livemode, amount_cents, status, version, salt, expires_at)
			SELECT 'ses_01JB' || lpad(g::text, 22, '0'), CASE WHEN g % 2 = 0 THEN 'mer_1' ELSE 'mer_2' END, g % 3 = 0, g, 'open', 1,
				sha256(g::text::bytea), now() + interval '1 hour' FROM generate_series(1, 20000) g`,
		`ANALYZE sessions`,
		`SET LOCAL enable_seqscan = off`, `SET LOCAL enable_bitmapscan = off`, `SET LOCAL plan_cache_mode = force_generic_plan`,
	} {
		if _, err := tx.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct{ index, cond, prepare, execute string }{
		{"sessions_merchant_id_pattern", "(id ~>=~ $3)",
			`SELECT id FROM sessions WHERE merchant_id = $1 AND livemode = $2 AND id ~>=~ $3 AND id ~<~ $4`, `('mer_1', true, 'ses_01JB', 'ses_01JC')`},
		{"sessions_merchant_amount", "(amount_cents",
			`SELECT id FROM sessions WHERE merchant_id = $1 AND livemode = $2 AND amount_cents = $3::numeric * 100`, `('mer_1', true, '25.00')`},
		{"payments_tx_hash_pattern", "(tx_hash ~>=~ $1)",
			`SELECT session_id FROM payments WHERE tx_hash ~>=~ $1 AND tx_hash ~<~ $2`, `('5KtPn1LG', '5KtPn1LH')`},
		{"payments_tx_hash_lower_pattern", "(lower(tx_hash) ~>=~ $1)",
			`SELECT session_id FROM payments WHERE lower(tx_hash) ~>=~ $1 AND lower(tx_hash) ~<~ $2`, `('0xdeadbeef', '0xdeadbeeg')`},
	} {
		t.Run(c.index, func(t *testing.T) {
			name := "probe_" + c.index
			if _, err := tx.Exec(ctx, `PREPARE `+name+` AS `+c.prepare); err != nil {
				t.Fatal(err)
			}
			rows, err := tx.Query(ctx, `EXPLAIN EXECUTE `+name+c.execute)
			if err != nil {
				t.Fatal(err)
			}
			lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(ctx, `DEALLOCATE `+name); err != nil {
				t.Fatal(err)
			}
			cond := ""
			for _, l := range lines {
				if strings.Contains(l, "Index Cond:") {
					cond = l
				}
			}
			if plan := strings.Join(lines, "\n"); !strings.Contains(plan, c.index) || !strings.Contains(cond, c.cond) {
				t.Errorf("%s not used for %s:\n%s", c.index, c.cond, plan)
			}
		})
	}
}

func TestMigration00015SandboxInbox(t *testing.T) {
	t.Parallel()
	pool := testdb.Pool(t)
	seedMerchant(t, pool)
	exec(t, pool, `INSERT INTO merchants (id, name) VALUES ('mer_2', 'Other')`)
	for _, e := range []string{"we_1", "we_2", "we_3"} {
		exec(t, pool, `INSERT INTO webhook_endpoints (id, merchant_id, livemode, url, enabled_events, secret_ciphertext)
			VALUES ($1, 'mer_1', false, 'https://x/'||$1, $2, $3)`, e, []string{}, []byte{1})
	}
	tok := strings.Repeat("A", 44)
	exec(t, pool, `INSERT INTO sandbox_inboxes (id, merchant_id, token, endpoint_id, created_at) VALUES ('sbx_1', 'mer_1', $1, 'we_1', now())`, tok)
	mustFail(t, pool, "a second active inbox", `INSERT INTO sandbox_inboxes (id, merchant_id, token, endpoint_id, created_at)
		VALUES ('sbx_2', 'mer_1', $1, 'we_2', now())`, strings.Repeat("B", 44))
	mustFail(t, pool, "a short token", `INSERT INTO sandbox_inboxes (id, merchant_id, token, endpoint_id, created_at, disabled_at)
		VALUES ('sbx_2', 'mer_1', 'short', 'we_2', now(), now())`)
	mustFail(t, pool, "a reused token", `INSERT INTO sandbox_inboxes (id, merchant_id, token, endpoint_id, created_at, disabled_at)
		VALUES ('sbx_2', 'mer_1', $1, 'we_2', now(), now())`, tok)
	mustFail(t, pool, "two inboxes on one endpoint", `INSERT INTO sandbox_inboxes (id, merchant_id, token, endpoint_id, created_at, disabled_at)
		VALUES ('sbx_2', 'mer_1', $1, 'we_1', now(), now())`, strings.Repeat("C", 44))
	exec(t, pool, `UPDATE sandbox_inboxes SET disabled_at = now() WHERE id = 'sbx_1'`)
	exec(t, pool, `INSERT INTO sandbox_inboxes (id, merchant_id, token, endpoint_id, created_at) VALUES ('sbx_2', 'mer_1', $1, 'we_2', now())`,
		strings.Repeat("B", 44))
	exec(t, pool, `INSERT INTO sandbox_requests (id, inbox_id, received_at, method, headers, body, signature_valid)
		VALUES ('sbr_1', 'sbx_2', now(), 'POST', '[["Signature","t=1,v1=a"]]', '\x7b7d', true)`)
	mustFail(t, pool, "valid with an error", `INSERT INTO sandbox_requests (id, inbox_id, received_at, method, headers, body, signature_valid, signature_error)
		VALUES ('sbr_2', 'sbx_2', now(), 'POST', '[]', '', true, 'no_match')`)
	mustFail(t, pool, "invalid without a reason", `INSERT INTO sandbox_requests (id, inbox_id, received_at, method, headers, body, signature_valid)
		VALUES ('sbr_2', 'sbx_2', now(), 'POST', '[]', '', false)`)
	mustFail(t, pool, "unknown reason", `INSERT INTO sandbox_requests (id, inbox_id, received_at, method, headers, body, signature_valid, signature_error)
		VALUES ('sbr_2', 'sbx_2', now(), 'POST', '[]', '', false, 'nope')`)
	mustFail(t, pool, "headers not an array", `INSERT INTO sandbox_requests (id, inbox_id, received_at, method, headers, body, signature_valid)
		VALUES ('sbr_2', 'sbx_2', now(), 'POST', '{}', '', true)`)
	exec(t, pool, `DELETE FROM sandbox_inboxes WHERE id = 'sbx_2'`)
	if got := scanText(t, pool, `SELECT count(*)::text FROM sandbox_requests`); got != "0" {
		t.Fatalf("requests outlived their inbox: %s", got)
	}
}

// tx runs statements in one transaction, so commit-time checks see them together.
func tx(t *testing.T, pool *pgxpool.Pool, stmts ...string) {
	t.Helper()
	if err := txErr(pool, stmts...); err != nil {
		t.Fatalf("%v: %v", stmts, err)
	}
}

func txErr(pool *pgxpool.Pool, stmts ...string) error {
	return pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		for _, s := range stmts {
			if _, err := tx.Exec(context.Background(), s); err != nil {
				return err
			}
		}
		return nil
	})
}
