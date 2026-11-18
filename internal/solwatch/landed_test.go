package solwatch_test

import (
	"context"
	"testing"

	"github.com/prismaport/prismaport/internal/chainwatch"
	"github.com/prismaport/prismaport/internal/solfake"
	"github.com/prismaport/prismaport/internal/solrpc"
	"github.com/prismaport/prismaport/internal/solwatch"
)

// noStatuses is a provider whose getSignatureStatuses fails like an unreachable node.
type noStatuses struct{ *solfake.Provider }

func (noStatuses) SignatureStatuses(context.Context, []string) ([]*solrpc.SignatureStatus, error) {
	return nil, solrpc.ErrTransport
}

func TestLanded(t *testing.T) {
	ctx := t.Context()
	c := solfake.New()
	c.MineN(3)
	q := solwatch.NewQuorum("solana-devnet", []solwatch.Provider{c.Provider("a"), c.Provider("b")}, chainwatch.Options{})
	ok := solfake.Tx(solfake.Signature("ok"), solfake.Key("payer"), nil, nil, nil, nil)
	failed := solfake.Tx(solfake.Signature("failed"), solfake.Key("payer"), nil, nil, nil, nil)
	failed.Err = `{"InstructionError":[0,"Custom"]}`
	c.Mine(ok, failed)
	for sig, want := range map[string]bool{solfake.Signature("ok"): true, solfake.Signature("failed"): false, solfake.Signature("unknown"): false} {
		t.Run(sig, func(t *testing.T) {
			got, err := q.Landed(ctx, sig)
			if err != nil || got != want {
				t.Errorf("%s: %v %v, want %v", sig, got, err, want)
			}
		})
	}
	down := solwatch.NewQuorum("solana-devnet", []solwatch.Provider{c.Provider("a"), noStatuses{c.Provider("b")}}, chainwatch.Options{})
	if got, err := down.Landed(ctx, solfake.Signature("unknown")); err == nil || got {
		t.Fatalf("one provider without an answer: %v %v, want an error", got, err)
	}
}
