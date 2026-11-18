package checkout

import "testing"

func TestDepth(t *testing.T) {
	for _, c := range []struct{ block, head, need, want uint64 }{
		{100, 99, 10, 0},   // the scan has not reached the block
		{100, 100, 10, 1},  // the block itself is the first confirmation
		{100, 105, 10, 6},  // like CreditPolicy.Confirmed: head-block+1
		{100, 109, 10, 10}, // the block that confirms it
		{100, 500, 10, 10}, // capped at the requirement
	} {
		if got := Depth(c.block, c.head, c.need); got != c.want {
			t.Errorf("Depth(%d, %d, %d) = %d, want %d", c.block, c.head, c.need, got, c.want)
		}
	}
}
