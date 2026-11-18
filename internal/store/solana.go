package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/solanaderive"
)

// TokenAccount is the deposit token account of a Solana forwarder entry: the PDA's classic
// SPL Token ATA for the entry's mint. Empty for other entries.
func (p PayToRecord) TokenAccount() string {
	if p.Program == "" {
		return ""
	}
	owner, err1 := solanaderive.Parse(p.Address)
	mint, err2 := solanaderive.Parse(p.Token)
	if err1 != nil || err2 != nil {
		return ""
	}
	ata, err := solanaderive.AssociatedTokenAddress(owner, mint)
	if err != nil {
		return ""
	}
	return ata.String()
}

// Solana watch kinds.
const (
	SolanaDeposit   = "deposit"
	SolanaReference = "reference"
)

// SolanaWatch is the detector's progress on one Solana account: a deposit token account of a
// session's PDA, or the session's connected-wallet reference key.
type SolanaWatch struct {
	Chain     string
	Address   string
	Kind      string
	SessionID string
	// Asset is set for deposit rows.
	Asset           string
	CursorSignature string
	CursorSlot      uint64
	// Balance-check state of deposit rows: the token balance seen last, when (moved by a
	// settlement too), the next late check and the "Check again" request last served.
	LastBalance *money.Units
	// BalanceSlot is the slot LastBalance was read at (or the slot our flush emptied the account
	// at); nil when unknown. A reading from an older slot says nothing new.
	BalanceSlot  *uint64
	CheckedAt    *time.Time
	NextCheckAt  *time.Time
	RecheckedFor *time.Time
	// Created is set by EnsureSolanaWatch on a row it has just inserted.
	Created bool
}

// SolanaWalletWindow is how long a wallet transaction built for a Solana session can still land
// (its blockhash lives about 150 blocks). The checkout builds none for a session that expires
// sooner, and the session's reference key is scanned only inside WalletScanEnd.
const SolanaWalletWindow = 120 * time.Second

// WalletScanEnd is when scanning a session's Solana reference key stops (D49): two wallet windows
// after the last wallet transaction was built or submitted, and never later than one window after
// the session expires. ok is false when no wallet transaction was ever built.
func (e WatchEntry) WalletScanEnd(window time.Duration) (end time.Time, ok bool) {
	if e.WalletTxAt == nil {
		return time.Time{}, false
	}
	end = e.WalletTxAt.Add(2 * window)
	if exp := e.ExpiresAt.Add(window); exp.Before(end) {
		end = exp
	}
	return end, true
}

// walletScanCond is true while a session's reference key is scanned ($w is the wallet window in
// microseconds, $now the time).
func walletScanCond(now, w string) string {
	return `(s.wallet_tx_at IS NOT NULL AND ` + now + `::timestamptz < least(s.wallet_tx_at + 2 * ` + w + `::bigint * interval '1 microsecond',
		s.expires_at + ` + w + `::bigint * interval '1 microsecond'))`
}

// SolanaWatchSet lists the Solana pay_to entries of a chain whose session expires after now, or
// whose reference key is still scanned after a wallet transaction (WalletScanEnd with window).
// Only entries with a committed program (real forwarder addresses) on a revealed network (D48)
// of a session that is not settled (D47) are watched, and of a customer address only its current
// session's.
func (s *Store) SolanaWatchSet(ctx context.Context, chainID string, now time.Time, window time.Duration) ([]WatchEntry, error) {
	rows, err := s.db.Query(ctx, `SELECT `+watchColumns+` `+watchFrom+`
		WHERE p.chain = $1 AND (s.expires_at > $2 OR `+walletScanCond("$2", "$3")+`) AND p.program IS NOT NULL`+revealedCond+ownerCond+`
			AND NOT `+settledExpr+`
		ORDER BY s.id, p.position`, chainID, now, micros(window))
	if err != nil {
		return nil, opErr("solana watch set", err)
	}
	out, err := pgx.CollectRows(rows, scanWatch)
	return out, opErr("solana watch set", err)
}

func scanSolanaWatch(r pgx.CollectableRow) (SolanaWatch, error) {
	var w SolanaWatch
	var asset, sig, balance *string
	var slot, balanceSlot *int64
	if err := r.Scan(&w.Chain, &w.Address, &w.Kind, &w.SessionID, &asset, &sig, &slot, &balance, &w.CheckedAt, &w.NextCheckAt,
		&w.RecheckedFor, &balanceSlot); err != nil {
		return w, err
	}
	if balanceSlot != nil {
		b := uint64(*balanceSlot)
		w.BalanceSlot = &b
	}
	if asset != nil {
		w.Asset = *asset
	}
	if sig != nil && slot != nil {
		w.CursorSignature, w.CursorSlot = *sig, uint64(*slot)
	}
	var err error
	w.LastBalance, err = parseOptUnits("solana_watch.last_balance", balance)
	return w, err
}

