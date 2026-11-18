package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prismaport/prismaport/internal/ids"
	"github.com/prismaport/prismaport/internal/money"
)

// FlushCandidate is a deposit address whose session is closed and holds confirmed deposits
// that no flush job has been planned for yet.
type FlushCandidate struct {
	WatchEntry
	Total money.Units
	// FirstPaymentBlock is the lowest block (slot on Solana) of the counted deposits of the session on
	// the entry's asset: no flush before it can have moved them.
	FirstPaymentBlock uint64
}

// FlushRef is a flush job's dedupe key: base names the deposit address, token and planned total.
// Sessions on one customer address share the address, and two of them can plan the same total,
// so theirs also names the session.
func FlushRef(base string, e WatchEntry) string {
	if e.CustomerAddressID == nil {
		return base
	}
	return base + "|" + e.SessionID
}

// FlushCandidates finds closed sessions (paid, overpaid, expired with funds, late payment)
// whose confirmed deposit-path total on a chain exceeds what the last flush job planned for,
// and that have no payment still waiting for confirmation on that asset. It reads the sessions in
// flushScope, and the whole history on a chain's first call and every flushHistoryEvery.
func (s *Store) FlushCandidates(ctx context.Context, chainID string, limit int) ([]FlushCandidate, error) {
	now := s.now()
	chains := []string{chainID}
	full := s.flush.due("candidates", chains, now)
	args := []any{chainID, limit}
	if !full {
		args = append(args, now)
	}
	rows, err := s.db.Query(ctx, `WITH totals AS (
			SELECT p.session_id, p.asset,
				coalesce(sum(p.amount) FILTER (WHERE p.status IN ('confirmed', 'finalized')), 0) AS counted,
				count(*) FILTER (WHERE p.status = 'detected') AS pending,
				coalesce(min(c.block_number) FILTER (WHERE p.status IN ('confirmed', 'finalized')), 0) AS first_block
			FROM payments p JOIN chain_payments c ON c.payment_id = p.id AND c.kind = 'transfer'
			WHERE p.chain = $1`+flushScope(full, "= $1", "$3")+` GROUP BY p.session_id, p.asset)
		SELECT `+watchColumns+`, t.counted::text, t.first_block `+watchFrom+`
		JOIN totals t ON t.session_id = p.session_id AND t.asset = p.asset
		WHERE p.chain = $1 AND s.status IN ('paid', 'overpaid', 'expired', 'late_payment') AND t.pending = 0 AND t.counted > 0
			AND t.counted > coalesce((SELECT max(j.planned_total) FROM chain_tx_jobs j WHERE j.chain = $1 AND j.kind = 'flush'
				AND j.session_id = p.session_id AND j.asset = p.asset), 0)
		ORDER BY s.id LIMIT $2`, args...)
	if err != nil {
		return nil, opErr("flush candidates", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (FlushCandidate, error) {
		var c FlushCandidate
		var total string
		var first int64
		e, err := scanWatchRow(r, &total, &first)
		if err != nil {
			return c, err
		}
		c.WatchEntry, c.FirstPaymentBlock = e, uint64(first)
		c.Total, err = parseUnits("total", total)
		return c, err
	})
	// A whole-history read that filled the batch may have left candidates: the next call reads
	// the history again.
	if err == nil && full && len(out) < limit {
		s.flush.done("candidates", chains, now)
	}
	return out, opErr("flush candidates", err)
}

// TxJob is one transaction the gas wallet sends.
type TxJob struct {
	ID            string
	Chain         string
	Kind          string
	Ref           string
	SessionID     string
	Asset         string
	Address       string
	PlannedTotal  *money.Units
	To            string
	Calldata      []byte
	Sender        string
	Details       map[string]string
	Status        string
	Outcome       string
	Attempts      int
	NextAttemptAt time.Time
	Nonce         *uint64
	// LastValidHeight bounds a submitted Solana transaction: past this block height its
	// blockhash has expired and it can never land.
	LastValidHeight *uint64
	GasLimit        uint64
	MaxFee          *money.Units
	Tip             *money.Units
	TxHashes        []string
	RawTx           []byte
	SubmittedAt     *time.Time
	MinedTxHash     string
	MinedBlock      *uint64
	MinedBlockHash  string
	GasUsed         uint64
	LastError       string
	// FinalityCheckAt is when a mined job's next finalized-head read is due (D49); nil is due now.
	FinalityCheckAt *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Transaction job kinds and statuses.
const (
	JobFlush  = "flush"
	JobSettle = "settle"

	JobPending   = "pending"
	JobSubmitted = "submitted"
	JobMined     = "mined"
	JobDone      = "done"
	JobFailed    = "failed"
	// JobBlocked is a flush waiting for something only the merchant can provide (the
	// recipients' token accounts); it is re-checked, never sent, and never gives up.
	JobBlocked = "blocked"
)

const txJobColumns = `id, chain, kind, ref, session_id, asset, coalesce(address, ''), planned_total::text, to_address, calldata,
	sender, details, status, coalesce(outcome, ''), attempts, next_attempt_at, nonce, coalesce(gas_limit, 0), max_fee::text,
	tip::text, tx_hashes, raw_tx, submitted_at, coalesce(mined_tx_hash, ''), mined_block, coalesce(mined_block_hash, ''),
	coalesce(gas_used, 0), coalesce(last_error, ''), created_at, updated_at, last_valid_height, finality_check_at`

func scanTxJob(r pgx.Row) (TxJob, error) {
	var j TxJob
	var planned, maxFee, tip *string
	var nonce, minedBlock, lastValid *int64
	var gas, gasUsed int64
	var details []byte
	if err := r.Scan(&j.ID, &j.Chain, &j.Kind, &j.Ref, &j.SessionID, &j.Asset, &j.Address, &planned, &j.To, &j.Calldata,
		&j.Sender, &details, &j.Status, &j.Outcome, &j.Attempts, &j.NextAttemptAt, &nonce, &gas, &maxFee, &tip, &j.TxHashes,
		&j.RawTx, &j.SubmittedAt, &j.MinedTxHash, &minedBlock, &j.MinedBlockHash, &gasUsed, &j.LastError, &j.CreatedAt,
		&j.UpdatedAt, &lastValid, &j.FinalityCheckAt); err != nil {
		return j, notFound(err)
	}
	if err := json.Unmarshal(details, &j.Details); err != nil {
		return j, fmt.Errorf("store: tx job %s details: %w", j.ID, err)
	}
	j.GasLimit, j.GasUsed = uint64(gas), uint64(gasUsed)
	if nonce != nil {
		n := uint64(*nonce)
		j.Nonce = &n
	}
	if minedBlock != nil {
		n := uint64(*minedBlock)
		j.MinedBlock = &n
	}
	if lastValid != nil {
		n := uint64(*lastValid)
		j.LastValidHeight = &n
	}
	var err error
	if j.PlannedTotal, err = parseOptUnits("planned_total", planned); err != nil {
		return j, err
	}
	if j.MaxFee, err = parseOptUnits("max_fee", maxFee); err != nil {
		return j, err
	}
	j.Tip, err = parseOptUnits("tip", tip)
	return j, err
}

// InsertTxJob creates a job unless one with the same (chain, kind, ref) exists; the existing
// job is returned with inserted=false. A failed settle job is re-armed with the new calldata,
// so a payer can retry after fixing what made the simulation revert.
func (s *Store) InsertTxJob(ctx context.Context, j TxJob) (TxJob, bool, error) {
	now := s.now()
	if j.ID == "" {
		j.ID = ids.NewAt("txj", now)
	}
	if j.Details == nil {
		j.Details = map[string]string{}
	}
	details, err := json.Marshal(j.Details)
	if err != nil {
		return TxJob{}, false, opErr("insert tx job", err)
	}
	var address *string
	if j.Address != "" {
		address = &j.Address
	}
	var out TxJob
	inserted := false
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO chain_tx_jobs (id, chain, kind, ref, session_id, asset, address, planned_total,
			to_address, calldata, sender, details, status, next_attempt_at, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8::numeric, $9, $10, $11, $12, 'pending', $13, $13, $13)
			ON CONFLICT (chain, kind, ref) DO NOTHING`,
			j.ID, j.Chain, j.Kind, j.Ref, j.SessionID, j.Asset, address, optUnitsArg(j.PlannedTotal), j.To, j.Calldata,
			j.Sender, details, now)
		if err != nil {
			return err
		}
		inserted = tag.RowsAffected() == 1
		if !inserted && j.Kind == JobSettle {
			if _, err := tx.Exec(ctx, `UPDATE chain_tx_jobs SET status = 'pending', calldata = $4, details = $5, attempts = 0,
				next_attempt_at = $6, last_error = NULL, outcome = NULL, nonce = NULL, last_valid_height = NULL, tx_hashes = '{}', raw_tx = NULL,
				submitted_at = NULL, updated_at = greatest($6, updated_at + interval '1 microsecond')
				WHERE chain = $1 AND kind = $2 AND ref = $3 AND status = 'failed'`,
				j.Chain, j.Kind, j.Ref, j.Calldata, details, now); err != nil {
				return err
			}
		}
		out, err = scanTxJob(tx.QueryRow(ctx, `SELECT `+txJobColumns+` FROM chain_tx_jobs WHERE chain = $1 AND kind = $2 AND ref = $3`,
			j.Chain, j.Kind, j.Ref))
		return err
	})
	return out, inserted, opErr("insert tx job", err)
}

// GetTxJob returns the transaction job with the given id, or ErrNotFound when none exists.
func (s *Store) GetTxJob(ctx context.Context, id string) (TxJob, error) {
	out, err := scanTxJob(s.db.QueryRow(ctx, `SELECT `+txJobColumns+` FROM chain_tx_jobs WHERE id = $1`, id))
	return out, opErr("get tx job", err)
}

// TxJobs lists a chain's jobs in the given statuses, oldest first.
func (s *Store) TxJobs(ctx context.Context, chainID string, statuses ...string) ([]TxJob, error) {
	rows, err := s.db.Query(ctx, `SELECT `+txJobColumns+` FROM chain_tx_jobs WHERE chain = $1 AND status = ANY($2)
		ORDER BY created_at, id`, chainID, statuses)
	if err != nil {
		return nil, opErr("tx jobs", err)
	}
	var out []TxJob
	for rows.Next() {
		j, err := scanTxJob(rows)
		if err != nil {
			rows.Close()
			return nil, opErr("tx jobs", err)
		}
		out = append(out, j)
	}
	return out, opErr("tx jobs", rows.Err())
}

// SessionTxJobs lists a session's jobs, oldest first.
func (s *Store) SessionTxJobs(ctx context.Context, sessionID string) ([]TxJob, error) {
	rows, err := s.db.Query(ctx, `SELECT `+txJobColumns+` FROM chain_tx_jobs WHERE session_id = $1 ORDER BY created_at, id`, sessionID)
	if err != nil {
		return nil, opErr("session tx jobs", err)
	}
	var out []TxJob
	for rows.Next() {
		j, err := scanTxJob(rows)
		if err != nil {
			rows.Close()
			return nil, opErr("session tx jobs", err)
		}
		out = append(out, j)
	}
	return out, opErr("session tx jobs", rows.Err())
}

// NextDueTxJob returns the oldest pending job whose retry time has come.
func (s *Store) NextDueTxJob(ctx context.Context, chainID string) (TxJob, bool, error) {
	j, err := scanTxJob(s.db.QueryRow(ctx, `SELECT `+txJobColumns+` FROM chain_tx_jobs WHERE chain = $1 AND status = 'pending'
		AND next_attempt_at <= $2 ORDER BY created_at, id LIMIT 1`, chainID, s.now()))
	if errors.Is(err, ErrNotFound) {
		return j, false, nil
	}
	return j, err == nil, opErr("next due tx job", err)
}

// ErrTxJobChanged is an UpdateTxJob of a job that was written since it was read: another worker
// took the chain lock (this one lost it) and moved the job on. Nothing was written.
var ErrTxJobChanged = errors.New("store: tx job changed since it was read")

// UpdateTxJob writes the mutable fields of a job, only if the job is still as it was read: its
// updated_at is the row version, raised by every write (by at least a microsecond, so two writes
// at the same clock reading still differ). A job written since fails with ErrTxJobChanged, which
// fences off a worker that lost the chain lock mid-step (it would otherwise sign the job again at
// another nonce, or erase what the lock holder wrote). On success j.UpdatedAt is the new version,
// so the caller can write the same job again.
func (s *Store) UpdateTxJob(ctx context.Context, j *TxJob) error {
	var nonce, minedBlock, lastValid *int64
	if j.LastValidHeight != nil {
		n := int64(*j.LastValidHeight)
		lastValid = &n
	}
	if j.Nonce != nil {
		n := int64(*j.Nonce)
		nonce = &n
	}
	if j.MinedBlock != nil {
		n := int64(*j.MinedBlock)
		minedBlock = &n
	}
	opt := func(s string) *string {
		if s == "" {
			return nil
		}
		return &s
	}
	hashes := j.TxHashes
	if hashes == nil {
		hashes = []string{}
	}
	var version time.Time
	err := s.db.QueryRow(ctx, `UPDATE chain_tx_jobs SET status = $2, outcome = $3, attempts = $4, next_attempt_at = $5,
		nonce = $6, gas_limit = $7, max_fee = $8::numeric, tip = $9::numeric, tx_hashes = $10, raw_tx = $11, submitted_at = $12,
		mined_tx_hash = $13, mined_block = $14, mined_block_hash = $15, gas_used = $16, last_error = $17,
		updated_at = greatest($18, updated_at + interval '1 microsecond'), sender = $19, last_valid_height = $20,
		finality_check_at = $21 WHERE id = $1 AND updated_at = $22 RETURNING updated_at`, j.ID, j.Status, opt(j.Outcome), j.Attempts,
		j.NextAttemptAt, nonce, int64(j.GasLimit), optUnitsArg(j.MaxFee), optUnitsArg(j.Tip), hashes, j.RawTx, j.SubmittedAt,
		opt(j.MinedTxHash), minedBlock, opt(j.MinedBlockHash), int64(j.GasUsed), opt(j.LastError), s.now(), j.Sender, lastValid,
		j.FinalityCheckAt, j.UpdatedAt).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		var exists bool
		if err := s.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM chain_tx_jobs WHERE id = $1)`, j.ID).Scan(&exists); err != nil {
			return opErr("update tx job", err)
		}
		if exists {
			return fmt.Errorf("%w: %s", ErrTxJobChanged, j.ID)
		}
		return ErrNotFound
	}
	if err != nil {
		return opErr("update tx job", err)
	}
	j.UpdatedAt = version
	return nil
}

