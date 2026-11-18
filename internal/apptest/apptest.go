// Package apptest wires a full application (store, service, API, dispatcher) on a fresh test
// database for integration tests.
package apptest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prismaport/prismaport/internal/alert"
	"github.com/prismaport/prismaport/internal/api"
	"github.com/prismaport/prismaport/internal/apicontract"
	"github.com/prismaport/prismaport/internal/apikey"
	"github.com/prismaport/prismaport/internal/base58"
	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/checkout"
	"github.com/prismaport/prismaport/internal/derive"
	"github.com/prismaport/prismaport/internal/destination"
	"github.com/prismaport/prismaport/internal/mail"
	"github.com/prismaport/prismaport/internal/secretbox"
	"github.com/prismaport/prismaport/internal/service"
	"github.com/prismaport/prismaport/internal/store"
	"github.com/prismaport/prismaport/internal/testdb"
	"github.com/prismaport/prismaport/internal/view"
	"github.com/prismaport/prismaport/internal/webhook"
)

// Fixed addresses and URLs the test application is configured with.
const (
	EVMDestination = "0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed"
	EVMTreasury    = "0xfB6916095ca1df60bB79Ce92cE3Ea74c37c5d359"
	CheckoutBase   = "https://pay.test"
)

// Fixed Solana addresses the test application is configured with.
var (
	SolanaDestination = base58.Encode(bytes.Repeat([]byte{7}, 32))
	SolanaTreasury    = base58.Encode(bytes.Repeat([]byte{9}, 32))
)

// Clock is a settable time source.
type Clock struct {
	mu sync.Mutex
	t  time.Time
}

// Now returns the clock's current time.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// Set moves the clock to t (tests keep it in step with block timestamps).
func (c *Clock) Set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

// Advance moves the clock forward by d.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// AnyMode derives stub addresses for live chains too. Tests only: it lets the live path be
// exercised before the real derivation exists.
type AnyMode struct{}

// Address derives a stub address as if the chain were in test mode.
func (AnyMode) Address(c chain.Chain, salt [32]byte, p derive.Params) (string, error) {
	c.Livemode = false
	return derive.Stub{}.Address(c, salt, p)
}

type wallet struct{}

func (wallet) Inspect(context.Context, bool, string) (destination.SolanaAccountKind, error) {
	return destination.SolanaWallet, nil
}

func (wallet) HasCode(context.Context, string, string) (bool, error) { return false, nil }

// MissingTokenAccounts reports none: tests that need a missing merchant token account set a
// destination whose account they never create and drive the flusher, which checks on chain.
func (wallet) MissingTokenAccounts(context.Context, bool, string) ([]destination.MissingTokenAccount, error) {
	return nil, nil
}

// Blacklist is the issuer blacklist the test application consults: no address is listed until a
// test lists one, and Down makes every lookup fail as an unreachable provider would.
type Blacklist struct {
	mu     sync.Mutex
	listed map[string]bool
	down   bool
}

// IsBlacklisted reports whether a test listed the address, or fails as an unreachable provider
// while the blacklist is down.
func (b *Blacklist) IsBlacklisted(_ context.Context, _, _, address string) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.down {
		return false, errors.New("apptest: blacklist provider down")
	}
	return b.listed[strings.ToLower(address)], nil
}

// List marks an address as blacklisted (or not).
func (b *Blacklist) List(address string, listed bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.listed == nil {
		b.listed = map[string]bool{}
	}
	b.listed[strings.ToLower(address)] = listed
}

// Down makes every lookup fail.
func (b *Blacklist) Down(down bool) {
	b.mu.Lock()
	b.down = down
	b.mu.Unlock()
}

