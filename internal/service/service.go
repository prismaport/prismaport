// Package service is the application layer shared by the HTTP API, the worker and the
// command-line tools. It owns validation and the API error codes; the store owns persistence.
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prismaport/prismaport/internal/apikey"
	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/chainpause"
	"github.com/prismaport/prismaport/internal/dashauth"
	"github.com/prismaport/prismaport/internal/derive"
	"github.com/prismaport/prismaport/internal/destination"
	"github.com/prismaport/prismaport/internal/mail"
	"github.com/prismaport/prismaport/internal/secretbox"
	"github.com/prismaport/prismaport/internal/store"
	"github.com/prismaport/prismaport/internal/tuning"
	"github.com/prismaport/prismaport/internal/view"
)

// Kind classifies an Error by what went wrong, not by how a transport reports it; internal/api
// maps each kind to its HTTP status in one place. The zero kind is an internal failure. It is a
// string so a bare status number (400) is a compile error, not an unknown kind.
type Kind string

const (
	// KindInternal is a failure on our side; the client is told only that it failed.
	KindInternal Kind = ""
	// KindInvalid is a request that is malformed or breaks a rule.
	KindInvalid Kind = "invalid"
	// KindUnauthorized is a request without valid credentials (no key, no session, a wrong login).
	KindUnauthorized Kind = "unauthorized"
	// KindForbidden is a caller that is known but not allowed, or must confirm first.
	KindForbidden Kind = "forbidden"
	// KindNotFound is a resource that does not exist for this caller.
	KindNotFound Kind = "not_found"
	// KindConflict is a request that clashes with the resource's current state.
	KindConflict Kind = "conflict"
	// KindGone is a resource that existed and is closed for good (an expired session or link).
	KindGone Kind = "gone"
	// KindRateLimited is a caller that must wait (RetryAfter says how long).
	KindRateLimited Kind = "rate_limited"
	// KindUnavailable is a dependency or capacity that is missing for now; a retry may work.
	KindUnavailable Kind = "unavailable"
)

// Error is a client-facing failure with a stable code. Cause, when set, is the internal error it
// explains; it is never shown to clients.
type Error struct {
	Kind    Kind
	Code    string
	Message string
	// Param names the request field the error is about ("payout.base[1].share"), when one is.
	Param string
	// RetryAfter, when set, is sent as the Retry-After header (a 429 rate_limited).
	RetryAfter time.Duration
	// Details, when set, is sent under "details" in the error body.
	Details map[string]any
	Cause   error
}

// at sets the request field an error is about.
func at(err error, param string) error {
	var e *Error
	if errors.As(err, &e) && e.Param == "" {
		e.Param = param
	}
	return err
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func (e *Error) Unwrap() error { return e.Cause }

func newErr(kind Kind, code, format string, args ...any) *Error {
	return &Error{Kind: kind, Code: code, Message: fmt.Sprintf(format, args...)}
}

// Error codes the service returns to API and dashboard clients.
const (
	CodeAmountBelowMinimum  = "amount_below_minimum"
	CodeAmountAboveMaximum  = "amount_above_maximum"
	CodeProductInactive     = "product_inactive"
	CodeProductNotFound     = "product_not_found"
	CodeSessionNotFound     = "session_not_found"
	CodeIdempotencyConflict = "idempotency_conflict"
	CodeInvalidAPIKey       = "invalid_api_key"
	CodeInvalidRequest      = "invalid_request"
	CodeInsufficientScope   = "insufficient_scope"
	CodeNotFound            = "not_found"
	CodeServiceUnavailable  = "service_unavailable"
	CodeRateLimited         = "rate_limited"
	CodeInternal            = "internal_error"
	// CodeEndpointDisabled refuses a resend while the delivery's webhook endpoint is disabled.
	CodeEndpointDisabled = "endpoint_disabled"
)

func invalid(format string, args ...any) *Error {
	return newErr(KindInvalid, CodeInvalidRequest, format, args...)
}

// AsError maps any error to a client-facing Error; unknown errors become internal_error
// without leaking details.
func AsError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	if errors.Is(err, dashauth.ErrBusy) {
		// Every password hashing slot is taken (ADR 0008 C-2): a moment later one is free.
		return &Error{Kind: KindUnavailable, Code: CodeServiceUnavailable, Cause: err, RetryAfter: time.Second,
			Message: "We are checking many passwords right now. Try again in a moment."}
	}
	return newErr(KindInternal, CodeInternal, "internal error")
}

// PasswordBreachChecker says whether a password appears in a known data breach. An error means
// it could not tell.
type PasswordBreachChecker interface {
	Breached(ctx context.Context, password string) (bool, error)
}

