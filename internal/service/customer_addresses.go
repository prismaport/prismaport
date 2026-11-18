package service

import (
	"context"
	"crypto/rand"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/prismaport/prismaport/internal/apikey"
	"github.com/prismaport/prismaport/internal/ids"
	"github.com/prismaport/prismaport/internal/store"
	"github.com/prismaport/prismaport/internal/view"
)

// Static customer addresses (B11). Lock order, as for links:
// the address's product FOR KEY SHARE, then the address FOR UPDATE, then (through the session
// and idempotency inserts) the merchant KEY SHARE.

// Error codes of the customer address endpoints.
const (
	CodeCustomerAddressNotFound = "customer_address_not_found"
	// CodeAddressRotated is returned when a session is requested on a rotated address; its replaced_by is the new one.
	CodeAddressRotated = "address_rotated"
	// CodeConflict is returned when the customer's active address belongs to another product.
	CodeConflict = "conflict"
)

func customerAddressNotFound() *Error {
	return newErr(KindNotFound, CodeCustomerAddressNotFound, "no such customer address")
}

func addressRotated(ca store.CustomerAddress) *Error {
	replaced := ""
	if ca.ReplacedBy != nil {
		replaced = "; the customer's address is now " + *ca.ReplacedBy
	}
	return newErr(KindConflict, CodeAddressRotated, "this customer address was rotated%s", replaced)
}

// CustomerAddressInput is a POST /v1/customer_addresses request.
type CustomerAddressInput struct {
	CustomerID *string
	ProductID  *string
	Metadata   map[string]string
}

// CustomerAddressList is a page of addresses, newest first; next_cursor is null on the last page.
type CustomerAddressList struct {
	Data       []view.CustomerAddress `json:"data"`
	NextCursor *string                `json:"next_cursor"`
}

// AddressSessionList is a page of the sessions opened on an address, newest first.
type AddressSessionList struct {
	Data       []view.Session `json:"data"`
	NextCursor *string        `json:"next_cursor"`
}

// addressCreated is what an idempotent create stores: the address it answered and with which
// status (201 created, 200 the customer's existing address). A replay renders it as it is now.
type addressCreated struct {
	ID     string `json:"id"`
	Status int    `json:"status"`
}

// CreateCustomerAddress answers the customer's active address (200) or creates it (201). The
// customer's active address of another product is a conflict: rotating does not change the
// product, so the merchant opens sessions on that address or uses another customer_id. The
// Idempotency-Key is optional (the call is idempotent by customer_id anyway); with one, a replay
// answers the same address and status.
func (s *Service) CreateCustomerAddress(ctx context.Context, a Auth, idemKey string, rawBody []byte, in CustomerAddressInput) (view.CustomerAddress, int, bool, error) {
	if err := a.Require(apikey.CustomerAddressesWrite); err != nil {
		return view.CustomerAddress{}, 0, false, err
	}
	if in.CustomerID == nil {
		return view.CustomerAddress{}, 0, false, at(invalid("customer_id is required"), "customer_id")
	}
	if !ClientReferencePattern.MatchString(*in.CustomerID) {
		return view.CustomerAddress{}, 0, false, at(invalid("customer_id must have 1 to 128 characters, each a letter, a digit, '.', '_', ':' or '-'"), "customer_id")
	}
	if in.ProductID == nil {
		return view.CustomerAddress{}, 0, false, at(invalid("product_id is required"), "product_id")
	}
	if err := validateMetadata(in.Metadata); err != nil {
		return view.CustomerAddress{}, 0, false, at(err, "metadata")
	}
	if idemKey != "" {
		if err := checkIdempotencyKey(idemKey); err != nil {
			return view.CustomerAddress{}, 0, false, err
		}
	}
	// The blacklist checks of the product's payout run before any lock (see precheckPayouts).
	if p, err := s.store.GetProduct(ctx, a.MerchantID, a.Livemode, *in.ProductID); err == nil && p.Active {
		if err := s.precheckProductPayouts(ctx, a, &p); err != nil {
			return view.CustomerAddress{}, 0, false, err
		}
	}
	var created addressCreated
	var replayed bool
	// A concurrent create of the same customer wins the active index; the retry answers its address.
	for attempt := 0; ; attempt++ {
		var err error
		created, replayed, err = s.createAddressOnce(ctx, a, idemKey, rawBody, in)
		if errors.Is(err, store.ErrActiveAddressExists) && attempt == 0 {
			continue
		}
		if err != nil {
			return view.CustomerAddress{}, 0, false, err
		}
		break
	}
	ca, err := s.store.GetCustomerAddress(ctx, a.MerchantID, a.Livemode, created.ID)
	if err != nil {
		return view.CustomerAddress{}, 0, false, err
	}
	return s.view.CustomerAddress(ca), created.Status, replayed, nil
}

