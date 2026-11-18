package soldetector

import (
	"context"
	"errors"

	"github.com/prismaport/prismaport/internal/solrpc"
	"github.com/prismaport/prismaport/internal/solwatch"
	"github.com/prismaport/prismaport/internal/store"
)

// rentProbe watches one rent receiver read with the deposit balances. Closing a deposit account
// (flush with close_source, permissionless) pays its rent to the rent receiver, so a payment
// someone else flushed and closed between two reads, invisible in the deposit account's balance,
// still raises the rent receiver's lamports. On a rise it lists the rent receiver's new
// signatures (one provider), skips our own transactions and failed ones, reads each other one
// (one provider) and rescans every watched deposit account it touched, through the usual agreed
// path. The first read only sets the baseline. Our own fees and treasury account creations
// lower the balance; a rise they hide is caught at the next rise or by "Check again".
func (d *Detector) rentProbe(ctx context.Context, rr string, acc solrpc.Account) error {
	seen, ok := d.rent[rr]
	if !ok || acc.Lamports <= seen.lamports {
		if !ok {
			seen.slot = acc.ContextSlot
		}
		seen.lamports = acc.Lamports
		d.rent[rr] = seen
		return nil
	}
	sigs, err := d.rot.SignaturesAfter(ctx, rr, seen.sig, seen.slot, rentPage)
	if err != nil {
		return err
	}
	var fresh []solrpc.SignatureInfo
	var hashes []string
	for _, s := range sigs {
		if seen.sig == "" && s.Slot <= seen.slot {
			break // before the baseline read
		}
		fresh = append(fresh, s)
		hashes = append(hashes, s.Signature)
	}
	own, err := d.st.OwnTxHashes(ctx, d.chainID(), hashes)
	if err != nil {
		return err
	}
	for i := len(fresh) - 1; i >= 0; i-- {
		s := fresh[i]
		if s.Failed() || own[s.Signature] {
			continue
		}
		done, err := d.foreignClose(ctx, s.Signature)
		if err != nil || !done {
			return err // the next rise (or read) tries again from the same baseline
		}
	}
	if len(sigs) > 0 {
		seen.sig, seen.slot = sigs[0].Signature, sigs[0].Slot
	}
	seen.lamports = acc.Lamports
	d.rent[rr] = seen
	return nil
}

// foreignClose rescans every watched deposit account a foreign transaction paying the rent
// receiver touched. done is false while the transaction is not served yet.
func (d *Detector) foreignClose(ctx context.Context, sig string) (bool, error) {
	tx, err := d.rot.Transaction(ctx, sig)
	if errors.Is(err, solrpc.ErrNotFound) || errors.Is(err, solwatch.ErrLag) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	rows, err := d.st.SolanaDepositRows(ctx, d.chainID(), tx.AccountKeys)
	if err != nil {
		return false, err
	}
	for _, row := range rows {
		entries, err := d.st.SessionWatchEntries(ctx, d.chainID(), row.SessionID)
		if err != nil {
			return false, err
		}
		for _, tg := range d.targets(entries) {
			if tg.kind != store.SolanaDeposit || tg.address != row.Address {
				continue
			}
			n, complete, err := d.scan(ctx, tg, row, 0)
			if err != nil {
				return false, err
			}
			d.log.Warn("deposit account closed by a foreign transaction; rescanned", "chain", d.chainID(), "session", row.SessionID,
				"account", row.Address, "tx", sig, "found", n)
			if !complete {
				return false, nil
			}
		}
	}
	return true, nil
}
