package chainpause

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSet(t *testing.T) {
	s := NewSet(Pause{Chain: "base"}, Pause{Chain: "solana", Asset: "USDT"})
	for _, c := range []struct {
		chain, asset string
		paused       bool
	}{
		{"base", "USDC", true},
		{"base", "USDe", true},
		{"solana", "USDT", true},
		{"solana", "USDC", false},
		{"base-sepolia", "USDC", false},
	} {
		t.Run(c.chain, func(t *testing.T) {
			if got := s.Paused(c.chain, c.asset); got != c.paused {
				t.Errorf("Paused(%s, %s) = %v", c.chain, c.asset, got)
			}
		})
	}
	if !s.ChainPaused("base") || s.ChainPaused("solana") {
		t.Error("ChainPaused counts only a whole-chain pause")
	}
	if !(Set{}).Empty() || (Set{}).Paused("base", "USDC") || s.Empty() {
		t.Error("the zero Set pauses nothing")
	}
}

type fakeSource struct {
	list  []Pause
	err   error
	reads int
}

func (f *fakeSource) read(context.Context) ([]Pause, error) {
	f.reads++
	return f.list, f.err
}

func TestReaderRefresh(t *testing.T) {
	// Arrange
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	src := &fakeSource{}
	r := NewReader(src.read, 30*time.Second, func() time.Time { return now })
	ctx := t.Context()

	// Act, Assert: the first call reads, later ones within the interval serve the copy.
	if s, err := r.Set(ctx); err != nil || !s.Empty() || src.reads != 1 {
		t.Fatalf("first read: %v %v %d", s, err, src.reads)
	}
	src.list = []Pause{{Chain: "base"}}
	now = now.Add(29 * time.Second)
	if s, _ := r.Set(ctx); !s.Empty() || src.reads != 1 {
		t.Fatalf("within the interval: read %d times, paused %v", src.reads, !s.Empty())
	}
	now = now.Add(time.Second)
	if s, _ := r.Set(ctx); !s.Paused("base", "USDC") || src.reads != 2 {
		t.Fatalf("after the interval: read %d times, %v", src.reads, s)
	}
	// A failed read keeps the last copy and tries again on the next call.
	src.err = errors.New("db down")
	var stale error
	r.Stale = func(err error) { stale = err }
	now = now.Add(time.Minute)
	if s, err := r.Set(ctx); err != nil || !s.Paused("base", "USDC") || stale == nil {
		t.Fatalf("failed read: %v %v %v", s, err, stale)
	}
	src.err, src.list = nil, nil
	if s, _ := r.Set(ctx); !s.Empty() || src.reads != 4 {
		t.Fatalf("retry after a failure: read %d times, %v", src.reads, s)
	}
	// A clock that went back reads again rather than trusting the copy.
	now = now.Add(-time.Hour)
	_, _ = r.Set(ctx)
	if src.reads != 5 {
		t.Fatalf("clock went back: read %d times", src.reads)
	}
}

func TestReaderZeroRefreshAndFirstFailure(t *testing.T) {
	src := &fakeSource{err: errors.New("db down")}
	r := NewReader(src.read, 0, nil)
	if _, err := r.Set(t.Context()); err == nil {
		t.Fatal("a reader that never read hides the error")
	}
	src.err = nil
	_, _ = r.Set(t.Context())
	_, _ = r.Set(t.Context())
	if src.reads != 3 {
		t.Fatalf("refresh 0 reads every call: %d", src.reads)
	}
	var nilReader *Reader
	if s, err := nilReader.Set(t.Context()); err != nil || !s.Empty() {
		t.Fatal("a nil reader pauses nothing")
	}
}