// Config holds the service's dependencies and limits; zero values get defaults in New.
type Config struct {
	Registry  *chain.Registry
	Deriver   derive.AddressDeriver
	Validator destination.Validator
	// Treasury holds the fee address per mode and family. A family without a treasury gets no
	// pay_to entries: there is nowhere to send the fee.
	Treasury map[bool]map[chain.Family]string
	// Solana holds, per Solana chain id, the forwarder program and the rent receiver its deposit
	// addresses commit to. A chain without an entry only gets addresses from a stub deriver.
	Solana    map[string]SolanaChain
	SecretBox *secretbox.Box
	// DefaultExpiry, MinExpiry and MaxExpiry bound a session's pay window: how long it stays open
	// after its first reveal (expires_in, D48). SelectWindow is how long a new session waits for a
	// network to be revealed before it expires.
	DefaultExpiry  time.Duration
	MinExpiry      time.Duration
	MaxExpiry      time.Duration
	SelectWindow   time.Duration
	SecretRotation time.Duration
	// AllowPrivateHooks lets live webhook endpoints name localhost or private addresses (local
	// development); live endpoints still need https.
	AllowPrivateHooks bool
	CheckoutBaseURL   string
	// APIBaseURL is the merchant API's public address, where sandbox inboxes receive; empty
	// means CheckoutBaseURL.
	APIBaseURL string
	// Mail sends sign-up codes, password resets and security notices; nil turns self-serve
	// sign-up off (503) since nobody could receive a code.
	Mail mail.Sender
	// DashboardURL is the dashboard's address in emails; empty means CheckoutBaseURL/dashboard/.
	DashboardURL string
	// Log receives what fails off the request path (an email that could not be queued).
	Log *slog.Logger
	// PasswordBreaches, when set, refuses a new password found in a known data breach. Tests
	// leave it nil, so they never reach the network.
	PasswordBreaches PasswordBreachChecker
	// WatchAfterExpiry is how long after expiry the worker still checks a session on its own;
	// "Check again" is refused before. Nil means DefaultWatchAfterExpiry (zero is valid).
	WatchAfterExpiry *time.Duration
	// CheckoutExpiryWarning is sent to the hosted checkout as expiry_warning_seconds. Nil means
	// DefaultCheckoutExpiryWarning (zero is valid: no warning).
	CheckoutExpiryWarning *time.Duration
	// ConfirmWindow is how long a right confirmation code lets the session's next covered Live
	// actions run without another (D59). Nil means DefaultConfirmWindow; zero turns the
	// window off, so every action asks for a code. ConfirmWindowWallets caps the Live wallets
	// added in one window; zero means DefaultConfirmWindowWallets.
	ConfirmWindow        *time.Duration
	ConfirmWindowWallets int
	// RecheckLimits bound "Check again"; a zero limit takes its default.
	RecheckLimits RecheckLimits
	// ExpiryHold is how long past expires_at expiry may wait for the scanner to read the
	// session's deposit addresses once more (its first late check) and for its Gateway settle
	// jobs to land, so a payment mined just before expiry is on time, not late (ADR-0008 H-3).
	// Zero expires at expires_at as before (tests, late checks off).
	ExpiryHold time.Duration
	// Hasher runs the dashboard's password hashes. The composition root makes one per process;
	// nil makes one for this service (dashauth.NewHasher defaults).
	Hasher *dashauth.Hasher
	// PauseRefresh is how long the chain pauses read from the database are served before they
	// are read again (D64). Nil means chainpause.DefaultRefresh; zero reads them on every use.
	PauseRefresh *time.Duration
}

// SolanaChain names the forwarder program and the rent receiver the deposit addresses of one Solana
// chain commit to.
type SolanaChain struct {
	Program      string
	RentReceiver string
}

// Session expiry defaults: the pay window when a request names none, its bounds, and the window
// to select a network. config reads PRISMAPORT_SESSION_* over them.
const (
	DefaultSessionExpiry    = tuning.DefaultSessionExpiry
	DefaultMinSessionExpiry = tuning.DefaultMinSessionExpiry
	DefaultMaxSessionExpiry = tuning.DefaultMaxSessionExpiry
	DefaultSelectWindow     = tuning.DefaultSelectWindow
)

