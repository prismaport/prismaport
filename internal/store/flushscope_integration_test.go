//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/store"
)

// oracleChainStatesSQL is ChainStates' query before ADR-0008 §F-1, kept as the oracle: its totals
// read every payment the chains ever had.
func oracleChainStatesSQL() string {
	due := `coalesce(CASE WHEN p.family = 'solana' THEN sw.next_check_at ELSE w.next_check_at END,
		s.expires_at + $3::bigint * interval '1 microsecond')`
	return `WITH cand AS (
			SELECT p.chain, s.status, s.expires_at, s.checkout_seen_at, s.recheck_requested_at,
				CASE WHEN p.family = 'solana' THEN sw.rechecked_for ELSE w.rechecked_for END AS rechecked_for,
				` + due + ` AS due, ` + store.SettledExpr + ` AS settled,
				(p.family = 'solana' AND $6::bigint > 0 AND ` + store.WalletScanCond("$2", "$6") + `) AS wallet
			` + store.WatchFrom + `
			LEFT JOIN solana_watch sw ON p.family = 'solana' AND sw.chain = p.chain AND sw.session_id = p.session_id
				AND sw.kind = 'deposit' AND sw.asset = p.asset
			WHERE p.chain = ANY($1) AND (p.family <> 'solana' OR p.program IS NOT NULL)` + store.RevealedCond + store.OwnerCond + `
				AND (s.expires_at > $2::timestamptz - ($4::bigint * interval '1 microsecond') - interval '` + store.LateHorizon + `'
					OR s.recheck_requested_at IS NOT NULL)),
		scan AS (SELECT chain,
				bool_or(status IN ('open', 'partially_paid') AND expires_at > $2
					AND checkout_seen_at > $2::timestamptz - $5::bigint * interval '1 microsecond') AS payer,
				bool_or((status IN ('open', 'partially_paid') AND expires_at > $2) OR (wallet AND NOT settled)) AS open,
				bool_or(recheck_requested_at IS NOT NULL AND recheck_requested_at IS DISTINCT FROM rechecked_for) AS recheck,
				min(due) FILTER (WHERE $4::bigint > 0 AND NOT settled AND expires_at <= $2
					AND expires_at > $2::timestamptz - ($4::bigint * interval '1 microsecond') - interval '` + store.LateHorizon + `'
					AND due <= expires_at + $4::bigint * interval '1 microsecond') AS late_due
			FROM cand GROUP BY chain),
		pays AS (SELECT y.chain, bool_or(y.status = 'detected') AS detected, bool_or(y.status = 'confirmed') AS confirmed
			FROM payments y JOIN chain_payments c ON c.payment_id = y.id
			WHERE y.chain = ANY($1) AND y.status IN ('detected', 'confirmed') GROUP BY y.chain),
		jobs AS (SELECT chain, bool_or(status = 'submitted') AS submitted, bool_or(status = 'mined') AS mined,
				bool_or(status = 'submitted' OR (status IN ('pending', 'blocked') AND next_attempt_at <= $2)
					OR (status = 'mined' AND (finality_check_at IS NULL OR finality_check_at <= $2))) AS due
			FROM chain_tx_jobs WHERE chain = ANY($1) AND status IN ('pending', 'blocked', 'submitted', 'mined') GROUP BY chain),
		totals AS (SELECT p.chain, p.session_id, p.asset,
				coalesce(sum(p.amount) FILTER (WHERE p.status IN ('confirmed', 'finalized')), 0) AS counted,
				count(*) FILTER (WHERE p.status = 'detected') AS pending
			FROM payments p JOIN chain_payments c ON c.payment_id = p.id AND c.kind = 'transfer'
			WHERE p.chain = ANY($1) GROUP BY p.chain, p.session_id, p.asset),
		flush AS (SELECT DISTINCT t.chain FROM totals t JOIN sessions s ON s.id = t.session_id
			WHERE s.status IN ('paid', 'overpaid', 'expired', 'late_payment') AND t.pending = 0 AND t.counted > 0
				AND t.counted > coalesce((SELECT max(j.planned_total) FROM chain_tx_jobs j WHERE j.chain = t.chain
					AND j.kind = 'flush' AND j.session_id = t.session_id AND j.asset = t.asset), 0))
		SELECT c.chain, coalesce(a.payer, false), coalesce(a.open, false), coalesce(a.recheck, false), a.late_due,
			coalesce(y.detected, false), coalesce(y.confirmed, false), coalesce(j.submitted, false), coalesce(j.mined, false),
			(SELECT max(r.revealed_at) FROM session_reveals r WHERE r.chain = c.chain),
			coalesce(j.due, false) OR f.chain IS NOT NULL
		FROM unnest($1::text[]) AS c(chain) LEFT JOIN scan a ON a.chain = c.chain LEFT JOIN pays y ON y.chain = c.chain
			LEFT JOIN jobs j ON j.chain = c.chain LEFT JOIN flush f ON f.chain = c.chain`
}

