// Package checkout serves the public, unauthenticated endpoints a payer's browser uses: a
// read-only view of a session and the EIP-3009 signed path (typed data to sign, then the
// signature, which the gas wallet submits to Gateway.settle). A session id is the only scope;
// nothing a merchant keeps private (metadata, merchant id, settlements) is exposed.
package checkout

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/prismaport/prismaport/internal/alert"
	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/chainwatch"
	"github.com/prismaport/prismaport/internal/contracts"
	"github.com/prismaport/prismaport/internal/derive"
	"github.com/prismaport/prismaport/internal/evm"
	"github.com/prismaport/prismaport/internal/evmrpc"
	"github.com/prismaport/prismaport/internal/fee"
	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/service"
	"github.com/prismaport/prismaport/internal/session"
	"github.com/prismaport/prismaport/internal/solana"
	"github.com/prismaport/prismaport/internal/store"
	"github.com/prismaport/prismaport/internal/txjob"
)

// Error codes of the public checkout endpoints.
const (
	CodeSessionNotPayable  = service.CodeSessionNotPayable
	CodeInvalidSignature   = "invalid_signature"
	CodeAuthorizationError = "authorization_rejected"
	// CodeSessionExpired answers the Solana Pay transaction request of a session past its
	// expires_at (410): a wallet holding an old QR cannot pay it.
	CodeSessionExpired = service.CodeSessionExpired
)

// Chain is the signed-path configuration of one chain. A chain without a Gateway or without
// providers has no signed path.
type Chain struct {
	Chain   chain.Chain
	Gateway *evm.Address
	Quorum  *chainwatch.Quorum
	// DomainVersion is the EIP-712 version of the chain's EIP-3009 tokens (FiatToken: "2").
	DomainVersion string
	// Confirmations is how many blocks deep a payment must be to count on this chain, for the
	// payer's progress; zero when the chain counts at the safe head, which has no count to show.
	Confirmations uint64
}

// Reader is what the checkout reads from the store. Its writes (presence, settle jobs, wallet
// transactions, the test panel's audit) go through the service.
type Reader interface {
	Now() time.Time
	LoadSession(ctx context.Context, id string) (*store.Session, error)
	GetMerchant(ctx context.Context, id string) (store.Merchant, error)
	GetProduct(ctx context.Context, merchantID string, livemode bool, id string) (store.Product, error)
	ChainPaymentsOfSession(ctx context.Context, sessionID string) ([]store.TrackedPayment, error)
	CreditHead(ctx context.Context, chainID string) (head uint64, found bool, err error)
	SessionTxJobs(ctx context.Context, sessionID string) ([]store.TxJob, error)
	GetWalletTx(ctx context.Context, sessionID string) (store.WalletTx, error)
}

// Checkout serves the public, unauthenticated payment endpoints of a session.
type Checkout struct {
	svc    *service.Service
	st     Reader
	chains map[string]Chain
	mu     sync.Mutex
	domain map[string]evm.Domain // chain|token -> verified domain
	gwOK   map[string]evm.Address
	sol    map[string]SolanaChain
	alerts alert.Sink
}

// errTreasuryMismatch is the cause of the 503 while a chain's Gateway names another treasury.
var errTreasuryMismatch = errors.New("gateway treasury mismatch")

// WithAlerts sends the checkout's alerts (a Gateway that does not match the
// configuration) to sink.
func (c *Checkout) WithAlerts(sink alert.Sink) *Checkout {
	c.alerts = sink
	return c
}

func (c *Checkout) alert(ctx context.Context, a alert.Alert) {
	if c.alerts != nil {
		c.alerts.Alert(ctx, a)
	}
}

// New returns a Checkout that opens and reads sessions through svc and st for the given chains.
func New(svc *service.Service, st Reader, chains map[string]Chain) *Checkout {
	return &Checkout{svc: svc, st: st, chains: chains, domain: map[string]evm.Domain{}, gwOK: map[string]evm.Address{}}
}

func errf(kind service.Kind, code, format string, args ...any) *service.Error {
	return &service.Error{Kind: kind, Code: code, Message: fmt.Sprintf(format, args...)}
}

func notFound() error {
	return errf(service.KindNotFound, service.CodeSessionNotFound, "no such session")
}

func (c *Checkout) load(ctx context.Context, id string) (*store.Session, error) {
	if !strings.HasPrefix(id, "ses_") || len(id) > 64 {
		return nil, notFound()
	}
	ses, err := c.st.LoadSession(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFound()
	}
	return ses, err
}