// EnsureSolanaWatch creates missing rows (with no cursor) and returns every row of addrs, the ones
// it created marked Created. An existing row keeps its session: on a customer address it may be an
// earlier session's until TakeSolanaWatch hands it over.
func (s *Store) EnsureSolanaWatch(ctx context.Context, rows []SolanaWatch) (map[string]SolanaWatch, error) {
	out := map[string]SolanaWatch{}
	if len(rows) == 0 {
		return out, nil
	}
	chainID := rows[0].Chain
	var addrs, kinds, sessions []string
	var assets []*string
	for _, r := range rows {
		addrs = append(addrs, r.Address)
		kinds = append(kinds, r.Kind)
		sessions = append(sessions, r.SessionID)
		assets = append(assets, optString(r.Asset))
	}
	ins, err := s.db.Query(ctx, `INSERT INTO solana_watch (chain, address, kind, session_id, asset)
		SELECT $1, a, k, sid, asset FROM unnest($2::text[], $3::text[], $4::text[], $5::text[]) AS t(a, k, sid, asset)
		ON CONFLICT (chain, address) DO NOTHING RETURNING address`, chainID, addrs, kinds, sessions, assets)
	if err != nil {
		return nil, opErr("ensure solana watch", err)
	}
	created, err := pgx.CollectRows(ins, pgx.RowTo[string])
	if err != nil {
		return nil, opErr("ensure solana watch", err)
	}
	q, err := s.db.Query(ctx, `SELECT chain, address, kind, session_id, asset, cursor_signature, cursor_slot, last_balance::text,
		checked_at, next_check_at, rechecked_for, balance_slot FROM solana_watch WHERE chain = $1 AND address = ANY($2)`, chainID, addrs)
	if err != nil {
		return nil, opErr("ensure solana watch", err)
	}
	list, err := pgx.CollectRows(q, scanSolanaWatch)
	if err != nil {
		return nil, opErr("ensure solana watch", err)
	}
	for _, w := range list {
		out[w.Address] = w
	}
	for _, a := range created {
		if w, ok := out[a]; ok {
			w.Created = true
			out[a] = w
		}
	}
	return out, nil
}

// TakeSolanaWatch hands the watch rows of a customer address's accounts to the address's current
// session once it has read their history from the cursors the previous owner left (the cursors
// stay: they are where the previous watch stopped). The late-check state is cleared (the new
// owner's schedule and "Check again" requests are its own; the balance seen is forgotten) and
// checked_at moves, so a check that read a row before does not write its balance back. Only the
// current session takes the rows.
func (s *Store) TakeSolanaWatch(ctx context.Context, chainID, sessionID string, addrs []string) error {
	_, err := s.db.Exec(ctx, `UPDATE solana_watch SET session_id = $3, last_balance = NULL, balance_slot = NULL, checked_at = $4, next_check_at = NULL,
			rechecked_for = NULL
		WHERE chain = $1 AND address = ANY($2) AND session_id <> $3 AND EXISTS (SELECT 1 FROM sessions s
			JOIN customer_addresses ca ON ca.id = s.customer_address_id WHERE s.id = $3 AND ca.current_session_id = s.id)`,
		chainID, addrs, sessionID, s.now())
	return opErr("take solana watch", err)
}

// SetSolanaCursor records that every transaction of address up to and including sig (at slot)
// has been attributed.
func (s *Store) SetSolanaCursor(ctx context.Context, chainID, address, sig string, slot uint64) error {
	_, err := s.db.Exec(ctx, `UPDATE solana_watch SET cursor_signature = $3, cursor_slot = $4 WHERE chain = $1 AND address = $2`,
		chainID, address, sig, int64(slot))
	return opErr("set solana cursor", err)
}

// ResetSolanaCursors makes the next scan of a session's accounts start from their full history.
// The balance seen is forgotten and a late check made due, so a balance-triggered scan (warm or
// late) runs again too. A session on a customer address shares its accounts with the address's
// other sessions, so the rows any of them holds are reset.
func (s *Store) ResetSolanaCursors(ctx context.Context, chainID, sessionID string) error {
	_, err := s.db.Exec(ctx, `UPDATE solana_watch SET cursor_signature = NULL, cursor_slot = NULL, last_balance = NULL,
		balance_slot = NULL, next_check_at = NULL WHERE chain = $1 AND (session_id = $2 OR session_id IN (SELECT o.id FROM sessions o
			JOIN sessions x ON x.customer_address_id = o.customer_address_id WHERE x.id = $2))`,
		chainID, sessionID)
	return opErr("reset solana cursors", err)
}

