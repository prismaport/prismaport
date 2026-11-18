package detector

import (
	"context"
	"slices"
	"time"

	"github.com/prismaport/prismaport/internal/store"
)

// firstBlock is where an entry without a watch row starts: the head at its network's reveal
// (estimated from the reveal's age) minus the margin. The first rescan covers every block since
// the address was shown, whenever the first tick runs.
func (d *Detector) firstBlock(head uint64, e store.WatchEntry) uint64 {
	if e.FirstBlock != nil {
		return *e.FirstBlock
	}
	since := e.CreatedAt
	if e.RevealedAt != nil {
		since = *e.RevealedAt
	}
	return d.startFor(head, &since)
}

// startFor is the first block that can hold a transfer to an address shown at since:
// head - ceil(age / BlockTime) - ReanchorMargin.
func (d *Detector) startFor(head uint64, since *time.Time) uint64 {
	var blocks uint64
	if since != nil {
		if age := d.st.Now().Sub(*since); age > 0 {
			blocks = uint64((age + d.cfg.BlockTime - 1) / d.cfg.BlockTime)
		}
	}
	blocks += d.cfg.ReanchorMargin
	if blocks >= head {
		return 0
	}
	return head - blocks
}

// readStart is where reading a customer address's history resumes: after its cursor, or where
// the address could first receive anything (it is derived from a salt drawn at its creation).
func (d *Detector) readStart(head uint64, c store.AddressCursor) uint64 {
	if c.Block != nil {
		return *c.Block + 1
	}
	return d.startFor(head, &c.CreatedAt)
}

// addressCursors reads the cursors of the customer addresses among the rescan items.
func (d *Detector) addressCursors(ctx context.Context, rs []rescanItem) (map[string]store.AddressCursor, error) {
	var ids []string
	for _, r := range rs {
		if id := r.e.CustomerAddressID; id != nil && !slices.Contains(ids, *id) {
			ids = append(ids, *id)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	return d.st.AddressCursors(ctx, d.chainID(), ids)
}

// takeOver gives the watch row of a customer address's deposit address to the address's current
// session (static-addresses.md, "Watch ownership"), reading from the address's cursor. It answers
// the entry as the row now stands; ok is false when a newer session owns the address meanwhile.
func (d *Detector) takeOver(ctx context.Context, e store.WatchEntry, head uint64) (store.WatchEntry, bool, error) {
	cursors, err := d.st.AddressCursors(ctx, d.chainID(), []string{*e.CustomerAddressID})
	if err != nil {
		return e, false, err
	}
	start := d.readStart(head, cursors[*e.CustomerAddressID])
	ok, err := d.st.TakeWatch(ctx, e, start)
	if err != nil || !ok {
		return e, false, err
	}
	e.FirstBlock, e.CheckedBlock, e.NextCheckAt, e.RecheckedFor = &start, nil, nil, nil
	// TakeWatch moved checked_at: read it again so this check's balance is stored. The token's
	// stored balance is the address's and was kept: it is this session's baseline too (H-1).
	if e.CheckedAt, e.LastBalance, e.BalanceBlock, err = d.st.WatchCheck(ctx, e); err != nil {
		return e, false, err
	}
	d.log.Info("customer address watch taken over", "chain", d.chainID(), "customer_address", *e.CustomerAddressID,
		"session", e.SessionID, "address", e.Address, "from", start)
	return e, true, nil
}