// oracleFlushCandidatesSQL is FlushCandidates' query before ADR-0008 §F-1 (the pair, its total and
// first block; the watch columns are read the same way by both).
const oracleFlushCandidatesSQL = `WITH totals AS (
			SELECT p.session_id, p.asset,
				coalesce(sum(p.amount) FILTER (WHERE p.status IN ('confirmed', 'finalized')), 0) AS counted,
				count(*) FILTER (WHERE p.status = 'detected') AS pending,
				coalesce(min(c.block_number) FILTER (WHERE p.status IN ('confirmed', 'finalized')), 0) AS first_block
			FROM payments p JOIN chain_payments c ON c.payment_id = p.id AND c.kind = 'transfer'
			WHERE p.chain = $1 GROUP BY p.session_id, p.asset)
		SELECT s.id, p.asset, t.counted::text, t.first_block ` + store.WatchFrom + `
		JOIN totals t ON t.session_id = p.session_id AND t.asset = p.asset
		WHERE p.chain = $1 AND s.status IN ('paid', 'overpaid', 'expired', 'late_payment') AND t.pending = 0 AND t.counted > 0
			AND t.counted > coalesce((SELECT max(j.planned_total) FROM chain_tx_jobs j WHERE j.chain = $1 AND j.kind = 'flush'
				AND j.session_id = p.session_id AND j.asset = p.asset), 0)
		ORDER BY s.id LIMIT $2`

const flushChain = "base-sepolia"

type flushFixture struct {
	t   *testing.T
	app *apptest.App
	ctx context.Context
	n   int
	// payTo is each session's deposit address and decimals per asset on flushChain.
	payTo map[string]map[string]flushPayTo
}

type flushPayTo struct {
	address  string
	decimals int16
}

func newFlushFixture(t *testing.T) *flushFixture {
	t.Helper()
	return &flushFixture{t: t, app: apptest.New(t, apptest.Options{}), ctx: t.Context(), payTo: map[string]map[string]flushPayTo{}}
}

func (f *flushFixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.app.Store.Pool().Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
}

// session creates a session revealed on flushChain and moves it to status, expiring at expires.
func (f *flushFixture) session(status string, expires time.Time) string {
	f.t.Helper()
	f.n++
	id := newSession(f.t, f.app, "10.00", fmt.Sprintf("flush-%d", f.n))
	f.app.Reveal(f.app.TestKey, id, flushChain)
	f.place(id, status, expires)
	return id
}

// onAddress opens a session on customer address ca and moves it to status, expiring at expires.
func (f *flushFixture) onAddress(ca, status string, expires time.Time) string {
	f.t.Helper()
	f.n++
	r := f.app.Do("POST", "/v1/sessions", f.app.TestKey, map[string]any{"customer_address_id": ca}, "Idempotency-Key", fmt.Sprintf("flush-%d", f.n))
	var v struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(r.Body, &v); err != nil || r.Status != 200 {
		f.t.Fatalf("session on %s: %d %s", ca, r.Status, r.Body)
	}
	f.place(v.ID, status, expires)
	return v.ID
}

