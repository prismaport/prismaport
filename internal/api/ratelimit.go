package api

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/prismaport/prismaport/internal/safe"
	"github.com/prismaport/prismaport/internal/service"
	"github.com/prismaport/prismaport/internal/store"
)

// Rate-limited public endpoints. Each has its own bucket per client IP.
const (
	RouteLinkOpen       = "link_open"
	RouteCheckoutReveal = "checkout_reveal"
	// RouteAddressOpen: opening (or resuming) a session from a customer address page.
	RouteAddressOpen       = "address_open"
	RouteEIP3009TypedData  = "eip3009_typed_data"
	RouteEIP3009Submit     = "eip3009_submit"
	RouteSolanaTransaction = "solana_transaction"
	RouteSolanaSubmit      = "solana_submit"
	// Dashboard login is limited per client IP, per email and client IP (tight), and per email
	// (loose, against guessing from many addresses); TOTP checks per user.
	RouteDashboardLogin        = "dashboard_login"
	RouteDashboardLoginEmailIP = "dashboard_login_email_ip"
	RouteDashboardLoginEmail   = "dashboard_login_email"
	RouteDashboardTOTP         = "dashboard_totp"
	// Self-serve accounts: sign-up, resending a code and asking for a password reset send
	// email, so they are limited per client IP and per email (hashed); checking a code is
	// limited per client IP (each code also takes only a few tries).
	RouteDashboardSignup = "dashboard_signup"
	RouteDashboardEmail  = "dashboard_email"
	RouteDashboardCode   = "dashboard_code"
	// Confirmation codes a signed-in user asks for (POST /v1/dashboard/confirmations), per user:
	// apart from dashboard_email, so anonymous requests naming the email cannot hold them back.
	RouteDashboardConfirm = "dashboard_confirm"
	// Hosted checkout Test mode panel: simulator steps per client IP.
	RouteCheckoutTestPanel = "checkout_test_panel"
	// Sandbox: test sends per merchant (each makes an outbound request).
	RouteWebhookTest = "webhook_test"
	// Sandbox: requests received per hosted inbox.
	RouteSandboxInbox = "sandbox_inbox"
	// Merchant API: per secret key, reads (GET) and writes (everything else) apart, so a busy
	// poller cannot starve session creation and the other way round.
	RouteAPIRead  = "api_read"
	RouteAPIWrite = "api_write"
)

// Limit is a token bucket: Burst requests at once, refilled at PerMinute.
type Limit struct {
	PerMinute int
	Burst     int
}

// defaultLimits apply to routes PublicOptions.Limits leaves out. The dashboard routes that hash
// a password (login, sign-up, and the code and reset routes) keep small bursts: each hash holds
// 64 MiB, and dashauth caps how many run at once (ADR 0008 C-2). Revealing a network costs
// watching its address (and a blacklist read on EVM); builds and submits each cost a simulation,
// and a valid EIP-3009 submit costs our gas.
var defaultLimits = map[string]Limit{
	RouteLinkOpen:              {PerMinute: 10, Burst: 5},
	RouteCheckoutReveal:        {PerMinute: 30, Burst: 10},
	RouteAddressOpen:           {PerMinute: 10, Burst: 5},
	RouteEIP3009TypedData:      {PerMinute: 30, Burst: 10},
	RouteEIP3009Submit:         {PerMinute: 10, Burst: 5},
	RouteSolanaTransaction:     {PerMinute: 30, Burst: 10},
	RouteSolanaSubmit:          {PerMinute: 10, Burst: 5},
	RouteDashboardLogin:        {PerMinute: 10, Burst: 5},
	RouteDashboardLoginEmailIP: {PerMinute: 5, Burst: 5},
	RouteDashboardLoginEmail:   {PerMinute: 30, Burst: 10},
	RouteDashboardTOTP:         {PerMinute: 10, Burst: 5},
	RouteDashboardSignup:       {PerMinute: 5, Burst: 3},
	RouteDashboardEmail:        {PerMinute: 1, Burst: 3},
	RouteDashboardCode:         {PerMinute: 10, Burst: 5},
	RouteDashboardConfirm:      {PerMinute: 1, Burst: 3},
	RouteWebhookTest:           {PerMinute: 20, Burst: 10},
	RouteCheckoutTestPanel:     {PerMinute: 30, Burst: 10},
	RouteSandboxInbox:          {PerMinute: 120, Burst: 60},
	RouteAPIRead:               {PerMinute: 600, Burst: 200},
	RouteAPIWrite:              {PerMinute: 120, Burst: 60},
}

// DefaultLimits returns a copy of the limits that apply to routes PublicOptions.Limits leaves out.
func DefaultLimits() map[string]Limit { return maps.Clone(defaultLimits) }

// PurgeIdle is how long a bucket may sit unused before it is deleted. Deleting is the same as
// a full bucket, so every limit must refill completely within it (checked in ConfigurePublic).
const PurgeIdle = time.Hour

// PublicOptions configures the public checkout endpoints.
type PublicOptions struct {
	// ClientIPHeader names a header set by a trusted reverse proxy. It must be one the proxy
	// overwrites (for example X-Real-IP or CF-Connecting-IP): its first address is used.
	// Empty uses the TCP peer address.
	ClientIPHeader string
	// Limits overrides DefaultLimits() per route.
	Limits map[string]Limit
	// Store holds the buckets; nil uses the server's store. Every API instance on the same
	// database shares them.
	Store *store.Store
	// Now replaces the database clock (tests only).
	Now func() time.Time
}

