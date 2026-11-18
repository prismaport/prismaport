package soldetector

import (
	"context"
	"slices"
	"time"

	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/solana"
	"github.com/prismaport/prismaport/internal/solrpc"
	"github.com/prismaport/prismaport/internal/store"
)

// walletScan reports whether a session's reference key is scanned now (D49).
func (d *Detector) walletScan(e store.WatchEntry, now time.Time) bool {
	end, ok := e.WalletScanEnd(d.cfg.WalletWindow)
	return ok && now.Before(end)
}

// balanceCheck is what reading one deposit account's balance did.
type balanceCheck struct {
	target  target
	row     store.SolanaWatch
	balance money.Units
	found   int
	// complete is false when a transaction of a rescanned account could not be processed yet.
	complete bool
	// stale is set when the reading was older than the stored balance and no scan ran: it says
	// nothing about the account now.
	stale bool
}

// balances reads the token balance of each deposit target with one getMultipleAccounts per
// AccountBatch accounts (the rent receivers of the targets ride along), from one provider in
// turn. A balance that rose (or an account that appeared, even empty, where none was seen), or
// fell to a non-zero amount (a flush, then a deposit), gets a signature scan; so does every target
// in force ("Check again"). The balance seen is stored, with its slot, once the account's
// transactions were all processed; zero below the stored balance is stored at once (a flush). A
// reading older than the stored balance's slot is ignored. It returns what it read, for the late pass.
func (d *Detector) balances(ctx context.Context, ts []target, force map[string]bool) ([]balanceCheck, error) {
	if len(ts) == 0 {
		return nil, nil
	}
	cursors, err := d.ensure(ctx, ts)
	if err != nil {
		return nil, err
	}
	keys, accs, err := d.readAccounts(ctx, ts)
	if err != nil {
		return nil, err
	}
	var out []balanceCheck
	for j, tg := range ts {
		c, err := d.checkAccount(ctx, tg, cursors[tg.address], accs[j], force[tg.address])
		if err != nil {
			return out, err
		}
		out = append(out, c)
	}
	for k, rr := range keys[len(ts):] {
		if err := d.rentProbe(ctx, rr, accs[len(ts)+k]); err != nil {
			return out, err
		}
	}
	return out, nil
}

// readAccounts reads the targets' accounts, then their rent receivers' (each once), with one
// getMultipleAccounts per AccountBatch accounts. keys lists the addresses read, in the order of
// accs: the targets first, then the rent receivers.
func (d *Detector) readAccounts(ctx context.Context, ts []target) (keys []string, accs []solrpc.Account, err error) {
	keys = make([]string, 0, len(ts)+1)
	for _, tg := range ts {
		keys = append(keys, tg.address)
	}
	rents := map[string]bool{}
	for _, tg := range ts {
		if rr := tg.entries[0].RentReceiver; rr != "" && !rents[rr] {
			rents[rr] = true
			keys = append(keys, rr)
		}
	}
	// Every batch asks for at least the highest slot seen before this read, not the slot an earlier
	// batch of the same read answered: batches go to the providers in turn, and one a slot or two
	// behind the other would refuse every second batch of a large watch set.
	floor := d.slot
	accs = make([]solrpc.Account, 0, len(keys))
	for batch := range slices.Chunk(keys, d.cfg.AccountBatch) {
		got, err := d.rot.Accounts(ctx, batch, floor)
		if err != nil {
			return nil, nil, err
		}
		for _, a := range got {
			d.slot = max(d.slot, a.ContextSlot)
		}
		accs = append(accs, got...)
	}
	return keys, accs, nil
}

