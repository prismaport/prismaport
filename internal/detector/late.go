package detector

import (
	"context"
	"strings"

	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/store"
)

// Late runs the due late checks and "Check again" requests with the same batched balance read as
// a tick (one aggregate per multicallBatch entries, one provider, no separate head read): a
// balance that rose rescans the address from the block the last rescan reached, and a "Check
// again" request always rescans (a deposit flushed by someone else leaves no balance). Whatever
// it finds goes through the normal path (a late payment after expiry). more is true when the
// batch was full.
func (d *Detector) Late(ctx context.Context) (bool, error) {
	now := d.st.Now()
	q := store.ScanQuery{Chain: d.chainID(), Family: chain.EVM, Now: now, LateFirst: d.cfg.Schedule.First(),
		LateWatch: d.cfg.WatchAfterExpiry}
	entries, err := d.st.LateEntries(ctx, q, d.cfg.LateBatch)
	if err != nil || len(entries) == 0 {
		return false, err
	}
	more := len(entries) == d.cfg.LateBatch
	if more {
		// A full batch may cut the last address's entries in two. They share its watch row (one
		// checked_block and next_check_at for every asset), so checking only some of them would
		// end the others' check, and a rescan would move the address past their transfers. That
		// address waits for the next batch (entries come address by address), unless it is the
		// whole batch.
		last := strings.ToLower(entries[len(entries)-1].Address)
		cut := len(entries)
		for cut > 0 && strings.ToLower(entries[cut-1].Address) == last {
			cut--
		}
		if cut > 0 {
			entries = entries[:cut]
		}
	}
	if _, _, err := d.observe(ctx, entries, now, true); err != nil {
		return false, err
	}
	return more, nil
}