type publicLimiter struct {
	st     *store.Store
	header string
	limits map[string]Limit
	now    func() time.Time
	log    *slog.Logger
	bg     *background
	mu     sync.Mutex
	purged time.Time
}

// background runs the server's work that outlives a request (the rate limit purge). Close
// cancels it and waits for it, so a shutdown never leaves it running against a closed pool.
type background struct {
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
}

func newBackground() *background {
	ctx, cancel := context.WithCancel(context.Background())
	return &background{ctx: ctx, cancel: cancel}
}

// Go runs f in the background unless Close was called.
func (b *background) Go(f func(ctx context.Context)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.wg.Go(func() { f(b.ctx) })
}

// Close cancels the background work and waits for it to return.
func (b *background) Close() {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	b.cancel()
	b.wg.Wait()
}

// ConfigurePublic sets the client IP source and the rate limits of the public checkout
// endpoints and the merchant API. Call it before serving; without it they use DefaultLimits.
func (s *Server) ConfigurePublic(o PublicOptions) error {
	limits := maps.Clone(defaultLimits)
	for route, l := range o.Limits {
		if _, ok := defaultLimits[route]; !ok {
			return fmt.Errorf("api: unknown rate-limited route %q", route)
		}
		if l.PerMinute <= 0 || l.Burst <= 0 || time.Duration(l.Burst)*time.Minute/time.Duration(l.PerMinute) > PurgeIdle {
			return fmt.Errorf("api: route %s: limit %d/min burst %d must be positive and refill within %s", route, l.PerMinute, l.Burst, PurgeIdle)
		}
		limits[route] = l
	}
	st := o.Store
	if st == nil {
		st = s.st
	}
	if s.bg == nil {
		s.bg = newBackground()
	}
	s.limiter = &publicLimiter{st: st, header: o.ClientIPHeader, limits: limits, now: o.Now, log: s.log, bg: s.bg}
	return nil
}

func (s *Server) publicLimiter() *publicLimiter {
	if s.limiter == nil {
		_ = s.ConfigurePublic(PublicOptions{})
	}
	return s.limiter
}

// allow takes a token for the request's client IP on route. Over the limit it sets
// Retry-After and returns 429 rate_limited.
func (l *publicLimiter) allow(w http.ResponseWriter, r *http.Request, route string) error {
	return l.allowKey(w, r, route, clientIP(r, l.header))
}

// allowKey is allow with an explicit bucket key (an email or a user id instead of the IP).
func (l *publicLimiter) allowKey(w http.ResponseWriter, r *http.Request, route, key string) error {
	lim := l.limits[route]
	var at *time.Time
	if l.now != nil {
		t := l.now()
		at = &t
	}
	ok, wait, err := l.st.TakeRateToken(r.Context(), route+"|"+key, lim.PerMinute, lim.Burst, at)
	if err != nil {
		return err
	}
	l.purge(at)
	if ok {
		return nil
	}
	w.Header().Set("Retry-After", fmt.Sprint(max(1, int(math.Ceil(wait.Seconds())))))
	msg := "too many requests from this address; try again shortly"
	switch route {
	case RouteLinkOpen, RouteAddressOpen:
		msg = "too many payment sessions from this address; try again shortly"
	case RouteDashboardLogin, RouteDashboardLoginEmailIP, RouteDashboardLoginEmail:
		msg = "Too many sign-in attempts. Wait a minute and try again."
	case RouteDashboardTOTP, RouteDashboardCode:
		msg = "Too many attempts. Wait a minute and try again."
	case RouteDashboardSignup:
		msg = "Too many requests from this address. Wait a minute and try again."
	case RouteDashboardEmail:
		msg = "We just sent an email to this address. Wait a minute before asking for another one."
	case RouteDashboardConfirm:
		msg = "We just sent you a code. Wait a minute before asking for another one."
	case RouteAPIRead, RouteAPIWrite:
		msg = "too many requests with this API key; retry after the Retry-After delay"
	}
	return &service.Error{Kind: service.KindRateLimited, Code: CodeRateLimited, Message: msg}
}

// purgeEvery is how often one process purges idle buckets.
const purgeEvery = 10 * time.Minute

// purge drops idle buckets at most every purgeEvery per process, off the request path. at is the
// limiter's injected clock (nil: the wall clock).
func (l *publicLimiter) purge(at *time.Time) {
	now := time.Now()
	if at != nil {
		now = *at
	}
	l.mu.Lock()
	due := now.Sub(l.purged) > purgeEvery
	if due {
		l.purged = now
	}
	l.mu.Unlock()
	if !due {
		return
	}
	l.bg.Go(func(ctx context.Context) {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		// A panic here would take the API down with it.
		if err := safe.Call(func() error { _, err := l.st.PurgeRateLimits(ctx, PurgeIdle, at); return err }); err != nil {
			l.log.Warn("rate limit purge failed", "err", err)
		}
	})
}

// clientIP is the TCP peer, or the first address of header when the API runs behind a proxy
// that sets it. A spoofable header is only trusted when the deployment names it.
func clientIP(r *http.Request, header string) string {
	if header != "" {
		if v := strings.TrimSpace(strings.Split(r.Header.Get(header), ",")[0]); v != "" {
			if ip := net.ParseIP(v); ip != nil {
				return ip.String()
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
