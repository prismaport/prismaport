package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/alert"
	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/chainwatch"
	"github.com/prismaport/prismaport/internal/config"
	"github.com/prismaport/prismaport/internal/derive"
	"github.com/prismaport/prismaport/internal/destination"
	"github.com/prismaport/prismaport/internal/evm"
	"github.com/prismaport/prismaport/internal/fee"
	"github.com/prismaport/prismaport/internal/payout"
	"github.com/prismaport/prismaport/internal/service"
	"github.com/prismaport/prismaport/internal/solana"
	"github.com/prismaport/prismaport/internal/solrpc"
	"github.com/prismaport/prismaport/internal/solwatch"
	"github.com/prismaport/prismaport/internal/store"
	"github.com/prismaport/prismaport/internal/webhooksig"
	"github.com/prismaport/prismaport/internal/worker"
)

const (
	program = "3DHLtkMYKFmxLbQRHkHcNPibfaKYJNntQEnJvUvcnPya"
	renter  = "CDYn2if6T4voQoxm5mZKRAg5a65ZVzhQ5RK8H8e2DmS8"
)

// load is config.Load as a Root, failing the test on an error.
func load(t *testing.T) Root {
	t.Helper()
	e, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	return Root{Env: e}
}

func TestServiceRequiresSecretKeyWhenAsked(t *testing.T) {
	e := Root{Env: config.Env{DatabaseURL: "postgres://x", Treasury: map[bool]map[chain.Family]string{true: {}, false: {}}}}
	st := e.Store(nil)
	if _, err := e.Service(st, true); err == nil {
		t.Fatal("missing secret key accepted")
	}
	if _, err := e.Service(st, false); err != nil {
		t.Fatalf("CLI path needs no secret key: %v", err)
	}
	e.SecretKeyHex = strings.Repeat("ab", 32)
	if _, err := e.Service(st, true); err != nil {
		t.Fatal(err)
	}
}

// Live Base derives the real forwarder (the pilot-defaults CREATE2 vector) once a factory is
// configured; Solana live stays unavailable.
func TestLiveBaseDerivation(t *testing.T) {
	t.Setenv("PRISMAPORT_DATABASE_URL", "postgres://x")
	t.Setenv("PRISMAPORT_CHAIN_BASE_FACTORY", "0xEBC1282Dc447787Db5AaC24c0D23Aa7f25b44083")
	e := load(t)
	reg := chain.Pilot()
	base, _ := reg.Get("base")
	salt, _ := evm.ParseWord("0x1284b03fcf39cc58f706da9ab524756420ae720f4ffdc20163b2f3ed146bc82a")
	p := derive.Params{Treasury: "0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed", Fee: fee.Params{FeeBps: 100, FixedFeeMicros: 100_000},
		Recipients: []payout.Recipient{{Address: "0x8ba1f109551bD432803012645Ac136ddd64DBA72", Bps: 10_000}},
		Tokens:     []string{"0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913"}}
	addr, err := e.Deriver().Address(base, salt, p)
	if err != nil || addr != "0xb11EFD3f93C697D6abb6ad9C0372b4c7A987e724" {
		t.Fatalf("live base address %s: %v", addr, err)
	}
	sol, _ := reg.Get("solana")
	if _, err := e.Deriver().Address(sol, salt, derive.Params{Treasury: "y", Fee: chain.PilotFee(), Recipients: []payout.Recipient{{Address: "x", Bps: 10_000}}}); !errors.Is(err, derive.ErrUnavailable) {
		t.Fatalf("solana live must be unavailable: %v", err)
	}
}

// The configured program and rent receiver are protocol accounts, never a merchant destination.
func TestSolanaProtocol(t *testing.T) {
	t.Setenv("PRISMAPORT_DATABASE_URL", "postgres://x")
	t.Setenv("PRISMAPORT_CHAIN_SOLANA_PROGRAM_ID", program)
	t.Setenv("PRISMAPORT_CHAIN_SOLANA_RENT_RECEIVER", renter)
	if got := load(t).solanaProtocol(); len(got) != 2 {
		t.Fatalf("protocol addresses %v", got)
	}
}

