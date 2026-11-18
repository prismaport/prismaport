package detector

import (
	"context"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prismaport/prismaport/internal/alert"
	"github.com/prismaport/prismaport/internal/chainwatch"
	"github.com/prismaport/prismaport/internal/contracts"
	"github.com/prismaport/prismaport/internal/evm"
	"github.com/prismaport/prismaport/internal/evmrpc"
	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/store"
)

// A rescan stores a new balance only when the logs it read explain it (H-1): the stored balance,
// plus every Transfer into the address since the block it was read at, minus every flush out of
// it (the forwarder's Settled gross; only a deployed forwarder can move funds), equals the new
// reading. Both providers' log index can trail the head they report (the balance already shows a
// deposit whose log is not served yet): storing that balance would make every later tick read
// "unchanged" and the deposit would never be credited. An unexplained balance is kept as it was,
// with checked_block, the address's cursor and the late schedule, so the next tick rescans the
// same range; after ReconcileWait it is stored anyway with a Critical alert, so a balance the
// logs can never explain does not rescan the address on every tick for good. That commit leaves
// checked_block and the cursor before the window: the deposit is still in the blocks a later
// rescan ("Check again", the sender's, or the next rise's) reads once the logs show it.

// flow is one movement of a token at an address: a Transfer into it, or a flush out of it.
type flow struct {
	block  uint64
	index  uint32
	amount *big.Int
	out    bool
}

// flows are the movements a rescan read, by (address, token).
type flows map[[2]evm.Address][]flow

// add records the Transfers into watched addresses and the Settled events of the forwarders in f.
func (fl flows) add(d *Detector, f fetched) {
	for _, l := range f.transfers {
		if len(l.Topics) != 3 || l.Topics[0] != TransferTopic || len(l.Data) != 32 {
			continue
		}
		if _, ok := d.tokens[l.Address]; !ok {
			continue
		}
		to, ok := l.Topics[2].Address()
		if !ok {
			continue
		}
		k := [2]evm.Address{to, l.Address}
		fl[k] = append(fl[k], flow{block: l.BlockNumber, index: l.LogIndex, amount: new(big.Int).SetBytes(l.Data)})
	}
	for _, l := range f.settled {
		s, ok := contracts.ParseSettled(l)
		if !ok {
			continue
		}
		k := [2]evm.Address{s.Forwarder, s.Token}
		fl[k] = append(fl[k], flow{block: l.BlockNumber, index: l.LogIndex, amount: s.Gross.Big(), out: true})
	}
}

// sum is what moved in and out of (address, token) in the blocks [from, to].
func (fl flows) sum(k [2]evm.Address, from, to uint64) (in, out *big.Int) {
	in, out = new(big.Int), new(big.Int)
	for _, f := range fl[k] {
		if f.block < from || f.block > to {
			continue
		}
		if f.out {
			out.Add(out, f.amount)
		} else {
			in.Add(in, f.amount)
		}
	}
	return in, out
}

// afterFlush is what moved into (address, token) in block n after the last flush out of it in
// that block. A flush sweeps the whole balance, so right after its Settled event the balance is
// zero: that is the zero our flush's receipt stores at its block (FlushMined), and a deposit
// later in the same block is not in it. A zero read at the end of block n leaves nothing here.
func (fl flows) afterFlush(k [2]evm.Address, n uint64) *big.Int {
	in := new(big.Int)
	last, ok := uint32(0), false
	for _, f := range fl[k] {
		if f.block == n && f.out && (!ok || f.index > last) {
			last, ok = f.index, true
		}
	}
	for _, f := range fl[k] {
		if ok && f.block == n && !f.out && f.index > last {
			in.Add(in, f.amount)
		}
	}
	return in
}

