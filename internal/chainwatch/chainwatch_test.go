package chainwatch_test

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/chainwatch"
	"github.com/prismaport/prismaport/internal/destination"
	"github.com/prismaport/prismaport/internal/evm"
	"github.com/prismaport/prismaport/internal/evmfake"
	"github.com/prismaport/prismaport/internal/evmrpc"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func setup(t *testing.T, n int) (*evmfake.Chain, []*evmfake.Provider, *chainwatch.Quorum, *clock, *[]chainwatch.Status) {
	t.Helper()
	c := evmfake.New(8453)
	var ps []*evmfake.Provider
	var providers []chainwatch.Provider
	for i := range n {
		p := c.Provider(string(rune('a' + i)))
		ps = append(ps, p)
		providers = append(providers, p)
	}
	clk := &clock{t: time.Unix(1_800_000_000, 0)}
	var statuses []chainwatch.Status
	q := chainwatch.NewQuorum("base", providers, chainwatch.Options{Cooldown: 10 * time.Second, MaxCooldown: 40 * time.Second,
		Now: clk.now, OnStatus: func(s chainwatch.Status) { statuses = append(statuses, s) }})
	return c, ps, q, clk, &statuses
}

var token = evm.Address{0xaa}

func transferLog(to evm.Address, amount uint64) evmfake.LogSpec {
	return evmfake.LogSpec{Address: token, Topics: []evm.Word{evm.Topic("Transfer(address,address,uint256)"), {}, evm.AddressWord(to)},
		Data: func() []byte { w := evm.Uint64Word(amount); return w[:] }()}
}

func TestAgreementOnHeadsHashesAndLogs(t *testing.T) {
	c, ps, q, _, _ := setup(t, 2)
	ctx := t.Context()
	c.MineN(5)
	c.Mine(transferLog(evm.Address{1}, 25))
	c.MineN(3)
	ps[1].SetLag(2)
	h, err := q.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if h.Latest != 7 || h.Safe != 5 || h.Finalized != 3 {
		t.Fatalf("head must be the minimum of both providers: %+v", h)
	}
	hash, err := q.BlockHash(ctx, 6)
	if b, _ := c.Block(6); err != nil || hash != b.Hash {
		t.Fatalf("block hash: %v", err)
	}
	logs, err := q.Logs(ctx, evmrpc.Filter{FromBlock: 1, ToBlock: 7, Addresses: []evm.Address{token}})
	if err != nil || len(logs) != 1 || logs[0].BlockNumber != 6 {
		t.Fatalf("logs %+v %v", logs, err)
	}
	if _, err := q.BlockHash(ctx, 9); !errors.Is(err, evmrpc.ErrNotFound) {
		t.Fatalf("one provider lagging is not-found, not a disagreement: %v", err)
	}
	if q.Status().State != chainwatch.OK {
		t.Fatalf("status %+v", q.Status())
	}
}

func TestDisagreementHalts(t *testing.T) {
	c, ps, q, _, statuses := setup(t, 2)
	ctx := t.Context()
	c.MineN(3)
	fork := c.Fork()
	c.Mine(transferLog(evm.Address{1}, 25))
	fork.Mine()
	ps[1].Point(fork)
	if _, err := q.BlockHash(ctx, 4); !errors.Is(err, chainwatch.ErrDisagreement) {
		t.Fatalf("diverging hashes: %v", err)
	}
	if _, err := q.Logs(ctx, evmrpc.Filter{FromBlock: 4, ToBlock: 4}); !errors.Is(err, chainwatch.ErrDisagreement) {
		t.Fatalf("diverging logs: %v", err)
	}
	if st := q.Status(); st.State != chainwatch.Halted {
		t.Fatalf("status %+v", st)
	}
	if hash, err := q.BlockHash(ctx, 3); err != nil || hash == (evm.Word{}) {
		t.Fatalf("shared history still agrees: %v", err)
	}
	if st := q.Status(); st.State != chainwatch.OK {
		t.Fatalf("recovered status %+v", st)
	}
	if len(*statuses) < 2 {
		t.Fatalf("status changes not reported: %+v", *statuses)
	}
}