// seen records that a payer is on the session's checkout (the page is visible, or a wallet
// endpoint is used), throttled in the store. It is bookkeeping: a failure is logged and the
// request is served as if it had not been asked.
func (c *Checkout) seen(ctx context.Context, id string) {
	if err := c.svc.RecordCheckoutSeen(ctx, id); err != nil && ctx.Err() == nil {
		c.svc.Logger().Warn("checkout presence not recorded", "session", id, "err", err)
	}
}

// expired reports whether the session is past expires_at: its checkout is locked (D45).
func (c *Checkout) expired(ses *store.Session) bool {
	return !c.st.Now().Before(ses.State.ExpiresAt)
}

// PublicSession is what a payer may see.
type PublicSession struct {
	ID       string `json:"id"`
	Livemode bool   `json:"livemode"`
	Status   string `json:"status"`
	// Amount is the session's amount; null on an open-amount session (OpenAmount), where the payer
	// sends any amount. AmountRemaining is null there too: nothing is "left" to pay.
	Amount          *string         `json:"amount"`
	OpenAmount      bool            `json:"open_amount"`
	Currency        string          `json:"currency"`
	AmountReceived  string          `json:"amount_received"`
	AmountRemaining *string         `json:"amount_remaining"`
	Product         *PublicProduct  `json:"product"`
	PayTo           []PublicPayTo   `json:"pay_to"`
	Payments        []PublicPayment `json:"payments"`
	SignedPayments  []PublicSigned  `json:"signed_payments"`
	ExpiresAt       string          `json:"expires_at"`
	// PaymentStartedAt is the first reveal (null while the payer has not generated a payment);
	// PayWindowSeconds is how long the session stays open from it (D48).
	PaymentStartedAt *string `json:"payment_started_at"`
	PayWindowSeconds int64   `json:"pay_window_seconds"`
	SuccessURL       *string `json:"success_url"`
	// ExpiryWarningSeconds is how long before expires_at the page warns the payer not to send
	// funds after it closes (0: no warning).
	ExpiryWarningSeconds int64 `json:"expiry_warning_seconds"`
	// LinkID is the payment link that opened the session (nil for API sessions); LinkActive says
	// whether it still opens new sessions, so a locked page can offer "Start a new payment".
	LinkID     *string `json:"link_id"`
	LinkActive bool    `json:"link_active"`
	// CustomerAddressID is the static customer address the session was opened on (nil
	// otherwise), so a closed page can send the payer back to /a/{id} for a new payment.
	CustomerAddressID *string `json:"customer_address_id"`
	// Merchant brands the page once the merchant uploaded a logo; nil until then.
	Merchant *service.PublicMerchant `json:"merchant"`
}

// PublicProduct is the session's product as the payer sees it (shared with the address page).
type PublicProduct = service.PublicProduct

// PublicPayTo is one way to pay. Until its network is revealed ("Generate payment", D48) it has
// chain, asset and amount, and no deposit details (signed_payment false: no address, token
// account or reference). Once the session is past expires_at only chain and asset are left: no
// amount to send either.
type PublicPayTo struct {
	Chain   string `json:"chain"`
	Asset   string `json:"asset"`
	Address string `json:"address,omitempty"`
	// Amount is what one payment must be; absent on an open-amount session (any amount).
	Amount string `json:"amount,omitempty"`
	// SignedPayment and ConnectedWallet are false on an open-amount session: both paths need an
	// amount to sign or build. Both are also false on a session opened on a customer address: its
	// sessions share the address's salt, so they would share one Gateway nonce and one Solana
	// reference; such a session is paid by sending to the deposit address.
	SignedPayment bool `json:"signed_payment"`
	// TokenAccount is the Solana deposit token account (the PDA's ATA for the mint), for
	// senders that ask for a token account rather than an owner address.
	TokenAccount string `json:"token_account,omitempty"`
	// ConnectedWallet is true when a Solana connected wallet can pay through the
	// solana/transaction endpoint (the gateway pays the network fee).
	ConnectedWallet bool `json:"connected_wallet,omitempty"`
	// Reference is the Solana session reference key (base58) a wallet payment carries.
	Reference string `json:"reference,omitempty"`
	// Revealed says the network's deposit details were generated for the payer (D48).
	Revealed bool `json:"revealed"`
	// Paused says payments of this asset on this network are paused (D64): an unrevealed one
	// cannot be revealed (chain_unavailable), and a revealed one keeps its deposit address, still
	// watched and credited, but offers no wallet path (signed_payment and connected_wallet false).
	// Absent when not paused.
	Paused bool `json:"paused,omitempty"`
}

