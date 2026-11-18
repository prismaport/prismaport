package service

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prismaport/prismaport/internal/apikey"
	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/derive"
	"github.com/prismaport/prismaport/internal/ids"
	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/payout"
	"github.com/prismaport/prismaport/internal/session"
	"github.com/prismaport/prismaport/internal/store"
	"github.com/prismaport/prismaport/internal/view"
)

// SessionInput is the request to create a checkout session, either for a product or for a free
// amount.
type SessionInput struct {
	ProductID  *string
	Amount     *string
	Metadata   map[string]string
	SuccessURL *string
	// ExpiresIn is in seconds.
	ExpiresIn *int64
	// ClientReferenceID is optional; who set it is decided by the entry point, never the caller.
	ClientReferenceID *string
	// CustomerAddressID opens the session on a static customer address (B11): the session copies
	// the address's salt, deposit addresses and payout snapshot.
	CustomerAddressID *string
	referenceSource   string
	// link is the payment link opening the session from its page, product its product as read
	// under the link lock; both nil for API sessions. address is the customer address the session
	// opens on, read under its lock (product then holds the address's product).
	link    *store.PaymentLink
	address *store.CustomerAddress
	product *store.Product
}

const maxIdempotencyKeyLen = 255

func sessionNotFound() *Error {
	return newErr(KindNotFound, CodeSessionNotFound, "no such session")
}

// CreateSession creates a session exactly once per Idempotency-Key. rawBody is the request body
// as received; the same key with a different body is a conflict.
func (s *Service) CreateSession(ctx context.Context, a Auth, idemKey string, rawBody []byte, in SessionInput) (store.StoredResponse, bool, error) {
	if err := a.Require(apikey.SessionsWrite); err != nil {
		return store.StoredResponse{}, false, err
	}
	if err := checkIdempotencyKey(idemKey); err != nil {
		return store.StoredResponse{}, false, err
	}
	in.referenceSource = ReferenceMerchant
	scope := store.IdempotencyScope{MerchantID: a.MerchantID, Livemode: a.Livemode, Key: idemKey}
	if in.CustomerAddressID != nil {
		// A session on a customer address is revealed from creation (its customer already holds
		// the address, D48), so its recipients are checked now, not at a first reveal. The
		// address's snapshot never changes: checking it here is enough. An address that cannot be
		// read gets its error inside the transaction.
		if ca, err := s.store.GetCustomerAddress(ctx, a.MerchantID, a.Livemode, *in.CustomerAddressID); err == nil {
			if err := s.precheckAddressPayouts(ctx, a, scope, ca); err != nil {
				return store.StoredResponse{}, false, err
			}
		}
	}
	// The session is kept as the JSON the API answers, and a replay answers it as stored.
	req := idemRequest{scope: scope, route: "POST /v1/sessions", body: rawBody}
	body, status, replayed, err := idempotent(ctx, s.store, req, func(ctx context.Context, tx pgx.Tx) (json.RawMessage, int, error) {
		b := s.bind(tx)
		var ses *store.Session
		var err error
		if in.CustomerAddressID != nil {
			ses, err = b.openOnAddress(ctx, tx, a, in)
		} else {
			ses, err = b.buildSession(ctx, a, in)
			if err == nil {
				err = b.store.InsertSession(ctx, tx, ses)
			}
		}
		if err != nil {
			return nil, 0, err
		}
		body, err := json.Marshal(s.view.Session(ses))
		return body, statusOK, err
	})
	if err != nil {
		return store.StoredResponse{}, false, err
	}
	return store.StoredResponse{Status: status, Body: body}, replayed, nil
}