func TestFailoverAndCooldown(t *testing.T) {
	c, ps, q, clk, _ := setup(t, 3)
	ctx := t.Context()
	c.MineN(2)
	ps[0].SetDown(true)
	if _, err := q.BlockHash(ctx, 1); err != nil {
		t.Fatalf("third provider must replace the failed one: %v", err)
	}
	if st := q.Status(); st.State != chainwatch.Degraded {
		t.Fatalf("status %+v", st)
	}
	before := ps[0].Calls("eth_getBlockByNumber")
	if _, err := q.BlockHash(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if ps[0].Calls("eth_getBlockByNumber") != before {
		t.Fatal("provider in cooldown was called")
	}
	ps[0].SetDown(false)
	clk.add(11 * time.Second)
	if _, err := q.BlockHash(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if ps[0].Calls("eth_getBlockByNumber") == before {
		t.Fatal("provider not retried after cooldown")
	}
	if st := q.Status(); st.State != chainwatch.OK {
		t.Fatalf("status after recovery %+v", st)
	}

	ps[0].SetDown(true)
	ps[1].SetDown(true)
	if _, err := q.BlockHash(ctx, 1); !errors.Is(err, chainwatch.ErrNoQuorum) {
		t.Fatalf("one healthy provider must not be enough: %v", err)
	}
	if q.Status().State != chainwatch.Halted {
		t.Fatal("no quorum must halt")
	}
	// Cooldown doubles per consecutive failure and is capped.
	ps[1].SetDown(false)
	clk.add(11 * time.Second)
	if _, err := q.BlockHash(ctx, 1); err != nil {
		t.Fatalf("p1 back after its first cooldown: %v", err)
	}
}

func TestOneProviderRPCErrorFailsOver(t *testing.T) {
	c, ps, q, _, _ := setup(t, 3)
	ctx := t.Context()
	c.MineN(4)
	ps[0].SetLag(3) // its getLogs rejects a range beyond its head
	if _, err := q.Logs(ctx, evmrpc.Filter{FromBlock: 1, ToBlock: 4}); err != nil {
		t.Fatalf("lagging backend must fail over, not halt: %v", err)
	}
}

func TestCreditPolicy(t *testing.T) {
	h := chainwatch.Head{Latest: 100, Safe: 90, Finalized: 80}
	cases := []struct {
		p    chainwatch.CreditPolicy
		n    uint64
		want bool
	}{
		{chainwatch.CreditPolicy{Confirmations: 1}, 100, true},
		{chainwatch.CreditPolicy{Confirmations: 2}, 100, false},
		{chainwatch.CreditPolicy{Confirmations: 2}, 99, true},
		{chainwatch.CreditPolicy{}, 100, true},
		{chainwatch.CreditPolicy{Confirmations: 5}, 101, false},
		{chainwatch.CreditPolicy{UseSafeHead: true}, 90, true},
		{chainwatch.CreditPolicy{UseSafeHead: true}, 91, false},
	}
	for _, c := range cases {
		if got := c.p.Confirmed(c.n, h); got != c.want {
			t.Fatalf("%+v block %d: %v", c.p, c.n, got)
		}
	}
}

func TestBroadcastNonceAndHelpers(t *testing.T) {
	c, ps, q, clk, _ := setup(t, 2)
	ctx := t.Context()
	wallet := evm.Address{7}
	c.SetNonce(wallet, 5)
	if n, err := q.PendingNonce(ctx, wallet); err != nil || n != 5 {
		t.Fatalf("nonce %d %v", n, err)
	}
	ps[1].SetDown(true)
	n, errs := q.Broadcast(ctx, []byte{1})
	if n != 1 || len(errs) != 1 {
		t.Fatalf("broadcast accepted %d errs %v", n, errs)
	}
	if !chainwatch.AlreadyKnown(&evmrpc.RPCError{Message: "ALREADY KNOWN"}) || chainwatch.AlreadyKnown(errors.New("already known")) {
		t.Fatal("already known detection")
	}
	if !chainwatch.NonceTooLow(&evmrpc.RPCError{Message: "nonce too low: next nonce 5"}) {
		t.Fatal("nonce too low detection")
	}
	if err := q.VerifyChainID(ctx, 8453); err == nil {
		t.Fatal("down provider passed chain id verification")
	}
	ps[1].SetDown(false)
	if err := q.VerifyChainID(ctx, 1); err == nil {
		t.Fatal("wrong chain id accepted")
	}
	if err := q.VerifyChainID(ctx, 8453); err != nil {
		t.Fatal(err)
	}
	clk.add(time.Minute)
	checker := chainwatch.CodeChecker{Quorums: map[string]*chainwatch.Quorum{"base": q}}
	if has, err := checker.HasCode(ctx, "base", "0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed"); err != nil || has {
		t.Fatalf("code: %v %v", has, err)
	}
	if _, err := checker.HasCode(ctx, "polygon", "0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed"); !errors.Is(err, chainwatch.ErrNoProviders) {
		t.Fatalf("unknown chain: %v", err)
	}
}

func TestBlacklistReadsTheTokenAtLatestWithoutHeadReads(t *testing.T) {
	c, ps, q, clk, _ := setup(t, 2)
	ctx := t.Context()
	c.MineN(3)
	listed := evm.Address{0x0b}
	var calls atomic.Int64 // the quorum asks both providers at once
	c.CallHandler = func(m evmrpc.CallMsg, block uint64) ([]byte, error) {
		calls.Add(1)
		if m.To != token || len(m.Data) != 36 || [4]byte(m.Data[:4]) != evm.Selector("isBlacklisted(address)") {
			return nil, errors.New("unexpected call")
		}
		w := evm.Uint64Word(0)
		if [20]byte(m.Data[16:36]) == listed {
			w = evm.Uint64Word(1)
		}
		return w[:], nil
	}
	b := &chainwatch.Blacklist{Quorums: map[string]*chainwatch.Quorum{"base": q}, TTL: time.Minute, Now: clk.now}
	if got, err := b.IsBlacklisted(ctx, "base", token.Hex(), listed.Hex()); err != nil || !got {
		t.Fatalf("listed address: %v %v", got, err)
	}
	if calls.Load() != 2 || ps[0].Calls("eth_getBlockByNumber")+ps[1].Calls("eth_getBlockByNumber") != 0 {
		t.Fatalf("a lookup made %d calls and %d header reads, want 2 and 0", calls.Load(),
			ps[0].Calls("eth_getBlockByNumber")+ps[1].Calls("eth_getBlockByNumber"))
	}
	clean := evm.Address{0x0c}
	if got, err := b.IsBlacklisted(ctx, "base", token.Hex(), clean.Hex()); err != nil || got {
		t.Fatalf("clean address: %v %v", got, err)
	}
	before := calls.Load()
	if got, _ := b.IsBlacklisted(ctx, "base", token.Hex(), clean.Hex()); got || calls.Load() != before {
		t.Fatalf("a clean answer within the TTL queried again (%d calls)", calls.Load()-before)
	}
	if got, _ := b.IsBlacklisted(ctx, "base", token.Hex(), listed.Hex()); !got || calls.Load() == before {
		t.Fatal("a listed answer was cached")
	}
	clk.add(2 * time.Minute)
	before = calls.Load()
	if _, err := b.IsBlacklisted(ctx, "base", token.Hex(), clean.Hex()); err != nil || calls.Load() == before {
		t.Fatal("an expired clean answer was not refreshed")
	}
	if _, err := b.IsBlacklisted(ctx, "polygon", token.Hex(), clean.Hex()); !errors.Is(err, destination.ErrNoProviders) {
		t.Fatalf("chain without providers: %v", err)
	}
	ps[1].SetDown(true)
	clk.add(2 * time.Minute)
	if _, err := b.IsBlacklisted(ctx, "base", token.Hex(), evm.Address{0x0d}.Hex()); err == nil {
		t.Fatal("a single provider answered the blacklist alone")
	}
	ps[1].SetDown(false)
	c.CallHandler = func(evmrpc.CallMsg, uint64) ([]byte, error) { w := evm.Uint64Word(2); return w[:], nil }
	clk.add(2 * time.Minute)
	if _, err := b.IsBlacklisted(ctx, "base", token.Hex(), evm.Address{0x0e}.Hex()); err == nil {
		t.Fatal("a non-boolean answer accepted")
	}
}

// A Rotor alternates single reads between the first two healthy providers and fails over to the
// next one on a provider error, cooling the failed one down.
func TestRotorAlternatesAndFailsOver(t *testing.T) {
	c, ps, q, clk, _ := setup(t, 3)
	ctx := t.Context()
	c.MineN(3)
	r := q.Rotor()
	names := ""
	for range 4 {
		if _, err := r.Header(ctx, evmrpc.Latest); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range ps {
		names += fmt.Sprint(p.Calls("eth_getBlockByNumber"))
	}
	if names != "220" {
		t.Fatalf("header reads per provider %s, want 220", names)
	}
	ps[0].SetDown(true)
	for range 2 {
		if _, err := r.Header(ctx, evmrpc.Latest); err != nil {
			t.Fatal(err)
		}
	}
	// a was asked once and failed over to b; then a cools down, the two healthy ones are b and
	// c, and the turn goes to c.
	if got := []int{ps[0].Calls("eth_getBlockByNumber"), ps[1].Calls("eth_getBlockByNumber"), ps[2].Calls("eth_getBlockByNumber")}; got[0] != 3 || got[1] != 3 || got[2] != 1 {
		t.Fatalf("after a failure: %v", got)
	}
	ps[1].SetDown(true)
	ps[2].SetDown(true)
	if _, err := r.Header(ctx, evmrpc.Latest); err == nil {
		t.Fatal("no healthy provider answered")
	}
	clk.add(time.Hour)
	for _, p := range ps {
		p.SetDown(false)
	}
	// An error that is not an answer about the chain (here: no handler) fails over to every
	// provider and is returned.
	if out, _, err := r.Call(ctx, evmrpc.CallMsg{To: token}, evmrpc.Latest); err == nil || out != nil {
		t.Fatalf("a call without a handler: %v", err)
	}
	if n := ps[0].Calls("eth_call") + ps[1].Calls("eth_call") + ps[2].Calls("eth_call"); n != 3 {
		t.Fatalf("failed call reached %d providers, want 3", n)
	}
}