// PublicPayment is the payer-facing view of one payment on a session.
type PublicPayment struct {
	Chain       string  `json:"chain"`
	Asset       string  `json:"asset"`
	Amount      string  `json:"amount"`
	Status      string  `json:"status"`
	TxHash      string  `json:"tx_hash"`
	ConfirmedAt *string `json:"confirmed_at"`
	// Confirmations is how deep a detected payment is on a chain that counts blocks, as far as
	// the detector has scanned, capped at ConfirmationsRequired. Both are absent otherwise.
	Confirmations         *uint64 `json:"confirmations,omitempty"`
	ConfirmationsRequired *uint64 `json:"confirmations_required,omitempty"`
}

// PublicSigned is the payer-facing view of a signed payment authorization.
type PublicSigned struct {
	ID      string  `json:"id"`
	Chain   string  `json:"chain"`
	Asset   string  `json:"asset"`
	Status  string  `json:"status"`
	Outcome *string `json:"outcome"`
	TxHash  *string `json:"tx_hash"`
}

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func optTS(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := ts(*t)
	return &s
}

func usdExact(u money.Units) string {
	return money.FormatMinPlaces(u, chain.CanonicalDecimals, money.USDDecimals)
}

// Depth is how many confirmations a log in block has once the chain is scanned up to head,
// capped at need: the count CreditPolicy.Confirmed compares with the requirement.
func Depth(block, head, need uint64) uint64 {
	if head < block {
		return 0
	}
	return min(head-block+1, need)
}

// countConfirmations fills in how deep each detected payment is (out[i] is ps[i]). The progress
// is not worth failing a poll over: a failed lookup leaves the counts out this time.
func (c *Checkout) countConfirmations(ctx context.Context, sessionID string, ps []session.Payment, out []PublicPayment) {
	counted := func(p session.Payment) bool {
		return p.Status == session.Detected && c.chains[p.Chain].Confirmations > 0
	}
	want := false
	for _, p := range ps {
		want = want || counted(p)
	}
	if !want {
		return
	}
	tracked, err := c.st.ChainPaymentsOfSession(ctx, sessionID)
	if err != nil {
		return
	}
	blocks := make(map[string]uint64, len(tracked))
	for _, t := range tracked {
		blocks[t.PaymentID] = t.BlockNumber
	}
	type scanned struct {
		head uint64
		ok   bool
	}
	heads := map[string]scanned{}
	for i, p := range ps {
		block, ok := blocks[p.ID]
		if !counted(p) || !ok {
			continue
		}
		h, seen := heads[p.Chain]
		if !seen {
			head, found, err := c.st.CreditHead(ctx, p.Chain)
			h = scanned{head: head, ok: found && err == nil}
			heads[p.Chain] = h
		}
		if !h.ok {
			continue
		}
		need := c.chains[p.Chain].Confirmations
		n := Depth(block, h.head, need)
		out[i].Confirmations, out[i].ConfirmationsRequired = &n, &need
	}
}

func (c *Checkout) signedAvailable(chainID, asset string) bool {
	cc, ok := c.chains[chainID]
	if !ok || cc.Gateway == nil || cc.Quorum == nil {
		return false
	}
	a, ok := cc.Chain.Asset(asset)
	return ok && a.EIP3009
}