// resolveAmount reads the session's product and amount. Every session comes from a product: a
// fixed price sets the amount; on an open price a link that sets an amount sets it (never checked
// against the range again: the merchant may keep such a link when the range moves, and the fee
// floor still applies when the session is built), otherwise the requested amount (the payer's on
// a link page, the merchant's server through the API) must lie within the product's range. API
// sessions need an active product; a link's product was checked with the link, a customer
// address's when the address was locked. On a customer address an open price without an amount
// opens an open-amount session (open is true, the amount zero): the payer decides how much.
func (s *Service) resolveAmount(ctx context.Context, a Auth, in SessionInput) (cents money.Units, open bool, product *store.Product, err error) {
	p := in.product
	if p == nil {
		if in.ProductID == nil {
			return money.Zero, false, nil, invalid("product_id is required")
		}
		got, err := s.store.LockProductForSession(ctx, a.MerchantID, a.Livemode, *in.ProductID)
		if errors.Is(err, store.ErrNotFound) {
			return money.Zero, false, nil, productNotFound()
		}
		if err != nil {
			return money.Zero, false, nil, err
		}
		if !got.Active {
			return money.Zero, false, nil, newErr(KindInvalid, CodeProductInactive, "this product is no longer available")
		}
		p = &got
	}
	if p.PriceType == store.PriceFixed {
		if in.Amount != nil {
			return money.Zero, false, nil, at(invalid("amount is fixed by the product; omit it"), "amount")
		}
		return *p.AmountCents, false, p, nil
	}
	switch {
	case in.link != nil && in.link.AmountCents != nil:
		if in.Amount != nil {
			return money.Zero, false, nil, at(invalid("amount is set by this link; omit it"), "amount")
		}
		return *in.link.AmountCents, false, p, nil
	case in.Amount == nil && in.address != nil:
		// The product's range does not bound it: what was sent is what was paid.
		return money.Zero, true, p, nil
	case in.Amount == nil:
		return money.Zero, false, nil, at(invalid("amount is required for an open-price product"), "amount")
	default:
		if cents, err = parseAmount("amount", *in.Amount); err != nil {
			return money.Zero, false, nil, at(err, "amount")
		}
	}
	if err := checkRange(cents, *p); err != nil {
		return money.Zero, false, nil, at(err, "amount")
	}
	return cents, false, p, nil
}

// checkRange checks an amount against an open price's range.
func checkRange(cents money.Units, p store.Product) error {
	if cents.Cmp(*p.MinAmountCents) < 0 {
		return newErr(KindInvalid, CodeAmountBelowMinimum, "amount is below the product minimum of %s", money.FormatUSD(*p.MinAmountCents))
	}
	if p.MaxAmountCents != nil && cents.Cmp(*p.MaxAmountCents) > 0 {
		return newErr(KindInvalid, CodeAmountAboveMaximum, "amount is above the product maximum of %s", money.FormatUSD(*p.MaxAmountCents))
	}
	return nil
}