func (f *flushFixture) place(id, status string, expires time.Time) {
	f.t.Helper()
	f.exec(`UPDATE sessions SET status = $2, created_at = $3::timestamptz - interval '1 hour', expires_at = $3 WHERE id = $1`, id, status, expires)
	rows, err := f.app.Store.Pool().Query(f.ctx, `SELECT asset, address, decimals FROM session_pay_to WHERE session_id = $1 AND chain = $2`, id, flushChain)
	if err != nil {
		f.t.Fatal(err)
	}
	m := map[string]flushPayTo{}
	for rows.Next() {
		var a string
		var p flushPayTo
		if err := rows.Scan(&a, &p.address, &p.decimals); err != nil {
			f.t.Fatal(err)
		}
		m[a] = p
	}
	if rows.Err() != nil || len(m) == 0 {
		f.t.Fatalf("pay_to of %s on %s: %v %v", id, flushChain, m, rows.Err())
	}
	f.payTo[id] = m
}

// assets are the session's assets on flushChain, sorted.
func (f *flushFixture) assets(id string) []string {
	var out []string
	for a := range f.payTo[id] {
		out = append(out, a)
	}
	slices.Sort(out)
	return out
}

// pay records a payment of amount base units on the session's asset with its chain row (kind
// transfer or gateway).
func (f *flushFixture) pay(id, asset string, amount uint64, status, kind string) {
	f.t.Helper()
	f.n++
	p, ok := f.payTo[id][asset]
	if !ok {
		f.t.Fatalf("no %s pay_to for %s", asset, id)
	}
	pid := fmt.Sprintf("pay_f1_%05d", f.n)
	f.exec(`INSERT INTO payments (id, session_id, chain, asset, address, tx_hash, log_index, amount, decimals, status, late, detected_at,
			confirmed_at, finalized_at, reversed_at)
		VALUES ($1, $2, $3, $4, $5, $6, 0, $7::numeric, $8, $9::text, false, $10,
			CASE WHEN $9::text IN ('confirmed', 'finalized') THEN $10::timestamptz END,
			CASE WHEN $9::text = 'finalized' THEN $10::timestamptz END, CASE WHEN $9::text = 'reversed' THEN $10::timestamptz END)`,
		pid, id, flushChain, asset, p.address, fmt.Sprintf("0x%064x", f.n), fmt.Sprint(amount), p.decimals, status, f.app.Clock.Now())
	c := store.ChainPayment{PaymentID: pid, SessionID: id, Chain: flushChain, Asset: asset, Kind: kind, BlockNumber: uint64(1000 + f.n),
		BlockHash: fmt.Sprintf("0x%064x", f.n)}
	if kind == store.KindGateway {
		idx, fee, net := uint32(1), money.FromUint64(0), money.FromUint64(amount)
		c.SplitLogIndex, c.SplitFee, c.SplitNet, c.SplitPayouts = &idx, &fee, &net, []money.Units{net}
	}
	if err := f.app.Store.UpsertChainPayment(f.ctx, c); err != nil {
		f.t.Fatal(err)
	}
}