// Options change how New builds the test application.
type Options struct {
	Deriver derive.AddressDeriver
	// Registry replaces the pilot registry (for example with a local anvil chain).
	Registry *chain.Registry
	// Solana configures real forwarder addresses per Solana chain id.
	Solana map[string]service.SolanaChain
	// Start is the initial clock time; zero keeps the fixed default.
	Start time.Time
	// Checkout, when set, mounts the public checkout endpoints built from the service and its store.
	Checkout func(*service.Service, *store.Store) *checkout.Checkout
	// SolanaInspector replaces the stub that reports every Solana destination as a ready wallet.
	SolanaInspector destination.SolanaInspector
	// EVMCode replaces the stub that reports no contract code anywhere.
	EVMCode destination.EVMCodeChecker
	// Blacklist replaces the in-memory USDC issuer blacklist (App.Blacklist), for example with
	// one that reads a local node.
	Blacklist destination.BlacklistChecker
	// Public configures the public endpoints' rate limits; nil uses limits high enough that
	// tests about something else never hit them.
	Public           *api.PublicOptions
	WebhookAttempts  int
	WebhookBaseDelay time.Duration
	// WebhookAlerts receives the dispatcher's alerts (default: discarded).
	WebhookAlerts alert.Sink
	// DatabaseURL, when set, is a migrated database the app opens instead of a fresh one.
	DatabaseURL string
	// APIBaseURL is the service's merchant API address (sandbox inbox URLs); empty keeps them
	// on CheckoutBase.
	APIBaseURL string
	// WatchAfterExpiry, CheckoutExpiryWarning and RecheckLimits replace the service defaults.
	WatchAfterExpiry      *time.Duration
	CheckoutExpiryWarning *time.Duration
	RecheckLimits         service.RecheckLimits
	// ExpiryHold is service.Config.ExpiryHold (0: expire at expires_at).
	ExpiryHold time.Duration
	// ConfirmWindow and ConfirmWindowWallets are the service's confirmation window (nil and 0:
	// the defaults).
	ConfirmWindow        *time.Duration
	ConfirmWindowWallets int
	// PasswordBreaches is service.Config.PasswordBreaches (nil: no breach check, no network).
	PasswordBreaches service.PasswordBreachChecker
	// PauseRefresh is service.Config.PauseRefresh (nil: the 30 s default, on the app's clock).
	PauseRefresh *time.Duration
}

// App is a running test application: store, service, HTTP server, webhook dispatcher and clock over
// a fresh database.
type App struct {
	T          testing.TB
	Store      *store.Store
	Svc        *service.Service
	View       view.Renderer
	Server     *httptest.Server
	Dispatcher *webhook.Dispatcher
	Clock      *Clock
	Registry   *chain.Registry
	MerchantID string
	TestKey    string
	LiveKey    string
	// Box seals webhook secrets like the service does (tests plant legacy secrets with it).
	Box *secretbox.Box
	// Blacklist is the USDC issuer blacklist the application checks.
	Blacklist *Blacklist
	// Mail records every email the application sends.
	Mail *mail.Memory

	linksMu sync.Mutex
	links   map[string]string
	linkSeq int
	// walletMu guards walletUsers: per merchant, the dashboard user Wallet adds wallets as.
	walletMu    sync.Mutex
	walletUsers map[string]service.DashUser
}