// Session is the payer's view of a session. active says the payer has the checkout page open
// and visible (GET ?active=1), which records their presence. Past expires_at the deposit details
// leave the answer whatever the status (D45): a reload cannot bring them back, and a paid
// session's receipt (payments, signed payments, success_url) does not need them.
func (c *Checkout) Session(ctx context.Context, id string, active bool) (PublicSession, error) {
	ses, err := c.load(ctx, id)
	if err != nil {
		return PublicSession{}, err
	}
	if active {
		c.seen(ctx, ses.ID)
	}
	st := ses.State
	out := PublicSession{ID: ses.ID, Livemode: ses.Livemode, Status: string(st.Status), OpenAmount: st.OpenAmount,
		Currency: "USD", AmountReceived: usdExact(st.Received()),
		PayTo: []PublicPayTo{}, Payments: []PublicPayment{}, SignedPayments: []PublicSigned{}, ExpiresAt: ts(st.ExpiresAt),
		PayWindowSeconds:     int64(ses.PayWindow / time.Second),
		ExpiryWarningSeconds: int64(c.svc.CheckoutExpiryWarning() / time.Second), LinkID: ses.LinkID,
		CustomerAddressID: ses.CustomerAddressID}
	if ses.PaymentStartedAt != nil {
		v := ts(*ses.PaymentStartedAt)
		out.PaymentStartedAt = &v
	}
	if !st.OpenAmount {
		amount, remaining := money.FormatUSD(st.AmountCents), usdExact(st.Remaining())
		out.Amount, out.AmountRemaining = &amount, &remaining
	}
	if ses.LinkID != nil {
		if out.LinkActive, err = c.svc.LinkOpensSessions(ctx, *ses.LinkID); err != nil {
			return PublicSession{}, err
		}
	}
	locked := c.expired(ses)
	if st.Status == session.Paid || st.Status == session.Overpaid {
		out.SuccessURL = ses.SuccessURL
	}
	// Branding is not worth failing a poll over: on an error the page shows no logo this time.
	if m, err := c.svc.PublicMerchant(ctx, ses.MerchantID); err == nil {
		out.Merchant = m
	}
	if ses.ProductID != nil {
		if p, err := c.st.GetProduct(ctx, ses.MerchantID, ses.Livemode, *ses.ProductID); err == nil {
			out.Product = &PublicProduct{Name: p.Name, Description: p.Description}
		}
	}
	pauses, err := c.svc.ChainPauses(ctx)
	if err != nil {
		return PublicSession{}, err
	}
	for _, p := range ses.PayTo {
		revealed := ses.Revealed(p.Chain) != nil
		paused := pauses.Paused(p.Chain, p.Asset)
		if locked {
			out.PayTo = append(out.PayTo, PublicPayTo{Chain: p.Chain, Asset: p.Asset, Revealed: revealed, Paused: paused})
			continue
		}
		pt := PublicPayTo{Chain: p.Chain, Asset: p.Asset, Paused: paused}
		if !st.OpenAmount {
			pt.Amount = money.FormatDecimal(p.Amount, p.Decimals)
		}
		if !revealed {
			out.PayTo = append(out.PayTo, pt)
			continue
		}
		pt.Address, pt.TokenAccount, pt.Revealed = p.Address, p.TokenAccount(), true
		if !st.OpenAmount && !paused {
			pt.SignedPayment = ses.CustomerAddressID == nil && c.signedAvailable(p.Chain, p.Asset)
			pt.ConnectedWallet = ses.CustomerAddressID == nil && p.Program != "" && c.walletAvailable(p.Chain)
		}
		if p.Family == chain.Solana {
			pt.Reference = solana.SessionReference(ses.Salt).String()
		}
		out.PayTo = append(out.PayTo, pt)
	}
	for _, p := range st.Payments {
		out.Payments = append(out.Payments, PublicPayment{Chain: p.Chain, Asset: p.Asset, Amount: money.FormatDecimal(p.Amount, p.Decimals),
			Status: string(p.Status), TxHash: p.TxHash, ConfirmedAt: optTS(p.ConfirmedAt)})
	}
	c.countConfirmations(ctx, ses.ID, st.Payments, out.Payments)
	jobs, err := c.st.SessionTxJobs(ctx, ses.ID)
	if err != nil {
		return out, err
	}
	for _, j := range jobs {
		if j.Kind != store.JobSettle {
			continue
		}
		ps := PublicSigned{ID: j.ID, Chain: j.Chain, Asset: j.Asset, Status: j.Status}
		if j.Outcome != "" {
			o := j.Outcome
			ps.Outcome = &o
		}
		if j.MinedTxHash != "" {
			h := j.MinedTxHash
			ps.TxHash = &h
		}
		out.SignedPayments = append(out.SignedPayments, ps)
	}
	return out, nil
}

// Reveal is "Generate payment" (POST /v1/checkout/sessions/{id}/reveal): it reveals the deposit
// details of one network to the payer, which starts its monitoring and, on the session's first
// reveal, the pay window (D48). It counts as payer presence and answers the payer's view.
func (c *Checkout) Reveal(ctx context.Context, id, chainID string) (PublicSession, error) {
	ses, err := c.load(ctx, id)
	if err != nil {
		return PublicSession{}, err
	}
	c.seen(ctx, ses.ID)
	if _, err := c.svc.RevealForPayer(ctx, ses, chainID, store.RevealCheckout); err != nil {
		return PublicSession{}, err
	}
	return c.Session(ctx, id, false)
}

// walletReveal reveals the network a wallet endpoint pays on (D48) and records the session's
// wallet activity, before anything is built from the session's expiry. A session that cannot be
// paid is answered as is: the endpoint's own checks refuse it with their usual codes.
func (c *Checkout) walletReveal(ctx context.Context, ses *store.Session, chainID string) (*store.Session, error) {
	st := ses.State
	if (st.Status != session.Open && st.Status != session.PartiallyPaid) || c.expired(ses) {
		return ses, nil
	}
	return c.svc.RevealForPayer(ctx, ses, chainID, store.RevealWallet)
}

