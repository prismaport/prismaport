//go:build integration

package store_test

import (
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/store"
	"github.com/prismaport/prismaport/internal/testdb"
)

// A bucket of count tokens refilled over period: 1 per 10 minutes and 30 per hour behave as the
// "Check again" limits promise, on the shared rate_limits table.
func TestTakeRateTokenPer(t *testing.T) {
	t.Parallel()
	st := store.New(testdb.Pool(t), nil)
	ctx := t.Context()
	t0 := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	take := func(key string, count int, period time.Duration, at time.Time) (bool, time.Duration) {
		t.Helper()
		ok, wait, err := st.TakeRateTokenPer(ctx, key, count, period, &at)
		if err != nil {
			t.Fatal(err)
		}
		return ok, wait
	}
	if ok, _ := take("s", 1, 10*time.Minute, t0); !ok {
		t.Fatal("first token refused")
	}
	ok, wait := take("s", 1, 10*time.Minute, t0.Add(time.Minute))
	if ok || wait != 9*time.Minute {
		t.Fatalf("second token after 1 min: %v, wait %s", ok, wait)
	}
	if ok, _ := take("s", 1, 10*time.Minute, t0.Add(10*time.Minute-time.Second)); ok {
		t.Fatal("token before 10 min")
	}
	if ok, _ := take("s", 1, 10*time.Minute, t0.Add(10*time.Minute)); !ok {
		t.Fatal("token refused after 10 min")
	}
	for i := range 30 {
		if ok, _ := take("m", 30, time.Hour, t0); !ok {
			t.Fatalf("burst token %d refused", i)
		}
	}
	ok, wait = take("m", 30, time.Hour, t0)
	if ok || wait != 2*time.Minute {
		t.Fatalf("31st token: %v, wait %s", ok, wait)
	}
	if ok, _ := take("m", 30, time.Hour, t0.Add(2*time.Minute)); !ok {
		t.Fatal("refilled token refused after 2 min")
	}
	if _, _, err := st.TakeRateTokenPer(ctx, "x", 0, time.Hour, nil); err == nil {
		t.Fatal("zero count accepted")
	}
}