// balanceWindow is the first block of the movements that must explain a new balance of e: the
// block after the one its stored balance was read at; without a stored balance, the first block
// the address can hold anything at (it comes from a salt drawn at the session's creation, so the
// balance before it is zero). A customer address's stored balances are the address's and pass
// from session to session with its watch row; one without a stored balance for the token has a
// zero baseline only while nothing was read on it yet (unread: its cursor is unset, so the
// rescan starts where the address could first receive anything). known is false when the
// baseline is unknown: a customer address already read without a stored balance of the token
// (a token no earlier session watched, or check state reset after a reversed payment), one whose
// stored balance is more than BaselineReach before start, or a balance stored without its block.
// start is where the entry's rescan starts.
func (d *Detector) balanceWindow(head uint64, e store.WatchEntry, start uint64, unread bool) (first uint64, known bool) {
	switch {
	case e.LastBalance != nil && e.BalanceBlock != nil:
		if e.CustomerAddressID != nil && *e.BalanceBlock+1+d.reachBlocks() < start {
			return start, false
		}
		return *e.BalanceBlock + 1, true
	case e.LastBalance != nil:
		return start, false
	case e.CustomerAddressID != nil:
		return start, unread
	}
	return d.firstBlock(head, e), true
}

// reachBlocks is BaselineReach in blocks.
func (d *Detector) reachBlocks() uint64 {
	return uint64((d.cfg.BaselineReach + d.cfg.BlockTime - 1) / d.cfg.BlockTime)
}

// BaselineReach is BaselineReach in blocks: how far a flush's zero may move checked_block back
// (store.FlushMined; the sender asks).
func (d *Detector) BaselineReach() uint64 { return d.reachBlocks() }

// sinceLastFlush is what moved into (address, token) in [from, to] after the last flush out of it
// there (ok is false without one). A flush sweeps the whole balance, so right after its Settled
// event the balance is zero: a known baseline even where the balance before the window is not.
func (fl flows) sinceLastFlush(k [2]evm.Address, from, to uint64) (in *big.Int, ok bool) {
	var last flow
	for _, f := range fl[k] {
		if f.out && f.block >= from && f.block <= to && (!ok || f.block > last.block || (f.block == last.block && f.index > last.index)) {
			last, ok = f, true
		}
	}
	in = new(big.Int)
	for _, f := range fl[k] {
		if ok && !f.out && f.block <= to && (f.block > last.block || (f.block == last.block && f.index > last.index)) {
			in.Add(in, f.amount)
		}
	}
	return in, ok
}

// explains reports whether the movements in [from, r.at] take prev to the reading. A zero
// baseline also counts the deposits of its own block (from-1) that came after a flush there (see
// afterFlush). Without a known baseline a zero is explained; a non-zero balance must be what came
// in after the window's last flush (the zero that flush left is a known baseline), or, with no
// flush in the window, come with some deposit in it. Before, a flush alone "explained" any
// non-zero balance: a deposit after a third-party flush whose log lagged was stored as explained
// and never credited (ADR-0008 H-1).
func explains(fl flows, r reading, prev money.Units, from uint64, known bool) bool {
	k, ok := entryKey(r.e)
	if !ok {
		return true
	}
	in, out := fl.sum(k, from, r.at)
	if !known {
		if r.bal.IsZero() {
			return true
		}
		if after, flushed := fl.sinceLastFlush(k, from, r.at); flushed {
			return after.Cmp(r.bal.Big()) == 0
		}
		return in.Sign() > 0
	}
	if prev.IsZero() && from > 0 {
		in.Add(in, fl.afterFlush(k, from-1))
	}
	want := new(big.Int).Add(prev.Big(), in)
	return want.Sub(want, out).Cmp(r.bal.Big()) == 0
}

func entryKey(e store.WatchEntry) ([2]evm.Address, bool) {
	a, err1 := evm.ParseAddressLoose(e.Address)
	t, err2 := evm.ParseAddressLoose(e.Token)
	return [2]evm.Address{a, t}, err1 == nil && err2 == nil
}

// unexplained tracks, in memory, since when each session's (address, token) balance has not been
// explained by its rescans: on a customer address the next session's wait is its own. A restart
// only starts the wait again.
type unexplained struct {
	mu    sync.Mutex
	since map[string]time.Time
}

func unexplainedKey(e store.WatchEntry) string {
	return e.SessionID + "|" + strings.ToLower(e.Address) + "|" + strings.ToLower(e.Token)
}

// mark records an unexplained reading of e and reports since when its balance is unexplained.
func (u *unexplained) mark(e store.WatchEntry, now time.Time, wait time.Duration) time.Time {
	return u.markKey(unexplainedKey(e), now, wait)
}

