package service

import (
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/store"
)

func TestKeyStatus(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	later, earlier := now.Add(time.Hour), now.Add(-time.Hour)
	next := "key_next"
	cases := []struct {
		name string
		k    store.APIKey
		want string
	}{
		{"new", store.APIKey{}, KeyActive},
		{"rolled, in the overlap", store.APIKey{ExpiresAt: &later, ReplacedBy: &next}, KeyExpiring},
		{"rolled, at the deadline", store.APIKey{ExpiresAt: &now, ReplacedBy: &next}, KeyExpired},
		{"rolled, swept", store.APIKey{ExpiresAt: &earlier, RevokedAt: &earlier, ReplacedBy: &next}, KeyExpired},
		{"revoked", store.APIKey{RevokedAt: &earlier}, KeyRevoked},
		{"revoked during the overlap", store.APIKey{ExpiresAt: &later, RevokedAt: &earlier, ReplacedBy: &next}, KeyRevoked},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := keyStatus(c.k, now); got != c.want {
				t.Errorf("%s: %s, want %s", c.name, got, c.want)
			}
			if active := c.k.Active(now); active != (c.want == KeyActive || c.want == KeyExpiring) {
				t.Errorf("%s: Active = %v", c.name, active)
			}
		})
	}
}

func TestRollOverlaps(t *testing.T) {
	want := map[string]time.Duration{"now": 0, "1h": time.Hour, "24h": 24 * time.Hour, "7d": 168 * time.Hour}
	if len(RollOverlaps) != len(want) {
		t.Fatalf("overlaps: %v", RollOverlaps)
	}
	for k, d := range want {
		t.Run(k, func(t *testing.T) {
			if RollOverlaps[k] != d {
				t.Errorf("%s: %s", k, RollOverlaps[k])
			}
		})
	}
}
