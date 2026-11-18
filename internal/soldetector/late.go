package soldetector

import (
	"context"
	"fmt"
	"time"

	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/scan"
	"github.com/prismaport/prismaport/internal/store"
)

// Late runs the due late checks and "Check again" requests: the token balance of each entry's
// deposit account (the same batched read as a tick, one provider in turn). A balance that rose
// triggers a signature scan of that account from its cursor; a "Check again" request scans it
// whatever the balance (a deposit someone else flushed leaves none). Whatever it finds goes
// through the normal path and becomes a late payment. more is true when the batch was full. An
// account whose session awaits its first late check (store.WatchEntry.AwaitsFirstLateCheck) and
// that was not read completely keeps next_check_at NULL, so expiry keeps waiting, and fails the
// pass once the rest is done (the pacer retries at the warm interval).
func (d *Detector) Late(ctx context.Context) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.st.Now()
	q := store.ScanQuery{Chain: d.chainID(), Family: chain.Solana, Now: now, LateFirst: d.cfg.Schedule.First(),
		LateWatch: d.cfg.WatchAfterExpiry}
	entries, err := d.st.LateEntries(ctx, q, d.cfg.LateBatch)
	if err != nil || len(entries) == 0 {
		return false, err
	}
	var deps []target
	for _, t := range d.targets(entries) {
		if t.kind == store.SolanaDeposit {
			deps = append(deps, t)
		}
	}
	rows, err := d.ensure(ctx, deps)
	if err != nil {
		return false, err
	}
	force := map[string]bool{}
	for _, t := range deps {
		force[t.address] = t.entries[0].RecheckDue(rows[t.address].RecheckedFor)
	}
	checks, err := d.balances(ctx, deps, force)
	if err != nil {
		return false, fmt.Errorf("soldetector: late check: read balances: %w", err)
	}
	unread := 0
	for _, c := range checks {
		missed, err := d.markLate(ctx, c, now)
		if err != nil {
			return false, err
		}
		if missed {
			unread++
		}
	}
	if unread > 0 {
		return false, fmt.Errorf("late check on %s: %d deposit accounts not read completely (a transaction not served "+
			"by both providers, or a stale reading); retried, their sessions' expiry waits", d.chainID(), unread)
	}
	return len(entries) == d.cfg.LateBatch, nil
}

// markLate writes the late check of an account the late pass read, and clears the "Check again"
// request a complete read answered. unread is true for an account whose session awaits its first
// late check and that was not read completely: nothing is written for it.
func (d *Detector) markLate(ctx context.Context, c balanceCheck, now time.Time) (unread bool, err error) {
	e := c.target.entries[0]
	chk := store.SolanaCheck{SessionID: e.SessionID, Next: scan.NextCheck(d.cfg.Schedule, d.cfg.WatchAfterExpiry, e.ExpiresAt, now)}
	recheck := e.RecheckDue(c.row.RecheckedFor)
	var next *time.Time // the session's own next_check_at (the row may still be an earlier session's)
	if c.row.SessionID == e.SessionID {
		next = c.row.NextCheckAt
	}
	switch {
	case (!c.complete || c.stale) && e.AwaitsFirstLateCheck(next):
		// Expiry waits for this account's first late check (ADR-0008 H-3): writing
		// next_check_at now would end the hold before what the account holds is recorded. It
		// stays due, and the pass fails so the pacer retries it at the warm interval.
		return true, nil
	case !c.complete:
		// A transaction is not served by both providers yet: check again soon.
		retry := now.Add(time.Minute)
		chk.Next = &retry
	case recheck:
		chk.RecheckedFor = e.RecheckRequestedAt
	}
	if err := d.st.MarkSolanaChecked(ctx, d.chainID(), c.target.address, chk); err != nil {
		return false, err
	}
	if recheck && c.complete {
		if _, err := d.st.ClearRecheck(ctx, e.SessionID, *e.RecheckRequestedAt); err != nil {
			return false, err
		}
	}
	return false, nil
}

// RescanAccount lists the signatures of a deposit account through the agreed path and records
// what they hold. The sender calls it when our flush moved more than was credited: a deposit
// reached the account between two balance reads and left with the flush, which no balance read
// shows. The account is read for the session holding its watch row; on a customer address, when
// the address's current session does not hold the row yet, it is left to that session's next
// balance read, which takes the row over and reads the account from its cursor anyway (as on EVM,
// what nobody saw belongs to the current session). It returns the number of payments recorded.
func (d *Detector) RescanAccount(ctx context.Context, account string) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.st.SolanaDepositRows(ctx, d.chainID(), []string{account})
	if err != nil || len(rows) == 0 {
		return 0, err
	}
	row := rows[0]
	entries, err := d.st.SessionWatchEntries(ctx, d.chainID(), row.SessionID)
	if err != nil || len(entries) == 0 {
		return 0, err
	}
	if id := entries[0].CustomerAddressID; id != nil {
		cur, err := d.st.CurrentAddressSession(ctx, *id)
		if err != nil || cur != row.SessionID {
			return 0, err
		}
	}
	found := 0
	for _, tg := range d.targets(entries) {
		if tg.kind != store.SolanaDeposit || tg.address != account {
			continue
		}
		n, _, err := d.scan(ctx, tg, row, 0)
		found += n
		d.log.Warn("our flush moved more than was credited; deposit account rescanned", "chain", d.chainID(),
			"session", row.SessionID, "account", account, "found", n)
		if err != nil {
			return found, err
		}
	}
	return found, nil
}
