package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/prismaport/prismaport/internal/apikey"
	"github.com/prismaport/prismaport/internal/store"
	"github.com/prismaport/prismaport/internal/tuning"
)

// CodeSessionStillWatched refuses "Check again" while the worker still watches the session on
// its own (until expires_at + the watch window, or until a paid session's payments are final):
// a payment sent to it is found without asking.
const CodeSessionStillWatched = "session_still_watched"

// CodeSessionSuperseded refuses "Check again" on a session of a customer address that is not the
// address's latest session: the latest one owns the address's watch, and whatever arrives is
// credited to it.
const CodeSessionSuperseded = "session_superseded"

// CodeSessionNotRevealed refuses "Check again" on a session whose deposit details were never
// shown on any network (D48): nothing could have been sent to it, and no network of it is
// watched, so the check would never run. A session on a customer address is revealed from its
// creation.
const CodeSessionNotRevealed = "session_not_revealed"

// Defaults of the adaptive scanner settings the API reads.
const (
	DefaultWatchAfterExpiry      = tuning.DefaultWatchAfterExpiry
	DefaultCheckoutExpiryWarning = tuning.DefaultCheckoutExpiryWarning
)

type (
	// RecheckLimit is a count allowed within a time window for "Check again" (tuning holds it, for config).
	RecheckLimit = tuning.RecheckLimit
	// RecheckLimits bounds "Check again" per session and per merchant (tuning holds it, for config).
	RecheckLimits = tuning.RecheckLimits
)

// DefaultRecheckLimits allows "Check again" once per session every 10 minutes, 30 per merchant
// an hour.
var DefaultRecheckLimits = tuning.DefaultRecheckLimits

// WatchAfterExpiry is how long after expires_at the worker still checks a session on its own.
func (s *Service) WatchAfterExpiry() time.Duration {
	if s.cfg.WatchAfterExpiry == nil {
		return DefaultWatchAfterExpiry
	}
	return *s.cfg.WatchAfterExpiry
}

// CheckoutExpiryWarning is how long before expiry the hosted checkout warns the payer.
func (s *Service) CheckoutExpiryWarning() time.Duration {
	if s.cfg.CheckoutExpiryWarning == nil {
		return DefaultCheckoutExpiryWarning
	}
	return *s.cfg.CheckoutExpiryWarning
}

func (s *Service) recheckLimits() RecheckLimits {
	l := s.cfg.RecheckLimits
	if l.Session.Count == 0 {
		l.Session = DefaultRecheckLimits.Session
	}
	if l.Merchant.Count == 0 {
		l.Merchant = DefaultRecheckLimits.Merchant
	}
	return l
}

// WatchEndsAt is when the worker stops checking the session on its own: the end of its late
// checks, or, for a session paid exactly whose payments are all final, the moment it became so
// (D47), whichever comes first. "Check again" is accepted from then on.
func (s *Service) WatchEndsAt(ses *store.Session) time.Time {
	ends := ses.State.ExpiresAt.Add(s.WatchAfterExpiry())
	if at, ok := ses.State.Settled(); ok && at.Before(ends) {
		return at
	}
	return ends
}

// RecheckSession asks the worker for one more late check of a session past its watch window
// ("Check again"). A pending request answers again without a new request, audit row or rate
// limit token; otherwise the session's and the merchant's buckets each give a token first.
func (s *Service) RecheckSession(ctx context.Context, a Auth, id string) (*store.Session, error) {
	if err := a.Require(apikey.SessionsWrite); err != nil {
		return nil, err
	}
	ses, err := s.store.GetSession(ctx, a.MerchantID, a.Livemode, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, sessionNotFound()
	}
	if err != nil {
		return nil, err
	}
	if ses.CustomerAddressID != nil {
		current, err := s.store.CurrentAddressSession(ctx, *ses.CustomerAddressID)
		if err != nil {
			return nil, err
		}
		if current != ses.ID {
			return nil, newErr(KindConflict, CodeSessionSuperseded,
				"a newer session (%s) was opened on this customer address: it watches the address, check that one", current)
		}
	}
	if len(ses.Reveals) == 0 {
		return nil, newErr(KindConflict, CodeSessionNotRevealed,
			"no payment was generated for this session: no deposit address was shown, so there is nothing to check")
	}
	ends := s.WatchEndsAt(ses)
	if s.store.Now().Before(ends) {
		return nil, newErr(KindConflict, CodeSessionStillWatched,
			"this session is still watched until %s: a payment sent to it is found without Check again", rfc3339(ends))
	}
	pending, err := s.store.RecheckRequestedAt(ctx, id)
	if err != nil {
		return nil, err
	}
	if pending != nil {
		return ses, nil
	}
	lim := s.recheckLimits()
	if err := s.takeRecheckToken(ctx, "recheck_session|"+id, lim.Session,
		"This session was checked again a moment ago. Wait %s before asking again."); err != nil {
		return nil, err
	}
	if err := s.takeRecheckToken(ctx, fmt.Sprintf("recheck_merchant|%s|%v", a.MerchantID, a.Livemode), lim.Merchant,
		"Too many checks were asked for this account. Wait %s before asking again."); err != nil {
		return nil, err
	}
	if _, err := s.store.RequestRecheck(ctx, a.MerchantID, id, a.actor(),
		map[string]any{"session_id": id, "livemode": a.Livemode}); err != nil {
		return nil, err
	}
	return ses, nil
}

func (s *Service) takeRecheckToken(ctx context.Context, key string, l RecheckLimit, msg string) error {
	ok, wait, err := s.store.TakeRateTokenPer(ctx, key, l.Count, l.Period, nil)
	if err != nil || ok {
		return err
	}
	wait = max(wait, time.Second)
	e := newErr(KindRateLimited, CodeRateLimited, msg, waitText(wait))
	e.RetryAfter = wait
	return e
}

// waitText is a wait in whole minutes (seconds under one).
func waitText(d time.Duration) string {
	if d < time.Minute {
		n := int((d + time.Second - 1) / time.Second)
		if n == 1 {
			return "1 second"
		}
		return fmt.Sprintf("%d seconds", n)
	}
	n := int((d + time.Minute - 1) / time.Minute)
	if n == 1 {
		return "1 minute"
	}
	return fmt.Sprintf("%d minutes", n)
}

// LinkOpensSessions reports whether a payment link still opens new sessions (what its public
// view calls active), for a locked checkout's "Start a new payment". A link that is gone is not.
func (s *Service) LinkOpensSessions(ctx context.Context, id string) (bool, error) {
	l, err := s.PublicLink(ctx, id)
	var e *Error
	if errors.As(err, &e) && e.Kind == KindNotFound {
		return false, nil
	}
	return l.Active, err
}