// FlushSiblingInFlight reports whether j is the flush of a session on a customer address and
// another flush job of the same deposit account and asset (another session's, or this session's
// earlier one) is submitted or mined: a flush sweeps the whole balance, so j waits for that one to
// finish and then sees what is left.
func (s *Store) FlushSiblingInFlight(ctx context.Context, j TxJob) (bool, error) {
	var busy bool
	err := s.db.QueryRow(ctx, flushSiblingSQL, j.Chain, j.Address, j.Asset, j.SessionID, j.ID).Scan(&busy)
	return busy, opErr("flush sibling in flight", err)
}

// The flush jobs of one deposit account and asset. address = $2 is the match; lower(address) =
// lower($2) adds nothing to it but lets the index chain_tx_jobs_flush_address (migration 00102,
// on lower(address) for OwnFlushJob) serve these lookups (ADR-0008 §F-2).
const (
	flushSiblingSQL = `SELECT EXISTS (SELECT 1 FROM chain_tx_jobs o JOIN sessions s ON s.id = $4
		WHERE s.customer_address_id IS NOT NULL AND o.chain = $1 AND o.kind = 'flush' AND o.address = $2
			AND lower(o.address) = lower($2) AND o.asset = $3 AND o.id <> $5 AND o.status IN ('submitted', 'mined'))`
	otherSessionFlushesSQL = `SELECT mined_tx_hash, session_id FROM chain_tx_jobs WHERE chain = $1 AND kind = 'flush'
		AND address = $2 AND lower(address) = lower($2) AND asset = $3 AND session_id <> $4 AND mined_tx_hash IS NOT NULL`
)