func (s *Service) buildSession(ctx context.Context, a Auth, in SessionInput) (*store.Session, error) {
	cents, open, product, err := s.resolveAmount(ctx, a, in)
	if err != nil {
		return nil, err
	}
	// expires_in is the pay window: it starts at the first reveal (D48). Until then the session
	// waits SelectWindow for the payer to pick a network.
	expiresIn := s.cfg.DefaultExpiry
	if in.ExpiresIn != nil {
		// Compared in seconds first: a huge value would overflow the Duration and wrap into range.
		minSec, maxSec := int64(s.cfg.MinExpiry/time.Second), int64(s.cfg.MaxExpiry/time.Second)
		if *in.ExpiresIn < minSec || *in.ExpiresIn > maxSec {
			return nil, invalid("expires_in must be between %d and %d seconds", minSec, maxSec)
		}
		expiresIn = time.Duration(*in.ExpiresIn) * time.Second
	}
	// Precedence: the product's defaults, then the link's or the customer address's, then the API
	// request's.
	var linkID, addressID *string
	metadata := mergeMetadata(product.Metadata)
	successURL, productID := product.SuccessURL, &product.ID
	if in.link != nil {
		id := in.link.ID
		linkID = &id
		metadata = mergeMetadata(metadata, in.link.Metadata)
		if in.link.SuccessURL != nil {
			successURL = in.link.SuccessURL
		}
	}
	if in.address != nil {
		id := in.address.ID
		addressID = &id
		metadata = mergeMetadata(metadata, in.address.Metadata)
	}
	metadata = mergeMetadata(metadata, in.Metadata)
	if err := validateMetadata(metadata); err != nil {
		return nil, err
	}
	if in.SuccessURL != nil {
		if err := validateURL("success_url", *in.SuccessURL, !a.Livemode); err != nil {
			return nil, err
		}
		successURL = in.SuccessURL
	}
	var ref, refSource *string
	if in.ClientReferenceID != nil {
		if err := validateClientReference(*in.ClientReferenceID); err != nil {
			return nil, err
		}
		if in.referenceSource != ReferenceMerchant && in.referenceSource != ReferencePayer {
			return nil, errors.New("service: client reference without a source")
		}
		source := in.referenceSource
		ref, refSource = in.ClientReferenceID, &source
	}

	var salt [32]byte
	var payTo []store.PayToRecord
	var payouts []store.SessionPayout
	if in.address != nil {
		// The address's salt and inputs, never new ones: every session on it derives the same
		// deposit addresses.
		salt = in.address.Salt
		if payTo, payouts, err = s.addressEntries(ctx, a, *in.address, in.product); err != nil {
			return nil, err
		}
		if !open {
			if err := checkEntriesFloor("amount", cents, payTo); err != nil {
				return nil, err
			}
			if err := setAmounts(payTo, cents); err != nil {
				return nil, err
			}
		}
	} else {
		if _, err := rand.Read(salt[:]); err != nil {
			return nil, err
		}
		if payTo, payouts, err = s.payTo(ctx, a, cents, salt, product); err != nil {
			return nil, err
		}
	}
	now := s.store.Now()
	ses := &store.Session{
		ID: ids.NewAt(ids.Session, now), MerchantID: a.MerchantID, Livemode: a.Livemode, ProductID: productID, LinkID: linkID,
		CustomerAddressID: addressID, Metadata: metadata, SuccessURL: successURL, Salt: salt, CreatedAt: now, UpdatedAt: now,
		PayTo: payTo, Payouts: payouts, ClientReferenceID: ref, ClientReferenceSource: refSource, PayWindow: expiresIn,
	}
	// A session waits for its payer to pick a network for the selection window, then its pay
	// window runs from the first reveal (D48). A session on a customer address is revealed from
	// creation, so it expires a pay window after it opens, as before.
	expiresAt := now.Add(s.cfg.SelectWindow)
	if in.address != nil {
		expiresAt = now.Add(expiresIn)
	}
	if open {
		ses.State = session.NewOpenAmount(store.PayToState(payTo), expiresAt)
	} else {
		ses.State = session.New(cents, store.PayToState(payTo), expiresAt)
	}
	return ses, nil
}

// payableChains reads the merchant's accepted assets in a's mode and the chains among them a
// session can be paid on (known in the mode, with a treasury).
func (s *Service) payableChains(ctx context.Context, a Auth) ([]store.ChainAsset, map[string]chain.Family, error) {
	assets, err := s.store.AcceptedAssets(ctx, a.MerchantID, a.Livemode)
	if err != nil {
		return nil, nil, err
	}
	needed := map[string]chain.Family{}
	for _, x := range assets {
		if c, ok := s.cfg.Registry.Get(x.Chain); ok && c.Livemode == a.Livemode && s.cfg.Treasury[a.Livemode][c.Family] != "" {
			needed[c.ID] = c.Family
		}
	}
	return assets, needed, nil
}

// payTo derives one entry per accepted (chain, asset) with the session's amount in each token
// (depositEntries), after checking the amount settles after the fee on every chain offered.
func (s *Service) payTo(ctx context.Context, a Auth, cents money.Units, salt [32]byte, product *store.Product) ([]store.PayToRecord, []store.SessionPayout, error) {
	out, payouts, err := s.depositEntries(ctx, a, salt, product)
	if err != nil {
		return nil, nil, err
	}
	if err := checkEntriesFloor("amount", cents, out); err != nil {
		return nil, nil, err
	}
	if err := setAmounts(out, cents); err != nil {
		return nil, nil, err
	}
	return out, payouts, nil
}

