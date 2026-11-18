package solwatch_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/chainwatch"
	"github.com/prismaport/prismaport/internal/solana"
	"github.com/prismaport/prismaport/internal/solfake"
	"github.com/prismaport/prismaport/internal/solrpc"
	"github.com/prismaport/prismaport/internal/solwatch"
)

// These tests drive the quorum against solfake's commitment levels, rolled-back slots and
// per-method error injection (ADR 0008 E-3): a read never runs ahead of its commitment, a
// rolled-back slot disappears from every view, and a provider error is never an answer.

var (
	payer = solfake.Key("payer")
	mint  = solfake.Key("mint")
	owner = solfake.Key("owner")
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newQuorum(ps ...*solfake.Provider) (*solwatch.Quorum, *clock) {
	clk := &clock{t: time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)}
	var providers []solwatch.Provider
	for _, p := range ps {
		providers = append(providers, p)
	}
	return solwatch.NewQuorum("solana-local", providers, chainwatch.Options{Now: clk.Now}), clk
}

// deposit moves amount from the payer's token account into dst (a token account of owner).
func deposit(seed string, dst solana.PublicKey, before, amount uint64) *solrpc.Tx {
	src := solfake.Key("payer-ata")
	return solfake.Tx(solfake.Signature(seed), payer, []solrpc.Instruction{solfake.Transfer(src, dst, payer, amount)}, nil,
		[]solfake.TokenBalance{{Account: src, Mint: mint, Owner: payer, Amount: 1_000_000}, {Account: dst, Mint: mint, Owner: owner, Amount: before}},
		[]solfake.TokenBalance{{Account: src, Mint: mint, Owner: payer, Amount: 1_000_000 - amount}, {Account: dst, Mint: mint, Owner: owner, Amount: before + amount}})
}

func tokenAmount(t *testing.T, q *solwatch.Quorum, addr solana.PublicKey, cm solrpc.Commitment) (uint64, bool) {
	t.Helper()
	acc, err := q.Account(t.Context(), addr.String(), cm, 0)
	if err != nil {
		t.Fatalf("account %s at %s: %v", addr, cm, err)
	}
	if !acc.Exists {
		return 0, false
	}
	amt, _, _, ok := solana.TokenAmount(acc.Data)
	if !ok {
		t.Fatalf("account %s is not a token account", addr)
	}
	return amt, true
}