// signedTarget resolves everything the signed path needs for one (session, chain, asset).
type signedTarget struct {
	ses        *store.Session
	cc         Chain
	pt         store.PayToRecord
	token      evm.Address
	gateway    evm.Address
	tre        evm.Address
	recipients []evm.Address
	bps        []uint16
	nonce      evm.Word
	domain     evm.Domain
}

// noWalletPath refuses the signed and connected-wallet paths on a session opened on a customer
// address: its sessions derive the same Gateway nonce and Solana reference (from the address's
// salt), so a wallet payment for one could be spent, or credited, against another.
func noWalletPath(ses *store.Session) error {
	if ses.CustomerAddressID != nil {
		return errf(service.KindConflict, CodeSessionNotPayable, "a session on a customer address is paid by sending to its deposit address")
	}
	return nil
}

func (c *Checkout) target(ctx context.Context, id, chainID, asset string) (signedTarget, error) {
	var t signedTarget
	ses, err := c.load(ctx, id)
	if err != nil {
		return t, err
	}
	c.seen(ctx, ses.ID)
	t.ses = ses
	if err := noWalletPath(ses); err != nil {
		return t, err
	}
	if !c.signedAvailable(chainID, asset) {
		return t, errf(service.KindUnavailable, service.CodeServiceUnavailable, "signed payments are not available for %s on %s", asset, chainID)
	}
	t.cc = c.chains[chainID]
	found := false
	for _, p := range ses.PayTo {
		if p.Chain == chainID && p.Asset == asset {
			t.pt, found = p, true
		}
	}
	if !found {
		return t, errf(service.KindInvalid, service.CodeInvalidRequest, "this session does not accept %s on %s", asset, chainID)
	}
	if err := c.svc.CheckPayable(ctx, chainID, asset); err != nil {
		return t, err
	}
	if t.token, err = evm.ParseAddressLoose(t.pt.Token); err != nil {
		return t, err
	}
	if t.recipients, t.bps, err = derive.EVMRecipients(t.pt.Recipients); err != nil {
		return t, err
	}
	if t.tre, err = evm.ParseAddressLoose(t.pt.Treasury); err != nil {
		return t, err
	}
	t.gateway = *t.cc.Gateway
	if err := c.checkGateway(ctx, t); err != nil {
		return t, err
	}
	if t.domain, err = c.tokenDomain(ctx, t); err != nil {
		return t, err
	}
	if t.ses, err = c.walletReveal(ctx, t.ses, chainID); err != nil {
		return t, err
	}
	t.nonce = evm.GatewayNonce(t.cc.Chain.EVMID, t.gateway, ses.Salt, t.tre, t.pt.Fee.FeeBps, t.pt.Fee.FixedFeeMicros, t.recipients, t.bps)
	return t, nil
}

// unavailable is the 503 of a signed payment that cannot be prepared now. The cause stays on the
// error (service.Error unwraps to it) so the server log says why; the payer sees the same body.
func unavailable(cause error) error {
	return &service.Error{Kind: service.KindUnavailable, Code: service.CodeServiceUnavailable,
		Message: "signed payments are temporarily unavailable", Cause: cause}
}

// checkGateway confirms the Gateway's immutable treasury is the session's treasury: otherwise
// the nonce the Gateway rebuilds would differ from the one the payer signs.
func (c *Checkout) checkGateway(ctx context.Context, t signedTarget) error {
	key := t.cc.Chain.ID
	c.mu.Lock()
	tre, ok := c.gwOK[key]
	c.mu.Unlock()
	if !ok {
		out, err := t.cc.Quorum.FirstCall(ctx, evmrpc.CallMsg{To: t.gateway, Data: contracts.TreasuryCall()}, evmrpc.Latest)
		if err != nil {
			return unavailable(fmt.Errorf("gateway %s treasury(): %w", t.gateway.Hex(), err))
		}
		if tre, err = contracts.AddressResult(out); err != nil {
			return unavailable(fmt.Errorf("gateway %s treasury() result: %w", t.gateway.Hex(), err))
		}
		c.mu.Lock()
		c.gwOK[key] = tre
		c.mu.Unlock()
	}
	if tre != t.tre {
		// The deployment and the configuration disagree: no signed payment can settle on this
		// chain until one of them is fixed.
		c.alert(ctx, alert.Alert{Severity: alert.Critical, Chain: key, Kind: "gateway_treasury_mismatch",
			Message: "the Gateway's treasury is not the configured treasury; signed payments are off on this chain",
			Fields:  map[string]string{"gateway": t.gateway.Hex(), "gateway_treasury": tre.Hex(), "treasury": t.tre.Hex()}})
		return unavailable(fmt.Errorf("%w: gateway %s treasury is %s, the session's treasury is %s", errTreasuryMismatch,
			t.gateway.Hex(), tre.Hex(), t.tre.Hex()))
	}
	return nil
}

