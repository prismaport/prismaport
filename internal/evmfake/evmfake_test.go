package evmfake

import (
	"testing"

	"github.com/prismaport/prismaport/internal/evm"
	"github.com/prismaport/prismaport/internal/evmrpc"
)

var (
	token  = evm.Address{0x70}
	holder = evm.Address{0x71}
)

func deposit(amount uint64) LogSpec {
	w := evm.Uint64Word(amount)
	return LogSpec{Address: token, Topics: []evm.Word{transferTopic, evm.AddressWord(evm.Address{0x72}), evm.AddressWord(holder)}, Data: w[:]}
}

func balanceAt(t *testing.T, p *Provider, n uint64) uint64 {
	t.Helper()
	w := evm.AddressWord(holder)
	data := append(balanceOf[:], w[:]...)
	out, err := p.Call(t.Context(), evmrpc.CallMsg{To: token, Data: data}, evmrpc.Number(n))
	if err != nil {
		t.Fatal(err)
	}
	return evm.Word(out).Big().Uint64()
}

// A log index that trails the head answers the range but leaves out the newest blocks' logs,
// while the balance at the head already shows them (the H-1 scenario).
func TestLogLagHidesTheNewestLogsButNotTheBalance(t *testing.T) {
	c := New(1)
	c.Mine(deposit(5))
	c.Mine(deposit(7))
	c.MineN(1)
	p := c.Provider("a")
	p.SetLogLag(2)
	tip := c.Tip().Number
	ls, err := p.Logs(t.Context(), evmrpc.Filter{FromBlock: 1, ToBlock: tip, Addresses: []evm.Address{token}})
	if err != nil {
		t.Fatal(err)
	}
	if len(ls) != 1 || ls[0].BlockNumber != 1 {
		t.Fatalf("lagging index answered %d logs, want only block 1's", len(ls))
	}
	if b := balanceAt(t, p, tip); b != 12 {
		t.Fatalf("balance %d, want 12 (both deposits)", b)
	}
	p.SetLogLag(0)
	if ls, _ := p.Logs(t.Context(), evmrpc.Filter{FromBlock: 1, ToBlock: tip}); len(ls) != 2 {
		t.Fatalf("caught-up index answered %d logs, want 2", len(ls))
	}
}

// The safe and finalized tags follow the chain's depths, a provider's own finality lag and a
// finality stall.
func TestFinalityTags(t *testing.T) {
	c := New(1)
	c.SafeDepth, c.FinalDepth = 2, 4
	c.MineN(20)
	a, b := c.Provider("a"), c.Provider("b")
	b.SetFinalityLag(3)
	head := func(p *Provider, tag evmrpc.BlockTag) uint64 {
		t.Helper()
		h, err := p.HeaderByTag(t.Context(), tag)
		if err != nil {
			t.Fatal(err)
		}
		return h.Number
	}
	if s, f := head(a, evmrpc.Safe), head(a, evmrpc.Finalized); s != 18 || f != 16 {
		t.Fatalf("a: safe %d finalized %d, want 18 16", s, f)
	}
	if s, f := head(b, evmrpc.Safe), head(b, evmrpc.Finalized); s != 15 || f != 13 {
		t.Fatalf("b: safe %d finalized %d, want 15 13", s, f)
	}
	c.StallFinality(10)
	c.MineN(5)
	if l, s, f := head(a, evmrpc.Latest), head(a, evmrpc.Safe), head(a, evmrpc.Finalized); l != 25 || s != 10 || f != 10 {
		t.Fatalf("stalled: latest %d safe %d finalized %d, want 25 10 10", l, s, f)
	}
	c.ResumeFinality()
	if f := head(a, evmrpc.Finalized); f != 21 {
		t.Fatalf("resumed: finalized %d, want 21", f)
	}
}