func (s *Service) createAddressOnce(ctx context.Context, a Auth, idemKey string, rawBody []byte, in CustomerAddressInput) (addressCreated, bool, error) {
	run := func(ctx context.Context, tx pgx.Tx) (addressCreated, int, error) {
		res, err := s.bind(tx).getOrCreateAddress(ctx, tx, a, in)
		return res, res.Status, err
	}
	var res addressCreated
	replayed := false
	var err error
	if idemKey == "" {
		err = pgx.BeginFunc(ctx, s.store.Pool(), func(tx pgx.Tx) error {
			res, _, err = run(ctx, tx)
			return err
		})
	} else {
		scope := store.IdempotencyScope{MerchantID: a.MerchantID, Livemode: a.Livemode, Key: idemKey}
		req := idemRequest{scope: scope, route: "POST /v1/customer_addresses", body: rawBody}
		res, _, replayed, err = idempotent(ctx, s.store, req, run)
	}
	if err != nil {
		return addressCreated{}, false, err
	}
	if res.ID == "" || res.Status == 0 {
		return addressCreated{}, false, errors.New("stored customer address response is malformed")
	}
	return res, replayed, nil
}

// getOrCreateAddress runs inside tx: the product FOR KEY SHARE, then the customer's active
// address FOR UPDATE, or a new one derived from the product's payout now.
func (s *Service) getOrCreateAddress(ctx context.Context, tx pgx.Tx, a Auth, in CustomerAddressInput) (addressCreated, error) {
	p, err := s.store.LockProductForSession(ctx, a.MerchantID, a.Livemode, *in.ProductID)
	if errors.Is(err, store.ErrNotFound) {
		return addressCreated{}, at(productNotFound(), "product_id")
	}
	if err != nil {
		return addressCreated{}, err
	}
	existing, err := s.store.ActiveCustomerAddress(ctx, tx, a.MerchantID, a.Livemode, *in.CustomerID)
	switch {
	case err == nil && existing.ProductID != p.ID:
		return addressCreated{}, at(newErr(KindConflict, CodeConflict,
			"customer %s already has the active address %s, of product %s", existing.CustomerID, existing.ID, existing.ProductID), "product_id")
	case err == nil:
		return addressCreated{ID: existing.ID, Status: statusOK}, nil
	case !errors.Is(err, store.ErrNotFound):
		return addressCreated{}, err
	}
	if !p.Active {
		return addressCreated{}, at(newErr(KindInvalid, CodeProductInactive, "this product is no longer available"), "product_id")
	}
	ca, err := s.newAddress(ctx, a, *in.CustomerID, &p, in.Metadata)
	if err != nil {
		return addressCreated{}, err
	}
	if err := s.store.InsertCustomerAddress(ctx, tx, ca, a.actor()); err != nil {
		return addressCreated{}, err
	}
	return addressCreated{ID: ca.ID, Status: statusCreated}, nil
}

// newAddress draws a salt and derives the address of every accepted (chain, asset) with the
// product's payout and the current fee, exactly as a session's (depositEntries).
func (s *Service) newAddress(ctx context.Context, a Auth, customerID string, p *store.Product, metadata map[string]string) (*store.CustomerAddress, error) {
	// Every session on the address carries the product's metadata with the address's over it.
	if err := validateMetadata(mergeMetadata(p.Metadata, metadata)); err != nil {
		return nil, at(newErr(KindInvalid, CodeInvalidRequest, "with the product's metadata, %s", AsError(err).Message), "metadata")
	}
	var salt [32]byte
	if _, err := rand.Read(salt[:]); err != nil {
		return nil, err
	}
	payTo, payouts, err := s.depositEntries(ctx, a, salt, p)
	if err != nil {
		return nil, err
	}
	now := s.store.Now()
	return &store.CustomerAddress{ID: ids.NewAt(ids.CustomerAddress, now), MerchantID: a.MerchantID, Livemode: a.Livemode,
		CustomerID: customerID, ProductID: p.ID, Salt: salt, Status: store.AddressActive, Metadata: metadata, CreatedAt: now,
		PayTo: payTo, Payouts: payouts}, nil
}

