package detector

import (
	"context"
	"strings"
	"time"

	"github.com/prismaport/prismaport/internal/alert"
	"github.com/prismaport/prismaport/internal/evm"
	"github.com/prismaport/prismaport/internal/scan"
	"github.com/prismaport/prismaport/internal/store"
)

// rescanItem is one changed entry of a rescan: its reading, the block its rescan starts at, its
// balance window, how far the rescan covered it and what reconcile decided for its reading.
type rescanItem struct {
	reading
	start   uint64 // the first block the entry's rescan needs
	win     uint64 // the first block of its balance window (balanceWindow)
	known   bool   // the window starts at a known baseline
	covered uint64 // how far its rescan got
	commit  bool   // its reading is stored (reconcile)
	escaped bool   // its reading is stored unexplained after ReconcileWait (reconcile)
}

// stored reports whether the rescan reached the block the item's reading came from with an
// amount that can be stored; any other reading leaves the existing rules to apply.
func (it *rescanItem) stored() bool {
	return it.ok && !it.stale && it.covered >= it.at
}

// rescanPlan is what a rescan reads: its items, the cursors of their customer addresses, the
// watch set of every entry on their addresses and the earliest block any item needs.
type rescanPlan struct {
	items   []rescanItem
	cursors map[string]store.AddressCursor
	ws      watchSet
	from    uint64
}

// rescanEntries rescans the changed entries' addresses together, from the earliest block any of
// them still needs to the agreed head (the lower of both providers' heads, so both serve every
// block asked for; never a lower block a reading came from), then records how far each got. The
// logs of every entry on those addresses are recorded, not only the changed ones: checked_block
// is the address's, so a transfer of another asset in the range would otherwise be skipped for
// good. A balance is stored only when the rescan reached the block it was read at. It returns
// the addresses it held (a reading their logs do not explain yet): they wrote no check, late
// schedule or "Check again" either.
//
// It runs in phases over one rescanItem per entry: planRescan, the rescan itself, reconcile and
// applyChecks.
func (d *Detector) rescanEntries(ctx context.Context, changed, all []reading, now time.Time, late bool) (int, map[string]bool, error) {
	latest, err := d.q.AgreedLatest(ctx)
	if err != nil {
		return 0, nil, err
	}
	to := latest
	p, err := d.planRescan(ctx, changed, all, latest)
	if err != nil {
		return 0, nil, err
	}
	found := 0
	if p.from <= to {
		if found, err = d.rescan(ctx, p.ws, p.from, to); err != nil {
			return found, nil, err
		}
	}
	// Every block from the entry's start to `to` was scanned; an entry already past `to` keeps
	// its own mark.
	for i := range p.items {
		it := &p.items[i]
		it.covered = max(to, it.start-min(it.start, 1))
	}
	hold, more, err := d.reconcile(ctx, p.ws, p.items, p.from, latest, now)
	found += more
	if err != nil {
		return found, nil, err
	}
	if err := d.applyChecks(ctx, p, hold, latest, now, late); err != nil {
		return found, nil, err
	}
	d.log.Info("detector rescanned", "chain", d.chainID(), "addresses", len(p.ws.addrs), "from", p.from, "to", to, "found", found, "late", late)
	return found, hold, nil
}

// planRescan makes one item per changed reading, adds the other entries of a rescanned customer
// address that have no stored balance yet, sets each item's start and balance window (placeItem)
// and builds the watch set of every entry on the rescanned addresses.
func (d *Detector) planRescan(ctx context.Context, changed, all []reading, latest uint64) (rescanPlan, error) {
	var p rescanPlan
	var es []store.WatchEntry
	addrs := map[string]bool{}
	inChanged := map[[2]string]bool{}
	p.items = make([]rescanItem, 0, len(changed))
	for _, r := range changed {
		addrs[strings.ToLower(r.e.Address)] = true
		inChanged[[2]string{r.e.SessionID, strings.ToLower(r.e.Token)}] = true
		p.items = append(p.items, rescanItem{reading: r})
	}
	// The other entries of a rescanned customer address that have no stored balance yet are
	// reconciled with the changed ones (usually a zero, explained at once), so every watched
	// token gets a stored balance: until each has one (a reading that failed counts as none), the
	// address's cursor stays unset and the baseline stays the zero before its creation (H-1).
	for _, r := range all {
		k := [2]string{r.e.SessionID, strings.ToLower(r.e.Token)}
		if r.e.CustomerAddressID != nil && r.e.FirstBlock != nil && r.e.LastBalance == nil && r.asked &&
			addrs[strings.ToLower(r.e.Address)] && !inChanged[k] {
			p.items = append(p.items, rescanItem{reading: r})
			inChanged[k] = true
		}
	}
	var err error
	if p.cursors, err = d.addressCursors(ctx, p.items); err != nil {
		return p, err
	}
	for _, r := range all {
		if r.e.CustomerAddressID != nil && r.e.FirstBlock == nil {
			continue // not the owner of the address's watch row
		}
		if addrs[strings.ToLower(r.e.Address)] && !inChanged[[2]string{r.e.SessionID, strings.ToLower(r.e.Token)}] {
			es = append(es, r.e)
		}
	}
	settled := map[evm.Address]bool{}
	for i := range p.items {
		it := &p.items[i]
		d.placeItem(it, latest, p.cursors)
		if i == 0 || it.start < p.from {
			p.from = it.start
		}
		es = append(es, it.e)
		// Only a deployed forwarder can move funds out: its flushes are read with the transfers.
		if a, err := evm.ParseAddressLoose(it.e.Address); err == nil && (it.deployed || it.e.ForwarderSeenAt != nil) && !settled[a] {
			settled[a] = true
		}
	}
	p.ws = d.buildWatch(es)
	// No signed-path payment is found by a rescan: the Gateway path is followed from its settle
	// job, and none can land after expiry (valid_before <= expires_at).
	p.ws.byNonce = map[evm.Word]store.WatchEntry{}
	p.ws.flows = flows{}
	for a := range settled {
		p.ws.settledFrom = append(p.ws.settledFrom, a)
	}
	return p, nil
}