func signatures(t *testing.T, q *solwatch.Quorum, addr solana.PublicKey) []solrpc.SignatureInfo {
	t.Helper()
	sigs, err := q.Signatures(t.Context(), addr.String(), "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	return sigs
}

func TestCommitmentLevels(t *testing.T) {
	ctx := t.Context()
	c := solfake.New()
	c.ConfirmDepth, c.FinalDepth = 2, 3
	c.MineN(10)
	a, b := c.Provider("a"), c.Provider("b")
	q, _ := newQuorum(a, b)

	s, err := q.Slots(ctx)
	if err != nil || s.Confirmed != 8 || s.Finalized != 5 || s.BlockHeight != 8 {
		t.Fatalf("slots %+v %v, want confirmed 8, finalized 5", s, err)
	}
	if p, err := a.Slot(ctx, solrpc.Processed); err != nil || p != 10 {
		t.Fatalf("processed slot %d %v, want 10", p, err)
	}

	dst := solfake.Key("dst")
	tx := deposit("processed", dst, 0, 25)
	sig := tx.Signatures[0]
	slot := c.Mine(tx)

	// Processed only: nothing at confirmed sees it, though the status reports it.
	if _, err := q.Transaction(ctx, sig, solrpc.Confirmed); !errors.Is(err, solrpc.ErrNotFound) {
		t.Fatalf("processed-only transaction at confirmed: %v, want not found", err)
	}
	if got := signatures(t, q, dst); len(got) != 0 {
		t.Fatalf("processed-only signature listed at confirmed: %+v", got)
	}
	if _, ok := tokenAmount(t, q, dst, solrpc.Confirmed); ok {
		t.Fatal("account created by a processed-only slot visible at confirmed")
	}
	if amt, ok := tokenAmount(t, q, dst, solrpc.Processed); !ok || amt != 25 {
		t.Fatalf("processed read %d %v, want 25", amt, ok)
	}
	sts, err := q.SignatureStatuses(ctx, []string{sig})
	if err != nil || sts[0] == nil || sts[0].ConfirmationStatus != "processed" {
		t.Fatalf("status %+v %v, want processed", sts, err)
	}
	if _, err := q.BlockHash(ctx, slot, solrpc.Confirmed); !errors.Is(err, solrpc.ErrNotFound) {
		t.Fatalf("processed-only block at confirmed: %v, want not found", err)
	}

	c.Confirm(slot)
	if got, err := q.Transaction(ctx, sig, solrpc.Confirmed); err != nil || got.Slot != slot {
		t.Fatalf("confirmed transaction %+v %v", got, err)
	}
	if _, err := q.Transaction(ctx, sig, solrpc.Finalized); !errors.Is(err, solrpc.ErrNotFound) {
		t.Fatalf("confirmed transaction at finalized: %v, want not found", err)
	}
	if got := signatures(t, q, dst); len(got) != 1 || got[0].ConfirmationStatus != "confirmed" {
		t.Fatalf("signatures at confirmed %+v", got)
	}
	if amt, ok := tokenAmount(t, q, dst, solrpc.Confirmed); !ok || amt != 25 {
		t.Fatalf("confirmed read %d %v, want 25", amt, ok)
	}
	if _, ok := tokenAmount(t, q, dst, solrpc.Finalized); ok {
		t.Fatal("account visible at finalized before its slot is final")
	}

	c.Finalize(slot)
	if _, err := q.Transaction(ctx, sig, solrpc.Finalized); err != nil {
		t.Fatalf("finalized transaction: %v", err)
	}
	if h, err := q.BlockHash(ctx, slot, solrpc.Finalized); err != nil || h == "" {
		t.Fatalf("finalized block %q %v", h, err)
	}
	if sts, err := q.SignatureStatuses(ctx, []string{sig}); err != nil || sts[0].ConfirmationStatus != "finalized" {
		t.Fatalf("status %+v %v, want finalized", sts, err)
	}
	if amt, ok := tokenAmount(t, q, dst, solrpc.Finalized); !ok || amt != 25 {
		t.Fatalf("finalized read %d %v, want 25", amt, ok)
	}
}

func TestRollbackUndoesTheAbandonedFork(t *testing.T) {
	ctx := t.Context()
	c := solfake.New()
	c.FinalDepth = 4
	c.MineN(8)
	dst := solfake.Key("dst")
	c.SetTokenAccount(dst, mint, owner, 10)
	q, _ := newQuorum(c.Provider("a"), c.Provider("b"))

	tx := deposit("rolled-back", dst, 10, 25)
	sig := tx.Signatures[0]
	slot := c.Mine(tx)
	c.MineN(1)
	if amt, _ := tokenAmount(t, q, dst, solrpc.Confirmed); amt != 35 {
		t.Fatalf("balance before the rollback %d, want 35", amt)
	}

	c.Rollback(slot)
	for _, s := range []uint64{slot, slot + 1} {
		if _, err := q.BlockHash(ctx, s, solrpc.Confirmed); !errors.Is(err, solrpc.ErrSkipped) {
			t.Fatalf("rolled-back slot %d: %v, want skipped", s, err)
		}
	}
	if _, err := q.Transaction(ctx, sig, solrpc.Confirmed); !errors.Is(err, solrpc.ErrNotFound) {
		t.Fatalf("rolled-back transaction: %v, want not found", err)
	}
	if got := signatures(t, q, dst); len(got) != 0 {
		t.Fatalf("rolled-back signature still listed: %+v", got)
	}
	if landed, err := q.Landed(ctx, sig); err != nil || landed {
		t.Fatalf("rolled-back transaction landed=%v %v", landed, err)
	}
	if amt, ok := tokenAmount(t, q, dst, solrpc.Confirmed); !ok || amt != 10 {
		t.Fatalf("balance after the rollback %d %v, want the 10 it had before", amt, ok)
	}

	// The surviving fork builds on top with new slots.
	next := c.Mine(deposit("survivor", dst, 10, 5))
	if next <= slot+1 {
		t.Fatalf("surviving fork reused slot %d", next)
	}
	if h, err := q.BlockHash(ctx, next, solrpc.Confirmed); err != nil || h == "" {
		t.Fatalf("surviving block %q %v", h, err)
	}
	if amt, _ := tokenAmount(t, q, dst, solrpc.Confirmed); amt != 15 {
		t.Fatalf("balance on the surviving fork %d, want 15", amt)
	}

	c.Finalize(next)
	defer func() {
		if recover() == nil {
			t.Fatal("rolling back a finalized slot did not panic")
		}
	}()
	c.Rollback(next)
}

func TestInjectedErrorsNeverAnswer(t *testing.T) {
	ctx := t.Context()
	c := solfake.New()
	c.MineN(5)
	dst := solfake.Key("dst")
	tx := deposit("pay", dst, 0, 25)
	sig := tx.Signatures[0]
	slot := c.Mine(tx)

	t.Run("transport error with two providers is no quorum", func(t *testing.T) {
		a, b := c.Provider("a"), c.Provider("b")
		q, clk := newQuorum(a, b)
		a.FailNext("Transaction", 1, nil)
		if got, err := q.Transaction(ctx, sig, solrpc.Confirmed); !errors.Is(err, solwatch.ErrNoQuorum) || got != nil {
			t.Fatalf("transaction %+v %v, want no quorum", got, err)
		}
		if st := q.Status(); st.State != chainwatch.Halted {
			t.Fatalf("status %+v, want halted", st)
		}
		// The fault fired once; after the cooldown the providers answer again.
		clk.Advance(time.Minute)
		if got, err := q.Transaction(ctx, sig, solrpc.Confirmed); err != nil || got.Slot != slot {
			t.Fatalf("after the cooldown: %+v %v", got, err)
		}
	})

	t.Run("a third provider takes over", func(t *testing.T) {
		a, b, d := c.Provider("a"), c.Provider("b"), c.Provider("d")
		q, _ := newQuorum(a, b, d)
		a.FailNext("Transaction", 1, nil)
		if got, err := q.Transaction(ctx, sig, solrpc.Confirmed); err != nil || got.Slot != slot {
			t.Fatalf("transaction %+v %v", got, err)
		}
		if d.Calls("Transaction") != 1 {
			t.Fatalf("the spare provider was not asked: %d calls", d.Calls("Transaction"))
		}
	})

	t.Run("one provider not finding it is lag", func(t *testing.T) {
		a, b := c.Provider("a"), c.Provider("b")
		q, _ := newQuorum(a, b)
		b.FailNext("Transaction", 1, solrpc.ErrNotFound)
		if got, err := q.Transaction(ctx, sig, solrpc.Confirmed); !errors.Is(err, solwatch.ErrLag) || got != nil {
			t.Fatalf("transaction %+v %v, want lag", got, err)
		}
	})

	t.Run("a false skipped slot halts", func(t *testing.T) {
		a, b := c.Provider("a"), c.Provider("b")
		q, _ := newQuorum(a, b)
		a.FailNext("Block", 1, solrpc.ErrSkipped)
		if _, err := q.BlockHash(ctx, slot, solrpc.Confirmed); !errors.Is(err, solwatch.ErrDisagreement) {
			t.Fatalf("block hash: %v, want a disagreement", err)
		}
		if st := q.Status(); st.State != chainwatch.Halted {
			t.Fatalf("status %+v, want halted", st)
		}
	})

	t.Run("statuses that fail are not 'not landed'", func(t *testing.T) {
		a, b := c.Provider("a"), c.Provider("b")
		q, _ := newQuorum(a, b)
		b.FailNext("SignatureStatuses", 1, nil)
		if landed, err := q.Landed(ctx, solfake.Signature("unknown")); err == nil || landed {
			t.Fatalf("landed=%v %v, want an error", landed, err)
		}
	})

	t.Run("a node error on account reads", func(t *testing.T) {
		a, b := c.Provider("a"), c.Provider("b")
		q, _ := newQuorum(a, b)
		a.FailNext("MultipleAccounts", 1, &solrpc.RPCError{Code: -32005, Message: "node is behind"})
		if accs, err := q.Accounts(ctx, []string{dst.String()}, solrpc.Confirmed, 0); err == nil || accs != nil {
			t.Fatalf("accounts %+v %v, want an error", accs, err)
		}
	})

	t.Run("an any-method fault covers every call", func(t *testing.T) {
		a := c.Provider("a")
		a.FailNext("", 2, nil)
		if _, err := a.Slot(ctx, solrpc.Confirmed); !errors.Is(err, solrpc.ErrTransport) {
			t.Fatalf("slot: %v", err)
		}
		if _, err := a.Signatures(ctx, dst.String(), solrpc.SignaturesOpts{}); !errors.Is(err, solrpc.ErrTransport) {
			t.Fatalf("signatures: %v", err)
		}
		if _, err := a.Slot(ctx, solrpc.Confirmed); err != nil {
			t.Fatalf("the fault outlived its two calls: %v", err)
		}
	})
}
