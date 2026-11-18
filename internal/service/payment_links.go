package service

import (
	"context"
	"errors"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/prismaport/prismaport/internal/apikey"
	"github.com/prismaport/prismaport/internal/ids"
	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/store"
)

// CodePaymentLinkNotFound is returned when no payment link of the merchant has the given ID.
const CodePaymentLinkNotFound = "payment_link_not_found"

const (
	maxLinkPayments = 1_000_000
	maxLinkLifetime = 10 * 365 * 24 * time.Hour
)

func linkNotFound() *Error {
	return newErr(KindNotFound, CodePaymentLinkNotFound, "no such payment link")
}

// LinkInput is a payment link create or patch request. A *Set flag with a nil value clears the
// field on patch (null in the request); without the flag the field is absent.
type LinkInput struct {
	ProductID   Field[string]
	Name        Field[string]
	Amount      Field[string]
	Active      Field[bool]
	ExpiresAt   Field[string]
	MaxPayments Field[int64]
	SuccessURL  Field[string]
	Metadata    Field[map[string]string]
}

// PaymentLink is a payment link as the merchant API shows it: its public URL, whether it is live
// now and its stats. The dashboard adds a summary of its product (Link).
type PaymentLink struct {
	ID                string            `json:"id"`
	Livemode          bool              `json:"livemode"`
	ProductID         string            `json:"product_id"`
	Name              *string           `json:"name"`
	Amount            *string           `json:"amount"`
	Active            bool              `json:"active"`
	ExpiresAt         *string           `json:"expires_at"`
	MaxPayments       *int              `json:"max_payments"`
	SuccessURL        *string           `json:"success_url"`
	Metadata          map[string]string `json:"metadata"`
	URL               string            `json:"url"`
	Live              bool              `json:"live"`
	UnavailableReason *string           `json:"unavailable_reason"`
	Stats             LinkStats         `json:"stats"`
	CreatedAt         string            `json:"created_at"`
	UpdatedAt         string            `json:"updated_at"`

	counts  store.LinkCounts
	link    store.PaymentLink
	product store.Product
}

// LinkStats summarises a payment link's sessions: how many opened and paid, what they received and
// the conversion rate.
type LinkStats struct {
	Opened     int64        `json:"opened"`
	Paid       int64        `json:"paid"`
	Received   []AssetTotal `json:"received"`
	LastPaidAt *string      `json:"last_paid_at"`
	Conversion *string      `json:"conversion"`
}

func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func optRFC3339(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := rfc3339(*t)
	return &s
}

func statsOf(c store.LinkCounts) (LinkStats, error) {
	received, err := ReceivedByAsset(c.Received)
	if err != nil {
		return LinkStats{}, err
	}
	return LinkStats{Opened: c.Opened, Paid: c.Paid, Received: received, LastPaidAt: optRFC3339(c.LastPaidAt),
		Conversion: Conversion(c.Paid, c.Opened)}, nil
}

// renderLinks renders links with their counts and liveness; products holds every link's product.
func (s *Service) renderLinks(ctx context.Context, links []store.PaymentLink, products map[string]store.Product) ([]PaymentLink, error) {
	idList := make([]string, len(links))
	for i, l := range links {
		idList[i] = l.ID
	}
	counts, err := s.store.LinkCounts(ctx, idList)
	if err != nil {
		return nil, err
	}
	now := s.store.Now()
	out := make([]PaymentLink, 0, len(links))
	for _, l := range links {
		p, ok := products[l.ProductID]
		if !ok {
			return nil, errors.New("service: a payment link without its product")
		}
		c := counts[l.ID]
		stats, err := statsOf(c)
		if err != nil {
			return nil, err
		}
		live, reason := Liveness(l, p.Active, c, now)
		pl := PaymentLink{
			ID: l.ID, Livemode: l.Livemode, ProductID: l.ProductID, Name: l.Name, Amount: optUSD(l.AmountCents), Active: l.Active,
			ExpiresAt: optRFC3339(l.ExpiresAt), MaxPayments: l.MaxPayments, SuccessURL: l.SuccessURL, Metadata: l.Metadata,
			URL: s.view.URL("l", l.ID), Live: live, Stats: stats, CreatedAt: rfc3339(l.CreatedAt), UpdatedAt: rfc3339(l.UpdatedAt),
			counts: c, link: l, product: p,
		}
		if pl.Metadata == nil {
			pl.Metadata = map[string]string{}
		}
		if !live {
			pl.UnavailableReason = &reason
		}
		out = append(out, pl)
	}
	return out, nil
}