// GetCustomerAddress returns one customer address of the merchant, or a not-found error.
func (s *Service) GetCustomerAddress(ctx context.Context, a Auth, id string) (view.CustomerAddress, error) {
	ca, err := s.customerAddress(ctx, a, id)
	if err != nil {
		return view.CustomerAddress{}, err
	}
	return s.view.CustomerAddress(ca), nil
}

// customerAddress reads one address of a's merchant and mode, with its rows.
func (s *Service) customerAddress(ctx context.Context, a Auth, id string) (store.CustomerAddress, error) {
	if err := a.Require(apikey.CustomerAddressesRead); err != nil {
		return store.CustomerAddress{}, err
	}
	ca, err := s.store.GetCustomerAddress(ctx, a.MerchantID, a.Livemode, id)
	if errors.Is(err, store.ErrNotFound) {
		return store.CustomerAddress{}, customerAddressNotFound()
	}
	return ca, err
}

// ListCustomerAddresses lists a mode's addresses newest first, every status, of one customer when
// customerID is set. cursor is the last id of the previous page.
func (s *Service) ListCustomerAddresses(ctx context.Context, a Auth, customerID, cursor string, limit int) (CustomerAddressList, error) {
	list, next, err := s.customerAddresses(ctx, a, customerID, cursor, limit)
	if err != nil {
		return CustomerAddressList{}, err
	}
	out := CustomerAddressList{Data: []view.CustomerAddress{}, NextCursor: next}
	for _, ca := range list {
		out.Data = append(out.Data, s.view.CustomerAddress(ca))
	}
	return out, nil
}

// customerAddresses is a page of ListCustomerAddresses as stored, with the next cursor (nil on the
// last page).
func (s *Service) customerAddresses(ctx context.Context, a Auth, customerID, cursor string, limit int) ([]store.CustomerAddress, *string, error) {
	if err := a.Require(apikey.CustomerAddressesRead); err != nil {
		return nil, nil, err
	}
	if customerID != "" && !ClientReferencePattern.MatchString(customerID) {
		return nil, nil, at(invalid("customer_id must have 1 to 128 characters, each a letter, a digit, '.', '_', ':' or '-'"), "customer_id")
	}
	list, more, err := s.store.ListCustomerAddresses(ctx, a.MerchantID, a.Livemode, customerID, cursor, limit)
	if err != nil {
		return nil, nil, err
	}
	var next *string
	if more {
		last := list[len(list)-1].ID
		next = &last
	}
	return list, next, nil
}

