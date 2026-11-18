package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/money"
)

// ScanQuery is what the scanner asks the database about one chain.
type ScanQuery struct {
	Chain  string
	Family chain.Family
	Now    time.Time
	// PayerActive is how recent checkout_seen_at must be for the payer to count as present.
	PayerActive time.Duration
	// LateFirst is the delay of the first late check after expiry; LateWatch is how long after
	// expiry checks still run. LateWatch 0 turns scheduled late checks off.
	LateFirst time.Duration
	LateWatch time.Duration
	// WalletWindow keeps a Solana chain scanning while a session's reference key is (D49,
	// WatchEntry.WalletScanEnd); 0 for EVM chains.
	WalletWindow time.Duration
}

// lateHorizon is how far past its watch end an overdue late check still runs (a worker that
// was down still makes the check it owed). SQL interval literal.
const lateHorizon = "1 day"

// ChainScanState is what is pending on a chain (scan.State without the dependency).
type ChainScanState struct {
	PayerActive, Open, Recheck bool
	Detected, Confirmed        bool
	Submitted, Mined           bool
	LateDue                    *time.Time
	// LastReveal is the latest reveal of a session on the chain (nil: never).
	LastReveal *time.Time
	// SenderDue is set when the chain's sender has work: a flush to plan, a pending or blocked
	// job whose retry time has come, a submitted job, or a mined job whose finality check is due.
	SenderDue bool
}

func micros(d time.Duration) int64 { return d.Microseconds() }

// revealedCond keeps the entries of networks whose deposit details were revealed (D48): nothing
// else was ever shown, so nothing else is watched. p is the session_pay_to row.
const revealedCond = ` AND EXISTS (SELECT 1 FROM session_reveals r WHERE r.session_id = p.session_id AND r.chain = p.chain)`

// settledExpr is true for a session paid exactly whose payments are all final (D47): it leaves
// every automatic check, block scan and late checks alike. Only "Check again" still reaches it.
const settledExpr = `(s.status = 'paid' AND NOT EXISTS (SELECT 1 FROM payments y WHERE y.session_id = s.id
	AND y.status IN ('detected', 'confirmed')))`

// watchJoin joins an entry's late-check row: chain_watch by address (EVM), the deposit row of
// solana_watch (Solana, real forwarder addresses only).
func watchJoin(f chain.Family) (join, alias, cond string) {
	if f == chain.Solana {
		return `LEFT JOIN solana_watch sw ON sw.chain = p.chain AND sw.session_id = p.session_id AND sw.kind = 'deposit'
			AND sw.asset = p.asset`, "sw", " AND p.program IS NOT NULL"
	}
	return "", "w", ""
}

// lateDueExpr is when an entry's next late check is due: its stored time, or the first step after
// expiry. $3 is the first delay in microseconds.
func lateDueExpr(alias string) string {
	return `coalesce(` + alias + `.next_check_at, s.expires_at + $3::bigint * interval '1 microsecond')`
}

// lateCond selects entries whose scheduled check is due ($2 now, $4 watch in microseconds) or
// that wait for a "Check again" request. A settled session (D47) gets no scheduled check.
func lateCond(alias string) string {
	due := lateDueExpr(alias)
	return `((s.recheck_requested_at IS NOT NULL AND s.recheck_requested_at IS DISTINCT FROM ` + alias + `.rechecked_for)
		OR ($4::bigint > 0 AND NOT ` + settledExpr + ` AND s.expires_at <= $2::timestamptz AND s.expires_at > $2::timestamptz - ($4::bigint * interval '1 microsecond') - interval '` +
		lateHorizon + `' AND ` + due + ` <= $2 AND ` + due + ` <= s.expires_at + $4::bigint * interval '1 microsecond'))`
}

// ScanState reads what is pending on one chain (ChainStates for that chain alone).
func (s *Store) ScanState(ctx context.Context, q ScanQuery) (ChainScanState, error) {
	wallet := q.WalletWindow
	if q.Family != chain.Solana {
		wallet = 0
	}
	all, err := s.ChainStates(ctx, ChainStatesQuery{Chains: []string{q.Chain}, Now: q.Now, PayerActive: q.PayerActive,
		LateFirst: q.LateFirst, LateWatch: q.LateWatch, SolanaWalletWindow: wallet})
	return all[q.Chain], opErr("scan state", err)
}