// Live Solana sessions get real PDAs once a program and rent receiver are configured; without
// them the deriver keeps failing closed (503 at session creation).
func TestSolanaLiveDerivation(t *testing.T) {
	sol, _ := chain.Pilot().Get("solana")
	p := derive.Params{Treasury: "7vzEoA6qPLqGXe5rxmMK7iha63znnLfwGppBrUfELajg", Fee: fee.Params{FeeBps: 100, FixedFeeMicros: 100_000},
		Recipients: []payout.Recipient{{Address: "DEb5yphxEaPc5BN118svVN4R3GFu9jKs31Gcv5yekjZx", Bps: 6000},
			{Address: "CDYn2if6T4voQoxm5mZKRAg5a65ZVzhQ5RK8H8e2DmS8", Bps: 4000}},
		Tokens: []string{"EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"}}
	if _, err := (Root{}).Deriver().Address(sol, [32]byte{}, p); !errors.Is(err, derive.ErrUnavailable) {
		t.Fatalf("unconfigured live Solana: %v", err)
	}
	t.Setenv("PRISMAPORT_DATABASE_URL", "postgres://x")
	t.Setenv("PRISMAPORT_CHAIN_SOLANA_PROGRAM_ID", program)
	t.Setenv("PRISMAPORT_CHAIN_SOLANA_RENT_RECEIVER", renter)
	e := load(t)
	cfg := e.solanaService()["solana"]
	p.RentReceiver = cfg.RentReceiver
	got, err := e.Deriver().Address(sol, [32]byte{}, p)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := derive.SolanaForwarders{Program: *e.Solana["solana"].Program}.Address(sol, [32]byte{}, p)
	if got != want || cfg.Program != program {
		t.Fatalf("live address %s, want %s", got, want)
	}
}