// RotateCustomerAddress replaces an active address with a new one for the same customer and
// product, derived from the product's payout and the fee now, and answers the new one. A session
// open on the old address runs to its end; no new session opens there.
func (s *Service) RotateCustomerAddress(ctx context.Context, a Auth, id string) (view.CustomerAddress, error) {
	if err := a.Require(apikey.CustomerAddressesWrite); err != nil {
		return view.CustomerAddress{}, err
	}
	old, err := s.store.GetCustomerAddress(ctx, a.MerchantID, a.Livemode, id)
	if errors.Is(err, store.ErrNotFound) {
		return view.CustomerAddress{}, customerAddressNotFound()
	}
	if err != nil {
		return view.CustomerAddress{}, err
	}
	if old.Status != store.AddressActive {
		return view.CustomerAddress{}, addressRotated(old)
	}
	if p, err := s.store.GetProduct(ctx, a.MerchantID, a.Livemode, old.ProductID); err == nil && p.Active {
		if err := s.precheckProductPayouts(ctx, a, &p); err != nil {
			return view.CustomerAddress{}, err
		}
	}
	var newID string
	err = pgx.BeginFunc(ctx, s.store.Pool(), func(tx pgx.Tx) error {
		b := s.bind(tx)
		ca, p, err := b.store.LockCustomerAddress(ctx, tx, a.MerchantID, a.Livemode, id)
		if err != nil {
			return err
		}
		if ca.Status != store.AddressActive {
			return addressRotated(ca)
		}
		if !p.Active {
			return newErr(KindInvalid, CodeProductInactive, "this customer address's product is no longer available")
		}
		next, err := b.newAddress(ctx, a, ca.CustomerID, &p, ca.Metadata)
		if err != nil {
			return err
		}
		// The old row leaves the active index before the new one enters it.
		if err := b.store.MarkAddressRotated(ctx, tx, ca, next.ID, a.actor(), next.CreatedAt); err != nil {
			return err
		}
		newID = next.ID
		return b.store.InsertCustomerAddress(ctx, tx, next, a.actor())
	})
	if errors.Is(err, store.ErrNotFound) {
		return view.CustomerAddress{}, customerAddressNotFound()
	}
	if err != nil {
		return view.CustomerAddress{}, err
	}
	ca, err := s.store.GetCustomerAddress(ctx, a.MerchantID, a.Livemode, newID)
	if err != nil {
		return view.CustomerAddress{}, err
	}
	return s.view.CustomerAddress(ca), nil
}

// ListAddressSessions lists the sessions opened on an address, newest first (B11 "list its
// payments").
func (s *Service) ListAddressSessions(ctx context.Context, a Auth, id, cursor string, limit int) (AddressSessionList, error) {
	if _, err := s.customerAddress(ctx, a, id); err != nil {
		return AddressSessionList{}, err
	}
	list, more, err := s.store.AddressSessions(ctx, id, cursor, limit)
	if err != nil {
		return AddressSessionList{}, err
	}
	out := AddressSessionList{Data: []view.Session{}}
	for _, ses := range list {
		out.Data = append(out.Data, s.view.Session(ses))
	}
	if more {
		next := list[len(list)-1].ID
		out.NextCursor = &next
	}
	return out, nil
}

// precheckProductPayouts runs the blacklist checks of a product's payout before a transaction
// opens. A customer address is shown from creation (it never waits for a reveal, D48), so its
// recipients are checked here rather than at a first reveal; every session opened on it checks
// the snapshot again (precheckAddressPayouts).
func (s *Service) precheckProductPayouts(ctx context.Context, a Auth, p *store.Product) error {
	_, needed, err := s.payableChains(ctx, a)
	if err != nil {
		return err
	}
	for id, items := range s.resolvePayouts(p, needed) {
		for _, it := range items {
			if err := s.checkPayoutBlacklist(ctx, a.Livemode, needed[id], it.Address); err != nil {
				return blacklistError(err, KindConflict, it.Label, it.Address)
			}
		}
	}
	return nil
}

// precheckAddressPayouts checks the recipients of an address's snapshot against the USDC
// blacklist before a session opens on it: one frozen recipient would block the whole split on
// chain, so the merchant rotates the address instead. The snapshot never changes, so no check
// runs again inside the transaction. A key with a stored response skips it; an empty scope (the
// public address page, which takes no key) always checks.
func (s *Service) precheckAddressPayouts(ctx context.Context, a Auth, scope store.IdempotencyScope, ca store.CustomerAddress) error {
	if scope.Key != "" {
		if used, err := s.store.IdempotencyKeyUsed(ctx, scope); err != nil || used {
			return err
		}
	}
	for _, p := range ca.Payouts {
		if err := s.checkPayoutBlacklist(ctx, a.Livemode, p.Family, p.Address); err != nil {
			label := ""
			if p.Label != nil {
				label = *p.Label
			}
			return blacklistError(err, KindConflict, label, p.Address)
		}
	}
	return nil
}