// ChainStatesQuery is what the chain scheduler asks the database about every chain at once.
type ChainStatesQuery struct {
	Chains               []string
	Now                  time.Time
	PayerActive          time.Duration
	LateFirst, LateWatch time.Duration
	// SolanaWalletWindow keeps a Solana chain scanning while a session's reference key is
	// (WatchEntry.WalletScanEnd); EVM entries ignore it.
	SolanaWalletWindow time.Duration
}

// ChainStates reads what is pending on each of the chains in one query, whatever their number:
// the scan pacers' inputs (ScanQuery's rules: a Solana entry joins its solana_watch row and only
// real forwarder addresses count), the latest reveal and whether the sender has work. It is
// database only and cheap enough to run every second. Every chain asked for is in the result.
func (s *Store) ChainStates(ctx context.Context, q ChainStatesQuery) (map[string]ChainScanState, error) {
	full := s.flush.due("states", q.Chains, q.Now)
	due := `coalesce(CASE WHEN p.family = 'solana' THEN sw.next_check_at ELSE w.next_check_at END,
		s.expires_at + $3::bigint * interval '1 microsecond')`
	rows, err := s.db.Query(ctx, `WITH cand AS (
			SELECT p.chain, s.status, s.expires_at, s.checkout_seen_at, s.recheck_requested_at,
				CASE WHEN p.family = 'solana' THEN sw.rechecked_for ELSE w.rechecked_for END AS rechecked_for,
				`+due+` AS due, `+settledExpr+` AS settled,
				(p.family = 'solana' AND $6::bigint > 0 AND `+walletScanCond("$2", "$6")+`) AS wallet
			`+watchFrom+`
			LEFT JOIN solana_watch sw ON p.family = 'solana' AND sw.chain = p.chain AND sw.session_id = p.session_id
				AND sw.kind = 'deposit' AND sw.asset = p.asset
			WHERE p.chain = ANY($1) AND (p.family <> 'solana' OR p.program IS NOT NULL)`+revealedCond+ownerCond+`
				AND (s.expires_at > $2::timestamptz - ($4::bigint * interval '1 microsecond') - interval '`+lateHorizon+`'
					OR s.recheck_requested_at IS NOT NULL)),
		scan AS (SELECT chain,
				bool_or(status IN ('open', 'partially_paid') AND expires_at > $2
					AND checkout_seen_at > $2::timestamptz - $5::bigint * interval '1 microsecond') AS payer,
				bool_or((status IN ('open', 'partially_paid') AND expires_at > $2) OR (wallet AND NOT settled)) AS open,
				bool_or(recheck_requested_at IS NOT NULL AND recheck_requested_at IS DISTINCT FROM rechecked_for) AS recheck,
				min(due) FILTER (WHERE $4::bigint > 0 AND NOT settled AND expires_at <= $2
					AND expires_at > $2::timestamptz - ($4::bigint * interval '1 microsecond') - interval '`+lateHorizon+`'
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
			WHERE p.chain = ANY($1)`+flushScope(full, "= ANY($1)", "$2")+` GROUP BY p.chain, p.session_id, p.asset),
		flush AS (SELECT DISTINCT t.chain FROM totals t JOIN sessions s ON s.id = t.session_id
			WHERE s.status IN ('paid', 'overpaid', 'expired', 'late_payment') AND t.pending = 0 AND t.counted > 0
				AND t.counted > coalesce((SELECT max(j.planned_total) FROM chain_tx_jobs j WHERE j.chain = t.chain
					AND j.kind = 'flush' AND j.session_id = t.session_id AND j.asset = t.asset), 0))
		SELECT c.chain, coalesce(a.payer, false), coalesce(a.open, false), coalesce(a.recheck, false), a.late_due,
			coalesce(y.detected, false), coalesce(y.confirmed, false), coalesce(j.submitted, false), coalesce(j.mined, false),
			(SELECT max(r.revealed_at) FROM session_reveals r WHERE r.chain = c.chain),
			coalesce(j.due, false) OR f.chain IS NOT NULL
		FROM unnest($1::text[]) AS c(chain) LEFT JOIN scan a ON a.chain = c.chain LEFT JOIN pays y ON y.chain = c.chain
			LEFT JOIN jobs j ON j.chain = c.chain LEFT JOIN flush f ON f.chain = c.chain`,
		q.Chains, q.Now, micros(q.LateFirst), micros(q.LateWatch), micros(q.PayerActive), micros(q.SolanaWalletWindow))
	if err != nil {
		return nil, opErr("chain states", err)
	}
	out := make(map[string]ChainScanState, len(q.Chains))
	for rows.Next() {
		var id string
		var st ChainScanState
		if err := rows.Scan(&id, &st.PayerActive, &st.Open, &st.Recheck, &st.LateDue, &st.Detected, &st.Confirmed,
			&st.Submitted, &st.Mined, &st.LastReveal, &st.SenderDue); err != nil {
			rows.Close()
			return nil, opErr("chain states", err)
		}
		out[id] = st
	}
	if err := rows.Err(); err != nil {
		return nil, opErr("chain states", err)
	}
	if full {
		s.flush.done("states", q.Chains, q.Now)
	}
	return out, nil
}

