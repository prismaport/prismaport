// Package config reads the process configuration from the environment and validates it into
// plain values. It builds nothing: internal/app wires the process from them.
package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/destination"
	"github.com/prismaport/prismaport/internal/evm"
	"github.com/prismaport/prismaport/internal/mail"
	"github.com/prismaport/prismaport/internal/tuning"
)

// Env is the process configuration read from the environment.
type Env struct {
	DatabaseURL     string
	HTTPAddr        string
	CheckoutBaseURL string
	SecretKeyHex    string
	Treasury        map[bool]map[chain.Family]string
	// AllowPrivateWebhooks permits webhook deliveries to loopback and private networks, and live
	// endpoints on them. Local development only: live endpoints still need https, and test
	// endpoints take plain http only for localhost or the sandbox inbox.
	AllowPrivateWebhooks bool
	// PasswordBreachCheck refuses a new dashboard password found in a known data breach (D55).
	PasswordBreachCheck bool
	Chains              map[string]ChainEnv
	Solana              map[string]SolanaEnv
	// ChainsOff (PRISMAPORT_CHAINS_OFF, comma-separated chain ids) are the pilot chains this
	// environment does not serve at all: gone from the registry, so no merchant, product, session
	// or scanner sees them (D62).
	ChainsOff []string
	// PauseRefresh (PRISMAPORT_CHAIN_PAUSE_REFRESH, default 30s) is how long the chain pauses
	// (D64), the runtime layer over ChainsOff, are served before they are read again.
	PauseRefresh time.Duration
	// Scan is the adaptive scanner's configuration (PRISMAPORT_SCAN_*, PRISMAPORT_LATE_SCHEDULE,
	// PRISMAPORT_WATCH_AFTER_EXPIRY); Sessions bounds session lifetimes.
	Scan     ScanEnv
	Sessions SessionExpiry
	// IdempotencyTTL is how long the worker keeps a stored idempotent response
	// (PRISMAPORT_IDEMPOTENCY_TTL, default 7 days, at least MinIdempotencyTTL).
	IdempotencyTTL time.Duration
	// StepTimeout bounds one worker loop step (PRISMAPORT_WORKER_STEP_TIMEOUT, default
	// tuning.DefaultStepTimeout, at least MinStepTimeout).
	StepTimeout time.Duration
	// Worker is the worker's idle/active pacing (PRISMAPORT_WORKER_*_IDLE, _ACTIVE, _IDLE_AFTER).
	Worker WorkerEnv
	// GasKeyFile holds the gas wallet key; PRISMAPORT_GAS_KEY is the fallback. The key itself is
	// only read when the worker or relayer starts and is never kept in Env.
	GasKeyFile string
	// SolanaGasKeyFile holds the Solana fee payer (solana-keygen JSON); PRISMAPORT_SOLANA_GAS_KEY
	// is the fallback. Read by the worker and, for connected-wallet payments, the API.
	SolanaGasKeyFile string
	// SMTP is the mail relay for sign-up codes and security notices; nil when
	// PRISMAPORT_SMTP_HOST is unset. Resend is the HTTPS alternative for hosts that block SMTP
	// ports. With neither, self-serve sign-up answers 503.
	SMTP   *mail.SMTPConfig
	Resend *mail.ResendConfig
	// RequireMail (PRISMAPORT_REQUIRE_MAIL=true) makes the api refuse to start without a mail
	// transport: sign-up, password resets and every emailed confirmation code depend on it. Set
	// it on every hosted environment; off by default for local runs and tests.
	RequireMail bool
	// ConfirmWindow is the dashboard's confirmation window (D59).
	ConfirmWindow ConfirmWindow
	// DashboardURL is the dashboard's address in emails (default CheckoutBaseURL/dashboard/).
	DashboardURL string
	// SiteURL, when set, turns routing by host on: the landing page on its host, the dashboard
	// on DashboardURL's, the checkout on CheckoutBaseURL's, the API and the docs on APIURL's
	// (routing by host).
	SiteURL string
	// APIURL is the merchant API's public address; sandbox inbox URLs are built from it when it
	// is set (else from CheckoutBaseURL). The CLI reads the same variable as its default target.
	APIURL string
	// DocsURL is the developer portal's origin in split mode, which serves it at its root
	// ("https://docs.prismaport.com"); unused otherwise, where the binary serves it at /docs/.
	DocsURL string
	// CheckoutLock holds the checkout expiry warning, the watch window and the "Check again"
	// limits the API applies (adaptive scanner).
	CheckoutLock CheckoutLockEnv
	// ExternalFrontend (PRISMAPORT_FRONTEND=external) says the pages are served elsewhere
	// (Cloudflare Pages): the api serves no page and trusts the dashboard's origin across origins
	// (split mode). False (embedded, the default) serves every page.
	ExternalFrontend bool
	// AlertWebhookURL, when set, also receives the worker's alerts as JSON POSTs,
	// signed with AlertWebhookSecret when that is set.
	AlertWebhookURL    string
	AlertWebhookSecret string
}

