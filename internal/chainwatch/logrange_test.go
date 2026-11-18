package chainwatch_test

import (
	"math"
	"slices"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/chainwatch"
	"github.com/prismaport/prismaport/internal/evm"
	"github.com/prismaport/prismaport/internal/evmrpc"
)

func collect(from, to, limit uint64) [][2]uint64 {
	var out [][2]uint64
	for a, b := range chainwatch.LogRanges(from, to, limit) {
		out = append(out, [2]uint64{a, b})
	}
	return out
}

func TestLogRanges(t *testing.T) {
	cases := []struct {
		name            string
		from, to, limit uint64
		want            [][2]uint64
	}{
		{"single block", 7, 7, 10, [][2]uint64{{7, 7}}},
		{"narrower than the limit", 1, 5, 10, [][2]uint64{{1, 5}}},
		{"exactly the limit", 1, 10, 10, [][2]uint64{{1, 10}}},
		{"one past the limit", 1, 11, 10, [][2]uint64{{1, 10}, {11, 11}}},
		{"exact multiple", 0, 29, 10, [][2]uint64{{0, 9}, {10, 19}, {20, 29}}},
		{"remainder", 100, 124, 10, [][2]uint64{{100, 109}, {110, 119}, {120, 124}}},
		{"limit 1", 5, 8, 1, [][2]uint64{{5, 5}, {6, 6}, {7, 7}, {8, 8}}},
		{"limit 0 is read as 1", 5, 6, 0, [][2]uint64{{5, 5}, {6, 6}}},
		{"empty when from > to", 9, 8, 10, nil},
		{"ends at the top of the uint64 range", math.MaxUint64 - 2, math.MaxUint64, 2,
			[][2]uint64{{math.MaxUint64 - 2, math.MaxUint64 - 1}, {math.MaxUint64, math.MaxUint64}}},
		{"unbounded-looking limit", 3, 9, math.MaxUint64, [][2]uint64{{3, 9}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := collect(c.from, c.to, c.limit); !slices.Equal(got, c.want) {
				t.Errorf("%s: LogRanges(%d, %d, %d) = %v, want %v", c.name, c.from, c.to, c.limit, got, c.want)
			}
		})
	}
}

// A catch-up after a long gap covers every block once, in order, with no range over the limit.
func TestLogRangesLongGap(t *testing.T) {
	const from, to, limit = 18_000_001, 18_050_000, 10
	got := collect(from, to, limit)
	if len(got) != 5000 {
		t.Fatalf("%d ranges, want 5000", len(got))
	}
	next := uint64(from)
	for _, r := range got {
		if r[0] != next || r[1] < r[0] || r[1]-r[0]+1 > limit {
			t.Fatalf("range %v after block %d", r, next-1)
		}
		next = r[1] + 1
	}
	if next != to+1 {
		t.Fatalf("stopped at %d, want %d", next-1, to)
	}
	for a, b := range chainwatch.LogRanges(from, to, limit) {
		if a != from || b != from+limit-1 {
			t.Fatalf("first range [%d,%d]", a, b)
		}
		break
	}
}

func TestLogRangeEnd(t *testing.T) {
	for _, c := range []struct{ from, to, limit, want uint64 }{
		{1, 500, 500, 500}, {1, 501, 500, 500}, {1, 100, 500, 100}, {10, 10, 1, 10}, {10, 20, 1, 10}, {10, 20, 0, 10},
	} {
		if got := chainwatch.LogRangeEnd(c.from, c.to, c.limit); got != c.want {
			t.Errorf("LogRangeEnd(%d, %d, %d) = %d, want %d", c.from, c.to, c.limit, got, c.want)
		}
	}
}

// Two providers with different caps (an Alchemy-like 10 and a 200) agree once every range fits
// the stricter cap, and both are asked for exactly the same ranges.
func TestQuorumAgreesAcrossChunks(t *testing.T) {
	c, ps, q, clk, _ := setup(t, 2)
	ctx := t.Context()
	to := evm.Address{1}
	c.MineN(3)
	c.Mine(transferLog(to, 25)) // block 4
	c.MineN(10)
	c.Mine(transferLog(to, 30)) // block 15
	c.MineN(9)
	c.Mine(transferLog(to, 40)) // block 25
	c.MineN(5)                  // tip 30
	ps[0].SetMaxLogRange(10)
	ps[1].SetMaxLogRange(200)
	filter := func(a, b uint64) evmrpc.Filter {
		return evmrpc.Filter{FromBlock: a, ToBlock: b, Addresses: []evm.Address{token}}
	}

	if _, err := q.Logs(ctx, filter(1, 30)); err == nil {
		t.Fatal("a range over the stricter cap must not reach an answer")
	}
	clk.add(11 * time.Second) // the rejecting provider sits out its cooldown
	before := [2]int{len(ps[0].LogRanges()), len(ps[1].LogRanges())}

	var blocks []uint64
	for a, b := range chainwatch.LogRanges(1, 30, 10) {
		logs, err := q.Logs(ctx, filter(a, b))
		if err != nil {
			t.Fatalf("[%d,%d]: %v", a, b, err)
		}
		for _, l := range logs {
			blocks = append(blocks, l.BlockNumber)
		}
	}
	if !slices.Equal(blocks, []uint64{4, 15, 25}) {
		t.Fatalf("logs at blocks %v", blocks)
	}
	ra, rb := ps[0].LogRanges()[before[0]:], ps[1].LogRanges()[before[1]:]
	want := [][2]uint64{{1, 10}, {11, 20}, {21, 30}}
	if !slices.Equal(ra, want) || !slices.Equal(rb, want) {
		t.Fatalf("ranges asked: a %v, b %v, want both %v", ra, rb, want)
	}
}
