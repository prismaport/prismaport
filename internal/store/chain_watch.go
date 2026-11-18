package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/fee"
	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/payout"
	"github.com/prismaport/prismaport/internal/session"
)

// WatchEntry is one pay_to address the detector watches, with everything needed to attribute
// a payment to it and to flush it.
type WatchEntry struct {
	SessionID string
	// CustomerAddressID is set on a session opened on a customer address: other sessions on the
	// address share its deposit addresses.
	CustomerAddressID *string
	Livemode          bool
	Status            session.Status
	ExpiresAt         time.Time
	Salt              [32]byte
	Chain             string
	Asset             string
	Token             string
	Decimals          uint8
	Address           string
	Family            chain.Family
	// Recipients are the session's payout snapshot for the entry's family, in committed order.
	Recipients []payout.Recipient
	Treasury   string
	Fee        fee.Params
	// Program and RentReceiver are set for Solana entries.
	Program      string
	RentReceiver string
	// Tokens are the tokens the forwarder binds (session_pay_to.bound_tokens, D41), in canonical
	// order: the derivation input, not just this entry's asset.
	Tokens     []string
	FirstBlock *uint64
	CreatedAt  time.Time
	// RevealedAt is when the entry's network was revealed (D48); nil for an unrevealed one.
	RevealedAt *time.Time
	// WalletTxAt is when a wallet transaction was last built or submitted for the session (D49:
	// the Solana reference key is scanned only around it).
	WalletTxAt *time.Time
	// CheckoutSeenAt is when the payer last had the checkout page open; RecheckRequestedAt is a
	// pending dashboard "Check again" request.
	CheckoutSeenAt     *time.Time
	RecheckRequestedAt *time.Time
	// Check state of the EVM watch row (chain_watch); nil when the row or value is missing.
	// LastBalance is the entry's token balance the last check kept (D49: per token, every asset
	// shares the address). Solana entries keep theirs in SolanaWatch.
	CheckedBlock *uint64
	LastBalance  *money.Units
	// BalanceBlock is the block LastBalance was read at (or the block of the settlement that
	// emptied the token); nil when unknown. A reading from an older block says nothing new.
	BalanceBlock *uint64
	// ForwarderSeenAt is when a tick first saw the address's forwarder deployed (EVM, D49): a
	// deployment we did not make (Factory.deployAndFlush is permissionless) is someone else's
	// flush, which a balance read can miss.
	ForwarderSeenAt *time.Time
	CheckedAt       *time.Time
	NextCheckAt     *time.Time
	RecheckedFor    *time.Time
}

// RecheckDue reports whether the entry waits for a "Check again" request it was not checked for.
func (e WatchEntry) RecheckDue(rechecked *time.Time) bool {
	return e.RecheckRequestedAt != nil && (rechecked == nil || !rechecked.Equal(*e.RecheckRequestedAt))
}

// AwaitsFirstLateCheck reports whether expiry still waits for this entry's first late check
// (SessionsDueForExpiry, ADR-0008 H-3): next, the session's next_check_at on the entry's watch
// row, was never written and the session can still expire. Such a check writes next_check_at only
// once it read the address in full; a read that failed, was stale or is incomplete leaves it NULL
// so the session stays held.
func (e WatchEntry) AwaitsFirstLateCheck(next *time.Time) bool {
	return next == nil && (e.Status == session.Open || e.Status == session.PartiallyPaid)
}

const watchColumns = `s.id, s.livemode, s.status, s.expires_at, s.salt, p.chain, p.asset, p.token, p.decimals, p.address,
	p.family, coalesce((SELECT array_agg(sp.address ORDER BY sp.position) FROM session_payouts sp
		WHERE sp.session_id = p.session_id AND sp.chain_id = p.chain), '{}'),
	coalesce((SELECT array_agg(sp.bps ORDER BY sp.position) FROM session_payouts sp
		WHERE sp.session_id = p.session_id AND sp.chain_id = p.chain), '{}'),
	p.treasury, p.fee_bps, p.fixed_fee_micros, coalesce(p.program, ''), coalesce(p.rent_receiver, ''), p.bound_tokens,
	w.first_block, s.created_at, s.checkout_seen_at, s.recheck_requested_at, w.checked_block, w.token_balances ->> lower(p.token),
	w.checked_at, w.next_check_at, w.rechecked_for,
	(SELECT r.revealed_at FROM session_reveals r WHERE r.session_id = p.session_id AND r.chain = p.chain), s.wallet_tx_at, w.forwarder_seen_at,
	s.customer_address_id, (w.token_blocks ->> lower(p.token))::bigint`