// MinIdempotencyTTL is the shortest PRISMAPORT_IDEMPOTENCY_TTL: a key is replayed for at least the
// longest session lifetime (PRISMAPORT_SESSION_MAX_EXPIRY's bound), so a retry while its session
// can still be paid never creates a second one.
const MinIdempotencyTTL = 24 * time.Hour

// MinStepTimeout is the shortest PRISMAPORT_WORKER_STEP_TIMEOUT: a step makes requests bounded
// at 10 seconds each, so less would cut healthy steps.
const MinStepTimeout = 30 * time.Second

// loadStepTimeout reads PRISMAPORT_WORKER_STEP_TIMEOUT (default tuning.DefaultStepTimeout).
func loadStepTimeout() (time.Duration, error) {
	d, err := durationOr("PRISMAPORT_WORKER_STEP_TIMEOUT", tuning.DefaultStepTimeout)
	if err != nil {
		return 0, err
	}
	if d < MinStepTimeout {
		return 0, fmt.Errorf("PRISMAPORT_WORKER_STEP_TIMEOUT (%s) must be at least %s", d, MinStepTimeout)
	}
	return d, nil
}

// loadIdempotencyTTL reads PRISMAPORT_IDEMPOTENCY_TTL (default 7 days).
func loadIdempotencyTTL() (time.Duration, error) {
	d, err := durationOr("PRISMAPORT_IDEMPOTENCY_TTL", tuning.DefaultIdempotencyTTL)
	if err != nil {
		return 0, err
	}
	if d < MinIdempotencyTTL {
		return 0, fmt.Errorf("PRISMAPORT_IDEMPOTENCY_TTL (%s) must be at least %s", d, MinIdempotencyTTL)
	}
	return d, nil
}

// loadResend reads PRISMAPORT_RESEND_API_KEY and PRISMAPORT_MAIL_FROM.
func loadResend() (*mail.ResendConfig, error) {
	key := os.Getenv("PRISMAPORT_RESEND_API_KEY")
	if key == "" {
		return nil, nil
	}
	c := mail.ResendConfig{APIKey: key, From: os.Getenv("PRISMAPORT_MAIL_FROM"), BaseURL: get("PRISMAPORT_RESEND_BASE_URL", mail.ResendBaseURL)}
	if c.From == "" {
		return nil, fmt.Errorf("PRISMAPORT_MAIL_FROM is required with PRISMAPORT_RESEND_API_KEY (for example \"Prismaport <no-reply@example.com>\")")
	}
	if err := c.Check(); err != nil {
		return nil, err
	}
	return &c, nil
}

// loadSMTP reads PRISMAPORT_SMTP_* and PRISMAPORT_MAIL_FROM.
func loadSMTP() (*mail.SMTPConfig, error) {
	host := os.Getenv("PRISMAPORT_SMTP_HOST")
	if host == "" {
		return nil, nil
	}
	port, err := strconv.Atoi(get("PRISMAPORT_SMTP_PORT", "587"))
	if err != nil {
		return nil, fmt.Errorf("PRISMAPORT_SMTP_PORT must be a port number")
	}
	c := mail.SMTPConfig{Host: host, Port: port, Username: os.Getenv("PRISMAPORT_SMTP_USERNAME"), Password: os.Getenv("PRISMAPORT_SMTP_PASSWORD"),
		TLS: get("PRISMAPORT_SMTP_TLS", mail.TLSStartTLS), From: os.Getenv("PRISMAPORT_MAIL_FROM")}
	if c.From == "" {
		return nil, fmt.Errorf("PRISMAPORT_MAIL_FROM is required with PRISMAPORT_SMTP_HOST (for example \"Prismaport <no-reply@example.com>\")")
	}
	if err := c.Check(); err != nil {
		return nil, err
	}
	return &c, nil
}

