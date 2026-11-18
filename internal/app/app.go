// Package app is the composition root: it builds the service, the store, the checkout, the
// chain loops and the alert and mail sinks from the configuration config.Load parsed. The
// binaries call it; no other package does.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/prismaport/prismaport/internal/alert"
	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/chainwatch"
	"github.com/prismaport/prismaport/internal/config"
	"github.com/prismaport/prismaport/internal/dashauth"
	"github.com/prismaport/prismaport/internal/derive"
	"github.com/prismaport/prismaport/internal/destination"
	"github.com/prismaport/prismaport/internal/mail"
	"github.com/prismaport/prismaport/internal/pwned"
	"github.com/prismaport/prismaport/internal/secretbox"
	"github.com/prismaport/prismaport/internal/service"
	"github.com/prismaport/prismaport/internal/store"
	"github.com/prismaport/prismaport/internal/view"
	"github.com/prismaport/prismaport/internal/worker"
)

// Root wires one process from its environment.
type Root struct {
	config.Env
	// Mail and Log, when set by the process, reach the service (the API sends email through the
	// queue, the worker the failing webhook endpoint emails through the transport).
	Mail mail.Sender
	Log  *slog.Logger
	// VerifyRetry is how long a chain loop waits after a failed provider verification before
	// asking again; nil means DefaultVerifyRetry, zero asks on every tick.
	VerifyRetry *time.Duration
	// Activity is the worker process's idle/active state, which the chain scheduler marks while
	// a chain has anything pending (nil: the scheduler marks nothing).
	Activity *worker.Activity
}

// renderer is the view the store renders documents with.
func (e Root) renderer() view.Renderer {
	return view.Renderer{CheckoutBaseURL: e.CheckoutBaseURL}
}

// Store is the store over pool, rendering documents for this environment's checkout.
func (e Root) Store(pool *pgxpool.Pool) *store.Store {
	return store.New(pool, e.renderer())
}

// MailTransport is the configured mail transport (Resend or SMTP), sending each message while
// the caller waits for the provider's answer; nil without one.
func (e Root) MailTransport() (mail.Sender, error) {
	switch {
	case e.Resend != nil:
		return mail.NewResend(*e.Resend)
	case e.SMTP != nil:
		return mail.NewSMTP(*e.SMTP)
	}
	return nil, nil
}

// Mailer starts the background mail queue over the configured transport (Resend or SMTP).
// Without one it returns nil (no sender) and a no-op stop. stop drains the queue until its
// context ends (mail.Queue.Close).
func (e Root) Mailer(log *slog.Logger) (mail.Sender, func(context.Context) error, error) {
	s, err := e.MailTransport()
	if err != nil {
		return nil, nil, err
	}
	if s == nil {
		return nil, func(context.Context) error { return nil }, nil
	}
	q := mail.NewQueue(s, log, 256)
	return q, q.Close, nil
}

// Alerts is the worker's alert sink: the log, plus the alert webhook when one is
// configured. deliver sends the webhook's queued alerts until its context ends.
func (e Root) Alerts(log *slog.Logger) (sink alert.Sink, deliver func(context.Context)) {
	logged := alert.Log{Logger: log}
	if e.AlertWebhookURL == "" {
		return logged, func(context.Context) {}
	}
	w := alert.NewWebhook(alert.WebhookConfig{URL: e.AlertWebhookURL, Secret: e.AlertWebhookSecret, Log: log})
	return alert.Fanout{logged, w}, w.Run
}

// codeChecker checks destinations for contract code over the chains' providers; a chain
// without providers makes the check unverifiable (fail closed).
func (e Root) codeChecker() destination.EVMCodeChecker {
	q := e.Quorums(nil)
	if len(q) == 0 {
		return destination.Unavailable{}
	}
	return chainwatch.CodeChecker{Quorums: q}
}

// blacklist reads the USDC issuer blacklist over the chains' providers. A clean answer is kept
// for a minute so session opens do not query the same recipients again and again.
func (e Root) blacklist() destination.BlacklistChecker {
	return &chainwatch.Blacklist{Quorums: e.Quorums(nil), TTL: time.Minute}
}

// Deriver gives EVM chains with a Factory and Solana clusters with a program id and rent
// receiver real forwarder addresses. The rest fall back to the stub, which serves test-mode
// chains only, so live chains without them fail closed with 503.
func (e Root) Deriver() derive.AddressDeriver {
	forwarders := map[string]derive.AddressDeriver{}
	for id, c := range e.Chains {
		if c.Factory != nil {
			forwarders[id] = derive.Forwarders(*c.Factory)
		}
	}
	e.solanaDerivers(forwarders)
	return derive.PerChain{Chains: forwarders, Fallback: derive.ByFamily{chain.EVM: derive.Stub{}, chain.Solana: derive.Stub{}}}
}