// rpcAccounts serves getMultipleAccounts from a fixed set of existing accounts.
func rpcAccounts(t *testing.T, accounts map[string][]byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage   `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Method != "getMultipleAccounts" {
			http.Error(w, "unexpected "+req.Method, http.StatusBadRequest)
			return
		}
		var addrs []string
		_ = json.Unmarshal(req.Params[0], &addrs)
		var values []any
		for _, a := range addrs {
			data, ok := accounts[a]
			if !ok {
				values = append(values, nil)
				continue
			}
			values = append(values, map[string]any{"data": []string{base64.StdEncoding.EncodeToString(data), "base64"},
				"owner": solana.TokenProgramID.String(), "lamports": 2039280, "executable": false, "rentEpoch": 0, "space": len(data)})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID,
			"result": map[string]any{"context": map[string]any{"slot": 100}, "value": values}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestMissingTokenAccounts(t *testing.T) {
	reg := chain.Pilot()
	owner := solana.MustPublicKey("DEb5yphxEaPc5BN118svVN4R3GFu9jKs31Gcv5yekjZx")
	usdc := solana.MustPublicKey("EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v")
	usdt := solana.MustPublicKey("Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB")
	tokenAccount := func(mint solana.PublicKey, state byte) []byte {
		d := make([]byte, solana.TokenAccountSize)
		copy(d, mint[:])
		copy(d[32:], owner[:])
		d[108] = state
		return d
	}
	usdcATA, _ := solana.AssociatedTokenAddress(owner, usdc)
	usdtATA, _ := solana.AssociatedTokenAddress(owner, usdt)
	inspector := func(accounts map[string][]byte) solanaInspector {
		srv := rpcAccounts(t, accounts)
		q := solwatch.NewQuorum("solana", []solwatch.Provider{solrpc.New("a", srv.URL, time.Second), solrpc.New("b", srv.URL, time.Second)}, chainwatch.Options{})
		return solanaInspector{reg: reg, quorums: map[string]*solwatch.Quorum{"solana": q}}
	}
	ctx := t.Context()
	got, err := inspector(map[string][]byte{usdcATA.String(): tokenAccount(usdc, 1)}).MissingTokenAccounts(ctx, true, owner.String())
	if err != nil || len(got) != 1 || got[0].Asset != "USDT" || got[0].Account != usdtATA.String() || got[0].Chain != "solana" {
		t.Fatalf("USDT ATA missing: %+v %v", got, err)
	}
	got, err = inspector(map[string][]byte{usdcATA.String(): tokenAccount(usdc, 1), usdtATA.String(): tokenAccount(usdt, 2)}).
		MissingTokenAccounts(ctx, true, owner.String())
	if err != nil || len(got) != 1 || got[0].Asset != "USDT" {
		t.Fatalf("a frozen account is not usable: %+v %v", got, err)
	}
	got, err = inspector(map[string][]byte{usdcATA.String(): tokenAccount(usdc, 1), usdtATA.String(): tokenAccount(usdt, 1)}).
		MissingTokenAccounts(ctx, true, owner.String())
	if err != nil || len(got) != 0 {
		t.Fatalf("both present: %+v %v", got, err)
	}
	// Test mode has solana-devnet, which this inspector has no providers for: fail closed.
	if _, err := inspector(nil).MissingTokenAccounts(ctx, false, owner.String()); !errors.Is(err, destination.ErrUnverifiable) {
		t.Fatalf("cluster without providers: %v", err)
	}
}

// Zero values of the checkout lock settings reach the service, which would otherwise take its
// defaults.
func TestCheckoutLockReachesTheService(t *testing.T) {
	t.Setenv("PRISMAPORT_DATABASE_URL", "postgres://x")
	t.Setenv("PRISMAPORT_CHECKOUT_EXPIRY_WARNING", "0s")
	t.Setenv("PRISMAPORT_WATCH_AFTER_EXPIRY", "0")
	t.Setenv("PRISMAPORT_RECHECK_LIMITS", " merchant = 5/30m ")
	svc, err := load(t).Service(nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if svc.WatchAfterExpiry() != 0 || svc.CheckoutExpiryWarning() != 0 {
		t.Fatalf("service: watch %s warning %s", svc.WatchAfterExpiry(), svc.CheckoutExpiryWarning())
	}
}

// An Env built by hand (tests, tools) keeps the service defaults.
func TestCheckoutLockUnloadedKeepsServiceDefaults(t *testing.T) {
	svc, err := Root{}.Service((*store.Store)(nil), false)
	if err != nil {
		t.Fatal(err)
	}
	if svc.WatchAfterExpiry() != service.DefaultWatchAfterExpiry || svc.CheckoutExpiryWarning() != service.DefaultCheckoutExpiryWarning {
		t.Fatalf("service: watch %s warning %s", svc.WatchAfterExpiry(), svc.CheckoutExpiryWarning())
	}
}

func mailEnv(t *testing.T, vars map[string]string) {
	t.Helper()
	t.Setenv("PRISMAPORT_DATABASE_URL", "postgres://x")
	for _, k := range []string{"PRISMAPORT_SMTP_HOST", "PRISMAPORT_SMTP_PORT", "PRISMAPORT_SMTP_TLS", "PRISMAPORT_SMTP_USERNAME",
		"PRISMAPORT_SMTP_PASSWORD", "PRISMAPORT_MAIL_FROM", "PRISMAPORT_RESEND_API_KEY", "PRISMAPORT_RESEND_BASE_URL"} {
		t.Setenv(k, vars[k])
	}
}

func TestMailerOverResend(t *testing.T) {
	mailEnv(t, map[string]string{"PRISMAPORT_RESEND_API_KEY": "re_test_123", "PRISMAPORT_MAIL_FROM": "Prismaport <no-reply@example.com>"})
	s, stop, err := load(t).Mailer(slog.New(slog.DiscardHandler))
	if err != nil || s == nil {
		t.Fatalf("mailer %v, %v", s, err)
	}
	if err := stop(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestMailerWithoutTransportHasNoSender(t *testing.T) {
	mailEnv(t, nil)
	s, stop, err := load(t).Mailer(slog.New(slog.DiscardHandler))
	if err != nil || s != nil {
		t.Fatalf("mailer %v, %v", s, err)
	}
	if err := stop(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestAlertsWithoutAWebhookOnlyLog(t *testing.T) {
	sink, deliver := Root{}.Alerts(slog.New(slog.DiscardHandler))

	if _, ok := sink.(alert.Log); !ok {
		t.Fatalf("sink %T, want only the log", sink)
	}
	deliver(t.Context())
}

func TestAlertsWithAWebhookLogAndPostSigned(t *testing.T) {
	type post struct{ body, sig string }
	got := make(chan post, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- post{string(b), r.Header.Get(webhooksig.HeaderName)}
	}))
	defer srv.Close()
	var logged strings.Builder
	log := slog.New(slog.NewTextHandler(&logged, nil))
	sink, deliver := Root{Env: config.Env{AlertWebhookURL: srv.URL, AlertWebhookSecret: "s3cret"}}.Alerts(log)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go deliver(ctx)

	sink.Alert(ctx, alert.Alert{Severity: alert.Critical, Chain: "base", Kind: "gas_wallet_empty", Message: "no gas"})

	var p post
	select {
	case p = <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("alert not posted")
	}
	var body struct{ Kind, Chain string }
	_ = json.Unmarshal([]byte(p.body), &body)
	if body.Kind != "gas_wallet_empty" || body.Chain != "base" {
		t.Fatalf("posted %s", p.body)
	}
	if err := webhooksig.Verify([]byte(p.body), p.sig, "s3cret", webhooksig.DefaultTolerance, time.Now()); err != nil {
		t.Fatalf("signature: %v", err)
	}
	if !strings.Contains(logged.String(), "alert=gas_wallet_empty") {
		t.Fatalf("alert not logged: %s", logged.String())
	}
}

// A failed provider verification is retried every retry interval, not on every ModeCheck tick.
func TestVerificationRetryBacksOff(t *testing.T) {
	calls, fail := 0, true
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p := retryPolicy{retry: 30 * time.Second, now: func() time.Time { return now }}
	step := verifiedBy(p, func(context.Context) error {
		calls++
		if fail {
			return errors.New("wrong chain")
		}
		return nil
	}, func(context.Context) (int, error) { return 7, nil })
	if _, err := step(t.Context()); err == nil || calls != 1 {
		t.Fatalf("first: %v %d", err, calls)
	}
	if _, err := step(t.Context()); !errors.Is(err, worker.ErrSkipped) || calls != 1 {
		t.Fatalf("retried at once: %v %d", err, calls)
	}
	now = now.Add(29 * time.Second)
	if _, err := step(t.Context()); !errors.Is(err, worker.ErrSkipped) || calls != 1 {
		t.Fatalf("retried before the interval: %v %d", err, calls)
	}
	now = now.Add(time.Second)
	fail = false
	if n, err := step(t.Context()); err != nil || n != 7 || calls != 2 {
		t.Fatalf("after the retry interval: %d %v %d", n, err, calls)
	}
	if _, err := step(t.Context()); err != nil || calls != 2 {
		t.Fatal("verified again")
	}
}

// The verification retry defaults to DefaultVerifyRetry on the store's clock; zero asks again on
// every tick.
func TestVerifyPolicy(t *testing.T) {
	st := Root{}.Store(nil)
	if p := (Root{}).verifyPolicy(st); p.retry != DefaultVerifyRetry || p.now == nil {
		t.Fatalf("default: %+v", p)
	}
	zero := time.Duration(0)
	if p := (Root{VerifyRetry: &zero}).verifyPolicy(st); p.retry != 0 {
		t.Fatalf("zero: %s", p.retry)
	}
}

// The chain scheduler's mode check is 2 minutes idle and 5 seconds active by default (plan B);
// an Env built without Load gets the same.
func TestModeCheckIdleAndActive(t *testing.T) {
	for _, s := range []config.ScanEnv{{}, config.DefaultScan()} {
		if idle, active := modeCheck(s); idle != 2*time.Minute || active != 5*time.Second {
			t.Fatalf("%+v: idle %s active %s", s, idle, active)
		}
	}
	if idle, active := modeCheck(config.ScanEnv{ModeCheck: time.Minute, ModeCheckActive: time.Second}); idle != time.Minute || active != time.Second {
		t.Fatalf("set: idle %s active %s", idle, active)
	}
}