func (c *Config) defaults() {
	if c.DefaultExpiry == 0 {
		c.DefaultExpiry = DefaultSessionExpiry
	}
	if c.MinExpiry == 0 {
		c.MinExpiry = DefaultMinSessionExpiry
	}
	if c.MaxExpiry == 0 {
		c.MaxExpiry = DefaultMaxSessionExpiry
	}
	if c.SelectWindow == 0 {
		c.SelectWindow = DefaultSelectWindow
	}
	if c.SecretRotation == 0 {
		c.SecretRotation = 24 * time.Hour
	}
	if c.Hasher == nil {
		c.Hasher = dashauth.NewHasher(0, 0)
	}
}

// Service holds the business rules behind the API, the dashboard and checkout, on top of the store.
type Service struct {
	store *store.Store
	view  view.Renderer
	cfg   Config
	// pauses serves the chain pauses (D64) to every copy of the service (bind, withStore).
	pauses *chainpause.Reader
}

// New returns a service over the store, filling unset config fields with their defaults.
func New(st *store.Store, cfg Config) *Service {
	cfg.defaults()
	s := &Service{store: st, cfg: cfg}
	s.pauses = pauseReader(st, cfg.PauseRefresh, s.Logger)
	s.view = view.Renderer{CheckoutBaseURL: cfg.CheckoutBaseURL, Chains: s.chainIDs}
	return s
}

// chainIDs lists the registry's chain ids of a mode, in registry order.
func (s *Service) chainIDs(livemode bool) []string {
	var out []string
	for _, c := range s.cfg.Registry.ForMode(livemode) {
		out = append(out, c.ID)
	}
	return out
}

// bind returns a service whose store runs inside tx.
func (s *Service) bind(tx pgx.Tx) *Service {
	c := *s
	c.store = s.store.Bind(tx)
	return &c
}

// withStore returns a service over st, a store already bound to a transaction.
func (s *Service) withStore(st *store.Store) *Service {
	c := *s
	c.store = st
	return &c
}

// View returns the renderer the service uses to build API objects.
func (s *Service) View() view.Renderer { return s.view }

// Registry returns the registry of supported chains and assets.
func (s *Service) Registry() *chain.Registry { return s.cfg.Registry }

// Logger is the configured logger, or one that discards.
func (s *Service) Logger() *slog.Logger {
	if s.cfg.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.cfg.Log
}

// Auth is an authenticated API caller.
type Auth struct {
	KeyID      string
	MerchantID string
	Livemode   bool
	Scopes     []string
	// Actor names the caller in the audit log; empty means the API key.
	Actor string
	// useDue is set by Authenticate when the key's last recorded use is older than
	// LastUsedResolution, so RecordKeyUse writes.
	useDue bool
}

func (a Auth) actor() string {
	if a.Actor != "" {
		return a.Actor
	}
	return "api:" + a.KeyID
}

// Require returns a forbidden error unless the auth holds the given scope.
func (a Auth) Require(scope apikey.Scope) error {
	if slices.Contains(a.Scopes, string(scope)) {
		return nil
	}
	return newErr(KindForbidden, CodeInsufficientScope, "this key lacks the %s scope", scope)
}

// Authenticate resolves an API key token to its merchant, mode and scopes, or returns an
// unauthenticated error.
func (s *Service) Authenticate(ctx context.Context, token string) (Auth, error) {
	livemode, hash, err := apikey.Parse(token)
	if err != nil {
		return Auth{}, newErr(KindUnauthorized, CodeInvalidAPIKey, "invalid API key")
	}
	k, err := s.store.AuthenticateAPIKey(ctx, hash)
	if errors.Is(err, store.ErrNotFound) {
		return Auth{}, newErr(KindUnauthorized, CodeInvalidAPIKey, "invalid API key")
	}
	if err != nil {
		return Auth{}, err
	}
	if k.Livemode != livemode {
		return Auth{}, newErr(KindUnauthorized, CodeInvalidAPIKey, "invalid API key")
	}
	due := k.LastUsedAt == nil || s.store.Now().Sub(*k.LastUsedAt) >= LastUsedResolution
	return Auth{KeyID: k.ID, MerchantID: k.MerchantID, Livemode: k.Livemode, Scopes: k.Scopes, useDue: due}, nil
}

// LastUsedResolution is how precise an API key's last_used_at is: at most one write per key
// per window, however many requests and instances use it.
const LastUsedResolution = time.Minute

// RecordKeyUse stores the key's last use when the stored one is older than LastUsedResolution.
// It is bookkeeping: callers log a failure and serve the request anyway.
func (s *Service) RecordKeyUse(ctx context.Context, a Auth) error {
	if !a.useDue || a.KeyID == "" {
		return nil
	}
	_, err := s.store.TouchAPIKey(ctx, a.KeyID, LastUsedResolution)
	return err
}
