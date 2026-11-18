package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strconv"
	"time"

	"github.com/prismaport/prismaport/internal/apikey"
	"github.com/prismaport/prismaport/internal/base58"
	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/evm"
	"github.com/prismaport/prismaport/internal/ids"
	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/payout"
	"github.com/prismaport/prismaport/internal/session"
	"github.com/prismaport/prismaport/internal/store"
)

// CodeTestModeOnly refuses a sandbox tool (test events, inbox, simulator) in live mode.
const CodeTestModeOnly = "test_mode_only"

func testModeOnly() *Error {
	return newErr(KindForbidden, CodeTestModeOnly, "sandbox tools work in test mode only: use an sk_test_ key or the Test mode of the dashboard")
}

// RequireTestMode refuses live mode before anything else is looked at.
func RequireTestMode(a Auth) error {
	if a.Livemode {
		return testModeOnly()
	}
	return nil
}

// SimulatedHashPrefix starts every synthetic EVM transaction hash: 8 zero bytes, which a mined
// transaction hash practically never has. Synthetic Solana signatures start with the same 8
// zero bytes, so their base58 form starts with "11111111".
const SimulatedHashPrefix = "0x0000000000000000"

// SyntheticTxHash returns a well-formed, clearly synthetic transaction hash for the family.
func SyntheticTxHash(f chain.Family) string {
	if f == chain.Solana {
		var b [64]byte
		_, _ = rand.Read(b[8:])
		return base58.Encode(b[:])
	}
	var b [24]byte
	_, _ = rand.Read(b[:])
	return SimulatedHashPrefix + hex.EncodeToString(b[:])
}

// TestEvent is a sample event about to be sent to one endpoint as a test.
type TestEvent struct {
	Endpoint store.WebhookEndpoint
	ID       string
	Type     string
	Payload  []byte
	// Secrets are what a real delivery to this endpoint is signed with right now.
	Secrets []string
	At      time.Time
}

// PrepareTestEvent builds a realistic sample event of the given type for a test-mode endpoint
// and the secrets to sign it with, exactly as the dispatcher would for a real event.
func (s *Service) PrepareTestEvent(ctx context.Context, a Auth, endpointID, eventType string) (TestEvent, error) {
	if err := a.Require(apikey.WebhooksWrite); err != nil {
		return TestEvent{}, err
	}
	if err := RequireTestMode(a); err != nil {
		return TestEvent{}, err
	}
	if eventType == "" {
		return TestEvent{}, invalid("type is required: one of the webhook event types")
	}
	if !session.EventType(eventType).Valid() {
		return TestEvent{}, invalid("unknown event type %q", eventType)
	}
	e, err := s.store.GetWebhookEndpoint(ctx, a.MerchantID, a.Livemode, endpointID)
	// A disabled endpoint takes test events too, so a fix can be checked before enabling it.
	if errors.Is(err, store.ErrNotFound) {
		return TestEvent{}, endpointNotFound()
	}
	if err != nil {
		return TestEvent{}, err
	}
	now := s.store.Now()
	ses, err := s.sampleSession(a.MerchantID, session.EventType(eventType), now)
	if err != nil {
		return TestEvent{}, err
	}
	te := TestEvent{Endpoint: e, ID: ids.NewAt(ids.Event, now), Type: eventType, At: now}
	if te.Payload, err = s.view.EventPayload(store.EventMeta{ID: te.ID, Type: eventType, CreatedAt: now}, ses); err != nil {
		return TestEvent{}, err
	}
	if te.Secrets, err = s.SigningSecrets(e, now); err != nil {
		return TestEvent{}, err
	}
	return te, nil
}

// TestOutcome is what happened to a test send.
type TestOutcome struct {
	StatusCode int
	// StatusText is the reason phrase of StatusCode ("Bad Gateway"), set by the sender; empty
	// for a status without one.
	StatusText string
	Err        string
	At         time.Time
}

// RecordTestDelivery logs a test send in the endpoint's delivery log, marked as a test.
func (s *Service) RecordTestDelivery(ctx context.Context, a Auth, te TestEvent, o TestOutcome) (DeliveryView, error) {
	ok := o.Err == "" && o.StatusCode >= 200 && o.StatusCode < 300
	errText := o.Err
	if !ok && errText == "" {
		errText = o.StatusText
		if errText == "" {
			errText = "unexpected status"
		}
		errText = "HTTP " + strconv.Itoa(o.StatusCode) + " " + errText
	}
	if ok {
		errText = ""
	}
	at := o.At
	if at.IsZero() {
		at = s.store.Now()
	}
	d, err := s.store.InsertTestDelivery(ctx, a.MerchantID, a.Livemode, a.actor(), store.TestDelivery{ID: ids.NewAt(ids.WebhookDelivery, at),
		EndpointID: te.Endpoint.ID, EventID: te.ID, EventType: te.Type, StatusCode: o.StatusCode, Err: truncateString(errText, 500),
		Succeeded: ok, At: at})
	if err != nil {
		return DeliveryView{}, err
	}
	return deliveryView(d), nil
}

