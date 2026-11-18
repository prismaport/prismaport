package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/prismaport/prismaport/internal/tuning"
)

// CheckoutLockEnv is what the API reads of the adaptive scanner settings
// (adaptive scanner): the checkout's expiry warning, the watch window that gates
// "Check again", and its rate limits.
type CheckoutLockEnv struct {
	// ExpiryWarning (PRISMAPORT_CHECKOUT_EXPIRY_WARNING, 0..1h, default 5m) is sent to the page.
	ExpiryWarning time.Duration
	// WatchAfterExpiry is how long after expiry the worker still checks a session on its own:
	// PRISMAPORT_WATCH_AFTER_EXPIRY, parsed once with the scanner settings (loadScan).
	WatchAfterExpiry time.Duration
	// RecheckLimits (PRISMAPORT_RECHECK_LIMITS) bound "Check again".
	RecheckLimits tuning.RecheckLimits
	// Loaded is set by Load; an Env built by hand keeps the service defaults.
	Loaded bool
}

// maxRecheckPeriod is the longest recheck limit period: rate-limit buckets idle for an hour are
// purged (a purged bucket is a full one).
const maxRecheckPeriod = time.Hour

func loadCheckoutLock(watchAfterExpiry time.Duration) (CheckoutLockEnv, error) {
	c := CheckoutLockEnv{WatchAfterExpiry: watchAfterExpiry}
	var err error
	if c.ExpiryWarning, err = checkoutLockDuration("PRISMAPORT_CHECKOUT_EXPIRY_WARNING", tuning.DefaultCheckoutExpiryWarning, 0, time.Hour); err != nil {
		return c, err
	}
	if c.RecheckLimits, err = parseRecheckLimits(os.Getenv("PRISMAPORT_RECHECK_LIMITS")); err != nil {
		return c, fmt.Errorf("PRISMAPORT_RECHECK_LIMITS: %w", err)
	}
	c.Loaded = true
	return c, nil
}

// checkoutLockDuration reads a Go duration ("5m", "1h") between lo and hi; unset or blank is def.
func checkoutLockDuration(name string, def, lo, hi time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a Go duration such as 5m or 1h", name, raw)
	}
	if d < lo || d > hi {
		return 0, fmt.Errorf("%s: %s must be between %s and %s", name, d, lo, hi)
	}
	return d, nil
}

// parseRecheckLimits reads "session=<count>/<period>,merchant=<count>/<period>" (for example
// "session=1/10m,merchant=30/1h"). Either part may be left out to keep its default; each count
// is at least 1 and each period between 1s and 1h.
func parseRecheckLimits(raw string) (tuning.RecheckLimits, error) {
	out := tuning.DefaultRecheckLimits
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return out, nil
	}
	seen := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		name, spec, ok := strings.Cut(strings.TrimSpace(part), "=")
		name = strings.TrimSpace(name)
		if !ok || (name != "session" && name != "merchant") {
			return out, fmt.Errorf("%q: want session=<count>/<period> or merchant=<count>/<period>", part)
		}
		if seen[name] {
			return out, fmt.Errorf("%s is set twice", name)
		}
		seen[name] = true
		count, period, ok := strings.Cut(strings.TrimSpace(spec), "/")
		n, err := strconv.Atoi(strings.TrimSpace(count))
		if !ok || err != nil || n < 1 || n > 10000 {
			return out, fmt.Errorf("%s: %q: want <count>/<period> with a count from 1 to 10000, such as 1/10m", name, spec)
		}
		d, err := time.ParseDuration(strings.TrimSpace(period))
		if err != nil || d < time.Second || d > maxRecheckPeriod {
			return out, fmt.Errorf("%s: %q: the period must be a Go duration from 1s to %s", name, spec, maxRecheckPeriod)
		}
		l := tuning.RecheckLimit{Count: n, Period: d}
		if name == "session" {
			out.Session = l
		} else {
			out.Merchant = l
		}
	}
	return out, nil
}
