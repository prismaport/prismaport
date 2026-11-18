package detector

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/prismaport/prismaport/internal/contracts"
	"github.com/prismaport/prismaport/internal/evm"
	"github.com/prismaport/prismaport/internal/evmrpc"
	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/scan"
	"github.com/prismaport/prismaport/internal/store"
)

// reading is one entry's balance from an aggregate.
type reading struct {
	e store.WatchEntry
	// asked is set when a balanceOf was asked for the entry (its token is configured on the
	// chain); ok when it answered an amount.
	asked bool
	ok    bool
	bal   money.Units
	at    uint64 // the aggregate's block
	// stale is set when the aggregate's block is older than the block of the balance stored for
	// the entry's token (a provider behind, for instance one still before our own flush): the
	// reading says nothing new and is neither compared nor stored.
	stale bool
	// deployed is the forwarder probe of the entry's address (only probed until a tick saw it
	// deployed once, store.WatchEntry.ForwarderSeenAt).
	deployed bool
}

// balances reads every entry's token balance with Multicall3 aggregate3 calls of up to
// multicallBatch balanceOf calls, one request each, from one provider in turn. It always makes at
// least one request: each aggregate3 first reads its block (Multicall3's getBlockNumber), so one
// without balance calls still returns the head. head is the lowest block of the aggregates. The
// same aggregate probes each address whose forwarder was not seen deployed yet
// (contracts.ForwarderProbe, one more call in the request, no more requests).
//
// A probe may fail on its own (AllowFailure): a forwarder of an implementation without tokens()
// (before D41) reverts it, which must not cost every other entry its reading; a failed probe
// counts as deployed (an address without code never reverts). balanceOf is not allowed to fail:
// a configured token whose balanceOf reverts is a broken configuration, and failing the whole read
// (loop_failing) is louder than skipping its entries; a balance is never read from a failed call.
func (d *Detector) balances(ctx context.Context, entries []store.WatchEntry) ([]reading, uint64, error) {
	rs := make([]reading, len(entries))
	var calls []contracts.Call
	var idx []int
	var idxAddr []evm.Address // the parsed address of each idx entry, so it is never parsed twice
	addrs := map[evm.Address][]int{}
	for i, e := range entries {
		rs[i].e = e
		addr, err1 := evm.ParseAddressLoose(e.Address)
		token, err2 := evm.ParseAddressLoose(e.Token)
		if err := errors.Join(err1, err2); err != nil {
			d.unparseable(ctx, e, "address or token", err) // buildWatch skips it too
			continue
		}
		if a, ok := d.tokens[token]; !ok || a.Symbol != e.Asset {
			continue // not a configured token of this chain
		}
		calls = append(calls, contracts.Call{Target: token, Data: contracts.BalanceOf(addr)}) // AllowFailure false
		idx = append(idx, i)
		idxAddr = append(idxAddr, addr)
		rs[i].asked = true
		if e.ForwarderSeenAt == nil {
			addrs[addr] = append(addrs[addr], i)
		}
	}
	var head uint64
	probed := map[evm.Address]bool{}
	for start := 0; start == 0 || start < len(calls); start += multicallBatch {
		chunk := calls[start:min(start+multicallBatch, len(calls))]
		var probes []evm.Address
		for j := range chunk {
			addr := idxAddr[start+j]
			if _, ok := addrs[addr]; ok && !probed[addr] {
				probed[addr] = true
				probes = append(probes, addr)
			}
		}
		req := chunk
		if len(probes) > 0 {
			req = slices.Clone(chunk)
			for _, a := range probes {
				req = append(req, contracts.Call{Target: a, AllowFailure: true, Data: contracts.ForwarderProbe()})
			}
		}
		out, _, err := d.rot.Call(ctx, evmrpc.CallMsg{To: d.multicall, Data: contracts.BlockAndAggregate3(d.multicall, req)}, evmrpc.Latest)
		if err != nil {
			return nil, 0, fmt.Errorf("multicall on %s: %w", d.chainID(), err)
		}
		block, results, err := contracts.ParseBlockAndAggregate3(out, len(req))
		if err != nil {
			return nil, 0, fmt.Errorf("multicall on %s: %w", d.chainID(), err)
		}
		for k, a := range probes {
			if contracts.ForwarderDeployed(results[len(chunk)+k]) {
				for _, i := range addrs[a] {
					rs[i].deployed = true
				}
			}
		}
		results = results[:len(chunk)]
		if start == 0 || block < head {
			head = block
		}
		for j, r := range results {
			i := idx[start+j]
			bal, ok := amountWord(r.Data)
			if !r.Success || !ok {
				d.log.Error("balanceOf answered no amount", "chain", d.chainID(), "session", entries[i].SessionID, "token", entries[i].Token)
				continue
			}
			rs[i].ok, rs[i].bal, rs[i].at = true, bal, block
			rs[i].stale = entries[i].BalanceBlock != nil && block < *entries[i].BalanceBlock
		}
	}
	return rs, head, nil
}

