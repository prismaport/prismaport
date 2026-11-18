package detector

import (
	"math/big"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/evm"
	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/store"
)

// The wait before an unexplained balance is stored is each session's: on a customer address the
// next session reads the same address and token, and the time the earlier session's balance went
// unexplained must not cut the next session's wait short.
func TestUnexplainedWaitIsPerSession(t *testing.T) {
	var u unexplained
	wait := 10 * time.Minute
	t0 := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	s1 := store.WatchEntry{SessionID: "ses_1", Address: "0xAbC", Token: "0xDeF"}
	s2 := s1
	s2.SessionID = "ses_2"
	if since := u.mark(s1, t0, wait); !since.Equal(t0) {
		t.Fatalf("first mark: %v", since)
	}
	at := t0.Add(9 * time.Minute)
	if since := u.mark(s2, at, wait); !since.Equal(at) {
		t.Fatalf("the next session's wait started at %v, want %v", since, at)
	}
	if since := u.mark(s1, at, wait); !since.Equal(t0) {
		t.Fatalf("the first session's wait restarted: %v", since)
	}
}

// Without a known baseline, a flush in the window is one: it swept the whole balance, so a
// non-zero reading is exactly what came in after the window's last flush (a deposit earlier in
// the flush's block went with it). With no flush in the window any deposit explains it.
func TestUnknownBaselineIsTheZeroAfterTheLastFlush(t *testing.T) {
	a, tok := evm.Address{0xa1}, evm.Address{0x70}
	k := [2]evm.Address{a, tok}
	r := reading{e: store.WatchEntry{Address: a.Hex(), Token: tok.Hex()}, bal: money.FromUint64(7), at: 20}
	fl := flows{k: {
		{block: 10, amount: big.NewInt(5)},
		{block: 12, index: 1, amount: big.NewInt(2)},
		{block: 12, index: 3, amount: big.NewInt(7), out: true},
	}}
	if explains(fl, r, money.Zero, 10, false) {
		t.Fatal("a flush alone explained a non-zero balance")
	}
	fl[k] = append(fl[k], flow{block: 12, index: 4, amount: big.NewInt(7)})
	if !explains(fl, r, money.Zero, 10, false) {
		t.Fatal("the deposit after the flush did not explain the balance")
	}
	fl[k] = append(fl[k], flow{block: 21, amount: big.NewInt(1)}) // after the reading
	if !explains(fl, r, money.Zero, 10, false) {
		t.Fatal("a deposit after the reading's block counted")
	}
	if !explains(flows{k: {{block: 11, amount: big.NewInt(1)}}}, r, money.Zero, 10, false) {
		t.Fatal("without a flush in the window a deposit no longer explains an unknown baseline")
	}
	if explains(flows{}, r, money.Zero, 10, false) || !explains(flows{}, reading{e: r.e, at: 20}, money.Zero, 10, false) {
		t.Fatal("no movement: a non-zero reading is not explained, a zero one is")
	}
}
