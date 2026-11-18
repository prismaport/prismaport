// Package tuning holds the defaults of the settings config reads from the environment and the
// service and worker fall back to, so config imports neither of them.
package tuning

import (
	"fmt"
	"time"
)

// Session expiry defaults: the pay window when a request names none, its bounds, and the window
// to select a network. config reads PRISMAPORT_SESSION_* over them.
const (
	DefaultSessionExpiry    = 30 * time.Minute
	DefaultMinSessionExpiry = time.Minute
	DefaultMaxSessionExpiry = 24 * time.Hour
	DefaultSelectWindow     = time.Hour
)

// DefaultWatchAfterExpiry is how long after expires_at the worker still checks a session on its
// own, and "Check again" is refused before.
const DefaultWatchAfterExpiry = 23*time.Hour + 30*time.Minute

// DefaultCheckoutExpiryWarning is how long before expiry the hosted checkout warns the payer.
const DefaultCheckoutExpiryWarning = 5 * time.Minute

// The confirmation window (D59): a right code for a covered Live action lets the session's next
// covered actions run without another, for a while, adding at most this many Live wallets.
const (
	DefaultConfirmWindow        = 10 * time.Minute
	DefaultConfirmWindowWallets = 20
)

// DefaultStepTimeout bounds one worker loop step. The longest legitimate step is a chain
// detector's rescan after downtime (many 10-second-bounded provider requests); a few minutes is
// far past it on a healthy provider. The database bounds each statement on its own
// (db.AppTimeouts).
const DefaultStepTimeout = 5 * time.Minute

// DefaultIdempotencyTTL is how long an idempotency key's stored response is kept.
const DefaultIdempotencyTTL = 7 * 24 * time.Hour

// RecheckLimit is a token bucket of Count requests refilled completely over Period.
type RecheckLimit struct {
	Count  int
	Period time.Duration
}

func (l RecheckLimit) String() string { return fmt.Sprintf("%d/%s", l.Count, l.Period) }

// RecheckLimits bound "Check again": per session and per merchant (mode apart).
type RecheckLimits struct {
	Session  RecheckLimit
	Merchant RecheckLimit
}

// DefaultRecheckLimits allows "Check again" once per session every 10 minutes, 30 per merchant
// an hour.
var DefaultRecheckLimits = RecheckLimits{
	Session:  RecheckLimit{Count: 1, Period: 10 * time.Minute},
	Merchant: RecheckLimit{Count: 30, Period: time.Hour},
}
