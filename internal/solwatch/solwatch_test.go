package solwatch

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/chainwatch"
	"github.com/prismaport/prismaport/internal/solrpc"
)

type fakeProvider struct {
	name      string
	down      bool
	slots     [2]uint64 // confirmed, finalized
	height    uint64
	genesis   string
	blocks    map[uint64]string
	skipped   map[uint64]bool
	txs       map[string]*solrpc.Tx
	sigs      []solrpc.SignatureInfo
	accounts  map[string]solrpc.Account
	sigCalls  int
	sendCalls int
	accCalls  int
	// sigSlot is the slot this provider's signature list has reached (0: any).
	sigSlot uint64
}

func (f *fakeProvider) Name() string { return f.name }

func (f *fakeProvider) fail() error {
	if f.down {
		return fmt.Errorf("%w: %s down", solrpc.ErrTransport, f.name)
	}
	return nil
}

func (f *fakeProvider) GenesisHash(context.Context) (string, error) { return f.genesis, f.fail() }
func (f *fakeProvider) Slot(_ context.Context, cm solrpc.Commitment) (uint64, error) {
	if cm == solrpc.Finalized {
		return f.slots[1], f.fail()
	}
	return f.slots[0], f.fail()
}
func (f *fakeProvider) BlockHeight(context.Context, solrpc.Commitment) (uint64, error) {
	return f.height, f.fail()
}
func (f *fakeProvider) LatestBlockhash(context.Context, solrpc.Commitment) (solrpc.Blockhash, error) {
	return solrpc.Blockhash{Hash: "h"}, f.fail()
}
func (f *fakeProvider) Block(_ context.Context, slot uint64, _ solrpc.Commitment) (solrpc.Block, error) {
	if err := f.fail(); err != nil {
		return solrpc.Block{}, err
	}
	if f.skipped[slot] {
		return solrpc.Block{}, solrpc.ErrSkipped
	}
	h, ok := f.blocks[slot]
	if !ok {
		return solrpc.Block{}, solrpc.ErrNotFound
	}
	return solrpc.Block{Slot: slot, Blockhash: h}, nil
}
func (f *fakeProvider) Signatures(_ context.Context, _ string, o solrpc.SignaturesOpts) ([]solrpc.SignatureInfo, error) {
	f.sigCalls++
	if err := f.fail(); err != nil {
		return nil, err
	}
	if f.sigSlot > 0 && o.MinContextSlot > f.sigSlot {
		return nil, fmt.Errorf("%w: %s", solrpc.ErrBehind, f.name)
	}
	var out []solrpc.SignatureInfo
	started := o.Before == ""
	for _, s := range f.sigs {
		if !started {
			started = s.Signature == o.Before
			continue
		}
		if s.Signature == o.Until {
			break
		}
		out = append(out, s)
		if len(out) == o.Limit {
			break
		}
	}
	return out, nil
}
func (f *fakeProvider) Transaction(_ context.Context, sig string, _ solrpc.Commitment) (*solrpc.Tx, error) {
	if err := f.fail(); err != nil {
		return nil, err
	}
	tx, ok := f.txs[sig]
	if !ok {
		return nil, solrpc.ErrNotFound
	}
	return tx, nil
}
func (f *fakeProvider) Account(ctx context.Context, addr string, cm solrpc.Commitment, min uint64) (solrpc.Account, error) {
	a, err := f.MultipleAccounts(ctx, []string{addr}, cm, min)
	if err != nil {
		return solrpc.Account{}, err
	}
	return a[0], nil
}
func (f *fakeProvider) MultipleAccounts(_ context.Context, addrs []string, _ solrpc.Commitment, minSlot uint64) ([]solrpc.Account, error) {
	f.accCalls++
	if err := f.fail(); err != nil {
		return nil, err
	}
	var out []solrpc.Account
	for _, a := range addrs {
		if f.accounts[a].ContextSlot < minSlot {
			return nil, fmt.Errorf("%w: %s", solrpc.ErrBehind, f.name)
		}
		out = append(out, f.accounts[a])
	}
	return out, nil
}
func (f *fakeProvider) Balance(context.Context, string, solrpc.Commitment) (uint64, error) {
	return 1, f.fail()
}
func (f *fakeProvider) MinimumBalanceForRentExemption(context.Context, int) (uint64, error) {
	return 1, f.fail()
}
func (f *fakeProvider) SendTransaction(context.Context, []byte, bool) (string, error) {
	f.sendCalls++
	return "sig", f.fail()
}
func (f *fakeProvider) Simulate(context.Context, []byte, bool) (solrpc.Simulation, error) {
	return solrpc.Simulation{}, f.fail()
}
func (f *fakeProvider) SignatureStatuses(_ context.Context, sigs []string) ([]*solrpc.SignatureStatus, error) {
	return make([]*solrpc.SignatureStatus, len(sigs)), f.fail()
}