func truncateString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// sampleSession is a fake but well-formed test-mode session in the state that the event type
// announces, built by the session state machine itself so status, amounts and version are the
// ones a real session would have.
func (s *Service) sampleSession(merchantID string, t session.EventType, now time.Time) (*store.Session, error) {
	cents := money.FromUint64(2500)
	created := now.Add(-10 * time.Minute)
	expires := created.Add(time.Hour)
	if t == session.EventExpired || t == session.EventLatePayment {
		created = now.Add(-2 * time.Hour)
		expires = created.Add(time.Hour)
	}
	payouts, recipients := s.samplePayouts(created)
	payTo, err := s.samplePayTo(cents, recipients)
	if err != nil {
		return nil, err
	}
	var salt [32]byte
	_, _ = rand.Read(salt[:])
	ses := &store.Session{ID: ids.NewAt(ids.Session, created), MerchantID: merchantID, Livemode: false, Salt: salt,
		Metadata: map[string]string{"order_id": "order_test_1001"}, CreatedAt: created, UpdatedAt: now, PayTo: payTo, Payouts: payouts}
	family := chain.EVM
	if c, ok := s.cfg.Registry.Get(payTo[0].Chain); ok {
		family = c.Family
	}
	p := samplePayer{target: payTo[0], family: family}
	st, err := p.reach(session.New(cents, store.PayToState(payTo), expires), t, expires, now)
	if err != nil {
		return nil, err
	}
	ses.State = st
	return ses, nil
}

// randomAddress is a random address of the family's format.
func randomAddress(fam chain.Family) string {
	if fam == chain.Solana {
		var b [32]byte
		_, _ = rand.Read(b[:])
		return base58.Encode(b[:])
	}
	var a evm.Address
	_, _ = rand.Read(a[:])
	return a.Hex()
}

// samplePayouts is a sample payout of every test network, 70% and 30% to two random wallets,
// and the recipients each network's pay_to entries carry.
func (s *Service) samplePayouts(created time.Time) ([]store.SessionPayout, map[string][]payout.Recipient) {
	var payouts []store.SessionPayout
	recipients := map[string][]payout.Recipient{}
	for _, c := range s.cfg.Registry.ForMode(false) {
		for i, share := range []struct {
			label string
			bps   uint16
		}{{"Main wallet", 7000}, {"Partner wallet", 3000}} {
			addr := randomAddress(c.Family)
			walletID, label := ids.NewAt(ids.Wallet, created), share.label
			payouts = append(payouts, store.SessionPayout{Chain: c.ID, Family: c.Family, Position: i, WalletID: &walletID,
				Label: &label, Address: addr, Bps: share.bps})
			recipients[c.ID] = append(recipients[c.ID], payout.Recipient{Address: addr, Bps: share.bps})
		}
	}
	return payouts, recipients
}

// samplePayTo is a pay_to entry for every asset of every test network, each at a random deposit
// address; 503 when no test network is configured.
func (s *Service) samplePayTo(cents money.Units, recipients map[string][]payout.Recipient) ([]store.PayToRecord, error) {
	var payTo []store.PayToRecord
	for _, c := range s.cfg.Registry.ForMode(false) {
		for _, as := range c.Assets {
			amount, err := money.USDToToken(cents, as.Decimals)
			if err != nil {
				return nil, err
			}
			pt := store.PayToRecord{Chain: c.ID, Asset: as.Symbol, Family: c.Family, Token: as.Token, Decimals: as.Decimals, Amount: amount,
				Fee: c.Fee, Recipients: recipients[c.ID], Address: randomAddress(c.Family)}
			if sol, ok := s.cfg.Solana[c.ID]; ok && c.Family == chain.Solana {
				pt.Program = sol.Program
			}
			payTo = append(payTo, pt)
		}
	}
	if len(payTo) == 0 {
		return nil, newErr(KindUnavailable, CodeServiceUnavailable, "no test networks are configured")
	}
	return payTo, nil
}

// samplePayer pays a sample session's first pay_to entry with synthetic payments.
type samplePayer struct {
	target store.PayToRecord
	family chain.Family
}

// pay detects a payment of amount at at, and confirms it 20 seconds later when confirm is set.
func (p samplePayer) pay(st session.State, amount money.Units, at time.Time, confirm bool) (session.State, string, error) {
	id := ids.NewAt(ids.Payment, at)
	st, _, err := st.DetectPayment(session.NewPayment{ID: id, Chain: p.target.Chain, Asset: p.target.Asset, Address: p.target.Address,
		TxHash: SyntheticTxHash(p.family), Amount: amount, Simulated: true}, at)
	if err == nil && confirm {
		st, _, err = st.ConfirmPayment(id, at.Add(20*time.Second))
	}
	return st, id, err
}

// units is c US dollars in the target's token units.
func (p samplePayer) units(c uint64) money.Units {
	u, _ := money.USDToToken(money.FromUint64(c), p.target.Decimals)
	return u
}

// reach takes a new session's state st to the state event type t announces.
func (p samplePayer) reach(st session.State, t session.EventType, expires, now time.Time) (session.State, error) {
	paidAt := now.Add(-time.Minute)
	var err error
	switch t {
	case session.EventPaymentDetected:
		st, _, err = p.pay(st, p.target.Amount, now.Add(-30*time.Second), false)
	case session.EventPartiallyPaid:
		st, _, err = p.pay(st, p.units(1000), paidAt, true)
	case session.EventPaid:
		st, _, err = p.pay(st, p.target.Amount, paidAt, true)
	case session.EventOverpaid:
		st, _, err = p.pay(st, p.units(3000), paidAt, true)
	case session.EventExpired:
		st, _, err = st.Expire(expires)
	case session.EventLatePayment:
		if st, _, err = st.Expire(expires); err == nil {
			st, _, err = p.pay(st, p.target.Amount, paidAt, true)
		}
	case session.EventUnderReview:
		if st, _, err = p.pay(st, p.target.Amount, paidAt, true); err == nil {
			st, _, err = st.FlagForReview("screening_hit", now)
		}
	case session.EventPaymentReversed:
		var id string
		if st, id, err = p.pay(st, p.target.Amount, paidAt, true); err == nil {
			st, _, err = st.ReversePayment(id, now)
		}
	}
	return st, err
}