// OtherSessionFlushes maps the transactions of the flush jobs other sessions sent from j's deposit
// account and asset (sessions on one customer address share it) to their session.
func (s *Store) OtherSessionFlushes(ctx context.Context, j TxJob) (map[string]string, error) {
	rows, err := s.db.Query(ctx, otherSessionFlushesSQL, j.Chain, j.Address, j.Asset, j.SessionID)
	if err != nil {
		return nil, opErr("other session flushes", err)
	}
	out := map[string]string{}
	for rows.Next() {
		var tx, sid string
		if err := rows.Scan(&tx, &sid); err != nil {
			rows.Close()
			return nil, opErr("other session flushes", err)
		}
		out[tx] = sid
	}
	return out, opErr("other session flushes", rows.Err())
}

// SettlementSession is the session a recorded settlement (chain, transaction, log index) belongs
// to ("" when none is recorded).
func (s *Store) SettlementSession(ctx context.Context, chainID, txHash string, logIndex uint32) (string, error) {
	var id string
	err := s.db.QueryRow(ctx, `SELECT session_id FROM settlements WHERE chain = $1 AND tx_hash = $2 AND log_index = $3`,
		chainID, txHash, int32(logIndex)).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, opErr("settlement session", err)
}

// AddTxJobDetails merges extra keys into a job's details. Details are not among the fields
// UpdateTxJob writes, so this leaves the row version (updated_at) alone: the caller still holds
// the job as read and ends it next.
func (s *Store) AddTxJobDetails(ctx context.Context, id string, extra map[string]string) error {
	b, err := json.Marshal(extra)
	if err != nil {
		return opErr("add tx job details", err)
	}
	tag, err := s.db.Exec(ctx, `UPDATE chain_tx_jobs SET details = details || $2::jsonb WHERE id = $1`, id, b)
	if err != nil {
		return opErr("add tx job details", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrNotFound
	}
	return nil
}

// ReopenSettleJobs moves a session's settled settle jobs back to submitted after the Gateway
// payment of txHash was reorged out, keeping their hashes and signed transaction: the sender
// follows them again, re-reading the receipt (the transaction is usually mined again) or
// re-broadcasting it, and hands the new block to the detector, which records the payment again
// (no rescan reads the Gateway). Only jobs that sent a transaction are reopened: the one that
// mined txHash, and one whose transaction reverted on an authorization settled first (no mined
// hash), whose receipt makes the sender look back for that settlement again. It returns the
// number reopened.
func (s *Store) ReopenSettleJobs(ctx context.Context, chainID, sessionID, txHash string) (int, error) {
	tag, err := s.db.Exec(ctx, `UPDATE chain_tx_jobs SET status = 'submitted', outcome = NULL, mined_tx_hash = NULL,
		mined_block = NULL, mined_block_hash = NULL, finality_check_at = NULL, submitted_at = $4,
		updated_at = greatest($4, updated_at + interval '1 microsecond')
		WHERE chain = $1 AND session_id = $2 AND kind = 'settle' AND status = 'done' AND outcome = 'settled'
			AND nonce IS NOT NULL AND raw_tx IS NOT NULL AND cardinality(tx_hashes) > 0
			AND (mined_tx_hash = $3 OR mined_tx_hash IS NULL)`,
		chainID, sessionID, txHash, s.now())
	if err != nil {
		return 0, opErr("reopen settle jobs", err)
	}
	return int(tag.RowsAffected()), nil
}

// maxSenderNonceSQL reads the end of the index chain_tx_jobs_sender_nonce (migration 00101).
const maxSenderNonceSQL = `SELECT max(nonce) FROM chain_tx_jobs WHERE chain = $1 AND sender = $2`

// MaxSenderNonce is the highest nonce any job of this sender used on the chain.
func (s *Store) MaxSenderNonce(ctx context.Context, chainID, sender string) (uint64, bool, error) {
	var n *int64
	if err := s.db.QueryRow(ctx, maxSenderNonceSQL, chainID, sender).Scan(&n); err != nil {
		return 0, false, opErr("max sender nonce", err)
	}
	if n == nil {
		return 0, false, nil
	}
	return uint64(*n), true, nil
}
