package store

import (
	"testing"
	"time"
)

// Outcomes apply in attempt start order; warning and disable come only on failures, the disable
// only a day after the warning; disabled and inbox endpoints have no streak.
func TestStreakStep(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { v := t0.Add(d); return &v }
	day := 24 * time.Hour
	for _, c := range []struct {
		name      string
		st        streak
		succeeded bool
		started   time.Time
		want      streakStep
	}{
		{"first failure opens", streak{enabled: true}, false, t0, streakOpen},
		{"success closes", streak{enabled: true, failingSince: at(0), outcomeStartedAt: at(0)}, true, t0.Add(time.Minute), streakClose},
		{"a slow success cannot close a newer streak", streak{enabled: true, failingSince: at(time.Hour), outcomeStartedAt: at(time.Hour)},
			true, t0, streakIgnore},
		{"a slow failure after a later success is ignored", streak{enabled: true, outcomeStartedAt: at(time.Hour)}, false, t0, streakIgnore},
		{"inside the first day", streak{enabled: true, failingSince: at(0)}, false, t0.Add(day - time.Second), streakKeep},
		{"a day warns", streak{enabled: true, failingSince: at(0)}, false, t0.Add(day), streakWarn},
		{"past both limits only warns", streak{enabled: true, failingSince: at(0)}, false, t0.Add(6 * day), streakWarn},
		{"warned, before 5 days", streak{enabled: true, failingSince: at(0), warnedAt: at(day)}, false, t0.Add(5*day - time.Second), streakKeep},
		{"5 days and a day after the warning", streak{enabled: true, failingSince: at(0), warnedAt: at(day)}, false, t0.Add(5 * day), streakDisable},
		{"5 days but warned less than a day ago", streak{enabled: true, failingSince: at(0), warnedAt: at(5 * day)}, false, t0.Add(5*day + time.Hour), streakKeep},
		{"a success never warns", streak{enabled: true, failingSince: at(0)}, true, t0.Add(6 * day), streakClose},
		{"disabled", streak{failingSince: at(0), warnedAt: at(day)}, false, t0.Add(6 * day), streakIgnore},
		{"inbox", streak{enabled: true, inbox: true}, false, t0, streakIgnore},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := c.st.step(c.succeeded, c.started); got != c.want {
				t.Errorf("%s: %d, want %d", c.name, got, c.want)
			}
		})
	}
}