// SolanaCheck is what one balance check of a Solana deposit account wrote.
type SolanaCheck struct {
	// SessionID is the entry's session: only the row's owner writes it (a customer address's
	// rows may have passed to a newer session since they were read).
	SessionID string
	// Balance, read at Slot, is stored as the last balance unless a settlement moved checked_at
	// since the row was read (Prev) or the balance stored is from a later slot; nil leaves the
	// stored balance alone.
	Balance *money.Units
	Slot    uint64
	Prev    *time.Time
	// Next is the next late check (nil leaves it). RecheckedFor marks a "Check again" request
	// served (nil leaves it).
	Next         *time.Time
	RecheckedFor *time.Time
}

// MarkSolanaChecked records a balance check of a deposit account by its owner.
func (s *Store) MarkSolanaChecked(ctx context.Context, chainID, address string, c SolanaCheck) error {
	var bal *string
	if c.Balance != nil {
		v := c.Balance.String()
		bal = &v
	}
	const write = `$3::text IS NOT NULL AND checked_at IS NOT DISTINCT FROM $4 AND coalesce(balance_slot, -1) <= $9::bigint`
	_, err := s.db.Exec(ctx, `UPDATE solana_watch SET
			last_balance = CASE WHEN `+write+` THEN $3::numeric ELSE last_balance END,
			balance_slot = CASE WHEN `+write+` THEN $9::bigint ELSE balance_slot END,
			checked_at = CASE WHEN `+write+` THEN $5 ELSE checked_at END,
			next_check_at = coalesce($6, next_check_at), rechecked_for = coalesce($7, rechecked_for)
		WHERE chain = $1 AND address = $2 AND session_id = $8`, chainID, address, bal, c.Prev, s.now(), c.Next, c.RecheckedFor, c.SessionID,
		int64(c.Slot))
	return opErr("mark solana checked", err)
}

// SolanaFlushLanded records that our flush emptied (and usually closed) the deposit token account
// at slot: its stored balance becomes 0, stamped with slot, so the next deposit reads as a rise
// whatever its amount (one equal to or below the balance credited before the flush would
// otherwise look like nothing new). A balance stored from slot or later is kept: it was read
// after the flush. The row is the account's, whoever holds it (customer addresses share it).
func (s *Store) SolanaFlushLanded(ctx context.Context, chainID, address string, slot uint64) error {
	_, err := s.db.Exec(ctx, `UPDATE solana_watch SET last_balance = 0, balance_slot = $3
		WHERE chain = $1 AND address = $2 AND kind = 'deposit' AND coalesce(balance_slot, -1) < $3::bigint`, chainID, address, int64(slot))
	return opErr("solana flush landed", err)
}

// SolanaDepositRows returns the deposit watch rows among addrs (none is created).
func (s *Store) SolanaDepositRows(ctx context.Context, chainID string, addrs []string) ([]SolanaWatch, error) {
	q, err := s.db.Query(ctx, `SELECT chain, address, kind, session_id, asset, cursor_signature, cursor_slot, last_balance::text,
		checked_at, next_check_at, rechecked_for, balance_slot FROM solana_watch WHERE chain = $1 AND kind = 'deposit' AND address = ANY($2)
		ORDER BY address`, chainID, addrs)
	if err != nil {
		return nil, opErr("solana deposit rows", err)
	}
	out, err := pgx.CollectRows(q, scanSolanaWatch)
	return out, opErr("solana deposit rows", err)
}

// ownTxHashesSQL lists which of $2 a job of chain $1 sent. tx_hashes && $2 keeps only the jobs
// holding one of them (the GIN index chain_tx_jobs_tx_hashes, migration 00103) before their
// hashes are unnested; h = ANY($2) then picks the matching ones, as before (ADR-0008 §F-2).
const ownTxHashesSQL = `SELECT DISTINCT h FROM chain_tx_jobs j, unnest(j.tx_hashes) h
	WHERE j.chain = $1 AND j.tx_hashes && $2::text[] AND h = ANY($2::text[])`

// OwnTxHashes reports which of hashes a transaction job of the chain sent (any attempt).
func (s *Store) OwnTxHashes(ctx context.Context, chainID string, hashes []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(hashes) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(ctx, ownTxHashesSQL, chainID, hashes)
	if err != nil {
		return nil, opErr("own tx hashes", err)
	}
	list, err := pgx.CollectRows(rows, pgx.RowTo[string])
	for _, h := range list {
		out[h] = true
	}
	return out, opErr("own tx hashes", err)
}