func (f *fakeProvider) IsBlockhashValid(context.Context, string) (bool, error) { return true, f.fail() }

func pair() (*fakeProvider, *fakeProvider, *fakeProvider) {
	mk := func(n string) *fakeProvider {
		return &fakeProvider{name: n, slots: [2]uint64{100, 68}, height: 90, genesis: "G", blocks: map[uint64]string{50: "B50"},
			skipped: map[uint64]bool{51: true}, txs: map[string]*solrpc.Tx{"s1": {Slot: 50, PreToken: []solrpc.TokenBalance{{Amount: "1"}}}},
			accounts: map[string]solrpc.Account{"acc": {ContextSlot: 70, Exists: true, Data: []byte{1}}}}
	}
	return mk("a"), mk("b"), mk("c")
}

func quorum(ps ...*fakeProvider) (*Quorum, *[]chainwatch.Status) {
	var seen []chainwatch.Status
	var provs []Provider
	for _, p := range ps {
		provs = append(provs, p)
	}
	return NewQuorum("sol", provs, chainwatch.Options{OnStatus: func(s chainwatch.Status) { seen = append(seen, s) }}), &seen
}

func TestAgreementAndDisagreement(t *testing.T) {
	ctx := t.Context()
	a, b, _ := pair()
	q, seen := quorum(a, b)
	s, err := q.Slots(ctx)
	if err != nil || s.Confirmed != 100 || s.Finalized != 68 || s.BlockHeight != 90 {
		t.Fatalf("slots %+v %v", s, err)
	}
	b.slots = [2]uint64{98, 66}
	if s, _ := q.Slots(ctx); s.Confirmed != 98 || s.Finalized != 66 {
		t.Fatalf("slots take the minimum: %+v", s)
	}
	if h, err := q.BlockHash(ctx, 50, solrpc.Finalized); err != nil || h != "B50" {
		t.Fatalf("block %s %v", h, err)
	}
	if _, err := q.BlockHash(ctx, 51, solrpc.Finalized); !errors.Is(err, solrpc.ErrSkipped) {
		t.Fatalf("skipped: %v", err)
	}
	if _, err := q.BlockHash(ctx, 52, solrpc.Finalized); !errors.Is(err, solrpc.ErrNotFound) {
		t.Fatalf("both missing: %v", err)
	}
	b.blocks[52] = "B52"
	if _, err := q.BlockHash(ctx, 52, solrpc.Finalized); !errors.Is(err, ErrLag) {
		t.Fatalf("one missing is lag: %v", err)
	}
	if tx, err := q.Transaction(ctx, "s1", solrpc.Confirmed); err != nil || tx.Slot != 50 {
		t.Fatalf("tx %v", err)
	}
	if q.Status().State != chainwatch.OK {
		t.Fatalf("status %+v", q.Status())
	}
	b.txs["s1"] = &solrpc.Tx{Slot: 50, PreToken: []solrpc.TokenBalance{{Amount: "2"}}}
	if _, err := q.Transaction(ctx, "s1", solrpc.Confirmed); !errors.Is(err, ErrDisagreement) {
		t.Fatalf("different transactions: %v", err)
	}
	if st := q.Status(); st.State != chainwatch.Halted || len(*seen) == 0 {
		t.Fatalf("not halted: %+v", st)
	}
	b.blocks[50] = "OTHER"
	if _, err := q.BlockHash(ctx, 50, solrpc.Finalized); !errors.Is(err, ErrDisagreement) {
		t.Fatalf("different block hashes: %v", err)
	}
	b.skipped[50] = true
	if _, err := q.BlockHash(ctx, 50, solrpc.Finalized); !errors.Is(err, ErrDisagreement) {
		t.Fatalf("skipped vs produced: %v", err)
	}
}

func TestAccountsLagVersusConflict(t *testing.T) {
	ctx := t.Context()
	a, b, _ := pair()
	q, _ := quorum(a, b)
	if acc, err := q.Account(ctx, "acc", solrpc.Finalized, 0); err != nil || !acc.Exists {
		t.Fatalf("agreed account %v", err)
	}
	b.accounts["acc"] = solrpc.Account{ContextSlot: 71, Exists: true, Data: []byte{2}}
	if _, err := q.Account(ctx, "acc", solrpc.Finalized, 0); !errors.Is(err, ErrLag) {
		t.Fatalf("different slots must be lag: %v", err)
	}
	b.accounts["acc"] = solrpc.Account{ContextSlot: 70, Exists: true, Data: []byte{2}}
	if _, err := q.Account(ctx, "acc", solrpc.Finalized, 0); !errors.Is(err, ErrDisagreement) {
		t.Fatalf("same slot, different data: %v", err)
	}
}