// addressEntries are the pay_to entries (without amounts) and payout snapshot a session on ca
// copies: the address's own, limited to the (chain, asset) pairs the merchant still accepts with
// the same token and the product p still takes (addressOffers), so a network or asset turned off
// since is not offered. Nothing is derived again: the addresses stay the ones the address
// committed to, and their bound tokens with them. A paused chain or asset (D64) stays: the
// customer already holds the address, and the session on it is what keeps it watched, so what
// is sent there during the pause is still credited.
func (s *Service) addressEntries(ctx context.Context, a Auth, ca store.CustomerAddress, p *store.Product) ([]store.PayToRecord, []store.SessionPayout, error) {
	assets, err := s.store.AcceptedAssets(ctx, a.MerchantID, a.Livemode)
	if err != nil {
		return nil, nil, err
	}
	accepted := map[[2]string]bool{}
	for _, x := range assets {
		accepted[[2]string{x.Chain, x.Asset}] = true
	}
	var out []store.PayToRecord
	used := map[string]bool{}
	for _, e := range ca.PayTo {
		c, ok := s.cfg.Registry.Get(e.Chain)
		if !ok || c.Livemode != a.Livemode || !accepted[[2]string{e.Chain, e.Asset}] || !addressOffers(p, e.Chain, e.Asset) {
			continue
		}
		if asset, ok := c.Asset(e.Asset); !ok || !strings.EqualFold(asset.Token, e.Token) {
			continue
		}
		out = append(out, e)
		used[e.Chain] = true
	}
	if len(out) == 0 {
		return nil, nil, invalid("none of the networks of this customer address is accepted in %s mode any more; rotate it", modeName(a.Livemode))
	}
	var payouts []store.SessionPayout
	for _, x := range ca.Payouts {
		if used[x.Chain] {
			payouts = append(payouts, x)
		}
	}
	return out, payouts, nil
}

// addressOffers reports whether an existing customer address of product p may still offer asset
// on chain, as far as the product decides: a product that narrowed the chain to a some list
// without asset stops it, exactly like the merchant turning the asset off (D60). A chain the
// product no longer takes keeps its snapshot, as a payout change does (payout_changed); rotating
// the address applies the product as it is now.
func addressOffers(p *store.Product, chainID, asset string) bool {
	if p == nil {
		return true
	}
	n, ok := p.Networks[chainID]
	return !ok || n.Takes(asset)
}

// resumableOnAddress is the customer address's current session while it can still be paid: open
// or partially paid, before its expires_at (D50). nil otherwise: the next open makes a new session
// on the address. Read it in the transaction that holds the address lock when a session may be
// opened after it.
func (s *Service) resumableOnAddress(ctx context.Context, tx pgx.Tx, addressID string) (*store.Session, error) {
	return s.store.OpenAddressSession(ctx, tx, addressID, s.store.Now())
}

// openOnAddress opens a session on a customer address, or answers the address's current session
// while it still waits for payment (open or partially paid, not expired; D50). Once that session
// is paid, overpaid, expired or otherwise closed, a new session opens on the same address. The
// address row lock makes two requests never open two sessions; the new session becomes the
// address's current one, which owns the watch of its deposit addresses.
func (s *Service) openOnAddress(ctx context.Context, tx pgx.Tx, a Auth, in SessionInput) (*store.Session, error) {
	ca, p, err := s.store.LockCustomerAddress(ctx, tx, a.MerchantID, a.Livemode, *in.CustomerAddressID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, at(customerAddressNotFound(), "customer_address_id")
	}
	if err != nil {
		return nil, err
	}
	if in.ProductID != nil && *in.ProductID != ca.ProductID {
		return nil, at(invalid("product_id must be omitted or be the customer address's product %s", ca.ProductID), "product_id")
	}
	if ca.Status != store.AddressActive {
		return nil, at(addressRotated(ca), "customer_address_id")
	}
	open, err := s.resumableOnAddress(ctx, tx, ca.ID)
	if err != nil || open != nil {
		return open, err
	}
	if !p.Active {
		return nil, newErr(KindInvalid, CodeProductInactive, "this customer address's product is no longer available")
	}
	in.address, in.product = &ca, &p
	ses, err := s.buildSession(ctx, a, in)
	if err != nil {
		return nil, err
	}
	if err := s.store.InsertSession(ctx, tx, ses); err != nil {
		return nil, err
	}
	return ses, s.store.SetCurrentSession(ctx, tx, ca.ID, ses.ID)
}