// markKey is mark for any key (a wait of another kind on the same tracker).
func (u *unexplained) markKey(k string, now time.Time, wait time.Duration) time.Time {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.since == nil {
		u.since = map[string]time.Time{}
	}
	for k, t := range u.since {
		if now.Sub(t) > 2*wait {
			delete(u.since, k) // an entry that left the watch while unexplained
		}
	}
	t, ok := u.since[k]
	if !ok {
		t = now
		u.since[k] = t
	}
	return t
}

func (u *unexplained) clear(e store.WatchEntry) { u.clearKey(unexplainedKey(e)) }

func (u *unexplained) clearKey(k string) {
	u.mu.Lock()
	delete(u.since, k)
	u.mu.Unlock()
}

// reconcile decides, for every item whose rescan reached the block its reading came from
// (rescanItem.stored), whether the reading can be stored (commit) and which addresses must keep
// their check state (hold) because one of their readings is not explained yet. An item's covered
// is how far its rescan got, win and known its balance window. Items that are not explained by
// the window but have a known zero baseline further back (a non-customer address, from its first
// block) get a second chance from there (retryFromFirstBlock): the stored balance may come from a
// block that was reorged out (our flush's receipt). escaped marks a reading stored unexplained
// after ReconcileWait (unexplainedReading): the deposit its logs do not show yet lies in the
// window, so checked_block and the address's cursor must not pass the window's start
// (rescanEntries).
func (d *Detector) reconcile(ctx context.Context, ws watchSet, items []rescanItem, from, head uint64, now time.Time) (hold map[string]bool, found int, err error) {
	hold = map[string]bool{}
	var retry []int
	for i := range items {
		it := &items[i]
		if !it.stored() {
			continue // nothing to store: the existing rules apply
		}
		if it.commit = explains(ws.flows, it.reading, prevBalance(it.e), it.win, it.known); !it.commit && it.known &&
			it.e.CustomerAddressID == nil && it.win > d.firstBlock(head, it.e) {
			retry = append(retry, i)
		}
	}
	if len(retry) > 0 {
		if found, err = d.retryFromFirstBlock(ctx, ws, items, retry, from, head); err != nil {
			return hold, found, err
		}
	}
	for i := range items {
		it := &items[i]
		if !it.stored() {
			continue
		}
		if it.commit {
			d.unexplained.clear(it.e)
			continue
		}
		if d.unexplainedReading(ctx, it, now) {
			hold[strings.ToLower(it.e.Address)] = true
		}
	}
	return hold, found, nil
}

// retryFromFirstBlock rescans the blocks before from on the retry items' addresses, from the
// earliest first block among them, into ws.flows, then checks each retry item against a zero
// baseline at its first block.
func (d *Detector) retryFromFirstBlock(ctx context.Context, ws watchSet, items []rescanItem, retry []int, from, head uint64) (int, error) {
	var es []store.WatchEntry
	addrs := map[string]bool{}
	first := from
	for _, i := range retry {
		addrs[strings.ToLower(items[i].e.Address)] = true
		first = min(first, d.firstBlock(head, items[i].e))
	}
	for _, e := range ws.byAddr {
		if addrs[strings.ToLower(e.Address)] {
			es = append(es, e)
		}
	}
	full := d.buildWatch(es)
	full.byNonce, full.flows = map[evm.Word]store.WatchEntry{}, ws.flows
	for _, a := range ws.settledFrom {
		if addrs[strings.ToLower(a.Hex())] {
			full.settledFrom = append(full.settledFrom, a)
		}
	}
	found := 0
	if first < from {
		var err error
		if found, err = d.rescan(ctx, full, first, from-1); err != nil {
			return found, err
		}
	}
	for _, i := range retry {
		items[i].commit = explains(ws.flows, items[i].reading, money.Zero, d.firstBlock(head, items[i].e), true)
	}
	return found, nil
}