func TestFailoverAndNoQuorum(t *testing.T) {
	ctx := t.Context()
	a, b, c := pair()
	now := time.Unix(1_000, 0)
	q := NewQuorum("sol", []Provider{a, b, c}, chainwatch.Options{Now: func() time.Time { return now }, Cooldown: time.Minute})
	a.down = true
	if _, err := q.Slots(ctx); err != nil {
		t.Fatalf("failover: %v", err)
	}
	if st := q.Status(); st.State != chainwatch.Degraded {
		t.Fatalf("status %+v", st)
	}
	b.down = true
	if _, err := q.Slots(ctx); !errors.Is(err, ErrNoQuorum) {
		t.Fatalf("one provider left: %v", err)
	}
	a.down, b.down = false, false
	now = now.Add(2 * time.Minute)
	if _, err := q.Slots(ctx); err != nil {
		t.Fatalf("recovered: %v", err)
	}
	if err := q.VerifyGenesis(ctx, "G"); err != nil {
		t.Fatal(err)
	}
	c.genesis = "devnet"
	if err := q.VerifyGenesis(ctx, "G"); err == nil {
		t.Fatal("wrong cluster accepted")
	}
	accepted, _ := q.Broadcast(ctx, []byte{1})
	if accepted != 3 || a.sendCalls != 1 || c.sendCalls != 1 {
		t.Fatalf("broadcast to every provider: %d", accepted)
	}
}

func TestSignaturesUnionAndPaging(t *testing.T) {
	ctx := t.Context()
	a, b, _ := pair()
	for i := 9; i >= 1; i-- {
		a.sigs = append(a.sigs, solrpc.SignatureInfo{Signature: fmt.Sprint("s", i), Slot: uint64(i * 10)})
	}
	b.sigs = append([]solrpc.SignatureInfo{{Signature: "only-b", Slot: 95}}, a.sigs[1:]...)
	q, _ := quorum(a, b)
	got, err := q.Signatures(ctx, "addr", "s3", 30, 2)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range got {
		names = append(names, s.Signature)
	}
	if fmt.Sprint(names) != "[only-b s9 s8 s7 s6 s5 s4]" {
		t.Fatalf("union %v", names)
	}
	if a.sigCalls < 3 {
		t.Fatalf("pages not followed: %d calls", a.sigCalls)
	}
	// A cursor that vanished from the history stops at its slot instead of walking everything.
	got, _ = q.Signatures(ctx, "addr", "gone", 60, 100)
	if len(got) != 5 {
		t.Fatalf("stopped at the cursor slot: %d", len(got))
	}
}

// A provider behind the minContextSlot asked for hands a single-provider read to the next one,
// without a cooldown; only when every provider is behind is ErrBehind returned.
func TestRotorFailsOverOnAProviderBehind(t *testing.T) {
	ctx := t.Context()
	a, b, _ := pair()
	b.accounts["acc"] = solrpc.Account{ContextSlot: 69, Exists: true, Data: []byte{1}}
	q, _ := quorum(a, b)
	r := q.Rotor()
	for i := range 4 {
		accs, err := r.Accounts(ctx, []string{"acc"}, 70)
		if err != nil || accs[0].ContextSlot != 70 {
			t.Fatalf("read %d: %v %v", i, accs, err)
		}
	}
	if b.accCalls != 2 || a.accCalls != 4 {
		t.Fatalf("calls a %d, b %d: b must be asked on its turns and handed over", a.accCalls, b.accCalls)
	}
	if st := q.Status(); st.State == chainwatch.Degraded {
		t.Fatalf("a provider a slot behind was cooled down: %+v", st)
	}
	if _, err := r.Accounts(ctx, []string{"acc"}, 71); !errors.Is(err, solrpc.ErrBehind) {
		t.Fatalf("every provider behind: %v", err)
	}
}

// A signature listing asked at a slot reports whether either provider reached it.
func TestSignaturesAtReportsWhetherTheSlotWasReached(t *testing.T) {
	ctx := t.Context()
	a, b, _ := pair()
	a.sigs = []solrpc.SignatureInfo{{Signature: "s1", Slot: 50}}
	b.sigs = a.sigs
	a.sigSlot, b.sigSlot = 60, 80
	q, _ := quorum(a, b)
	if got, reached, err := q.SignaturesAt(ctx, "addr", "", 0, 10, 70); err != nil || !reached || len(got) != 1 {
		t.Fatalf("one provider reached the slot: %v %v %v", got, reached, err)
	}
	if _, reached, err := q.SignaturesAt(ctx, "addr", "", 0, 10, 90); err != nil || reached {
		t.Fatalf("neither reached the slot: %v %v", reached, err)
	}
}