// Service builds the application service over st. The secret box is optional for commands that
// never touch webhook secrets.
func (e Root) Service(st *store.Store, needSecrets bool) (*service.Service, error) {
	var box *secretbox.Box
	if e.SecretKeyHex != "" || needSecrets {
		b, err := secretbox.FromHex(strings.TrimSpace(e.SecretKeyHex))
		if err != nil {
			return nil, fmt.Errorf("PRISMAPORT_SECRET_KEY: %w", err)
		}
		box = b
	}
	reg := e.Registry()
	var protocol []string
	for _, fams := range e.Treasury {
		if a := fams[chain.EVM]; a != "" {
			protocol = append(protocol, a)
		}
	}
	for _, c := range e.Chains {
		if c.Factory != nil {
			protocol = append(protocol, c.Factory.Hex(), derive.CloneImplementation(*c.Factory).Hex())
		}
		if c.Gateway != nil {
			protocol = append(protocol, c.Gateway.Hex())
		}
	}
	solProtocol := e.solanaProtocol()
	for _, fams := range e.Treasury {
		if a := fams[chain.Solana]; a != "" {
			solProtocol = append(solProtocol, a)
		}
	}
	cfg := service.Config{
		Registry: reg,
		Deriver:  e.Deriver(),
		Validator: destination.Validator{
			Registry:          reg,
			ProtocolAddresses: map[chain.Family][]string{chain.EVM: protocol, chain.Solana: solProtocol},
			Solana:            e.solanaValidator(reg),
			EVMCode:           e.codeChecker(),
			Blacklist:         e.blacklist(),
		},
		Treasury:          e.Treasury,
		DefaultExpiry:     e.Sessions.Default,
		MinExpiry:         e.Sessions.Min,
		MaxExpiry:         e.Sessions.Max,
		SelectWindow:      e.Sessions.SelectWindow,
		Solana:            e.solanaService(),
		SecretBox:         box,
		AllowPrivateHooks: e.AllowPrivateWebhooks,
		CheckoutBaseURL:   e.CheckoutBaseURL,
		APIBaseURL:        e.APIURL,
		DashboardURL:      e.DashboardURL,
		Mail:              e.Mail,
		Log:               e.Log,
		ExpiryHold:        e.Scan.ExpiryHold(),
		PauseRefresh:      &e.PauseRefresh,
		Hasher:            dashauth.NewHasher(0, dashauth.DefaultHashWait),
	}
	if e.PasswordBreachCheck {
		cfg.PasswordBreaches = pwned.New()
	}
	checkoutLock(e.CheckoutLock, &cfg)
	confirmWindow(e.ConfirmWindow, &cfg)
	return service.New(st, cfg), nil
}

// checkoutLock sets the API's view of the checkout lock settings on a service configuration; an
// Env built without config.Load keeps the service defaults.
func checkoutLock(c config.CheckoutLockEnv, cfg *service.Config) {
	if !c.Loaded {
		return
	}
	warn, watch := c.ExpiryWarning, c.WatchAfterExpiry
	cfg.CheckoutExpiryWarning, cfg.WatchAfterExpiry, cfg.RecheckLimits = &warn, &watch, c.RecheckLimits
}

// confirmWindow sets the dashboard's confirmation window (D59) on a service configuration.
func confirmWindow(w config.ConfirmWindow, cfg *service.Config) {
	d := w.Length
	cfg.ConfirmWindow = &d
	cfg.ConfirmWindowWallets = w.Wallets
}

func (e Root) solanaService() map[string]service.SolanaChain {
	out := map[string]service.SolanaChain{}
	for id, c := range e.Solana {
		if c.Addresses() {
			out[id] = service.SolanaChain{Program: c.Program.String(), RentReceiver: c.RentReceiver}
		}
	}
	return out
}

func (e Root) solanaDerivers(into map[string]derive.AddressDeriver) {
	for id, c := range e.Solana {
		if c.Addresses() {
			into[id] = derive.SolanaForwarders{Program: *c.Program}
		}
	}
}

// solanaProtocol lists our own Solana accounts, never acceptable as a merchant destination.
func (e Root) solanaProtocol() []string {
	var out []string
	for _, c := range e.Solana {
		if c.Program != nil {
			out = append(out, c.Program.String())
		}
		if c.RentReceiver != "" {
			out = append(out, c.RentReceiver)
		}
	}
	return out
}
