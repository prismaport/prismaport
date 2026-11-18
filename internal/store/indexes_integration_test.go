//go:build integration

package store_test

import (
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/store"
)

// oracleOwnTxHashesSQL is OwnTxHashes' query before ADR-0008 §F-2 (every job of the chain
// unnested), kept as the oracle for the && rewrite.
const oracleOwnTxHashesSQL = `SELECT DISTINCT h FROM chain_tx_jobs j, unnest(j.tx_hashes) h WHERE j.chain = $1 AND h = ANY($2)`

const indexChains = 4

// seedTxJobs writes n jobs of sessionID spread over indexChains chains (index-0 to index-3): half
// flush jobs on a checksummed-looking address, half settle jobs; 20 senders with growing nonces;
// most with one or two transaction hashes (some shared by two jobs), every seventh with none.
// It returns the hashes written per chain.
func seedTxJobs(t *testing.T, pool *pgxpool.Pool, sessionID string, n int) map[string][]string {
	t.Helper()
	ctx := t.Context()
	_, err := pool.Exec(ctx, `INSERT INTO chain_tx_jobs (id, chain, kind, ref, session_id, asset, address, planned_total, to_address,
			calldata, sender, status, next_attempt_at, created_at, updated_at, nonce, tx_hashes)
		SELECT 'txj_idx_' || g, 'index-' || (g % $3), CASE WHEN g % 2 = 0 THEN 'flush' ELSE 'settle' END, 'idx|' || g, $1,
			CASE WHEN g % 3 = 0 THEN 'USDT' ELSE 'USDC' END,
			CASE WHEN g % 2 = 0 THEN '0xAbCd' || lpad(to_hex(g), 36, '0') END, CASE WHEN g % 2 = 0 THEN 1000 END,
			'0xfactory', '\x01'::bytea, 'sender-' || (g % 20), 'done', now(), now(), now(), g,
			CASE WHEN g % 7 = 0 THEN '{}'::text[]
				WHEN g % 5 = 0 THEN ARRAY['0x' || md5(g::text), '0x' || md5((g - 5)::text)]
				ELSE ARRAY['0x' || md5(g::text)] END
		FROM generate_series(1, $2::int) g`, sessionID, n, indexChains)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := pool.Query(ctx, `SELECT DISTINCT j.chain, h FROM chain_tx_jobs j, unnest(j.tx_hashes) h ORDER BY 1, 2`)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for rows.Next() {
		var c, h string
		if err := rows.Scan(&c, &h); err != nil {
			t.Fatal(err)
		}
		out[c] = append(out[c], h)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func oracleOwnTxHashes(t *testing.T, pool *pgxpool.Pool, chainID string, hashes []string) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), oracleOwnTxHashesSQL, chainID, hashes)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(out)
	return out
}

// OwnTxHashes reads only the jobs whose tx_hashes overlap the asked hashes (tx_hashes && $2, the
// GIN index of migration 00103) and returns exactly what the query that unnested every job of the
// chain returned.
func TestOwnTxHashesMatchesTheUnnestQuery(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	ctx := t.Context()
	pool := app.Store.Pool()
	byChain := seedTxJobs(t, pool, newSession(t, app, "5.00", "own-tx"), 700)
	var all []string
	for _, hs := range byChain {
		all = append(all, hs...)
	}
	unknown := []string{"0xnone", "0x" + strings.Repeat("0", 32), ""}
	cases := [][]string{
		byChain["index-1"],
		append(slices.Clone(byChain["index-2"][:5]), byChain["index-3"][:5]...),
		append(slices.Clone(byChain["index-1"][:3]), byChain["index-1"][:3]...),
		unknown,
		{byChain["index-0"][0]},
	}
	r := rand.New(rand.NewPCG(1, 2))
	for range 40 {
		var c []string
		for range 1 + r.IntN(30) {
			if r.IntN(5) == 0 {
				c = append(c, unknown[r.IntN(len(unknown))])
			} else {
				c = append(c, all[r.IntN(len(all))])
			}
		}
		cases = append(cases, c)
	}
	matched := 0
	for i, hashes := range cases {
		for c := range indexChains + 1 {
			chainID := fmt.Sprintf("index-%d", c)
			want := oracleOwnTxHashes(t, pool, chainID, hashes)
			got, err := app.Store.OwnTxHashes(ctx, chainID, hashes)
			if err != nil {
				t.Fatal(err)
			}
			var gotList []string
			for h, ok := range got {
				if !ok {
					t.Fatalf("case %d %s: %s mapped to false", i, chainID, h)
				}
				gotList = append(gotList, h)
			}
			slices.Sort(gotList)
			if !slices.Equal(gotList, want) {
				t.Fatalf("case %d on %s: OwnTxHashes %v, the unnest query %v", i, chainID, gotList, want)
			}
			matched += len(want)
		}
	}
	if matched == 0 {
		t.Fatal("no case matched a hash: the comparison proves nothing")
	}
	if got, err := app.Store.OwnTxHashes(ctx, "index-1", nil); err != nil || len(got) != 0 {
		t.Fatalf("no hashes: %v %v", got, err)
	}
}