// LateEntries lists up to limit pay_to entries due for a late check (oldest due first): expired
// sessions inside their watch whose next check has come (not settled ones, D47), and every entry
// of a session with a "Check again" request it was not checked for. Only revealed networks count.
func (s *Store) LateEntries(ctx context.Context, q ScanQuery, limit int) ([]WatchEntry, error) {
	join, alias, cond := watchJoin(q.Family)
	rows, err := s.db.Query(ctx, `SELECT `+watchColumns+` `+watchFrom+` `+join+`
		WHERE p.chain = $1`+cond+revealedCond+ownerCond+` AND `+lateCond(alias)+`
		ORDER BY `+lateDueExpr(alias)+`, s.id, p.position LIMIT $5`,
		q.Chain, q.Now, micros(q.LateFirst), micros(q.LateWatch), limit)
	if err != nil {
		return nil, opErr("late entries", err)
	}
	out, err := pgx.CollectRows(rows, scanWatch)
	return out, opErr("late entries", err)
}

// EVMCheck is what one check (tick or late check) of an EVM deposit entry wrote. Nil fields keep
// the stored value.
type EVMCheck struct {
	// FirstBlock is only used when the watch row does not exist yet.
	FirstBlock uint64
	// Block is the block a successful rescan reached (checked_block).
	Block *uint64
	// Balance is the entry's token balance, read at BalanceBlock, stored unless a settlement
	// moved checked_at since the entry was read or the balance stored is from a later block.
	Balance      *money.Units
	BalanceBlock uint64
	Next         *time.Time
	RecheckedFor *time.Time
	// ForwarderSeen records that the address's forwarder is deployed (kept from the first time).
	ForwarderSeen bool
}