// tokenDomain builds the token's EIP-712 domain from its name() and the configured version,
// and only uses it when it reproduces the token's own DOMAIN_SEPARATOR().
func (c *Checkout) tokenDomain(ctx context.Context, t signedTarget) (evm.Domain, error) {
	key := t.cc.Chain.ID + "|" + t.token.Lower()
	c.mu.Lock()
	d, ok := c.domain[key]
	c.mu.Unlock()
	if ok {
		return d, nil
	}
	q := t.cc.Quorum
	nameOut, err := q.FirstCall(ctx, evmrpc.CallMsg{To: t.token, Data: contracts.NameCall()}, evmrpc.Latest)
	if err != nil {
		return d, unavailable(fmt.Errorf("token %s name(): %w", t.token.Hex(), err))
	}
	name, err := contracts.StringResult(nameOut)
	if err != nil {
		return d, unavailable(fmt.Errorf("token %s name() result: %w", t.token.Hex(), err))
	}
	version := t.cc.DomainVersion
	if version == "" {
		version = "2"
	}
	d = evm.Domain{Name: name, Version: version, ChainID: t.cc.Chain.EVMID, VerifyingContract: t.token}
	sepOut, err := q.FirstCall(ctx, evmrpc.CallMsg{To: t.token, Data: contracts.DomainSeparatorCall()}, evmrpc.Latest)
	if err != nil {
		return evm.Domain{}, unavailable(fmt.Errorf("token %s DOMAIN_SEPARATOR(): %w", t.token.Hex(), err))
	}
	if len(sepOut) != 32 || evm.Word(sepOut) != d.Separator() {
		return evm.Domain{}, unavailable(fmt.Errorf("token %s DOMAIN_SEPARATOR() does not match name %q version %q", t.token.Hex(), name, version))
	}
	c.mu.Lock()
	c.domain[key] = d
	c.mu.Unlock()
	return d, nil
}

// payable checks the session can take a signed payment now and returns what is still due, in
// the token's base units, after confirmed and pending payments.
func (c *Checkout) payable(t signedTarget) (money.Units, error) {
	st := t.ses.State
	now := c.st.Now()
	if (st.Status != session.Open && st.Status != session.PartiallyPaid) || !now.Before(st.ExpiresAt) {
		return money.Zero, errf(service.KindConflict, CodeSessionNotPayable, "this session no longer accepts payments")
	}
	due := st.Remaining()
	for _, p := range st.Payments {
		if p.Status != session.Detected {
			continue
		}
		pc, err := chain.ToCanonical(p.Amount, p.Decimals)
		if err != nil {
			return money.Zero, err
		}
		if due, err = due.Sub(pc); err != nil {
			due = money.Zero
		}
	}
	if due.IsZero() {
		return money.Zero, errf(service.KindConflict, CodeSessionNotPayable, "nothing is left to pay on this session")
	}
	// Rounded up to a whole token unit, as the simulator's remainingIn: after a deposit in a token
	// of more decimals what is due may not end on one.
	v, err := chain.FromCanonicalUp(due, t.pt.Decimals)
	if err != nil {
		return money.Zero, errf(service.KindConflict, CodeSessionNotPayable, "the remaining amount is not representable in %s", t.pt.Asset)
	}
	return v, nil
}

func (c *Checkout) notDust(t signedTarget, v money.Units) error {
	split, err := fee.Compute(v, t.pt.Decimals, t.pt.Fee)
	if err != nil {
		return err
	}
	if split.Dust {
		// The Gateway reverts DustPayment; never ask a payer to sign one.
		return errf(service.KindConflict, service.CodeAmountBelowMinimum, "the amount is too small to settle after the fee and cannot be paid with a signature")
	}
	return nil
}

// TypedDataRequest asks for the authorization a payer should sign.
type TypedDataRequest struct {
	Chain string
	Asset string
	From  string
}

// TypedData is the EIP-712 typed data the payer signs to authorize a transfer.
type TypedData struct {
	Types       map[string][]TypedField `json:"types"`
	PrimaryType string                  `json:"primaryType"`
	Domain      TypedDomain             `json:"domain"`
	Message     TypedMessage            `json:"message"`
}

// TypedField is one named, typed field of an EIP-712 type.
type TypedField struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// TypedDomain is the EIP-712 signing domain of the token being transferred.
type TypedDomain struct {
	Name              string `json:"name"`
	Version           string `json:"version"`
	ChainID           uint64 `json:"chainId"`
	VerifyingContract string `json:"verifyingContract"`
}