// linksWithProducts renders links, reading their products.
func (s *Service) linksWithProducts(ctx context.Context, a Auth, links []store.PaymentLink) ([]PaymentLink, error) {
	var productIDs []string
	for _, l := range links {
		productIDs = append(productIDs, l.ProductID)
	}
	products, err := s.store.ProductsByID(ctx, a.MerchantID, a.Livemode, productIDs)
	if err != nil {
		return nil, err
	}
	return s.renderLinks(ctx, links, products)
}

func (s *Service) paymentLink(ctx context.Context, a Auth, id string) (PaymentLink, error) {
	l, err := s.store.GetLink(ctx, a.MerchantID, a.Livemode, id)
	if errors.Is(err, store.ErrNotFound) {
		return PaymentLink{}, linkNotFound()
	}
	if err != nil {
		return PaymentLink{}, err
	}
	list, err := s.linksWithProducts(ctx, a, []store.PaymentLink{l})
	if err != nil {
		return PaymentLink{}, err
	}
	return list[0], nil
}

func mergeMetadata(layers ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range layers {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// applyLinkInput merges in into l and validates the result against its product p. A member sent
// as null clears it.
func (s *Service) applyLinkInput(ctx context.Context, a Auth, l *store.PaymentLink, p store.Product, in LinkInput) error {
	if in.Name.Set {
		l.Name = nil
		if in.Name.Given() && in.Name.Value != "" {
			if utf8.RuneCountInString(in.Name.Value) > maxNameLen {
				return at(invalid("name must have at most %d characters", maxNameLen), "name")
			}
			l.Name = in.Name.Ptr()
		}
	}
	if in.Amount.Set {
		if err := s.applyLinkAmount(ctx, a, l, p, in.Amount); err != nil {
			return at(err, "amount")
		}
	}
	if in.Active.Given() {
		l.Active = in.Active.Value
	}
	if in.ExpiresAt.Set {
		if err := s.applyLinkExpiry(l, in.ExpiresAt); err != nil {
			return at(err, "expires_at")
		}
	}
	if in.MaxPayments.Set {
		l.MaxPayments = nil
		if in.MaxPayments.Given() {
			if in.MaxPayments.Value < 1 || in.MaxPayments.Value > maxLinkPayments {
				return at(invalid("max_payments must be between 1 and %d", maxLinkPayments), "max_payments")
			}
			n := int(in.MaxPayments.Value)
			l.MaxPayments = &n
		}
	}
	if in.SuccessURL.Set {
		l.SuccessURL = nil
		if in.SuccessURL.Given() && in.SuccessURL.Value != "" {
			if err := validateURL("success_url", in.SuccessURL.Value, !a.Livemode); err != nil {
				return at(err, "success_url")
			}
			l.SuccessURL = in.SuccessURL.Ptr()
		}
	}
	if in.Metadata.Set {
		l.Metadata = in.Metadata.Value
	}
	if err := validateMetadata(l.Metadata); err != nil {
		return at(err, "metadata")
	}
	// Every session of the link carries the product's metadata with the link's over it. It is
	// checked when the link's metadata changes and when the link is turned on, so a link that
	// could not open a session never looks live; deactivating one is never blocked by it.
	if in.Metadata.Set || (in.Active.Given() && in.Active.Value) {
		if err := validateMetadata(mergeMetadata(p.Metadata, l.Metadata)); err != nil {
			return at(newErr(KindInvalid, CodeInvalidRequest, "with the product's metadata, %s", AsError(err).Message), "metadata")
		}
	}
	return nil
}

// applyLinkAmount sets or (on null) clears the amount a link sets for its open-price product p.
func (s *Service) applyLinkAmount(ctx context.Context, a Auth, l *store.PaymentLink, p store.Product, amount Field[string]) error {
	l.AmountCents = nil
	if !amount.Given() {
		return nil
	}
	if p.PriceType == store.PriceFixed {
		return invalid("amount is set by the product's fixed price; a link can set the amount of a variable price only")
	}
	u, err := parseAmount("amount", amount.Value)
	if err != nil {
		return err
	}
	// Only a new amount is checked: a stored one may lie outside a range the product moved to
	// since (the merchant kept the link), and a later fee change must not block deactivating the
	// link.
	if err := s.checkLinkAmount(ctx, a, u, p); err != nil {
		return err
	}
	l.AmountCents = &u
	return nil
}

// applyLinkExpiry sets or (on null) clears a link's expiry: an RFC 3339 time in the next 10 years.
func (s *Service) applyLinkExpiry(l *store.PaymentLink, expiresAt Field[string]) error {
	l.ExpiresAt = nil
	if !expiresAt.Given() {
		return nil
	}
	t, err := time.Parse(time.RFC3339, expiresAt.Value)
	if err != nil {
		return invalid("expires_at must be an RFC 3339 time such as \"2026-12-31T23:59:59Z\"")
	}
	t = t.UTC().Truncate(time.Microsecond)
	now := s.store.Now()
	// A past expiry is surely a typo; deactivating is how a link is stopped now.
	if !t.After(now) {
		return invalid("expires_at must be in the future")
	}
	if t.After(now.Add(maxLinkLifetime)) {
		return invalid("expires_at must be within 10 years")
	}
	l.ExpiresAt = &t
	return nil
}

// checkLinkAmount checks an amount a link sets against its open-price product: inside the
// product's range and at least the smallest amount that settles on the accepted networks.
func (s *Service) checkLinkAmount(ctx context.Context, a Auth, cents money.Units, p store.Product) error {
	if p.PriceType != store.PriceOpen || p.MinAmountCents == nil {
		return invalid("amount is set by the product's fixed price; a link can set the amount of a variable price only")
	}
	if err := checkRange(cents, p); err != nil {
		return err
	}
	chains, err := s.acceptedChains(ctx, a)
	if err != nil {
		return err
	}
	return checkFloor("amount", cents, chains)
}

// createLink creates a link at most once per idempotency request (its scope, and the endpoint
// that is part of the request fingerprint). It returns the new link id.
func (s *Service) createLink(ctx context.Context, a Auth, req idemRequest, in LinkInput) (string, bool, error) {
	if !in.ProductID.Given() {
		return "", false, at(invalid("product_id is required"), "product_id")
	}
	if in.Active.Given() {
		return "", false, at(invalid("a new link is always active; deactivate it afterwards"), "active")
	}
	created, _, replayed, err := idempotent(ctx, s.store, req, func(ctx context.Context, tx pgx.Tx) (linkCreated, int, error) {
		b := s.bind(tx)
		p, err := b.store.LockProductShared(ctx, tx, a.MerchantID, a.Livemode, in.ProductID.Value)
		if errors.Is(err, store.ErrNotFound) {
			return linkCreated{}, 0, at(productNotFound(), "product_id")
		}
		if err != nil {
			return linkCreated{}, 0, err
		}
		l := store.PaymentLink{ID: ids.New(ids.PaymentLink), MerchantID: a.MerchantID, Livemode: a.Livemode, ProductID: p.ID, Active: true}
		if err := b.applyLinkInput(ctx, a, &l, p, in); err != nil {
			return linkCreated{}, 0, err
		}
		if err := b.store.CreateLinkTx(ctx, tx, l, a.actor()); err != nil {
			return linkCreated{}, 0, err
		}
		return linkCreated{ID: l.ID}, statusOK, nil
	})
	if err != nil {
		return "", false, err
	}
	if created.ID == "" {
		return "", false, errors.New("stored link response is malformed")
	}
	return created.ID, replayed, nil
}

type linkCreated struct {
	ID string `json:"id"`
}

func checkIdempotencyKey(key string) error {
	if key == "" {
		return invalid("the Idempotency-Key header is required")
	}
	if len(key) > maxIdempotencyKeyLen {
		return invalid("Idempotency-Key exceeds %d characters", maxIdempotencyKeyLen)
	}
	return nil
}

func (s *Service) updateLink(ctx context.Context, a Auth, id string, in LinkInput) error {
	if in.ProductID.Given() {
		return at(invalid("product_id cannot be changed; create a link for the other product"), "product_id")
	}
	_, err := s.store.UpdateLinkAs(ctx, a.MerchantID, a.Livemode, id, a.actor(), func(l *store.PaymentLink, p store.Product) error {
		return s.applyLinkInput(ctx, a, l, p, in)
	})
	if errors.Is(err, store.ErrNotFound) {
		return linkNotFound()
	}
	return err
}

// CreatePaymentLink creates a link of a product at most once per Idempotency-Key (the merchant
// API's namespace, shared with POST /v1/sessions; the endpoint is part of the request hash). A
// replay answers with the link as it is now.
func (s *Service) CreatePaymentLink(ctx context.Context, a Auth, idemKey string, rawBody []byte, in LinkInput) (PaymentLink, bool, error) {
	if err := a.Require(apikey.PaymentLinksWrite); err != nil {
		return PaymentLink{}, false, err
	}
	if err := checkIdempotencyKey(idemKey); err != nil {
		return PaymentLink{}, false, err
	}
	scope := store.IdempotencyScope{MerchantID: a.MerchantID, Livemode: a.Livemode, Key: idemKey}
	id, replayed, err := s.createLink(ctx, a, idemRequest{scope: scope, route: "POST /v1/payment_links", body: rawBody}, in)
	if err != nil {
		return PaymentLink{}, false, err
	}
	l, err := s.paymentLink(ctx, a, id)
	return l, replayed, err
}

// GetPaymentLink returns one of the merchant's payment links, or a not-found error; it needs the
// payment links read scope.
func (s *Service) GetPaymentLink(ctx context.Context, a Auth, id string) (PaymentLink, error) {
	if err := a.Require(apikey.PaymentLinksRead); err != nil {
		return PaymentLink{}, err
	}
	return s.paymentLink(ctx, a, id)
}

// UpdatePaymentLink applies a patch to one of the merchant's payment links; it needs the payment
// links write scope.
func (s *Service) UpdatePaymentLink(ctx context.Context, a Auth, id string, in LinkInput) (PaymentLink, error) {
	if err := a.Require(apikey.PaymentLinksWrite); err != nil {
		return PaymentLink{}, err
	}
	if err := s.updateLink(ctx, a, id, in); err != nil {
		return PaymentLink{}, err
	}
	return s.paymentLink(ctx, a, id)
}

// PaymentLinkList is one page of payment links and the cursor of the next page, if any.
type PaymentLinkList struct {
	Data       []PaymentLink `json:"data"`
	NextCursor *string       `json:"next_cursor"`
}

// maxLinksPerMode bounds the links the overview's attention list reads; the link lists page in
// SQL (PageLinks).
const maxLinksPerMode = 5000

func checkLinkStatus(status string) error {
	switch status {
	case "", "all", "live", "inactive":
		return nil
	}
	return at(invalid("status must be live, inactive or all"), "status")
}

// ListPaymentLinks lists a mode's links newest first, optionally of one product and by status
// (live, inactive: every link that is not live, or all). cursor is the last id of the previous
// page.
func (s *Service) ListPaymentLinks(ctx context.Context, a Auth, productID, status, cursor string, limit int) (PaymentLinkList, error) {
	if err := a.Require(apikey.PaymentLinksRead); err != nil {
		return PaymentLinkList{}, err
	}
	if err := checkLinkStatus(status); err != nil {
		return PaymentLinkList{}, err
	}
	page, more, err := s.store.PageLinks(ctx, a.MerchantID, a.Livemode,
		store.LinkQuery{ProductID: productID, Status: linkStatusArg(status), AfterID: cursor, Limit: limit}, s.store.Now())
	if err != nil {
		return PaymentLinkList{}, err
	}
	links, err := s.linksWithProducts(ctx, a, listedLinks(page))
	if err != nil {
		return PaymentLinkList{}, err
	}
	out := PaymentLinkList{Data: links}
	if out.Data == nil {
		out.Data = []PaymentLink{}
	}
	if more && len(links) > 0 {
		next := links[len(links)-1].ID
		out.NextCursor = &next
	}
	return out, nil
}

// modeLinks renders every link of the mode, or of one product when productID is set.
func (s *Service) modeLinks(ctx context.Context, a Auth, productID string) ([]PaymentLink, error) {
	links, err := s.store.AllLinks(ctx, a.MerchantID, a.Livemode, productID)
	if err != nil {
		return nil, err
	}
	if len(links) > maxLinksPerMode {
		links = links[:maxLinksPerMode]
	}
	return s.linksWithProducts(ctx, a, links)
}