// MarkChecked records a check of an EVM deposit entry, creating its watch row (from FirstBlock)
// if the address has none yet. The balance is kept per token (every asset shares the address).
// Only the row's owner writes it (a customer address's row may have passed to a newer session
// since the entry was read).
func (s *Store) MarkChecked(ctx context.Context, e WatchEntry, c EVMCheck) error {
	var block *int64
	if c.Block != nil {
		b := int64(*c.Block)
		block = &b
	}
	var bal *string
	if c.Balance != nil {
		v := c.Balance.String()
		bal = &v
	}
	var checkedAt *time.Time
	if bal != nil {
		now := s.now()
		checkedAt = &now
	}
	token := strings.ToLower(e.Token)
	var seen *time.Time
	if c.ForwarderSeen {
		now := s.now()
		seen = &now
	}
	// A balance is written only when nothing moved checked_at since the entry was read and no
	// balance of that token from a later block is stored.
	const write = `$6::text IS NOT NULL AND chain_watch.checked_at IS NOT DISTINCT FROM $10
		AND coalesce((chain_watch.token_blocks ->> $11::text)::bigint, -1) <= $13::bigint`
	_, err := s.db.Exec(ctx, `INSERT INTO chain_watch (chain, address, session_id, first_block, checked_block, token_balances, token_blocks,
			checked_at, next_check_at, rechecked_for, forwarder_seen_at)
		VALUES ($1, $2, $3, $4, $5, CASE WHEN $6::text IS NULL THEN NULL ELSE jsonb_build_object($11::text, $6::text) END,
			CASE WHEN $6::text IS NULL THEN NULL ELSE jsonb_build_object($11::text, $13::bigint) END, $7, $8, $9, $12)
		ON CONFLICT (chain, address) DO UPDATE SET
			checked_block = coalesce(EXCLUDED.checked_block, chain_watch.checked_block),
			token_balances = CASE WHEN `+write+`
				THEN coalesce(chain_watch.token_balances, '{}'::jsonb) || jsonb_build_object($11::text, $6::text)
				ELSE chain_watch.token_balances END,
			token_blocks = CASE WHEN `+write+`
				THEN coalesce(chain_watch.token_blocks, '{}'::jsonb) || jsonb_build_object($11::text, $13::bigint)
				ELSE chain_watch.token_blocks END,
			checked_at = CASE WHEN `+write+`
				THEN EXCLUDED.checked_at ELSE chain_watch.checked_at END,
			next_check_at = coalesce(EXCLUDED.next_check_at, chain_watch.next_check_at),
			rechecked_for = coalesce(EXCLUDED.rechecked_for, chain_watch.rechecked_for),
			forwarder_seen_at = coalesce(chain_watch.forwarder_seen_at, EXCLUDED.forwarder_seen_at)
			WHERE chain_watch.session_id = EXCLUDED.session_id`,
		e.Chain, strings.ToLower(e.Address), e.SessionID, int64(c.FirstBlock), block, bal, checkedAt, c.Next, c.RecheckedFor, e.CheckedAt,
		token, seen, int64(c.BalanceBlock))
	return opErr("mark checked", err)
}

// ClearRecheck ends a session's "Check again" request once every watched deposit entry of the
// session (revealed networks only), on every chain, was checked for it (requestedAt is the value the check read; a newer
// request stays). It reports whether the request was cleared.
func (s *Store) ClearRecheck(ctx context.Context, sessionID string, requestedAt time.Time) (bool, error) {
	tag, err := s.db.Exec(ctx, `UPDATE sessions s SET recheck_requested_at = NULL
		WHERE s.id = $1 AND s.recheck_requested_at = $2
			AND NOT EXISTS (SELECT 1 FROM session_pay_to p LEFT JOIN chain_watch w ON w.chain = p.chain AND w.address = lower(p.address)
				AND (s.customer_address_id IS NULL OR w.session_id = s.id)
				WHERE p.session_id = s.id AND p.family = 'evm' AND w.rechecked_for IS DISTINCT FROM $2`+revealedCond+`)
			AND NOT EXISTS (SELECT 1 FROM session_pay_to p LEFT JOIN solana_watch sw ON sw.chain = p.chain
				AND sw.session_id = p.session_id AND sw.kind = 'deposit' AND sw.asset = p.asset
				WHERE p.session_id = s.id AND p.family = 'solana' AND p.program IS NOT NULL AND sw.rechecked_for IS DISTINCT FROM $2`+revealedCond+`)`,
		sessionID, requestedAt)
	if err != nil {
		return false, opErr("clear recheck", err)
	}
	return tag.RowsAffected() == 1, nil
}

