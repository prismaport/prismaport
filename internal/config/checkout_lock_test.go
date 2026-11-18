package config

import (
	"strings"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/tuning"
)

func TestCheckoutLockDefaults(t *testing.T) {
	t.Setenv("PRISMAPORT_DATABASE_URL", "postgres://x")
	t.Setenv("PRISMAPORT_CHECKOUT_EXPIRY_WARNING", "")
	t.Setenv("PRISMAPORT_WATCH_AFTER_EXPIRY", "")
	t.Setenv("PRISMAPORT_RECHECK_LIMITS", "")
	e, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	c := e.CheckoutLock
	if c.ExpiryWarning != 5*time.Minute || c.WatchAfterExpiry != 23*time.Hour+30*time.Minute || c.RecheckLimits != tuning.DefaultRecheckLimits {
		t.Fatalf("defaults: %+v", c)
	}
	if c.RecheckLimits.Session != (tuning.RecheckLimit{Count: 1, Period: 10 * time.Minute}) ||
		c.RecheckLimits.Merchant != (tuning.RecheckLimit{Count: 30, Period: time.Hour}) {
		t.Fatalf("recheck defaults: %+v", c.RecheckLimits)
	}
}

func TestCheckoutLockValues(t *testing.T) {
	t.Setenv("PRISMAPORT_DATABASE_URL", "postgres://x")
	t.Setenv("PRISMAPORT_CHECKOUT_EXPIRY_WARNING", "0s")
	t.Setenv("PRISMAPORT_WATCH_AFTER_EXPIRY", "0")
	t.Setenv("PRISMAPORT_RECHECK_LIMITS", " merchant = 5/30m ")
	e, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	c := e.CheckoutLock
	if c.ExpiryWarning != 0 || c.WatchAfterExpiry != 0 {
		t.Fatalf("zero values not kept: %+v", c)
	}
	if c.RecheckLimits.Session != tuning.DefaultRecheckLimits.Session || c.RecheckLimits.Merchant != (tuning.RecheckLimit{Count: 5, Period: 30 * time.Minute}) {
		t.Fatalf("limits: %+v", c.RecheckLimits)
	}
	t.Setenv("PRISMAPORT_RECHECK_LIMITS", "session=2/1m,merchant=100/1h")
	if e, err = Load(); err != nil || e.CheckoutLock.RecheckLimits != (tuning.RecheckLimits{
		Session: tuning.RecheckLimit{Count: 2, Period: time.Minute}, Merchant: tuning.RecheckLimit{Count: 100, Period: time.Hour}}) {
		t.Fatalf("both limits: %+v %v", e.CheckoutLock.RecheckLimits, err)
	}
}

func TestCheckoutLockInvalid(t *testing.T) {
	for _, c := range []struct{ name, value, want string }{
		{"PRISMAPORT_CHECKOUT_EXPIRY_WARNING", "5", "not a Go duration"},
		{"PRISMAPORT_CHECKOUT_EXPIRY_WARNING", "-1s", "between 0s and 1h0m0s"},
		{"PRISMAPORT_CHECKOUT_EXPIRY_WARNING", "61m", "between 0s and 1h0m0s"},
		{"PRISMAPORT_WATCH_AFTER_EXPIRY", "169h", "between 0 (no late checks) and 168h"},
		{"PRISMAPORT_WATCH_AFTER_EXPIRY", "1d", "must be a duration such as 30s or 1h"},
		{"PRISMAPORT_RECHECK_LIMITS", "1/10m", "want session=<count>/<period>"},
		{"PRISMAPORT_RECHECK_LIMITS", "key=1/10m", "want session=<count>/<period>"},
		{"PRISMAPORT_RECHECK_LIMITS", "session=0/10m", "count from 1"},
		{"PRISMAPORT_RECHECK_LIMITS", "session=x/10m", "count from 1"},
		{"PRISMAPORT_RECHECK_LIMITS", "session=1", "count from 1"},
		{"PRISMAPORT_RECHECK_LIMITS", "session=1/2h", "from 1s to 1h0m0s"},
		{"PRISMAPORT_RECHECK_LIMITS", "session=1/0s", "from 1s to 1h0m0s"},
		{"PRISMAPORT_RECHECK_LIMITS", "session=1/10m,session=2/10m", "set twice"},
	} {
		t.Run(c.name+"="+c.value, func(t *testing.T) {
			t.Setenv("PRISMAPORT_DATABASE_URL", "postgres://x")
			t.Setenv(c.name, c.value)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), c.name) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want %s ... %s", err, c.name, c.want)
			}
		})
	}
}