// placeItem sets the block an item's rescan starts at and its balance window.
func (d *Detector) placeItem(it *rescanItem, latest uint64, cursors map[string]store.AddressCursor) {
	it.start = d.firstBlock(latest, it.e)
	if it.e.CheckedBlock != nil {
		it.start = *it.e.CheckedBlock + 1
	}
	unread := false
	if id := it.e.CustomerAddressID; id != nil {
		// Never past the address's cursor: the history below it was read (by this or an earlier
		// session), above it the rescan must cover every block, so the cursor can follow.
		it.start = min(it.start, d.readStart(latest, cursors[*id]))
		unread = cursors[*id].Block == nil
	}
	// Never past the block after the stored balance either: the movements since it must
	// explain the new reading (reconcile.go), even where checked_block moved further. A zero
	// stored balance also needs its own block: deposits there after a flush count.
	it.win, it.known = d.balanceWindow(latest, it.e, it.start, unread)
	it.start = min(it.start, it.win)
	if bb := it.e.BalanceBlock; it.known && bb != nil && it.win == *bb+1 && prevBalance(it.e).IsZero() {
		it.start = min(it.start, *bb)
	}
	if !it.known {
		d.log.Warn("no known baseline for the token; its reading is checked against the window's movements only",
			"chain", d.chainID(), "session", it.e.SessionID, "address", it.e.Address, "token", it.e.Token, "from", it.start)
	}
}

// rewinds is, per address with a reading stored unexplained after ReconcileWait, the block
// before the earliest start of such a reading's rescan. That reading keeps its address's
// checked_block (shared by every asset) and cursor before the rescan's start, which is no later
// than its window's: a later rescan ("Check again", the sender's RescanSession, the next rise's)
// then reads the blocks of the deposit whose log lagged. The stored balance ends the rescans of
// every tick.
func rewinds(items []rescanItem) map[string]uint64 {
	rewind := map[string]uint64{}
	for _, it := range items {
		if !it.escaped {
			continue
		}
		a, b := strings.ToLower(it.e.Address), it.start-min(it.start, 1)
		if v, ok := rewind[a]; !ok || b < v {
			rewind[a] = b
		}
	}
	return rewind
}

// unreadAddresses is the customer addresses never read before whose cursor stays unset: one of
// their tokens has no stored balance (a reading not covered by the rescan, say), and with a
// cursor and no stored balance the token would have no known baseline. Not for good: a token
// whose balance cannot be stored for ReconcileWait (its balanceOf answers no amount) lets the
// cursor move with an alert, or every rescan of the address would keep starting at its creation.
// A token this detector does not read (not configured) never holds it.
func (d *Detector) unreadAddresses(ctx context.Context, items []rescanItem, cursors map[string]store.AddressCursor, now time.Time) map[string]bool {
	unread := map[string]bool{}
	for _, it := range items {
		id := it.e.CustomerAddressID
		if id == nil || cursors[*id].Block != nil || it.e.LastBalance != nil || !it.asked {
			continue
		}
		key := "cursor|" + unexplainedKey(it.e)
		if it.commit {
			d.unexplained.clearKey(key)
			continue
		}
		if since := d.unexplained.markKey(key, now, d.cfg.ReconcileWait); now.Sub(since) < d.cfg.ReconcileWait {
			unread[strings.ToLower(it.e.Address)] = true
			continue
		}
		d.unexplained.clearKey(key)
		d.alerts.Alert(ctx, alert.Alert{Severity: alert.Warning, Chain: d.chainID(), Kind: "balance_unexplained",
			Message: "no balance of the token could be stored on a new customer address; its cursor moves without that token's baseline",
			Fields:  map[string]string{"session": it.e.SessionID, "address": it.e.Address, "token": it.e.Token}})
	}
	return unread
}