// CheckMail refuses an environment that requires mail (RequireMail) without a transport.
func (e Env) CheckMail() error {
	if e.RequireMail && e.Resend == nil && e.SMTP == nil {
		return fmt.Errorf("PRISMAPORT_REQUIRE_MAIL is true but no mail transport is configured: set PRISMAPORT_RESEND_API_KEY or PRISMAPORT_SMTP_HOST, with PRISMAPORT_MAIL_FROM")
	}
	return nil
}

// GasKey loads the gas wallet that pays for flushes and signed-path settlements.
func (e Env) GasKey() (*evm.PrivateKey, error) {
	return evm.LoadPrivateKey(e.GasKeyFile, os.Getenv("PRISMAPORT_GAS_KEY"))
}

func get(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// Load reads the configuration from the environment and validates it.
func Load() (Env, error) {
	e := Env{
		DatabaseURL:          os.Getenv("PRISMAPORT_DATABASE_URL"),
		HTTPAddr:             get("PRISMAPORT_HTTP_ADDR", ":8080"),
		CheckoutBaseURL:      get("PRISMAPORT_CHECKOUT_BASE_URL", "http://localhost:8080"),
		SecretKeyHex:         os.Getenv("PRISMAPORT_SECRET_KEY"),
		AllowPrivateWebhooks: get("PRISMAPORT_WEBHOOK_ALLOW_PRIVATE", "false") == "true",
		PasswordBreachCheck:  get("PRISMAPORT_PASSWORD_BREACH_CHECK", "true") != "false",
		RequireMail:          get("PRISMAPORT_REQUIRE_MAIL", "false") == "true",
		GasKeyFile:           os.Getenv("PRISMAPORT_GAS_KEY_FILE"),
		DashboardURL:         os.Getenv("PRISMAPORT_DASHBOARD_URL"),
		SiteURL:              os.Getenv("PRISMAPORT_SITE_URL"),
		APIURL:               os.Getenv("PRISMAPORT_API_URL"),
		DocsURL:              os.Getenv("PRISMAPORT_DOCS_URL"),
		SolanaGasKeyFile:     os.Getenv("PRISMAPORT_SOLANA_GAS_KEY_FILE"),
		AlertWebhookURL:      os.Getenv("PRISMAPORT_ALERT_WEBHOOK_URL"),
		AlertWebhookSecret:   os.Getenv("PRISMAPORT_ALERT_WEBHOOK_SECRET"),
		Treasury: map[bool]map[chain.Family]string{
			true: {
				chain.EVM:    os.Getenv("PRISMAPORT_TREASURY_EVM_LIVE"),
				chain.Solana: os.Getenv("PRISMAPORT_TREASURY_SOLANA_LIVE"),
			},
			false: {
				chain.EVM:    os.Getenv("PRISMAPORT_TREASURY_EVM_TEST"),
				chain.Solana: os.Getenv("PRISMAPORT_TREASURY_SOLANA_TEST"),
			},
		},
	}
	if e.DatabaseURL == "" {
		return e, fmt.Errorf("PRISMAPORT_DATABASE_URL is required")
	}
	switch f := os.Getenv("PRISMAPORT_FRONTEND"); f {
	case "", "embedded":
	case "external":
		e.ExternalFrontend = true
	default:
		return e, fmt.Errorf("PRISMAPORT_FRONTEND must be embedded or external, got %q", f)
	}
	if err := checkAlertWebhook(e.AlertWebhookURL, e.AlertWebhookSecret); err != nil {
		return e, err
	}
	smtp, err := loadSMTP()
	if err != nil {
		return e, err
	}
	resend, err := loadResend()
	if err != nil {
		return e, err
	}
	if smtp != nil && resend != nil {
		return e, fmt.Errorf("set PRISMAPORT_SMTP_HOST or PRISMAPORT_RESEND_API_KEY, not both")
	}
	e.SMTP, e.Resend = smtp, resend
	if e.Scan, err = loadScan(); err != nil {
		return e, err
	}
	if e.Sessions, err = loadSessionExpiry(); err != nil {
		return e, err
	}
	if e.IdempotencyTTL, err = loadIdempotencyTTL(); err != nil {
		return e, err
	}
	if e.Worker, err = loadWorker(); err != nil {
		return e, err
	}
	if e.StepTimeout, err = loadStepTimeout(); err != nil {
		return e, err
	}
	if e.ConfirmWindow, err = loadConfirmWindow(); err != nil {
		return e, err
	}
	if e.CheckoutLock, err = loadCheckoutLock(e.Scan.WatchAfterExpiry); err != nil {
		return e, err
	}
	if e.ChainsOff, err = loadChainsOff(); err != nil {
		return e, err
	}
	if e.PauseRefresh, err = loadPauseRefresh(); err != nil {
		return e, err
	}
	chains, err := loadChains(e.Registry(), e.Scan.Intervals)
	if err != nil {
		return e, err
	}
	e.Chains = chains
	if e.Solana, err = loadSolanaChains(e.Registry(), e.Scan.Intervals); err != nil {
		return e, err
	}
	for live, fams := range e.Treasury {
		for fam, addr := range fams {
			if addr == "" {
				delete(fams, fam)
				continue
			}
			canonical, err := validateTreasury(fam, addr)
			if err != nil {
				return e, fmt.Errorf("treasury %s (live=%v): %w", fam, live, err)
			}
			fams[fam] = canonical
		}
	}
	return e, nil
}

// checkAlertWebhook accepts an absolute https URL, or plain http to a loopback host.
func checkAlertWebhook(raw, secret string) error {
	if raw == "" {
		if secret != "" {
			return fmt.Errorf("PRISMAPORT_ALERT_WEBHOOK_SECRET is set without PRISMAPORT_ALERT_WEBHOOK_URL")
		}
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("PRISMAPORT_ALERT_WEBHOOK_URL must be an absolute URL")
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if h := u.Hostname(); h == "localhost" || h == "127.0.0.1" || h == "::1" {
			return nil
		}
	}
	return fmt.Errorf("PRISMAPORT_ALERT_WEBHOOK_URL must use https (plain http only to localhost)")
}

func validateTreasury(fam chain.Family, addr string) (string, error) {
	switch fam {
	case chain.EVM:
		a, err := destination.ValidateEVMFormat(addr)
		if err != nil {
			return "", err
		}
		return a.Hex(), nil
	case chain.Solana:
		return addr, destination.ValidateSolanaFormat(addr)
	}
	return "", fmt.Errorf("unknown family %s", fam)
}

// LiveProtocolGaps names the variables a live payout write for the family needs and this
// environment lacks: the family's live treasury and every live chain's contracts (EVM Factory
// and Gateway, Solana program and rent receiver). They feed the destination denylist, so a
// process started without them would accept, say, the live treasury as a merchant's payout
// wallet. Nil means the denylist is complete.
func (e Env) LiveProtocolGaps(fam chain.Family) []string {
	var missing []string
	if e.Treasury[true][fam] == "" {
		missing = append(missing, "PRISMAPORT_TREASURY_"+strings.ToUpper(string(fam))+"_LIVE")
	}
	for _, c := range e.Registry().ForMode(true) {
		if c.Family != fam {
			continue
		}
		p := envKey(c.ID)
		switch fam {
		case chain.EVM:
			if e.Chains[c.ID].Factory == nil {
				missing = append(missing, p+"FACTORY")
			}
			if e.Chains[c.ID].Gateway == nil {
				missing = append(missing, p+"GATEWAY")
			}
		case chain.Solana:
			// The loader sets the program and the rent receiver together.
			if e.Solana[c.ID].Program == nil {
				missing = append(missing, p+"PROGRAM_ID", p+"RENT_RECEIVER")
			}
		}
	}
	return missing
}