// checkAccount compares one target's account with its watch row: it scans the account when its
// balance rose, when forced or when a customer address's row is taken over (takeOverAccount),
// and stores the balance seen once the account's transactions were all processed.
func (d *Detector) checkAccount(ctx context.Context, tg target, row store.SolanaWatch, acc solrpc.Account, forced bool) (balanceCheck, error) {
	bal := money.Zero
	if acc.Exists {
		if amt, _, _, ok := solana.TokenAmount(acc.Data); ok {
			bal = money.FromUint64(amt)
		}
	}
	c := balanceCheck{target: tg, row: row, balance: bal, complete: true}
	ses := tg.entries[0].SessionID
	// An account of a customer address whose row an earlier session holds, or that was just
	// created, is read from the row's cursor whatever its balance: that is where the previous
	// watch stopped, so funds sent between sessions (or before the first one) are credited to
	// the current session with their real transaction (static-addresses.md, "Watch ownership").
	takeOver := tg.entries[0].CustomerAddressID != nil && (c.row.SessionID != ses || c.row.Created)
	prev := c.row.LastBalance
	// A reading from a slot older than the stored balance's (a provider behind, still before
	// our flush) says nothing new: neither compared nor stored.
	stale := c.row.BalanceSlot != nil && acc.ContextSlot < *c.row.BalanceSlot
	// A rise scans; so does any non-zero balance below the stored one (a flush emptied the
	// account and a deposit followed, between two reads). Zero below it is a flush: stored.
	rose := !stale && (appeared(prev, acc.Exists, bal) || (moved(prev, bal) && !bal.IsZero()))
	scanned := rose || forced || takeOver
	if scanned {
		// The listing must reach the slot the balance was read at (or find the transfer):
		// a signature index behind the account read would otherwise let the risen balance be
		// stored without its transaction, and the next tick would see nothing to rescan.
		n, complete, err := d.scan(ctx, tg, c.row, acc.ContextSlot)
		c.found, c.complete = n, complete
		if err != nil {
			return c, err
		}
		d.log.Info("deposit account rescanned", "chain", d.chainID(), "session", ses,
			"balance", bal.String(), "found", n, "forced", forced, "previous", c.row.SessionID)
	}
	if takeOver && c.complete && c.row.SessionID != ses {
		// The history is read: the row passes to the current session (late state cleared,
		// cursor kept). An incomplete read is repeated on the next pass.
		taken, err := d.takeOverAccount(ctx, tg, ses)
		if err != nil {
			return c, err
		}
		c.row = taken
		prev = c.row.LastBalance
		stale = c.row.BalanceSlot != nil && acc.ContextSlot < *c.row.BalanceSlot
	}
	// A balance is kept once seen; an account never seen keeps none, so it appearing (even
	// empty, after someone else's flush that left it open) still rescans.
	if c.complete && !stale && (appeared(prev, acc.Exists, bal) || moved(prev, bal)) {
		b := bal
		if err := d.st.MarkSolanaChecked(ctx, d.chainID(), tg.address, store.SolanaCheck{SessionID: ses, Balance: &b,
			Slot: acc.ContextSlot, Prev: c.row.CheckedAt}); err != nil {
			return c, err
		}
	}
	c.stale = stale && !scanned
	return c, nil
}

// appeared reports an account seen for the first time: no stored balance, and it exists or
// holds funds.
func appeared(prev *money.Units, exists bool, bal money.Units) bool {
	return prev == nil && (exists || !bal.IsZero())
}

// moved reports a balance that differs from the stored one.
func moved(prev *money.Units, bal money.Units) bool {
	return prev != nil && !bal.Equal(*prev)
}

// takeOverAccount passes a customer address account's watch row to the current session ses and
// returns the row as it now stands.
func (d *Detector) takeOverAccount(ctx context.Context, tg target, ses string) (store.SolanaWatch, error) {
	if err := d.st.TakeSolanaWatch(ctx, d.chainID(), ses, []string{tg.address}); err != nil {
		return store.SolanaWatch{}, err
	}
	rows, err := d.ensure(ctx, []target{tg})
	if err != nil {
		return store.SolanaWatch{}, err
	}
	d.log.Info("customer address account taken over", "chain", d.chainID(), "customer_address", *tg.entries[0].CustomerAddressID,
		"session", ses, "account", tg.address)
	return rows[tg.address], nil
}

func (d *Detector) ensure(ctx context.Context, ts []target) (map[string]store.SolanaWatch, error) {
	rows := make([]store.SolanaWatch, 0, len(ts))
	for _, t := range ts {
		w := store.SolanaWatch{Chain: d.chainID(), Address: t.address, Kind: t.kind, SessionID: t.entries[0].SessionID}
		if t.kind == store.SolanaDeposit {
			w.Asset = t.entries[0].Asset
		}
		rows = append(rows, w)
	}
	return d.st.EnsureSolanaWatch(ctx, rows)
}
