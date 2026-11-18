package checkout

import (
	"context"
	"testing"

	"github.com/prismaport/prismaport/internal/chainwatch"
	"github.com/prismaport/prismaport/internal/solana"
	"github.com/prismaport/prismaport/internal/solfake"
	"github.com/prismaport/prismaport/internal/solrpc"
	"github.com/prismaport/prismaport/internal/solwatch"
	"github.com/prismaport/prismaport/internal/store"
)

type silentStatuses struct{ *solfake.Provider }

func (silentStatuses) SignatureStatuses(context.Context, []string) ([]*solrpc.SignatureStatus, error) {
	return nil, solrpc.ErrTransport
}

// An expired wallet transaction is released only when both providers say it never landed.
func TestWalletTxGone(t *testing.T) {
	ctx := t.Context()
	w := testWalletTarget(t)
	payer := testKey(t, 8).PublicKey()
	build := func() (string, *solana.Transaction) {
		t.Helper()
		tx := walletTx(t, w, payer, testSplit(t, 25_000_000, w.bps), nil)
		b64, err := tx.Base64()
		if err != nil {
			t.Fatal(err)
		}
		return b64, tx
	}
	b64, tx := build()
	cl := solfake.New()
	q := solwatch.NewQuorum("solana-devnet", []solwatch.Provider{cl.Provider("a"), cl.Provider("b")}, chainwatch.Options{})
	c := &Checkout{}
	prev := store.WalletTx{Transaction: b64}
	if !c.walletTxGone(ctx, q, prev) {
		t.Fatal("a transaction no provider knows is gone")
	}
	raw, _ := tx.Serialize()
	onChain, err := solfake.FromRaw(raw, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	onChain.Err = `{"InstructionError":[1,{"Custom":1}]}`
	cl.Mine(onChain)
	if !c.walletTxGone(ctx, q, prev) {
		t.Fatal("a transaction that failed on chain can never land again")
	}
	onChain.Err = ""
	cl2 := solfake.New()
	cl2.Mine(onChain)
	q2 := solwatch.NewQuorum("solana-devnet", []solwatch.Provider{cl2.Provider("a"), cl2.Provider("b")}, chainwatch.Options{})
	if c.walletTxGone(ctx, q2, prev) {
		t.Fatal("a transaction that landed was released")
	}
	q3 := solwatch.NewQuorum("solana-devnet", []solwatch.Provider{cl.Provider("a"), silentStatuses{cl.Provider("b")}}, chainwatch.Options{})
	if c.walletTxGone(ctx, q3, store.WalletTx{Transaction: b64}) {
		t.Fatal("released while a provider could not answer")
	}
	if c.walletTxGone(ctx, q, store.WalletTx{Transaction: "not base64"}) {
		t.Fatal("released an unreadable transaction")
	}
}