// history clones template n times as settled sessions: one finalized transfer of amount on asset
// each, every fifth also with a reorged one, and a done flush job covering it. It returns the
// number of payments written.
func (f *flushFixture) history(template, asset string, n int, amount uint64) int {
	f.t.Helper()
	now := f.app.Clock.Now()
	f.exec(`INSERT INTO sessions SELECT (jsonb_populate_record(NULL::sessions, to_jsonb(s) || jsonb_build_object('id', s.id || '_h' || g,
			'salt', '\x' || encode(sha256((s.id || '_h' || g)::bytea), 'hex')))).*
		FROM sessions s, generate_series(1, $2::int) g WHERE s.id = $1`, template, n)
	f.exec(`INSERT INTO session_payouts SELECT (jsonb_populate_record(NULL::session_payouts, to_jsonb(p) ||
			jsonb_build_object('session_id', p.session_id || '_h' || g))).*
		FROM session_payouts p, generate_series(1, $2::int) g WHERE p.session_id = $1`, template, n)
	f.exec(`INSERT INTO session_pay_to SELECT (jsonb_populate_record(NULL::session_pay_to, to_jsonb(p) ||
			jsonb_build_object('session_id', p.session_id || '_h' || g))).*
		FROM session_pay_to p, generate_series(1, $2::int) g WHERE p.session_id = $1`, template, n)
	f.exec(`INSERT INTO payments (id, session_id, chain, asset, address, tx_hash, log_index, amount, decimals, status, late, detected_at,
			confirmed_at, finalized_at, reversed_at)
		SELECT 'pay_h' || g || '_' || i, p.session_id || '_h' || g, p.chain, p.asset, p.address,
			'0x' || encode(sha256(('h' || g)::bytea), 'hex'), i, $5::numeric, p.decimals,
			CASE i WHEN 0 THEN 'finalized' ELSE 'reversed' END, false, $6, $6, CASE i WHEN 0 THEN $6::timestamptz END,
			CASE i WHEN 1 THEN $6::timestamptz END
		FROM session_pay_to p, generate_series(1, $2::int) g, generate_series(0, 1) i
		WHERE p.session_id = $1 AND p.chain = $3 AND p.asset = $4 AND (i = 0 OR g % 5 = 0)`, template, n, flushChain, asset, fmt.Sprint(amount), now)
	f.exec(`INSERT INTO chain_payments (payment_id, session_id, chain, asset, kind, block_number, block_hash)
		SELECT id, session_id, chain, asset, 'transfer', 100, '0x' || encode(sha256(id::bytea), 'hex') FROM payments WHERE id LIKE 'pay_h%'`)
	f.exec(`INSERT INTO chain_tx_jobs (id, chain, kind, ref, session_id, asset, address, planned_total, to_address, calldata, sender, status,
			next_attempt_at, created_at, updated_at)
		SELECT 'job_h' || g, $3, 'flush', 'h|' || g, $1 || '_h' || g, $4, 'deposit', $5::numeric, 'factory', '\x01'::bytea, 'sender', 'done',
			$6, $6, $6
		FROM generate_series(1, $2::int) g`, template, n, flushChain, asset, fmt.Sprint(amount), now)
	return n + n/5
}

// job records a flush job of the session's asset planned for total base units, in status.
func (f *flushFixture) job(id, asset string, total uint64, status string) {
	f.t.Helper()
	f.n++
	f.plan(id, asset, money.FromUint64(total), status, fmt.Sprintf("f1|%d", f.n))
}

func (f *flushFixture) plan(id, asset string, planned money.Units, status, ref string) {
	f.t.Helper()
	if _, _, err := f.app.Store.InsertTxJob(f.ctx, store.TxJob{Chain: flushChain, Kind: store.JobFlush, Ref: ref, SessionID: id, Asset: asset,
		Address: "deposit", PlannedTotal: &planned, To: "factory", Calldata: []byte{1}}); err != nil {
		f.t.Fatal(err)
	}
	f.exec(`UPDATE chain_tx_jobs SET status = $3 WHERE chain = $1 AND ref = $2`, flushChain, ref, status)
}

// flushPair is a flush candidate as both queries see it.
type flushPair struct {
	Session, Asset, Total string
	FirstBlock            uint64
}