// explain plans sql with args, with sequential scans off so that a usable index is never passed
// over because the table is small. It never runs the statement.
func explain(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) string {
	t.Helper()
	ctx := t.Context()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.Query(ctx, "EXPLAIN "+sql, args...)
	if err != nil {
		t.Fatalf("explain %s: %v", sql, err)
	}
	lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(lines, "\n")
}

// The audit's indexes (ADR-0008 §F-2, F-3, F-5, F-8; migrations 00101-00106 and 00108) are valid
// and the planner uses each for the store query it was added for, run as the store runs it.
func TestAuditIndexesServeTheirQueries(t *testing.T) {
	t.Parallel()
	f := newFlushFixture(t)
	app, ctx, pool := f.app, f.ctx, f.app.Store.Pool()
	// 2,000 settled sessions of the merchant with a payment each (every fifth with a reversed
	// one too), and 4,000 transaction jobs on other chains.
	sid := f.session("paid", app.Clock.Now())
	f.history(sid, f.assets(sid)[0], 2000, 1000)
	seedTxJobs(t, pool, sid, 4000)
	// 8 endpoints, 400 events, each delivered to two endpoints.
	f.exec(`INSERT INTO webhook_endpoints (id, merchant_id, livemode, url, secret_ciphertext)
		SELECT 'we_idx_' || g, $1, false, 'https://merchant.example/' || g, '\x01'::bytea FROM generate_series(0, 7) g`, app.MerchantID)
	f.exec(`INSERT INTO events (id, merchant_id, livemode, type, payload, created_at)
		SELECT 'evt_idx_' || g, $1, false, 'session.paid', '{}', now() FROM generate_series(1, 400) g`, app.MerchantID)
	f.exec(`INSERT INTO webhook_deliveries (id, endpoint_id, event_id, status, next_attempt_at, merchant_id, livemode)
		SELECT 'whd_idx_' || lpad(g::text, 4, '0') || '_' || e, 'we_idx_' || ((g + e) % 8), 'evt_idx_' || g, 'succeeded', now(), $1, false
		FROM generate_series(1, 400) g, generate_series(0, 1) e`, app.MerchantID)
	f.exec(`ANALYZE`)

	indexes := []string{"chain_tx_jobs_sender_nonce", "chain_tx_jobs_flush_address", "chain_tx_jobs_tx_hashes", "webhook_deliveries_event",
		"webhook_deliveries_endpoint", "payments_detected", "payments_id_session_id_key"}
	rows, err := pool.Query(ctx, `SELECT c.relname FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
		WHERE c.relname = ANY($1) AND i.indisvalid AND i.indisready ORDER BY 1`, indexes)
	if err != nil {
		t.Fatal(err)
	}
	valid, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if want := slices.Sorted(slices.Values(indexes)); !slices.Equal(valid, want) {
		t.Fatalf("valid indexes %v, want %v", valid, want)
	}

	// A flush job's address as the detector asks for it (lowercase) and as the job stores it.
	stored := "0xAbCd" + fmt.Sprintf("%036x", 2)
	checks := []struct {
		name, sql string
		args      []any
		index     []string // the plan uses one of these
		noSort    bool
	}{
		{"MaxSenderNonce", store.MaxSenderNonceSQL, []any{"index-1", "sender-3"}, []string{"chain_tx_jobs_sender_nonce"}, false},
		{"OwnFlushJob", store.OwnFlushJobSQL, []any{"index-2", strings.ToLower(stored)}, []string{"chain_tx_jobs_flush_address"}, false},
		{"OtherSessionFlushes", store.OtherSessionFlushesSQL, []any{"index-2", stored, "USDC", "ses_other"},
			[]string{"chain_tx_jobs_flush_address"}, false},
		// Open jobs are few, so chain_tx_jobs_open (status in flight) serves it as well.
		{"FlushSiblingInFlight", store.FlushSiblingSQL, []any{"index-2", stored, "USDC", sid, "txj_other"},
			[]string{"chain_tx_jobs_flush_address", "chain_tx_jobs_open"}, false},
		{"OwnTxHashes", store.OwnTxHashesSQL, []any{"index-1", []string{"0x" + md5Hex("1"), "0xnone"}}, []string{"chain_tx_jobs_tx_hashes"}, false},
		{"DeliveriesForEvent", store.DeliveriesForEventSQL, []any{"evt_idx_7", app.MerchantID}, []string{"webhook_deliveries_event"}, false},
		{"ListDeliveries first page", store.ListDeliveriesSQL, []any{"we_idx_3", "", 21}, []string{"webhook_deliveries_endpoint"}, true},
		{"ListDeliveries next page", store.ListDeliveriesSQL, []any{"we_idx_3", "whd_idx_0200_0", 21}, []string{"webhook_deliveries_endpoint"}, true},
		{"RecentPayments", store.RecentPaymentsSQL, []any{app.MerchantID, false, 20}, []string{"payments_detected"}, true},
	}
	for _, c := range checks {
		t.Run(c.name, func(t *testing.T) {
			plan := explain(t, pool, c.sql, c.args...)
			t.Logf("%s:\n%s", c.name, plan)
			if !slices.ContainsFunc(c.index, func(ix string) bool {
				return strings.Contains(plan, " using "+ix+" ") || strings.Contains(plan, " on "+ix+" ")
			}) {
				t.Errorf("%s does not use %v:\n%s", c.name, c.index, plan)
			}
			if c.noSort && strings.Contains(plan, "Sort") {
				t.Errorf("%s sorts instead of reading the index in order:\n%s", c.name, plan)
			}
		})
	}
}

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// A chain payment must carry its payment's session (ADR-0008 §F-8, migrations 00108-00110): the
// foreign key refuses one that does not, and every chain payment the suite writes passes it.
func TestChainPaymentMustCarryItsPaymentsSession(t *testing.T) {
	t.Parallel()
	f := newFlushFixture(t)
	now := f.app.Clock.Now()
	a := f.session("paid", now.Add(-30*time.Minute))
	b := f.session("paid", now.Add(-30*time.Minute))
	f.pay(a, f.assets(a)[0], 1000, "confirmed", store.KindTransfer)
	var pid string
	if err := f.app.Store.Pool().QueryRow(f.ctx, `SELECT id FROM payments WHERE session_id = $1`, a).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	// The upsert keeps a row's session, so write it again from scratch on the other session.
	f.exec(`DELETE FROM chain_payments WHERE payment_id = $1`, pid)
	err := f.app.Store.UpsertChainPayment(f.ctx, store.ChainPayment{PaymentID: pid, SessionID: b, Chain: flushChain,
		Asset: f.assets(a)[0], Kind: store.KindTransfer, BlockNumber: 1, BlockHash: "0x" + strings.Repeat("ab", 32)})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.ConstraintName != "chain_payments_payment_session" {
		t.Fatalf("a chain payment on another session than its payment's: %v", err)
	}
	var validated bool
	if err := f.app.Store.Pool().QueryRow(f.ctx, `SELECT convalidated FROM pg_constraint WHERE conname = 'chain_payments_payment_session'`).
		Scan(&validated); err != nil || !validated {
		t.Fatalf("chain_payments_payment_session validated %v %v", validated, err)
	}
}