// applyChecks writes, item by item, how far each rescan got (its check, its stored balance, its
// late schedule), moves its customer address's cursor and clears the "Check again" request it
// answered. The addresses in hold keep their check state.
func (d *Detector) applyChecks(ctx context.Context, p rescanPlan, hold map[string]bool, latest uint64, now time.Time, late bool) error {
	rewind := rewinds(p.items)
	unread := d.unreadAddresses(ctx, p.items, p.cursors, now)
	for _, it := range p.items {
		e := it.e
		chk := store.EVMCheck{FirstBlock: d.firstBlock(latest, e)}
		// An address with a reading its logs do not explain yet keeps its check state, so the
		// next tick rescans the same blocks; the readings that are explained are stored. One
		// with a reading stored unexplained moves no further than the start of its rescan (held
		// or not: only back).
		held := hold[strings.ToLower(e.Address)]
		reached := it.covered
		back, rewound := rewind[strings.ToLower(e.Address)]
		if rewound {
			reached = min(reached, back)
		}
		if !held || rewound {
			chk.Block = &reached
		}
		if it.commit {
			bal := it.bal
			chk.Balance, chk.BalanceBlock = &bal, it.at
		}
		// The deployment is handled once the rescan reached the block that showed it.
		chk.ForwarderSeen = !held && it.deployed && it.covered >= it.at
		recheck := !held && late && e.RecheckDue(e.RecheckedFor)
		if late && !held {
			chk.Next = scan.NextCheck(d.cfg.Schedule, d.cfg.WatchAfterExpiry, e.ExpiresAt, now)
			if recheck {
				chk.RecheckedFor = e.RecheckRequestedAt
			}
		}
		if err := d.st.MarkChecked(ctx, e, chk); err != nil {
			return err
		}
		if e.CustomerAddressID != nil && !held && !unread[strings.ToLower(e.Address)] && it.covered >= it.start {
			// Moves only while the session is still the address's current one.
			if err := d.st.SetAddressCursor(ctx, d.chainID(), *e.CustomerAddressID, e.SessionID, reached); err != nil {
				return err
			}
		}
		if recheck {
			if _, err := d.st.ClearRecheck(ctx, e.SessionID, *e.RecheckRequestedAt); err != nil {
				return err
			}
		}
	}
	return nil
}

// rescan scans [from, to] for transfers into the watch set's addresses (and, when it has nonces,
// the Gateway's events), in MaxRange chunks.
func (d *Detector) rescan(ctx context.Context, ws watchSet, from, to uint64) (int, error) {
	return d.scanRange(ctx, ws, from, to, map[uint64]evm.Word{})
}

// RescanSession rescans a session's deposit address up to block (bounded by the agreed head) from
// the block the last rescan reached. The sender calls it when a flush moved more than was
// credited: something reached the address between two balance reads and left with the flush.
//
// The sessions of a customer address share its forwarder, so their flushes move each other's
// funds: the address is rescanned for its current session (what nobody saw belongs to it,
// static-addresses.md), from no later than the address's cursor, which then follows. A current
// session that does not hold the watch row yet is left to its next balance read, which takes the
// row over and reads from the cursor anyway.
func (d *Detector) RescanSession(ctx context.Context, sessionID string, block uint64) (int, error) {
	entries, err := d.st.SessionWatchEntries(ctx, d.chainID(), sessionID)
	if err != nil || len(entries) == 0 {
		return 0, err
	}
	var addressID *string
	if id := entries[0].CustomerAddressID; id != nil {
		addressID = id
		cur, err := d.st.CurrentAddressSession(ctx, *id)
		if err != nil || cur == "" {
			return 0, err
		}
		if cur != sessionID {
			sessionID = cur
			if entries, err = d.st.SessionWatchEntries(ctx, d.chainID(), cur); err != nil || len(entries) == 0 {
				return 0, err
			}
		}
		for _, e := range entries {
			if e.FirstBlock == nil {
				return 0, nil
			}
		}
	}
	latest, err := d.q.AgreedLatest(ctx)
	if err != nil {
		return 0, err
	}
	to := min(block, latest)
	ws := d.buildWatch(entries)
	ws.byNonce = map[evm.Word]store.WatchEntry{}
	from := to
	for _, e := range entries {
		s := d.firstBlock(to, e)
		if e.CheckedBlock != nil {
			s = *e.CheckedBlock + 1
		}
		from = min(from, s)
	}
	// An unset cursor stays unset while a token has no stored balance (see rescanEntries).
	moveCursor := addressID != nil
	if addressID != nil {
		cursors, err := d.st.AddressCursors(ctx, d.chainID(), []string{*addressID})
		if err != nil {
			return 0, err
		}
		from = min(from, d.readStart(to, cursors[*addressID]))
		for _, e := range entries {
			moveCursor = moveCursor && (cursors[*addressID].Block != nil || e.LastBalance != nil)
		}
	}
	n, err := d.rescan(ctx, ws, from, to)
	d.log.Info("detector rescanned a flushed address", "chain", d.chainID(), "session", sessionID, "from", from, "to", to, "found", n)
	if err == nil && moveCursor && from <= to {
		err = d.st.SetAddressCursor(ctx, d.chainID(), *addressID, sessionID, to)
	}
	return n, err
}