func (f *flushFixture) oracleCandidates() []flushPair {
	f.t.Helper()
	rows, err := f.app.Store.Pool().Query(f.ctx, oracleFlushCandidatesSQL, flushChain, 10_000)
	if err != nil {
		f.t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (flushPair, error) {
		var p flushPair
		var first int64
		err := r.Scan(&p.Session, &p.Asset, &p.Total, &first)
		p.FirstBlock = uint64(first)
		return p, err
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return sortPairs(out)
}

func (f *flushFixture) candidates(scope int) []flushPair {
	f.t.Helper()
	store.SetFlushScope(f.app.Store, scope)
	got, err := f.app.Store.FlushCandidates(f.ctx, flushChain, 10_000)
	if err != nil {
		f.t.Fatal(err)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].SessionID > got[i].SessionID {
			f.t.Fatalf("candidates not in session order: %s before %s", got[i-1].SessionID, got[i].SessionID)
		}
	}
	out := make([]flushPair, len(got))
	for i, c := range got {
		out[i] = flushPair{Session: c.SessionID, Asset: c.Asset, Total: c.Total.String(), FirstBlock: c.FirstPaymentBlock}
	}
	return sortPairs(out)
}

func sortPairs(p []flushPair) []flushPair {
	slices.SortFunc(p, func(a, b flushPair) int { return strings.Compare(a.Session+"|"+a.Asset, b.Session+"|"+b.Asset) })
	return p
}

func (f *flushFixture) query() store.ChainStatesQuery {
	return store.ChainStatesQuery{Chains: []string{flushChain, "solana-devnet"}, Now: f.app.Clock.Now(), LateFirst: 20 * time.Minute,
		LateWatch: 23 * time.Hour, PayerActive: 30 * time.Second, SolanaWalletWindow: store.SolanaWalletWindow}
}

func (f *flushFixture) oracleStates() map[string]store.ChainScanState {
	f.t.Helper()
	q := f.query()
	rows, err := f.app.Store.Pool().Query(f.ctx, oracleChainStatesSQL(), q.Chains, q.Now, q.LateFirst.Microseconds(), q.LateWatch.Microseconds(),
		q.PayerActive.Microseconds(), q.SolanaWalletWindow.Microseconds())
	if err != nil {
		f.t.Fatal(err)
	}
	out := map[string]store.ChainScanState{}
	for rows.Next() {
		var id string
		var st store.ChainScanState
		if err := rows.Scan(&id, &st.PayerActive, &st.Open, &st.Recheck, &st.LateDue, &st.Detected, &st.Confirmed,
			&st.Submitted, &st.Mined, &st.LastReveal, &st.SenderDue); err != nil {
			f.t.Fatal(err)
		}
		out[id] = st
	}
	if rows.Err() != nil {
		f.t.Fatal(rows.Err())
	}
	return out
}

func (f *flushFixture) states(scope int) map[string]store.ChainScanState {
	f.t.Helper()
	store.SetFlushScope(f.app.Store, scope)
	all, err := f.app.Store.ChainStates(f.ctx, f.query())
	if err != nil {
		f.t.Fatal(err)
	}
	return all
}

func sameStates(a, b map[string]store.ChainScanState) bool {
	if len(a) != len(b) {
		return false
	}
	for k, x := range a {
		y, ok := b[k]
		if !ok || x.PayerActive != y.PayerActive || x.Open != y.Open || x.Recheck != y.Recheck || x.Detected != y.Detected ||
			x.Confirmed != y.Confirmed || x.Submitted != y.Submitted || x.Mined != y.Mined || x.SenderDue != y.SenderDue ||
			!sameTime(x.LateDue, y.LateDue) || !sameTime(x.LastReveal, y.LastReveal) {
			return false
		}
	}
	return true
}

func sameTime(a, b *time.Time) bool {
	return (a == nil) == (b == nil) && (a == nil || a.Equal(*b))
}

// tuplesRead is how many payments rows this transaction has read so far (sequential and index).
func tuplesRead(ctx context.Context, t *testing.T, tx pgx.Tx) int64 {
	t.Helper()
	var n int64
	if err := tx.QueryRow(ctx, `SELECT coalesce(sum(seq_tup_read + coalesce(idx_tup_fetch, 0)), 0)::bigint FROM pg_stat_xact_user_tables
		WHERE relname = 'payments'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The flush checks (ChainStates' SenderDue, FlushCandidates) read the sessions that can still need
// a flush, not the whole history (ADR-0008 §F-1), and answer exactly what the whole-history
// queries answer for every pair in that scope: settled, flushed, partially paid, reversed,
// gateway, multi-asset, under review, customer-address and late-payment sessions. A session out
// of scope (closed, final and unflushed for over a day) is left to the periodic history read.
func TestFlushChecksReadOnlyTheSessionsThatCanNeedAFlush(t *testing.T) {
	t.Parallel()
	f := newFlushFixture(t)
	app := f.app
	now := app.Clock.Now()
	old, recent, future := now.Add(-30*24*time.Hour), now.Add(-2*time.Hour), now.Add(20*time.Minute)
	const unit = 1_000_000

	// Settled history: paid long ago, every payment final and flushed (some with a reorged one).
	template := f.session("paid", old)
	asset := f.assets(template)[0]
	historyRows := int64(f.history(template, asset, 3000, unit))

	want := map[string]bool{} // session|asset the whole-history query reports
	straggler := f.session("paid", old)
	f.pay(straggler, asset, 5*unit, "finalized", store.KindTransfer) // never flushed: only the history read finds it
	want[straggler+"|"+asset] = true

	s := f.session("paid", recent) // confirmed, no job yet
	f.pay(s, asset, 10*unit, "confirmed", store.KindTransfer)
	want[s+"|"+asset] = true
	s = f.session("paid", recent) // flushed
	f.pay(s, asset, 10*unit, "finalized", store.KindTransfer)
	f.job(s, asset, 10*unit, store.JobDone)
	s = f.session("expired", recent) // expired holding funds
	f.pay(s, asset, 4*unit, "finalized", store.KindTransfer)
	want[s+"|"+asset] = true
	s = f.session("partially_paid", future) // still open
	f.pay(s, asset, 4*unit, "confirmed", store.KindTransfer)
	s = f.session("late_payment", recent) // flushed, then a late payment
	f.pay(s, asset, 10*unit, "finalized", store.KindTransfer)
	f.job(s, asset, 10*unit, store.JobDone)
	f.pay(s, asset, 3*unit, "finalized", store.KindTransfer)
	want[s+"|"+asset] = true
	s = f.session("overpaid", recent) // a payment still unconfirmed
	f.pay(s, asset, 12*unit, "confirmed", store.KindTransfer)
	f.pay(s, asset, unit, "detected", store.KindTransfer)
	s = f.session("paid", recent) // a reorged payment and its replacement
	f.pay(s, asset, 10*unit, "reversed", store.KindTransfer)
	f.pay(s, asset, 10*unit, "confirmed", store.KindTransfer)
	want[s+"|"+asset] = true
	s = f.session("expired", recent) // only a reorged payment
	f.pay(s, asset, 5*unit, "reversed", store.KindTransfer)
	s = f.session("late_payment", old) // flushed long ago, a late payment confirmed now ("Check again")
	f.pay(s, asset, 5*unit, "finalized", store.KindTransfer)
	f.job(s, asset, 5*unit, store.JobDone)
	f.pay(s, asset, 2*unit, "confirmed", store.KindTransfer)
	want[s+"|"+asset] = true
	s = f.session("late_payment", old) // flushed long ago, a late payment still unconfirmed
	f.pay(s, asset, 5*unit, "finalized", store.KindTransfer)
	f.job(s, asset, 5*unit, store.JobDone)
	f.pay(s, asset, 2*unit, "detected", store.KindTransfer)
	s = f.session("paid", old) // confirmed long ago, never finalized nor flushed
	f.pay(s, asset, 5*unit, "confirmed", store.KindTransfer)
	want[s+"|"+asset] = true
	s = f.session("paid", recent) // a failed flush job still counts as planned
	f.pay(s, asset, 10*unit, "finalized", store.KindTransfer)
	f.job(s, asset, 10*unit, store.JobFailed)
	s = f.session("paid", recent) // flushed short: the rest needs another flush
	f.pay(s, asset, 10*unit, "finalized", store.KindTransfer)
	f.job(s, asset, 6*unit, store.JobDone)
	want[s+"|"+asset] = true
	s = f.session("paid", recent) // signed path only: nothing to flush
	f.pay(s, asset, 10*unit, "confirmed", store.KindGateway)
	s = f.session("under_review", recent) // held out of the paid flow
	f.pay(s, asset, 10*unit, "confirmed", store.KindTransfer)
	if as := f.assets(s); len(as) > 1 { // two assets: one flushed, the other not
		s = f.session("paid", recent)
		f.pay(s, as[0], 10*unit, "finalized", store.KindTransfer)
		f.job(s, as[0], 10*unit, store.JobDone)
		f.pay(s, as[1], 3*unit, "confirmed", store.KindTransfer)
		want[s+"|"+as[1]] = true
	}

	// A customer address: its first session was flushed long ago and then got a late payment, the
	// next one is paid now.
	r := app.Do("POST", "/v1/customer_addresses", app.TestKey, map[string]any{"customer_id": "flush-customer", "product_id": app.Link(app.TestKey)})
	var ca struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(r.Body, &ca); err != nil || r.Status != 201 {
		t.Fatalf("customer address: %d %s", r.Status, r.Body)
	}
	first := f.onAddress(ca.ID, "paid", old)
	f.pay(first, asset, 10*unit, "finalized", store.KindTransfer)
	f.job(first, asset, 10*unit, store.JobDone)
	f.pay(first, asset, unit, "confirmed", store.KindTransfer)
	want[first+"|"+asset] = true
	next := f.onAddress(ca.ID, "paid", recent)
	f.pay(next, asset, 10*unit, "confirmed", store.KindTransfer)
	want[next+"|"+asset] = true
	f.exec(`ANALYZE payments, chain_payments, sessions, chain_tx_jobs`)

	// FlushCandidates: the whole-history read is the oracle; the hot scope is the oracle without
	// the straggler.
	oracle := f.oracleCandidates()
	got := map[string]bool{}
	for _, p := range oracle {
		got[p.Session+"|"+p.Asset] = true
	}
	if len(got) != len(want) {
		t.Fatalf("oracle candidates %v, want %v", oracle, want)
	}
	for k := range want {
		if !got[k] {
			t.Fatalf("oracle misses %s: %v", k, oracle)
		}
	}
	if full := f.candidates(store.FlushScopeHistory); !slices.Equal(full, oracle) {
		t.Fatalf("history candidates\n got %v\nwant %v", full, oracle)
	}
	hot := slices.DeleteFunc(slices.Clone(oracle), func(p flushPair) bool { return p.Session == straggler })
	if len(hot) != len(oracle)-1 {
		t.Fatalf("straggler %s not among %v", straggler, oracle)
	}
	if got := f.candidates(store.FlushScopeHot); !slices.Equal(got, hot) {
		t.Fatalf("hot candidates\n got %v\nwant %v", got, hot)
	}

	// ChainStates: with every pair covered by a job, then with each pair alone uncovered, both
	// scopes give the oracle's whole answer (the straggler is due in the history read only).
	for _, p := range oracle {
		total, err := money.ParseInteger(p.Total)
		if err != nil {
			t.Fatal(err)
		}
		f.plan(p.Session, p.Asset, total, store.JobDone, "cover|"+p.Session+"|"+p.Asset)
	}
	check := func(label string, wantDue, wantHot bool) {
		t.Helper()
		o := f.oracleStates()
		if o[flushChain].SenderDue != wantDue {
			t.Fatalf("%s: oracle sender due %v", label, o[flushChain].SenderDue)
		}
		if h := f.states(store.FlushScopeHistory); !sameStates(h, o) {
			t.Fatalf("%s: history states\n got %+v\nwant %+v", label, h, o)
		}
		h := f.states(store.FlushScopeHot)
		if h[flushChain].SenderDue != wantHot {
			t.Fatalf("%s: hot sender due %v", label, h[flushChain].SenderDue)
		}
		if wantHot == wantDue && !sameStates(h, o) {
			t.Fatalf("%s: hot states\n got %+v\nwant %+v", label, h, o)
		}
	}
	check("every pair flushed", false, false)
	if c := f.candidates(store.FlushScopeHistory); len(c) != 0 {
		t.Fatalf("candidates once every pair is flushed: %v", c)
	}
	for _, p := range oracle {
		ref := "cover|" + p.Session + "|" + p.Asset
		f.exec(`UPDATE chain_tx_jobs SET asset = asset || '-off' WHERE chain = $1 AND ref = $2`, flushChain, ref)
		check(p.Session+" "+p.Asset+" alone", true, p.Session != straggler)
		f.exec(`UPDATE chain_tx_jobs SET asset = $3 WHERE chain = $1 AND ref = $2`, flushChain, ref, p.Asset)
	}

	// Reads: the hot scope reads the in-scope payments only, not the settled history.
	tx, err := app.Store.Pool().Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(f.ctx) }()
	before := tuplesRead(f.ctx, t, tx)
	if _, err := tx.Exec(f.ctx, oracleFlushCandidatesSQL, flushChain, 10_000); err != nil {
		t.Fatal(err)
	}
	oracleRead := tuplesRead(f.ctx, t, tx) - before
	bound := app.Store.Bind(tx)
	store.SetFlushScope(bound, store.FlushScopeHot)
	before = tuplesRead(f.ctx, t, tx)
	if _, err := bound.FlushCandidates(f.ctx, flushChain, 10_000); err != nil {
		t.Fatal(err)
	}
	hotRead := tuplesRead(f.ctx, t, tx) - before
	before = tuplesRead(f.ctx, t, tx)
	if _, err := bound.ChainStates(f.ctx, f.query()); err != nil {
		t.Fatal(err)
	}
	statesRead := tuplesRead(f.ctx, t, tx) - before
	t.Logf("payments rows read: whole history %d, hot candidates %d, hot states %d (settled history %d rows)",
		oracleRead, hotRead, statesRead, historyRows)
	if oracleRead < historyRows || hotRead >= historyRows/4 || statesRead >= historyRows/4 {
		t.Fatalf("payments rows read: whole history %d, hot candidates %d, hot states %d; settled history is %d rows",
			oracleRead, hotRead, statesRead, historyRows)
	}
}

// Each flush check reads the whole history on a chain's first call and again every
// flushHistoryEvery, so a session outside the hot scope is still flushed; a history read that
// filled its batch is repeated at the next call.
func TestFlushChecksReadTheHistoryPeriodically(t *testing.T) {
	t.Parallel()
	f := newFlushFixture(t)
	app := f.app
	straggler := f.session("paid", app.Clock.Now().Add(-30*24*time.Hour))
	asset := f.assets(straggler)[0]
	f.pay(straggler, asset, 5_000_000, "finalized", store.KindTransfer)
	store.SetFlushScope(app.Store, store.FlushScopeAuto)
	due := func() bool {
		t.Helper()
		all, err := app.Store.ChainStates(f.ctx, f.query())
		if err != nil {
			t.Fatal(err)
		}
		return all[flushChain].SenderDue
	}
	cands := func(limit int) int {
		t.Helper()
		c, err := app.Store.FlushCandidates(f.ctx, flushChain, limit)
		if err != nil {
			t.Fatal(err)
		}
		return len(c)
	}
	if !due() || due() {
		t.Fatal("states: the first read covers the history, the next one the hot scope")
	}
	if cands(1) != 1 || cands(10) != 1 || cands(10) != 0 {
		t.Fatal("candidates: a full batch reads the history again, a partial one is the last until the period ends")
	}
	app.Clock.Advance(store.FlushHistoryEvery - time.Second)
	if due() || cands(10) != 0 {
		t.Fatal("history read again before the period ended")
	}
	app.Clock.Advance(time.Second)
	if !due() || cands(10) != 1 {
		t.Fatal("history not read again once the period ended")
	}
}
