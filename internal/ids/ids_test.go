package ids

import (
	"sort"
	"testing"
	"time"
)

func TestFormatAndOrder(t *testing.T) {
	base := time.Date(2026, 10, 23, 18, 0, 0, 0, time.UTC)
	var got []string
	for i := 0; i < 50; i++ {
		id := NewAt(Session, base.Add(time.Duration(i)*time.Millisecond))
		if !Valid(Session, id) {
			t.Fatalf("invalid id %q", id)
		}
		got = append(got, id)
	}
	if !sort.StringsAreSorted(got) {
		t.Fatal("ids are not time ordered")
	}
	if Valid(Product, got[0]) || Valid(Session, "ses_short") || Valid(Session, "ses_"+"8ZZZZZZZZZZZZZZZZZZZZZZZZZ") {
		t.Fatal("Valid accepted a malformed id")
	}
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := NewAt(Event, base)
		if seen[id] {
			t.Fatal("collision")
		}
		seen[id] = true
	}
}

func TestKnownTimestamp(t *testing.T) {
	// The ULID spec example timestamp 1469922850259 encodes as 01ARZ3NDEK (canonical ULID spec example).
	id := NewAt("x", time.UnixMilli(1469922850259))
	if id[2:12] != "01ARZ3NDEK" {
		t.Fatalf("timestamp prefix %s", id[2:12])
	}
}