// observe reads the entries' balances and rescans those whose balance rose (or that wait for a
// "Check again" request, late passes only), or whose balance fell to a non-zero amount (a flush
// and a deposit after it). A balance that fell to zero (a flush) only updates the stored balance;
// an unchanged one writes nothing (a late pass still records its schedule).
// checked_block and the balance move only after a rescan succeeded, so a provider that answers a
// wrong balance can delay a rescan but never skip a block. It returns the head and the number of
// transfers recorded.
//
// In a late pass next_check_at, which ends the expiry hold (ADR-0008 H-3), is the address's (every
// asset shares the watch row): an address that is rescanned gets it from the rescan's own check,
// once the rescan recorded what it found, never from an unchanged asset before it; and an address
// whose session awaits its first late check (store.WatchEntry.AwaitsFirstLateCheck) gets none
// while one of its balances could not be read (no amount, or a stale reading). Such an address
// stays due, and the pass fails once the rest is done so the pacer retries it at the warm
// interval instead of on every step. So does an address the rescan held (a balance its logs do
// not explain yet, reconcile.go): it wrote no next_check_at either.
//
// Its steps: sortReadings, probeQuiet, writeChecks, rescanEntries and, in a late pass, lateErrors.
func (d *Detector) observe(ctx context.Context, entries []store.WatchEntry, now time.Time, late bool) (uint64, int, error) {
	rs, head, err := d.balances(ctx, entries)
	if err != nil {
		return 0, 0, err
	}
	o, err := d.sortReadings(ctx, rs, head, now, late)
	if err != nil {
		return head, 0, err
	}
	if err := d.probeQuiet(ctx, o, head, late); err != nil {
		return head, 0, err
	}
	if err := d.writeChecks(ctx, o); err != nil {
		return head, 0, err
	}
	found := 0
	var held map[string]bool
	if len(o.changed) > 0 {
		if found, held, err = d.rescanEntries(ctx, o.changed, rs, now, late); err != nil {
			return head, found, err
		}
	}
	if !late {
		return head, found, nil // a held address is rescanned by the next tick
	}
	return head, found, d.lateErrors(o, held)
}

// observation is a tick's readings sorted by observe: the changed ones go to a rescan, the quiet
// ones (a deployed forwarder's unchanged balance) to a flush probe with their check, the rest
// only write their check.
type observation struct {
	changed []reading
	quiet   []reading
	checks  []store.EVMCheck // the check of each quiet reading
	writes  []checkWrite
	// rescanned and unread are keyed by the lowercase address: the addresses sent to a rescan,
	// and those awaiting their first late check whose balance could not be read.
	rescanned, unread map[string]bool
	own               map[string]bool // whether a deployed forwarder has a flush job of ours
}

// checkWrite is the check of an entry that is not rescanned.
type checkWrite struct {
	e   store.WatchEntry
	chk store.EVMCheck
}

// sortReadings sorts the readings into an observation.
func (d *Detector) sortReadings(ctx context.Context, rs []reading, head uint64, now time.Time, late bool) (*observation, error) {
	o := &observation{rescanned: map[string]bool{}, unread: map[string]bool{}, own: map[string]bool{}}
	for _, r := range rs {
		addr := strings.ToLower(r.e.Address)
		// A session on a customer address whose watch row is not its own yet takes it over and
		// reads the address from its cursor, whatever the balance: funds sent between sessions
		// may already have left with an earlier session's flush.
		if r.e.CustomerAddressID != nil && r.e.FirstBlock == nil {
			taken, ok, err := d.takeOver(ctx, r.e, head)
			if err != nil {
				return nil, err
			}
			if ok {
				// The row's stored balance (kept by the takeover) may be from a later block than
				// this reading: such a reading says nothing new.
				r.e, r.stale = taken, r.ok && taken.BalanceBlock != nil && r.at < *taken.BalanceBlock
				o.changed = append(o.changed, r)
				o.rescanned[addr] = true
			}
			continue
		}
		if err := d.sortReading(ctx, o, r, head, now, late); err != nil {
			return nil, err
		}
	}
	return o, nil
}

// sortReading sorts the reading of an entry that owns its watch row.
func (d *Detector) sortReading(ctx context.Context, o *observation, r reading, head uint64, now time.Time, late bool) error {
	addr := strings.ToLower(r.e.Address)
	e := r.e
	chk := store.EVMCheck{FirstBlock: d.firstBlock(head, e)}
	recheck := late && e.RecheckDue(e.RecheckedFor)
	if late {
		chk.Next = scan.NextCheck(d.cfg.Schedule, d.cfg.WatchAfterExpiry, e.ExpiresAt, now)
	}
	foreign, err := d.foreignDeploy(ctx, o.own, r, &chk)
	if err != nil {
		return err
	}
	prev := money.Zero
	if e.LastBalance != nil {
		prev = *e.LastBalance
	}
	if r.stale {
		d.log.Debug("balance read behind the stored one; ignored", "chain", d.chainID(), "session", e.SessionID,
			"token", e.Token, "block", r.at, "stored_block", *e.BalanceBlock)
	}
	fresh := r.ok && !r.stale
	// A rise rescans. So does any non-zero balance below the stored one: a flush (ours or
	// someone else's) that emptied the address plus a deposit after it, between two reads.
	// Only a reading of zero below it is stored without a rescan (a flush and nothing else),
	// unless the forwarder was deployed before: anyone can flush it, so the flush may have
	// carried a deposit no read saw (H-4).
	c := r.bal.Cmp(prev)
	if recheck || foreign || (fresh && c > 0) || (fresh && c < 0 && (!r.bal.IsZero() || e.ForwarderSeenAt != nil)) {
		o.changed = append(o.changed, r)
		o.rescanned[addr] = true
		return nil
	}
	if late && r.asked && !fresh && e.AwaitsFirstLateCheck(e.NextCheckAt) {
		o.unread[addr] = true
	}
	if e.ForwarderSeenAt != nil && e.FirstBlock != nil {
		o.quiet = append(o.quiet, r)
		o.checks = append(o.checks, chk)
		return nil
	}
	if chk, ok := d.keep(r, chk, fresh && c < 0, late); ok {
		o.writes = append(o.writes, checkWrite{e: e, chk: chk})
	}
	return nil
}