// ResetLateCheck forgets what checks saw on a session's EVM deposit addresses and makes the next
// late check due: a payment found at block was reversed by a hash re-check, and the transfer may be
// mined again, so the next balance read rescans from the address's first block. The rows are the
// session's addresses' (on a customer address they may belong to a newer session by now), and the
// address's cursor moves back below block so its history there is read again.
func (s *Store) ResetLateCheck(ctx context.Context, chainID, sessionID string, block uint64) error {
	return opErr("reset late check", s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE chain_watch SET token_balances = NULL, token_blocks = NULL, checked_block = NULL, next_check_at = NULL
			WHERE chain = $1 AND address IN (SELECT lower(address) FROM session_pay_to WHERE session_id = $2 AND chain = $1)`,
			chainID, sessionID); err != nil {
			return err
		}
		keep := int64(0)
		if block > 0 {
			keep = int64(block) - 1
		}
		_, err := tx.Exec(ctx, `UPDATE customer_address_cursors SET block_number = $3, updated_at = $4
			WHERE chain = $1 AND block_number > $3
				AND customer_address_id = (SELECT customer_address_id FROM sessions WHERE id = $2)`, chainID, sessionID, keep, s.now())
		return err
	}))
}

// OwnFlushJob reports whether the sender sent a flush of the EVM deposit address (our own
// deployAndFlush deploys its forwarder): a job holding a transaction of ours, submitted, mined or
// finished from its receipt. A job only planned (or one whose transaction reverted and waits to
// be sent again) deployed nothing, so a deployment next to it is someone else's.
func (s *Store) OwnFlushJob(ctx context.Context, chainID, address string) (bool, error) {
	var ok bool
	err := s.db.QueryRow(ctx, ownFlushJobSQL, chainID, address).Scan(&ok)
	return ok, opErr("own flush job", err)
}

// ownFlushJobSQL matches lower(address): an EVM flush job stores the checksummed address and the
// detector asks with the lowercase one. The index chain_tx_jobs_flush_address (migration 00102)
// serves it.
const ownFlushJobSQL = `SELECT EXISTS (SELECT 1 FROM chain_tx_jobs WHERE chain = $1 AND kind = 'flush' AND lower(address) = lower($2)
	AND cardinality(tx_hashes) > 0)`

// FlushMined records that our flush of token emptied the EVM deposit address at block: the
// token's stored balance becomes 0, stamped with block, so the next deposit reads as a rise
// whatever its amount (a reading equal to or below the balance credited before the flush would
// otherwise look like nothing new). A balance stored from block or later is kept: it was read
// after the flush. checked_block moves back to the block of the balance the zero replaces, by at
// most reach blocks (the detector's baseline reach; 0 leaves it): the blocks after that balance
// are no longer in any window a rescan must explain a reading with, so a deposit there whose log
// lagged when checked_block passed it is read again by the next rescan (the sender's, after a
// flush that moved other than was credited; ADR-0008 H-1). The row is the address's, whoever
// holds it (on a customer address a newer session may own the watch).
func (s *Store) FlushMined(ctx context.Context, chainID, address, token string, block, reach uint64) error {
	_, err := s.db.Exec(ctx, `UPDATE chain_watch SET token_balances = coalesce(token_balances, '{}'::jsonb) || jsonb_build_object($3::text, '0'),
			token_blocks = coalesce(token_blocks, '{}'::jsonb) || jsonb_build_object($3::text, $4::bigint),
			checked_block = CASE WHEN checked_block IS NULL OR token_blocks ->> $3::text IS NULL THEN checked_block
				ELSE least(checked_block, greatest((token_blocks ->> $3::text)::bigint, checked_block - $5::bigint)) END
		WHERE chain = $1 AND address = lower($2) AND coalesce((token_blocks ->> $3::text)::bigint, -1) < $4::bigint`,
		chainID, address, strings.ToLower(token), int64(block), int64(reach))
	return opErr("flush mined", err)
}

// SetCreditHead records head as the block the detector last credited payments on for chainID:
// the checkout counts a detected payment's confirmations up to it. It only moves forward: a
// provider behind the last reading does not take confirmations back.
func (s *Store) SetCreditHead(ctx context.Context, chainID string, head uint64) error {
	_, err := s.db.Exec(ctx, `INSERT INTO chain_credit_heads (chain, block_number) VALUES ($1, $2)
		ON CONFLICT (chain) DO UPDATE SET block_number = EXCLUDED.block_number, updated_at = now()
		WHERE chain_credit_heads.block_number < EXCLUDED.block_number`, chainID, int64(head))
	return opErr("set credit head", err)
}

// CreditHead is the block SetCreditHead last recorded for chainID; found is false before any.
func (s *Store) CreditHead(ctx context.Context, chainID string) (head uint64, found bool, err error) {
	var n int64
	err = s.db.QueryRow(ctx, `SELECT block_number FROM chain_credit_heads WHERE chain = $1`, chainID).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	return uint64(n), err == nil, opErr("credit head", err)
}