// TypedMessage is the transfer authorization message the payer signs.
type TypedMessage struct {
	From        string `json:"from"`
	To          string `json:"to"`
	Value       string `json:"value"`
	ValidAfter  string `json:"validAfter"`
	ValidBefore string `json:"validBefore"`
	Nonce       string `json:"nonce"`
}

// TypedDataResponse returns the typed data to sign along with the session, chain, asset, amount and gateway.
type TypedDataResponse struct {
	SessionID string    `json:"session_id"`
	Chain     string    `json:"chain"`
	Asset     string    `json:"asset"`
	Amount    string    `json:"amount"`
	Gateway   string    `json:"gateway"`
	TypedData TypedData `json:"typed_data"`
}

func parseFrom(s string) (evm.Address, error) {
	a, err := evm.ParseAddress(s)
	if err != nil || a.IsZero() {
		return a, errf(service.KindInvalid, service.CodeInvalidRequest, "from must be the payer's EVM address")
	}
	return a, nil
}

// TypedData builds the typed data a payer signs to pay a session in the requested asset.
func (c *Checkout) TypedData(ctx context.Context, id string, in TypedDataRequest) (TypedDataResponse, error) {
	from, err := parseFrom(in.From)
	if err != nil {
		return TypedDataResponse{}, err
	}
	t, err := c.target(ctx, id, in.Chain, in.Asset)
	if err != nil {
		return TypedDataResponse{}, err
	}
	value, err := c.payable(t)
	if err != nil {
		return TypedDataResponse{}, err
	}
	if err := c.notDust(t, value); err != nil {
		return TypedDataResponse{}, err
	}
	return TypedDataResponse{SessionID: id, Chain: in.Chain, Asset: in.Asset, Amount: money.FormatDecimal(value, t.pt.Decimals),
		Gateway: t.gateway.Hex(), TypedData: TypedData{
			Types: map[string][]TypedField{
				"EIP712Domain": {{"name", "string"}, {"version", "string"}, {"chainId", "uint256"}, {"verifyingContract", "address"}},
				"ReceiveWithAuthorization": {{"from", "address"}, {"to", "address"}, {"value", "uint256"}, {"validAfter", "uint256"},
					{"validBefore", "uint256"}, {"nonce", "bytes32"}},
			},
			PrimaryType: "ReceiveWithAuthorization",
			Domain: TypedDomain{Name: t.domain.Name, Version: t.domain.Version, ChainID: t.domain.ChainID,
				VerifyingContract: t.domain.VerifyingContract.Hex()},
			Message: TypedMessage{From: from.Hex(), To: t.gateway.Hex(), Value: value.String(), ValidAfter: "0",
				ValidBefore: fmt.Sprint(t.ses.State.ExpiresAt.Unix()), Nonce: t.nonce.Hex()},
		}}, nil
}

// SubmitRequest carries the signed authorization back.
type SubmitRequest struct {
	Chain       string
	Asset       string
	From        string
	Value       string
	ValidAfter  string
	ValidBefore string
	Signature   string
}

// SubmitResponse acknowledges a submitted signed payment with its id and status.
type SubmitResponse struct {
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
	Status    string `json:"status"`
}

// signedWindow checks an authorization's validity window: open now, ending by the session expiry
// and outlasting the sender's own margin plus a retry (txjob.SettleSubmitMargin), as one
// accepted closer to its end would only fail as expired.
func signedWindow(after, before *big.Int, expiresAt, now time.Time) error {
	if before.Cmp(big.NewInt(expiresAt.Unix())) > 0 || before.Cmp(big.NewInt(now.Add(txjob.SettleSubmitMargin).Unix())) <= 0 ||
		after.Cmp(big.NewInt(now.Unix())) >= 0 {
		return errf(service.KindInvalid, service.CodeInvalidRequest, "the validity window must be open now, last more than %d s and end by the session expiry",
			int(txjob.SettleSubmitMargin/time.Second))
	}
	return nil
}

func parseUint(name, s string) (*big.Int, error) {
	u, err := money.ParseInteger(s)
	if err != nil {
		return nil, errf(service.KindInvalid, service.CodeInvalidRequest, "%s must be a non-negative integer string", name)
	}
	return u.Big(), nil
}