// foreignDeploy reports whether the reading's forwarder is deployed without a flush job of ours
// (own caches the answer per address), and sets the check's ForwarderSeen when it is ours.
func (d *Detector) foreignDeploy(ctx context.Context, own map[string]bool, r reading, chk *store.EVMCheck) (bool, error) {
	// A forwarder deployed without a flush job of ours is someone else's deployAndFlush: the
	// deposit it moved may have come and gone between two reads. Rescan the address once.
	if !r.deployed {
		return false, nil
	}
	addr := strings.ToLower(r.e.Address)
	ours, ok := own[addr]
	if !ok {
		var err error
		if ours, err = d.st.OwnFlushJob(ctx, d.chainID(), addr); err != nil {
			return false, err
		}
		own[addr] = ours
	}
	foreign := !ours
	chk.ForwarderSeen = ours
	if foreign {
		d.log.Warn("forwarder deployed by someone else; rescanning", "chain", d.chainID(), "session", r.e.SessionID, "address", r.e.Address)
	}
	return foreign, nil
}

// probeQuiet sends to the rescan the quiet readings whose forwarder flushed since the stored
// balance, and keeps the check of the others. A deployed forwarder's balance can read unchanged
// across a deposit someone else flushed: its Settled events since the stored balance decide
// (H-4). This runs before any write: an address it sends to a rescan gets its late schedule from
// the rescan, as any other.
func (d *Detector) probeQuiet(ctx context.Context, o *observation, head uint64, late bool) error {
	if len(o.quiet) == 0 {
		return nil
	}
	flushed, err := d.flushedSince(ctx, o.quiet, head)
	if err != nil {
		return err
	}
	for i, r := range o.quiet {
		if flushed[i] {
			o.changed = append(o.changed, r)
			o.rescanned[strings.ToLower(r.e.Address)] = true
			continue
		}
		if chk, ok := d.keep(r, o.checks[i], false, late); ok {
			o.writes = append(o.writes, checkWrite{e: r.e, chk: chk})
		}
	}
	return nil
}

// writeChecks writes the checks of the entries that are not rescanned. An address that is
// rescanned, or that awaits its first late check unread, gets no late schedule from them.
func (d *Detector) writeChecks(ctx context.Context, o *observation) error {
	for _, w := range o.writes {
		if a := strings.ToLower(w.e.Address); o.rescanned[a] || o.unread[a] {
			w.chk.Next = nil
		}
		if err := d.st.MarkChecked(ctx, w.e, w.chk); err != nil {
			return err
		}
	}
	return nil
}

// lateErrors is the error of a late pass that leaves addresses due: the unread ones that were not
// rescanned, and the held ones.
func (d *Detector) lateErrors(o *observation, held map[string]bool) error {
	n := 0
	for a := range o.unread {
		if !o.rescanned[a] {
			n++
		}
	}
	var errs []error
	if n > 0 {
		errs = append(errs, fmt.Errorf("late check on %s: %d deposit addresses not read (no balance or a stale one); "+
			"retried, their sessions' expiry waits", d.chainID(), n))
	}
	// A held address stays due (its next_check_at past, its "Check again" open): without an error
	// the pacer would run the pass again on every step, each a rescan through both providers, for
	// as long as the logs lag (up to ReconcileWait).
	if len(held) > 0 {
		errs = append(errs, fmt.Errorf("late check on %s: %d deposit addresses held, their balance not explained by the logs yet "+
			"(ADR-0008 H-1); retried at the warm interval", d.chainID(), len(held)))
	}
	return errors.Join(errs...)
}

// keep is the check of an entry that is not rescanned, and whether it is written: a fall to zero
// (fell) stores the balance (a flush), an unchanged registered entry writes nothing outside late
// passes.
func (d *Detector) keep(r reading, chk store.EVMCheck, fell, late bool) (store.EVMCheck, bool) {
	switch {
	case fell:
		bal := r.bal
		chk.Balance, chk.BalanceBlock = &bal, r.at
	case !late && r.e.FirstBlock != nil && !chk.ForwarderSeen:
		return chk, false // unchanged and already registered: nothing to write
	}
	return chk, true
}
