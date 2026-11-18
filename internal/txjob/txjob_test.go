package txjob

import (
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/payout"
)

func TestBackoffDoublesUpToTheCap(t *testing.T) {
	l := &Lifecycle{BaseBackoff: 30 * time.Second, MaxBackoff: time.Hour}
	for attempts, want := range map[int]time.Duration{1: 30 * time.Second, 2: time.Minute, 4: 4 * time.Minute, 8: time.Hour, 1000: time.Hour} {
		if got := l.Backoff(attempts); got != want {
			t.Errorf("Backoff(%d) = %v, want %v", attempts, got, want)
		}
	}
}

func TestEncodeRecipients(t *testing.T) {
	got := EncodeRecipients([]payout.Recipient{{Address: "a", Bps: 9000}, {Address: "b", Bps: 1000}})
	if got != "a:9000,b:1000" {
		t.Fatalf("got %q", got)
	}
}