// unexplainedReading handles a reading the logs do not explain. Until ReconcileWait has passed
// it reports true: its address is held and rescanned again. After it, the reading is stored
// unexplained (commit and escaped) with an alert.
func (d *Detector) unexplainedReading(ctx context.Context, it *rescanItem, now time.Time) bool {
	since := d.unexplained.mark(it.e, now, d.cfg.ReconcileWait)
	if now.Sub(since) < d.cfg.ReconcileWait {
		d.log.Warn("balance not explained by the logs yet; rescanning again", "chain", d.chainID(), "session", it.e.SessionID,
			"address", it.e.Address, "token", it.e.Token, "balance", it.bal.String(), "block", it.at, "since", since)
		return true
	}
	it.commit, it.escaped = true, true
	d.unexplained.clear(it.e)
	sev, msg := alert.Critical, "the logs never explained the balance: a payment may be missing "+
		"(press Check again once the providers' logs catch up, then verify the session's payments)"
	if !it.known {
		sev, msg = alert.Warning, "no deposit found for the balance of a customer address without a stored baseline; stored as it is "+
			"(Check again rescans its window once the providers' logs catch up)"
	}
	d.alerts.Alert(ctx, alert.Alert{Severity: sev, Chain: d.chainID(), Kind: "balance_unexplained", Message: msg,
		Fields: map[string]string{"session": it.e.SessionID, "address": it.e.Address, "token": it.e.Token,
			"balance": it.bal.String(), "stored": prevBalance(it.e).String(), "block": strconv.FormatUint(it.at, 10)}})
	return false
}

func prevBalance(e store.WatchEntry) money.Units {
	if e.LastBalance != nil {
		return *e.LastBalance
	}
	return money.Zero
}

// probeChunks bounds the MaxRange chunks one flush probe reads (one provider): a stored balance
// older than that is refreshed by a rescan instead.
const probeChunks = 4

// flushedSince reads, from one provider, the Settled events of the deployed forwarders among the
// quiet entries (balance unchanged) since each entry's stored balance (H-4): anyone can flush()
// a deployed forwarder, so a deposit and its flush can both land between two balance reads,
// which then read 0 and 0. It reports the entries to rescan: a flush of the entry's token after
// the block of its stored balance (our own flush stores its block; without one, since the
// address's first block), an unknown baseline, or a window longer than probeChunks ranges. One
// provider is enough: a missed event is read again next tick, since nothing here moves the
// stored balance; the rescan reads through both providers. The read ends at the agreed head
// (the lower of both providers' heads, one header request each) rather than at head, which is
// the aggregate's provider's: the other provider, asked for a range past its own head, would
// fail and be cooled down. The blocks above it are read on a later tick.
func (d *Detector) flushedSince(ctx context.Context, quiet []reading, head uint64) ([]bool, error) {
	flag := make([]bool, len(quiet))
	lo := make([]uint64, len(quiet))
	byAddr := map[evm.Address][]int{}
	var addrs []evm.Address
	from := head + 1
	for i, r := range quiet {
		k, ok := entryKey(r.e)
		if !ok {
			continue
		}
		first, known := d.balanceWindow(head, r.e, head, false)
		if !known || first+uint64(probeChunks)*d.cfg.MaxRange <= head {
			flag[i] = true
			continue
		}
		lo[i] = first
		if lo[i] > head {
			continue
		}
		from = min(from, lo[i])
		if _, ok := byAddr[k[0]]; !ok {
			addrs = append(addrs, k[0])
		}
		byAddr[k[0]] = append(byAddr[k[0]], i)
	}
	if len(addrs) == 0 {
		return flag, nil
	}
	latest, err := d.q.AgreedLatest(ctx)
	if err != nil {
		return flag, err
	}
	to := min(head, latest)
	if from > to {
		return flag, nil
	}
	for a, b := range chainwatch.LogRanges(from, to, d.cfg.MaxRange) {
		for batch := range slices.Chunk(addrs, addressBatch) {
			ls, err := d.rot.Logs(ctx, evmrpc.Filter{FromBlock: a, ToBlock: b, Addresses: batch, Topics: [][]evm.Word{{contracts.SettledTopic}}})
			if err != nil {
				return flag, err
			}
			for _, l := range ls {
				s, ok := contracts.ParseSettled(l)
				if !ok {
					continue
				}
				for _, i := range byAddr[s.Forwarder] {
					if k, _ := entryKey(quiet[i].e); k[1] == s.Token && l.BlockNumber >= lo[i] {
						flag[i] = true
					}
				}
			}
		}
	}
	return flag, nil
}