// watchFrom joins each entry's EVM watch row. Sessions on one customer address share its deposit
// addresses and the row belongs to one of them at a time (the address's current session, once the
// detector handed it over): another session's entry joins no row.
const watchFrom = `FROM session_pay_to p JOIN sessions s ON s.id = p.session_id
	LEFT JOIN chain_watch w ON w.chain = p.chain AND w.address = lower(p.address)
		AND (s.customer_address_id IS NULL OR w.session_id = s.id)`

// ownerCond keeps the sessions that own their deposit addresses' watch: every session without a
// customer address, and only the current (latest) session of a customer address. An earlier
// session on the address is no longer scanned, late-checked or rechecked (static-addresses.md,
// "Watch ownership"); its payments already recorded still move to finality.
const ownerCond = ` AND (s.customer_address_id IS NULL OR EXISTS (SELECT 1 FROM customer_addresses ca
	WHERE ca.id = s.customer_address_id AND ca.current_session_id = s.id))`

func scanWatch(r pgx.CollectableRow) (WatchEntry, error) {
	return scanWatchRow(r)
}

// scanWatchRow reads watchColumns plus any extra columns selected after them.
func scanWatchRow(r pgx.Row, extra ...any) (WatchEntry, error) {
	var e WatchEntry
	var status, fam string
	var salt []byte
	var dec int16
	var bps int32
	var fixedFee int64
	var first, checked, balanceBlock *int64
	var balance *string
	var addrs []string
	var shares []int32
	dest := []any{&e.SessionID, &e.Livemode, &status, &e.ExpiresAt, &salt, &e.Chain, &e.Asset, &e.Token, &dec, &e.Address,
		&fam, &addrs, &shares, &e.Treasury, &bps, &fixedFee, &e.Program, &e.RentReceiver, &e.Tokens, &first, &e.CreatedAt, &e.CheckoutSeenAt,
		&e.RecheckRequestedAt, &checked, &balance, &e.CheckedAt, &e.NextCheckAt, &e.RecheckedFor, &e.RevealedAt, &e.WalletTxAt, &e.ForwarderSeenAt,
		&e.CustomerAddressID, &balanceBlock}
	if err := r.Scan(append(dest, extra...)...); err != nil {
		return e, err
	}
	e.Status, e.Family = session.Status(status), chain.Family(fam)
	copy(e.Salt[:], salt)
	e.Decimals, e.Fee = uint8(dec), fee.Params{FeeBps: uint16(bps), FixedFeeMicros: uint64(fixedFee)}
	if len(addrs) != len(shares) {
		return e, fmt.Errorf("store: session %s payout snapshot is malformed", e.SessionID)
	}
	for i := range addrs {
		e.Recipients = append(e.Recipients, payout.Recipient{Address: addrs[i], Bps: uint16(shares[i])})
	}
	if first != nil {
		f := uint64(*first)
		e.FirstBlock = &f
	}
	if checked != nil {
		c := uint64(*checked)
		e.CheckedBlock = &c
	}
	if balanceBlock != nil {
		b := uint64(*balanceBlock)
		e.BalanceBlock = &b
	}
	var err error
	e.LastBalance, err = parseOptUnits("chain_watch.token_balances", balance)
	return e, err
}