// New starts a test application over a fresh test database and stops it when the test ends.
func New(t testing.TB, opt Options) *App {
	t.Helper()
	var pool *pgxpool.Pool
	if opt.DatabaseURL != "" {
		pool = testdb.Open(t, opt.DatabaseURL)
	} else {
		pool = testdb.Pool(t)
	}
	clock := &Clock{t: time.Date(2026, 10, 23, 18, 0, 0, 0, time.UTC)}
	if !opt.Start.IsZero() {
		clock.t = opt.Start.UTC()
	}
	renderer := view.Renderer{CheckoutBaseURL: CheckoutBase}
	st := store.New(pool, renderer).WithClock(clock.Now)
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	box, err := secretbox.New(key)
	if err != nil {
		t.Fatal(err)
	}
	if opt.Deriver == nil {
		opt.Deriver = derive.ByFamily{chain.EVM: AnyMode{}, chain.Solana: AnyMode{}}
	}
	reg := opt.Registry
	if reg == nil {
		reg = chain.Pilot()
	}
	treasury := map[chain.Family]string{chain.EVM: EVMTreasury, chain.Solana: SolanaTreasury}
	var inspector destination.SolanaInspector = wallet{}
	if opt.SolanaInspector != nil {
		inspector = opt.SolanaInspector
	}
	var code destination.EVMCodeChecker = wallet{}
	if opt.EVMCode != nil {
		code = opt.EVMCode
	}
	blacklist := &Blacklist{}
	mailer := &mail.Memory{}
	var checker destination.BlacklistChecker = blacklist
	if opt.Blacklist != nil {
		checker = opt.Blacklist
	}
	svc := service.New(st, service.Config{
		Registry: reg,
		Deriver:  opt.Deriver,
		Validator: destination.Validator{Registry: reg, Solana: inspector, EVMCode: code, Blacklist: checker,
			ProtocolAddresses: map[chain.Family][]string{chain.EVM: {EVMTreasury}, chain.Solana: {SolanaTreasury}}},
		Treasury:          map[bool]map[chain.Family]string{true: treasury, false: treasury},
		Solana:            opt.Solana,
		SecretBox:         box,
		CheckoutBaseURL:   CheckoutBase,
		APIBaseURL:        opt.APIBaseURL,
		AllowPrivateHooks: true,
		Mail:              mailer,
		PasswordBreaches:  opt.PasswordBreaches,

		WatchAfterExpiry:      opt.WatchAfterExpiry,
		CheckoutExpiryWarning: opt.CheckoutExpiryWarning,
		RecheckLimits:         opt.RecheckLimits,
		ExpiryHold:            opt.ExpiryHold,
		ConfirmWindow:         opt.ConfirmWindow,
		ConfirmWindowWallets:  opt.ConfirmWindowWallets,
		PauseRefresh:          opt.PauseRefresh,
	})
	handler := api.New(svc, st, nil)
	public := api.PublicOptions{Limits: map[string]api.Limit{}}
	for route := range api.DefaultLimits() {
		public.Limits[route] = api.Limit{PerMinute: 10_000, Burst: 10_000}
	}
	if opt.Public != nil {
		public = *opt.Public
	}
	if err := handler.ConfigurePublic(public); err != nil {
		t.Fatal(err)
	}
	if opt.Checkout != nil {
		co := opt.Checkout(svc, st)
		handler.MountCheckout(co)
		handler.MountSolanaCheckout(co)
		handler.MountCheckoutLinks(co)
		handler.MountCheckoutAddresses(co)
	}
	d := webhook.NewDispatcher(svc, st, webhook.Config{AllowPrivateNetworks: true, MaxAttempts: opt.WebhookAttempts,
		BaseBackoff: opt.WebhookBaseDelay, Timeout: 5 * time.Second, Alerts: opt.WebhookAlerts}, nil)
	handler.MountSandbox(d)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	ctx := context.Background()
	// A family with no chain in the registry (Options.Registry, chains switched off) gets no wallet.
	has := func(f chain.Family) bool {
		return slices.ContainsFunc(append(reg.ForMode(true), reg.ForMode(false)...), func(c chain.Chain) bool { return c.Family == f })
	}
	evmDest, solDest := EVMDestination, SolanaDestination
	if !has(chain.EVM) {
		evmDest = ""
	}
	if !has(chain.Solana) {
		solDest = ""
	}
	seed, err := svc.SeedMerchant(ctx, service.SeedInput{Name: "Acme Store", Livemode: false,
		EVMDestination: evmDest, SolanaDestination: solDest, Actor: "test"})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	app := &App{T: t, Store: st, Svc: svc, View: renderer, Server: srv, Dispatcher: d, Clock: clock, Registry: reg,
		MerchantID: seed.Merchant.ID, TestKey: seed.Plaintext, Box: box, Blacklist: blacklist, Mail: mailer}
	for _, fd := range []struct {
		fam  chain.Family
		addr string
	}{{chain.EVM, evmDest}, {chain.Solana, solDest}} {
		if fd.addr == "" {
			continue
		}
		if _, err := svc.SetDestination(ctx, app.MerchantID, true, fd.fam, fd.addr, false, "test"); err != nil {
			t.Fatal(err)
		}
	}
	var live []store.ChainAsset
	for _, c := range reg.ForMode(true) {
		for _, a := range c.Assets {
			live = append(live, store.ChainAsset{Chain: c.ID, Asset: a.Symbol})
		}
	}
	if err := st.SetAcceptedAssets(ctx, app.MerchantID, true, live, "test"); err != nil {
		t.Fatal(err)
	}
	if _, app.LiveKey, err = svc.CreateAPIKey(ctx, app.MerchantID, true, nil, "test"); err != nil {
		t.Fatal(err)
	}
	return app
}

