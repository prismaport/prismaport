//go:build integration

package db_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/prismaport/prismaport/migrations"
)

// mismatchedChainPaymentsSQL lists the chain payments whose session is not their payment's: the
// rows 00110 refuses to validate.
const mismatchedChainPaymentsSQL = `SELECT c.payment_id FROM chain_payments c JOIN payments p ON p.id = c.payment_id
	WHERE c.session_id <> p.session_id ORDER BY 1`

// ADR-0008 §F-8: 00108 builds the unique index on payments (id, session_id), 00109 attaches it as
// a constraint and adds the composite foreign key NOT VALID (new rows only), 00110 validates the
// existing rows, which fails while one disagrees with its payment's session.
func TestMigration00110ChainPaymentSession(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool, m := chainOn(t, migrations.FS)
	if err := m.UpTo(ctx, 108); err != nil {
		t.Fatal(err)
	}
	seedSession(t, pool)
	// ses_2: a copy of ses_1.
	exec(t, pool, `INSERT INTO sessions SELECT (jsonb_populate_record(NULL::sessions, to_jsonb(s) ||
			jsonb_build_object('id', 'ses_2', 'salt', '\x01' || repeat('00', 31)))).*
		FROM sessions s WHERE s.id = 'ses_1'`)
	for _, table := range []string{"session_payouts", "session_pay_to"} {
		exec(t, pool, `INSERT INTO `+table+` SELECT (jsonb_populate_record(NULL::`+table+`, to_jsonb(p) || '{"session_id": "ses_2"}')).*
			FROM `+table+` p WHERE p.session_id = 'ses_1'`)
	}
	payment := `INSERT INTO payments (id, session_id, chain, asset, address, tx_hash, log_index, amount, decimals, status, late, detected_at)
		VALUES ($1, $2, 'base', 'USDC', '0xfwd', $3, 0, 25000000, 6, 'confirmed', false, now())`
	exec(t, pool, payment, "pay_1", "ses_1", "0xtx1")
	exec(t, pool, payment, "pay_2", "ses_2", "0xtx2")
	exec(t, pool, payment, "pay_3", "ses_2", "0xtx3")
	chainPayment := `INSERT INTO chain_payments (payment_id, session_id, chain, asset, kind, block_number, block_hash)
		VALUES ($1, $2, 'base', 'USDC', 'transfer', 7, '0x` + strings.Repeat("ab", 32) + `')`
	exec(t, pool, chainPayment, "pay_1", "ses_1")
	// pay_2 is ses_2's, recorded on ses_1: what the foreign key rules out.
	exec(t, pool, chainPayment, "pay_2", "ses_1")
	mismatched := func() []string {
		t.Helper()
		rows, err := pool.Query(ctx, mismatchedChainPaymentsSQL)
		if err != nil {
			t.Fatal(err)
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		return ids
	}
	if got := mismatched(); !slices.Equal(got, []string{"pay_2"}) {
		t.Fatalf("mismatched chain payments %v", got)
	}

	// NOT VALID: the existing row stays, a new one is checked.
	if err := m.UpTo(ctx, 109); err != nil {
		t.Fatal(err)
	}
	mustFail(t, pool, "a new chain payment on another session", chainPayment, "pay_3", "ses_1")
	exec(t, pool, chainPayment, "pay_3", "ses_2")
	if err := m.UpTo(ctx, 110); err == nil || !strings.Contains(err.Error(), "chain_payments_payment_session") {
		t.Fatalf("validated over a mismatched row: %v", err)
	}
	if v, err := m.Version(ctx); err != nil || v != 109 {
		t.Fatalf("version after the refused validation %d %v", v, err)
	}

	exec(t, pool, `UPDATE chain_payments SET session_id = 'ses_2' WHERE payment_id = 'pay_2'`)
	if got := mismatched(); len(got) != 0 {
		t.Fatalf("mismatched chain payments %v", got)
	}
	if err := m.UpTo(ctx, 110); err != nil {
		t.Fatal(err)
	}
	if got := scanText(t, pool, `SELECT convalidated::text FROM pg_constraint WHERE conname = 'chain_payments_payment_session'`); got != "true" {
		t.Fatalf("validated %s", got)
	}
	mustFail(t, pool, "moving a chain payment to another session", `UPDATE chain_payments SET session_id = 'ses_1' WHERE payment_id = 'pay_3'`)
	mustFail(t, pool, "moving a payment away from its chain payment", `UPDATE payments SET session_id = 'ses_1' WHERE id = 'pay_3'`)
}