// WatchSet lists the pay_to entries of a chain whose session expires after since (the block scan
// passes now: sessions not expired yet, whatever their status), on revealed networks only (D48)
// and without settled sessions (D47). Expired sessions are reached by late checks only. A session
// on a customer address is listed only while it is the address's current session.
func (s *Store) WatchSet(ctx context.Context, chainID string, since time.Time) ([]WatchEntry, error) {
	rows, err := s.db.Query(ctx, `SELECT `+watchColumns+` `+watchFrom+`
		WHERE p.chain = $1 AND s.expires_at > $2`+revealedCond+ownerCond+` AND NOT `+settledExpr+` ORDER BY s.id, p.position`, chainID, since)
	if err != nil {
		return nil, opErr("watch set", err)
	}
	out, err := pgx.CollectRows(rows, scanWatch)
	return out, opErr("watch set", err)
}

// SessionWatchEntries lists a session's entries on a chain whatever its expiry or status (the
// signed path records a settle the sender saw mined, which may be read after expiry).
func (s *Store) SessionWatchEntries(ctx context.Context, chainID, sessionID string) ([]WatchEntry, error) {
	rows, err := s.db.Query(ctx, `SELECT `+watchColumns+` `+watchFrom+`
		WHERE p.chain = $1 AND p.session_id = $2 ORDER BY p.position`, chainID, sessionID)
	if err != nil {
		return nil, opErr("session watch entries", err)
	}
	out, err := pgx.CollectRows(rows, scanWatch)
	return out, opErr("session watch entries", err)
}

// TakeWatch gives the EVM watch row of a customer address's deposit address to the address's
// current session, reading from firstBlock: a missing row is created, a row of an earlier session
// is handed over with its check state cleared (the new owner's schedule and "Check again"
// requests are its own; checked_block is reset, so its next balance read rescans from
// firstBlock). The stored balances (per token, with the block each was read at) and the
// forwarder's deployment are the address's and are kept: the new owner's rescans must explain
// their readings from a known balance (ADR-0008 H-1). checked_at moves, so a check of the
// previous owner that read the row before does not write its balance back. Only the current
// session takes the row; it reports whether the row is the session's now.
func (s *Store) TakeWatch(ctx context.Context, e WatchEntry, firstBlock uint64) (bool, error) {
	var owner string
	err := s.db.QueryRow(ctx, `INSERT INTO chain_watch (chain, address, session_id, first_block, checked_at)
		SELECT $1, $2, $3, $4, $5 WHERE EXISTS (SELECT 1 FROM sessions s JOIN customer_addresses ca ON ca.id = s.customer_address_id
			WHERE s.id = $3 AND ca.current_session_id = s.id)
		ON CONFLICT (chain, address) DO UPDATE SET session_id = EXCLUDED.session_id, first_block = EXCLUDED.first_block,
			checked_block = NULL, checked_at = EXCLUDED.checked_at, next_check_at = NULL, rechecked_for = NULL
			WHERE chain_watch.session_id <> EXCLUDED.session_id
		RETURNING session_id`, e.Chain, strings.ToLower(e.Address), e.SessionID, int64(firstBlock), s.now()).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) {
		// Not the current session, or the row is its own already.
		err = s.db.QueryRow(ctx, `SELECT session_id FROM chain_watch WHERE chain = $1 AND address = $2`,
			e.Chain, strings.ToLower(e.Address)).Scan(&owner)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
	}
	return owner == e.SessionID, opErr("take watch", err)
}

// WatchCheck reads an entry's EVM watch row as it stands: its checked_at, and the entry's token
// balance with the block it was read at (each nil when unset or the row is missing).
func (s *Store) WatchCheck(ctx context.Context, e WatchEntry) (checkedAt *time.Time, balance *money.Units, block *uint64, err error) {
	var bal *string
	var b *int64
	err = s.db.QueryRow(ctx, `SELECT checked_at, token_balances ->> $3, (token_blocks ->> $3)::bigint FROM chain_watch
		WHERE chain = $1 AND address = $2`, e.Chain, strings.ToLower(e.Address), strings.ToLower(e.Token)).Scan(&checkedAt, &bal, &b)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, nil, nil
	}
	if err != nil {
		return nil, nil, nil, opErr("watch check", err)
	}
	if b != nil {
		v := uint64(*b)
		block = &v
	}
	balance, err = parseOptUnits("chain_watch.token_balances", bal)
	return checkedAt, balance, block, opErr("watch check", err)
}