// Response is a decoded API response.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// JSON decodes the response body as a JSON object, failing the test when it is not one.
func (r Response) JSON(t testing.TB) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.Body, &m); err != nil {
		t.Fatalf("decode %s: %v", r.Body, err)
	}
	return m
}

// ErrorCode returns error.code from an error response.
func (r Response) ErrorCode(t testing.TB) string {
	t.Helper()
	m := r.JSON(t)
	e, ok := m["error"].(map[string]any)
	if !ok {
		t.Fatalf("not an error response (%d): %s", r.Status, r.Body)
	}
	return e["code"].(string)
}

// Do sends a request with the given key; body may be a string, []byte or any JSON value. A
// "Host" pair in headers sets the request's host.
func (a *App) Do(method, path, key string, body any, headers ...string) Response {
	a.T.Helper()
	var rdr io.Reader
	var sent []byte
	switch b := body.(type) {
	case nil:
	case string:
		sent = []byte(b)
	case []byte:
		sent = b
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			a.T.Fatal(err)
		}
		sent = raw
	}
	if body != nil {
		rdr = bytes.NewReader(sent)
	}
	req, err := http.NewRequest(method, a.Server.URL+path, rdr)
	if err != nil {
		a.T.Fatal(err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		if headers[i] == "Host" {
			req.Host = headers[i+1]
			continue
		}
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.T.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	contract := contractDoc(a.T)
	for _, p := range contract.CheckResponse(method, path, resp.StatusCode, raw) {
		a.T.Errorf("docs/openapi.json does not describe this answer: %s\n%s", p, raw)
	}
	if resp.StatusCode < 300 {
		for _, p := range contract.CheckRequest(method, path, sent) {
			a.T.Errorf("docs/openapi.json does not describe this accepted request: %s\n%s", p, sent)
		}
	}
	return Response{Status: resp.StatusCode, Header: resp.Header, Body: raw}
}

var (
	contractOnce sync.Once
	contract     *apicontract.Doc
	contractErr  error
)

// contractDoc is docs/openapi.json. Every answer Do receives must be one the contract describes,
// so the integration tests also keep the developer portal true.
func contractDoc(t testing.TB) *apicontract.Doc {
	t.Helper()
	contractOnce.Do(func() { contract, contractErr = apicontract.Load() })
	if contractErr != nil {
		t.Fatal(contractErr)
	}
	return contract
}

// Link answers an open-price product (minimum 0.11 USD, no maximum) of the key's merchant and
// mode, created once per merchant and mode with the default payout (the oldest active wallet of
// each family at 100% on every chain). Every session comes from a product; tests about something
// else open theirs from this one.
func (a *App) Link(key string) string {
	a.T.Helper()
	ctx := context.Background()
	auth, err := a.Svc.Authenticate(ctx, key)
	if err != nil {
		a.T.Fatalf("link: %v", err)
	}
	k := fmt.Sprintf("%s/%v", auth.MerchantID, auth.Livemode)
	a.linksMu.Lock()
	defer a.linksMu.Unlock()
	if id, ok := a.links[k]; ok {
		return id
	}
	auth.Scopes = nil
	for _, sc := range apikey.AllScopes {
		auth.Scopes = append(auth.Scopes, string(sc))
	}
	name, price, min := "Test link", "open", "0.11"
	p, err := a.Svc.CreateProduct(ctx, auth, service.ProductInput{Name: service.Set(name), PriceType: service.Set(price), MinAmount: service.Set(min)})
	if err != nil {
		a.T.Fatalf("link: %v", err)
	}
	if a.links == nil {
		a.links = map[string]string{}
	}
	a.links[k] = p.ID
	return p.ID
}

// PaymentLink creates a payment link of product with POST /v1/payment_links; body adds link
// fields (name, amount, max_payments, ...). It answers the link id.
func (a *App) PaymentLink(key, product string, body map[string]any) string {
	a.T.Helper()
	m := map[string]any{"product_id": product}
	for k, v := range body {
		m[k] = v
	}
	a.linksMu.Lock()
	a.linkSeq++
	n := a.linkSeq
	a.linksMu.Unlock()
	r := a.Do("POST", "/v1/payment_links", key, m, "Idempotency-Key", fmt.Sprintf("apptest-link-%d", n))
	if r.Status != http.StatusOK {
		a.T.Fatalf("payment link: %d %s", r.Status, r.Body)
	}
	return r.JSON(a.T)["id"].(string)
}

// OpenLink opens a session from a payment link's public page (POST
// /v1/checkout/links/{id}/sessions); the app needs Options.Checkout.
func (a *App) OpenLink(link, idemKey string, body any) Response {
	a.T.Helper()
	return a.Do("POST", "/v1/checkout/links/"+link+"/sessions", "", body, "Idempotency-Key", idemKey)
}

// Session opens a session with POST /v1/sessions. body (a JSON string or a map) gets the
// product_id of Link(key) unless it names one.
func (a *App) Session(key string, body any, headers ...string) Response {
	a.T.Helper()
	m := map[string]any{}
	switch b := body.(type) {
	case nil:
	case string:
		if err := json.Unmarshal([]byte(b), &m); err != nil {
			a.T.Fatalf("session body %s: %v", b, err)
		}
	case map[string]any:
		for k, v := range b {
			m[k] = v
		}
	default:
		a.T.Fatalf("session body of type %T", body)
	}
	if _, ok := m["product_id"]; !ok {
		m["product_id"] = a.Link(key)
	}
	return a.Do("POST", "/v1/sessions", key, m, headers...)
}

// Reveal reveals networks of a session through the merchant API (POST /v1/sessions/{id}/reveal),
// every network of its pay_to when chains is empty, as a payer pressing "Generate payment" on each
// would (D48): their deposit details show and they are watched. It fails the test unless every
// reveal answers 200, and answers the last one.
func (a *App) Reveal(key, id string, chains ...string) Response {
	a.T.Helper()
	if len(chains) == 0 {
		r := a.Do("GET", "/v1/sessions/"+id, key, nil)
		if r.Status != http.StatusOK {
			a.T.Fatalf("reveal %s: %d %s", id, r.Status, r.Body)
		}
		seen := map[string]bool{}
		for _, e := range r.JSON(a.T)["pay_to"].([]any) {
			c := e.(map[string]any)["chain"].(string)
			if !seen[c] {
				seen[c] = true
				chains = append(chains, c)
			}
		}
	}
	var r Response
	for _, c := range chains {
		r = a.Do("POST", "/v1/sessions/"+id+"/reveal", key, map[string]any{"chain": c})
		if r.Status != http.StatusOK {
			a.T.Fatalf("reveal %s on %s: %d %s", id, c, r.Status, r.Body)
		}
	}
	return r
}

// CheckoutReveal is the payer pressing "Generate payment" on one network of the hosted checkout
// (POST /v1/checkout/sessions/{id}/reveal); the app needs Options.Checkout.
func (a *App) CheckoutReveal(id, chain string) Response {
	a.T.Helper()
	return a.Do("POST", "/v1/checkout/sessions/"+id+"/reveal", "", map[string]any{"chain": chain})
}

// RevealedSession is Session followed by Reveal of every network: the session as tests about
// detection, payments or addresses need it. A refused creation answers as Session does.
func (a *App) RevealedSession(key string, body any, headers ...string) Response {
	a.T.Helper()
	r := a.Session(key, body, headers...)
	if r.Status != http.StatusOK {
		return r
	}
	return a.Reveal(key, r.JSON(a.T)["id"].(string))
}

// walletUserPassword signs the dashboard users Wallet creates.
const walletUserPassword = "Apptest-wallet-step-up-1"

// Wallet adds a payout wallet to the key's merchant and mode, the only way there is: as a
// dashboard user confirming with an emailed code in live mode. It fails the test unless the
// wallet is added.
func (a *App) Wallet(key string, family chain.Family, label, address string) service.WalletView {
	a.T.Helper()
	du, auth := a.walletUser(key)
	proof := a.confirmation(du, auth.Livemode, service.ConfirmWalletCreate)
	w, err := a.Svc.DashCreateWallet(context.Background(), du, auth.Livemode, service.WalletInput{Family: string(family), Label: label, Address: address}, proof)
	if err != nil {
		a.T.Fatalf("wallet %s: %v", address, err)
	}
	return w
}

// ArchiveWallet archives a wallet of the key's merchant and mode as a dashboard user and fails
// the test unless it is archived.
func (a *App) ArchiveWallet(key, id string) service.WalletView {
	a.T.Helper()
	du, auth := a.walletUser(key)
	w, err := a.Svc.DashArchiveWallet(context.Background(), du, auth.Livemode, id, a.confirmation(du, auth.Livemode, service.ConfirmWalletArchive))
	if err != nil {
		a.T.Fatalf("archive wallet %s: %v", id, err)
	}
	return w
}

// confirmationCode matches the code line of an emailed code.
var confirmationCode = regexp.MustCompile(`(?m)^    (\d{6})$`)

// confirmation asks for a code emailed for action and reads it from the user's newest email, as
// the owner would; test mode needs none.
func (a *App) confirmation(du service.DashUser, livemode bool, action string) service.StepUpProof {
	a.T.Helper()
	if !livemode {
		return service.StepUpProof{}
	}
	if _, err := a.Svc.SendConfirmationCode(context.Background(), du, true, action); err != nil {
		a.T.Fatalf("confirmation code: %v", err)
	}
	msgs := a.Mail.To(du.Email)
	if len(msgs) == 0 || msgs[len(msgs)-1].Kind != mail.KindConfirmationCode {
		a.T.Fatalf("no confirmation code emailed to %s", du.Email)
	}
	m := confirmationCode.FindStringSubmatch(msgs[len(msgs)-1].Text)
	if m == nil {
		a.T.Fatalf("no code in the confirmation email: %s", msgs[len(msgs)-1].Text)
	}
	return service.StepUpProof{EmailCode: &m[1]}
}

func (a *App) walletUser(key string) (service.DashUser, service.Auth) {
	a.T.Helper()
	ctx := context.Background()
	auth, err := a.Svc.Authenticate(ctx, key)
	if err != nil {
		a.T.Fatalf("wallet: %v", err)
	}
	a.walletMu.Lock()
	du, ok := a.walletUsers[auth.MerchantID]
	if !ok {
		u, err := a.Svc.CreateDashboardUser(ctx, auth.MerchantID, "wallets+"+strings.ToLower(auth.MerchantID)+"@apptest.example", walletUserPassword, "test")
		if err != nil {
			a.walletMu.Unlock()
			a.T.Fatalf("wallet user: %v", err)
		}
		du = service.DashUser{ID: u.ID, MerchantID: u.MerchantID, Email: u.Email}
		if a.walletUsers == nil {
			a.walletUsers = map[string]service.DashUser{}
		}
		a.walletUsers[auth.MerchantID] = du
	}
	a.walletMu.Unlock()
	return du, auth
}

// SplitLink makes Link(key) pay every chain of family in the key's mode to the given wallets in
// payout order: wallet id, share, wallet id, share, ...
func (a *App) SplitLink(key string, family chain.Family, pairs ...string) {
	a.T.Helper()
	auth, err := a.Svc.Authenticate(context.Background(), key)
	if err != nil {
		a.T.Fatalf("split link: %v", err)
	}
	var items []map[string]string
	for i := 0; i+1 < len(pairs); i += 2 {
		items = append(items, map[string]string{"wallet_id": pairs[i], "share": pairs[i+1]})
	}
	payout := map[string]any{}
	for _, c := range a.Registry.ForMode(auth.Livemode) {
		if c.Family == family {
			payout[c.ID] = items
		}
	}
	if r := a.Do("PATCH", "/v1/products/"+a.Link(key), key, map[string]any{"payout": payout}); r.Status != http.StatusOK {
		a.T.Fatalf("split link: %d %s", r.Status, r.Body)
	}
}