// checkEntriesFloor checks that cents settles after the fee each entry's forwarder commits to.
func checkEntriesFloor(field string, cents money.Units, entries []store.PayToRecord) error {
	chains := make([]chain.Chain, len(entries))
	for i, p := range entries {
		chains[i] = chain.Chain{ID: p.Chain, Fee: p.Fee}
	}
	return checkFloor(field, cents, chains)
}

// setAmounts sets each entry's amount: cents in the entry's token.
func setAmounts(entries []store.PayToRecord, cents money.Units) error {
	for i := range entries {
		amount, err := money.USDToToken(cents, entries[i].Decimals)
		if err != nil {
			return err
		}
		entries[i].Amount = amount
	}
	return nil
}

// effectiveMethods lists the (chain, asset) pairs a session of p opened now offers (D60), in the
// order the merchant's accepted assets come in (by chain, then asset): those the merchant accepts
// on a chain of the mode that the registry knows, on a chain a session can be paid on (payable:
// a treasury there) and that p takes (it has payout items there), narrowed by p's choice on that
// chain (all, or the some list). It is never stored: the product's payment_methods, every
// session's (and new customer address's) pay_to and bound tokens, and the customer address
// attention all read it from here.
func effectiveMethods(reg *chain.Registry, livemode bool, accepted []store.ChainAsset, payable map[string]chain.Family, p *store.Product) []store.ChainAsset {
	var out []store.ChainAsset
	for _, x := range accepted {
		c, ok := reg.Get(x.Chain)
		if !ok || c.Livemode != livemode {
			continue
		}
		if _, ok := c.Asset(x.Asset); !ok {
			continue
		}
		if _, ok := payable[c.ID]; !ok || len(p.Payout[c.ID]) == 0 {
			continue
		}
		if n, ok := p.Networks[c.ID]; ok && !n.Takes(x.Asset) {
			continue
		}
		out = append(out, x)
	}
	return out
}

// depositEntries derives one entry per effective (chain, asset) of the product
// (effectiveMethods) that no pause takes out (D64), without an amount: the deposit address and
// every input it commits to.
// Entries need the product to pay the chain's family and a treasury there; a derivation failure
// aborts the session (or customer address) rather than
// showing an address nobody controls. The snapshot copies the product's recipients of every
// family that got an entry: the addresses commit to exactly them, so later changes to the
// product never reach them.
func (s *Service) depositEntries(ctx context.Context, a Auth, salt [32]byte, product *store.Product) ([]store.PayToRecord, []store.SessionPayout, error) {
	assets, needed, err := s.payableChains(ctx, a)
	if err != nil {
		return nil, nil, err
	}
	resolved := s.resolvePayouts(product, needed)
	// A paused chain or asset (D64) is offered by nothing new; one with every method paused is
	// refused as such, not as an account without payment methods.
	pauses, err := s.ChainPauses(ctx)
	if err != nil {
		return nil, nil, err
	}
	all := effectiveMethods(s.cfg.Registry, a.Livemode, assets, needed, product)
	methods := unpaused(all, pauses)
	if len(methods) == 0 && len(all) > 0 {
		return nil, nil, noUnpausedMethods()
	}
	// Each forwarder binds every asset the session offers on its chain (D41): the product's
	// effective assets there (D60), so one Solana deposit owner serves both USDC and USDT.
	offered := map[string][]string{}
	for _, x := range methods {
		offered[x.Chain] = append(offered[x.Chain], x.Asset)
	}
	var out []store.PayToRecord
	used := map[string]bool{}
	for _, x := range methods {
		c, _ := s.cfg.Registry.Get(x.Chain)
		asset, _ := c.Asset(x.Asset)
		items := resolved[c.ID]
		treasury := s.cfg.Treasury[a.Livemode][c.Family]
		recipients := make([]payout.Recipient, len(items))
		for i, it := range items {
			recipients[i] = payout.Recipient{Address: it.Address, Bps: it.Bps}
		}
		tokens, err := derive.BoundTokens(c, offered[c.ID])
		if err != nil {
			return nil, nil, fmt.Errorf("bind tokens on %s: %w", c.ID, err)
		}
		// The same list is stored with every entry on the chain (bound_tokens): the senders
		// re-derive the address with it.
		params := derive.Params{Treasury: treasury, Fee: c.Fee, Recipients: recipients, Tokens: tokens}
		sol, hasSol := s.cfg.Solana[c.ID]
		if c.Family == chain.Solana && hasSol {
			params.RentReceiver = sol.RentReceiver
		} else {
			sol = SolanaChain{}
		}
		addr, err := s.cfg.Deriver.Address(c, salt, params)
		if err != nil {
			if errors.Is(err, derive.ErrUnavailable) {
				return nil, nil, newErr(KindUnavailable, CodeServiceUnavailable, "deposit addresses are not available on %s yet", c.ID)
			}
			return nil, nil, fmt.Errorf("derive %s: %w", c.ID, err)
		}
		used[c.ID] = true
		out = append(out, store.PayToRecord{
			Chain: c.ID, Asset: asset.Symbol, Family: c.Family, Token: asset.Token, Decimals: asset.Decimals, Address: addr,
			Recipients: recipients, Treasury: treasury, Fee: c.Fee,
			Program: sol.Program, RentReceiver: sol.RentReceiver, BoundTokens: tokens,
		})
	}
	if len(out) == 0 {
		return nil, nil, noPaymentMethods(a)
	}
	var payouts []store.SessionPayout
	for _, id := range store.PayoutChains(resolved) {
		if !used[id] {
			continue
		}
		for i, it := range resolved[id] {
			walletID, label := it.WalletID, it.Label
			payouts = append(payouts, store.SessionPayout{Chain: id, Family: it.Family, Position: i, WalletID: &walletID, Label: &label,
				Address: it.Address, Bps: it.Bps})
		}
	}
	return out, payouts, nil
}