// Submit verifies a payer's signature against the authorization the server itself rebuilds
// from the stored session (recipients, shares, fee and nonce are never taken from the client),
// simulates Gateway.settle, and queues it for the gas wallet.
func (c *Checkout) Submit(ctx context.Context, id string, in SubmitRequest) (SubmitResponse, error) {
	from, err := parseFrom(in.From)
	if err != nil {
		return SubmitResponse{}, err
	}
	value, err := parseUint("value", in.Value)
	if err != nil {
		return SubmitResponse{}, err
	}
	after, err := parseUint("valid_after", in.ValidAfter)
	if err != nil {
		return SubmitResponse{}, err
	}
	before, err := parseUint("valid_before", in.ValidBefore)
	if err != nil {
		return SubmitResponse{}, err
	}
	sigBytes, err := evm.ParseHexBytes(in.Signature)
	if err != nil {
		return SubmitResponse{}, errf(service.KindInvalid, CodeInvalidSignature, "signature must be 65 bytes of hex")
	}
	v, r, s, err := evm.SplitSignature(sigBytes)
	if err != nil {
		return SubmitResponse{}, errf(service.KindInvalid, CodeInvalidSignature, "signature must be 65 bytes of hex")
	}
	t, err := c.target(ctx, id, in.Chain, in.Asset)
	if err != nil {
		return SubmitResponse{}, err
	}
	if _, err := c.payable(t); err != nil {
		return SubmitResponse{}, err
	}
	ref := from.Lower() + "|" + t.nonce.Hex()
	if err := c.noOtherSettle(ctx, id, ref); err != nil {
		return SubmitResponse{}, err
	}
	vu, err := money.FromBig(value)
	if err != nil || vu.IsZero() {
		return SubmitResponse{}, errf(service.KindInvalid, service.CodeInvalidRequest, "value must be positive")
	}
	if err := c.notDust(t, vu); err != nil {
		return SubmitResponse{}, err
	}
	if err := signedWindow(after, before, t.ses.State.ExpiresAt, c.st.Now()); err != nil {
		return SubmitResponse{}, err
	}
	auth := evm.ReceiveAuthorization{From: from, To: t.gateway, Value: value, ValidAfter: after, ValidBefore: before, Nonce: t.nonce}
	signer, err := evm.RecoverAddress(auth.Digest(t.domain), v, r, s)
	if err != nil || signer != from {
		return SubmitResponse{}, errf(service.KindInvalid, CodeInvalidSignature, "the signature does not match this session's payment authorization")
	}
	calldata := contracts.Settle(contracts.Authorization{Token: t.token, From: from, Value: value, ValidAfter: after, ValidBefore: before,
		SessionSalt: t.ses.Salt, Recipients: t.recipients, Bps: t.bps, FeeBps: t.pt.Fee.FeeBps, FixedFeeMicros: t.pt.Fee.FixedFeeMicros,
		V: v, R: r, S: s})
	if _, err := t.cc.Quorum.FirstCall(ctx, evmrpc.CallMsg{To: t.gateway, Data: calldata}, evmrpc.Latest); err != nil {
		if evmrpc.IsRevert(err) {
			return SubmitResponse{}, errf(service.KindInvalid, CodeAuthorizationError, "the authorization would be rejected on chain: %s", revertText(err))
		}
		return SubmitResponse{}, unavailable(fmt.Errorf("settle simulation: %w", err))
	}
	j, err := c.svc.QueueSettle(ctx, store.TxJob{Chain: in.Chain, Ref: ref,
		SessionID: id, Asset: in.Asset, To: t.gateway.Hex(), Calldata: calldata,
		Details: map[string]string{"from": from.Hex(), "value": value.String(), "valid_before": before.String()}})
	if err != nil {
		return SubmitResponse{}, err
	}
	return SubmitResponse{ID: j.ID, SessionID: id, Status: j.Status}, nil
}

// noOtherSettle refuses a signed payment while another settle of the session is in flight. Its
// payment is not in the session yet, so payable still shows the whole amount due, and an
// EIP-3009 nonce is per authorizer: the same session signed from a second wallet would settle
// too and charge twice. The same authorization (same ref) stays idempotent.
func (c *Checkout) noOtherSettle(ctx context.Context, sessionID, ref string) error {
	jobs, err := c.st.SessionTxJobs(ctx, sessionID)
	if err != nil {
		return err
	}
	for _, j := range jobs {
		if j.Kind != store.JobSettle || j.Ref == ref {
			continue
		}
		switch j.Status {
		case store.JobPending, store.JobBlocked, store.JobSubmitted, store.JobMined:
			return errf(service.KindConflict, CodeSessionNotPayable, "another payment of this session is being settled")
		}
	}
	return nil
}

func revertText(err error) string {
	var re *evmrpc.RPCError
	if errors.As(err, &re) {
		if data, ok := re.RevertData(); ok && len(data) > 0 {
			return contracts.RevertReason(data)
		}
		return re.Message
	}
	return "reverted"
}
