package soldetector

import (
	"github.com/prismaport/prismaport/internal/solana"
	"github.com/prismaport/prismaport/internal/store"
)

// target is one watched account and the pay_to entries it serves.
type target struct {
	address string
	kind    string
	// entries has one element for a deposit account and every Solana entry of the session
	// for a reference key.
	entries []store.WatchEntry
	// owner is the PDA for deposits.
	owner string
}

// DepositAccount is the token account a deposit address receives a mint into.
func DepositAccount(e store.WatchEntry) (solana.PublicKey, error) {
	owner, err := solana.ParsePublicKey(e.Address)
	if err != nil {
		return solana.PublicKey{}, err
	}
	mint, err := solana.ParsePublicKey(e.Token)
	if err != nil {
		return solana.PublicKey{}, err
	}
	return solana.AssociatedTokenAddress(owner, mint)
}

func (d *Detector) targets(entries []store.WatchEntry) []target {
	var out []target
	refs := map[string]int{}
	for _, e := range entries {
		asset, ok := d.mints[e.Token]
		if !ok || asset.Symbol != e.Asset {
			continue // not a configured mint of this chain
		}
		ata, err := DepositAccount(e)
		if err != nil {
			d.log.Error("unparseable Solana pay_to entry", "session", e.SessionID, "chain", e.Chain)
			continue
		}
		out = append(out, target{address: ata.String(), kind: store.SolanaDeposit, entries: []store.WatchEntry{e}, owner: e.Address})
		ref := solana.SessionReference(e.Salt).String()
		if i, ok := refs[ref]; ok {
			out[i].entries = append(out[i].entries, e)
			continue
		}
		refs[ref] = len(out)
		out = append(out, target{address: ref, kind: store.SolanaReference, entries: []store.WatchEntry{e}})
	}
	return out
}