func noPaymentMethods(a Auth) *Error {
	return invalid("no payment methods are configured for this account in %s mode", modeName(a.Livemode))
}

func modeName(live bool) string {
	if live {
		return "live"
	}
	return "test"
}

// GetSession returns one of the merchant's sessions as the API renders it; it needs the sessions
// read scope.
func (s *Service) GetSession(ctx context.Context, a Auth, id string) (view.Session, error) {
	if err := a.Require(apikey.SessionsRead); err != nil {
		return view.Session{}, err
	}
	ses, err := s.store.GetSession(ctx, a.MerchantID, a.Livemode, id)
	if errors.Is(err, store.ErrNotFound) {
		return view.Session{}, sessionNotFound()
	}
	if err != nil {
		return view.Session{}, err
	}
	return s.view.Session(ses), nil
}

// EventList is one page of rendered events and whether more follow.
type EventList struct {
	Data       []json.RawMessage `json:"data"`
	HasMore    bool              `json:"has_more"`
	NextCursor string            `json:"next_cursor"`
}

// ListEvents serves the ordered feed. next_cursor is the seq of the last event returned, or the
// request cursor when the page is empty, so a poller can always resume from it.
func (s *Service) ListEvents(ctx context.Context, a Auth, after int64, limit int) (EventList, error) {
	if err := a.Require(apikey.EventsRead); err != nil {
		return EventList{}, err
	}
	list, more, err := s.store.ListEvents(ctx, a.MerchantID, a.Livemode, after, limit)
	if err != nil {
		return EventList{}, err
	}
	out := EventList{Data: []json.RawMessage{}, HasMore: more, NextCursor: fmt.Sprint(after)}
	for _, e := range list {
		out.Data = append(out.Data, e.Payload)
		out.NextCursor = fmt.Sprint(e.Seq)
	}
	return out, nil
}

// LatestEventCursor is an empty feed page whose next_cursor is the newest event's seq, so a
// poller can start from now without reading the history.
func (s *Service) LatestEventCursor(ctx context.Context, a Auth) (EventList, error) {
	if err := a.Require(apikey.EventsRead); err != nil {
		return EventList{}, err
	}
	n, err := s.store.LatestEventSeq(ctx, a.MerchantID, a.Livemode)
	if err != nil {
		return EventList{}, err
	}
	return EventList{Data: []json.RawMessage{}, NextCursor: fmt.Sprint(n)}, nil
}
