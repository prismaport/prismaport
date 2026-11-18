package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/store"
	"github.com/prismaport/prismaport/internal/view"
)

// Link is a payment link as the dashboard shows it: the merchant API shape plus a summary of the
// product it sells.
type Link struct {
	PaymentLink
	Product LinkProductSummary `json:"product"`
}

// LinkProductSummary is what a link row and the link drawer show of its product.
type LinkProductSummary struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	Description      *string  `json:"description"`
	PriceType        string   `json:"price_type"`
	Amount           *string  `json:"amount"`
	MinAmount        *string  `json:"min_amount"`
	MaxAmount        *string  `json:"max_amount"`
	SuggestedAmounts []string `json:"suggested_amounts"`
	Active           bool     `json:"active"`
}

func dashLink(l PaymentLink) Link {
	p := l.product
	sum := LinkProductSummary{ID: p.ID, Name: p.Name, Description: p.Description, PriceType: string(p.PriceType),
		Amount: optUSD(p.AmountCents), MinAmount: optUSD(p.MinAmountCents), MaxAmount: optUSD(p.MaxAmountCents),
		SuggestedAmounts: []string{}, Active: p.Active}
	for _, u := range p.SuggestedCents {
		sum.SuggestedAmounts = append(sum.SuggestedAmounts, money.FormatUSD(u))
	}
	return Link{PaymentLink: l, Product: sum}
}

// displayName is the name the dashboard lists a link under: its label, else its product's name.
func (l PaymentLink) displayName() string {
	if l.Name != nil {
		return *l.Name
	}
	return l.product.Name
}

// DashLink returns one of the merchant's payment links with its stats, or a not-found error.
func (s *Service) DashLink(ctx context.Context, a Auth, id string) (Link, error) {
	l, err := s.paymentLink(ctx, a, id)
	if err != nil {
		return Link{}, err
	}
	return dashLink(l), nil
}

// DashCreateLink creates a payment link at most once per Idempotency-Key (scoped to the merchant
// and mode, apart from API keys' namespace). A replay answers with the link as it is now.
func (s *Service) DashCreateLink(ctx context.Context, a Auth, idemKey string, rawBody []byte, in LinkInput) (Link, bool, error) {
	if err := checkIdempotencyKey(idemKey); err != nil {
		return Link{}, false, err
	}
	scope := store.IdempotencyScope{MerchantID: a.MerchantID, Livemode: a.Livemode, Key: "dashboard_link/" + idemKey}
	id, replayed, err := s.createLink(ctx, a, idemRequest{scope: scope, route: "POST /v1/dashboard/links", body: rawBody}, in)
	if err != nil {
		return Link{}, false, err
	}
	l, err := s.DashLink(ctx, a, id)
	return l, replayed, err
}

// DashUpdateLink edits a link. Changes reach only sessions opened afterwards; deactivating never
// touches sessions already open.
func (s *Service) DashUpdateLink(ctx context.Context, a Auth, id string, in LinkInput) (Link, error) {
	if err := s.updateLink(ctx, a, id, in); err != nil {
		return Link{}, err
	}
	return s.DashLink(ctx, a, id)
}

// LinkFilter narrows the dashboard link list.
type LinkFilter struct {
	// Status is live, inactive (every link that is not live) or all.
	Status    string
	ProductID string
	// Query matches the link's label or its product's name (case-insensitive substring), or the
	// exact link or product id.
	Query  string
	Cursor string
	Limit  int
}

// LinkList is one page of dashboard payment links and the cursor of the next page, if any.
type LinkList struct {
	Data       []Link  `json:"data"`
	NextCursor *string `json:"next_cursor"`
}

// linkCursor is the sort key of the last link of a page: live first, then by last payment
// (never paid last), then newest.
type linkCursor struct {
	Live     int    `json:"l"`
	LastPaid int64  `json:"p"`
	ID       string `json:"i"`
	// Now is the instant liveness is evaluated at, pinned by the first page so that a link expiring
	// between pages is not returned twice. Held sessions still change liveness between pages.
	Now int64 `json:"n,omitempty"`
}

// maxCursorMicro bounds the times a cursor may carry: a forged one beyond year 9999 cannot be
// encoded by the database driver and would be a 500 rather than a 400.
var maxCursorMicro = time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC).UnixMicro()

// validMicro reports whether v is a time a cursor may carry; min is -1 where "never" is allowed.
func validMicro(v, min int64) bool { return v >= min && v <= maxCursorMicro }

func cursorOf(l PaymentLink) linkCursor {
	c := linkCursor{LastPaid: -1, ID: l.ID}
	if l.Live {
		c.Live = 1
	}
	if l.counts.LastPaidAt != nil {
		c.LastPaid = l.counts.LastPaidAt.UnixMicro()
	}
	return c
}

// before reports whether a sorts before b.
func (a linkCursor) before(b linkCursor) bool {
	if a.Live != b.Live {
		return a.Live > b.Live
	}
	if a.LastPaid != b.LastPaid {
		return a.LastPaid > b.LastPaid
	}
	return a.ID > b.ID
}

func encodeCursor(v any) string {
	raw, _ := json.Marshal(v)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeCursor(s string, v any) error {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || json.Unmarshal(raw, v) != nil {
		return at(invalid("cursor is not valid"), "cursor")
	}
	return nil
}

// sortedLinks renders the mode's links (or one product's), sorted live first, then by last
// payment.
func (s *Service) sortedLinks(ctx context.Context, a Auth, productID string) ([]PaymentLink, error) {
	links, err := s.modeLinks(ctx, a, productID)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(links, func(i, j int) bool { return cursorOf(links[i]).before(cursorOf(links[j])) })
	return links, nil
}

// DashListLinks returns one page of the merchant's payment links matching the filter.
func (s *Service) DashListLinks(ctx context.Context, a Auth, f LinkFilter) (LinkList, error) {
	if err := checkLinkStatus(f.Status); err != nil {
		return LinkList{}, err
	}
	now := s.store.Now()
	q := store.LinkQuery{ProductID: f.ProductID, Status: linkStatusArg(f.Status), Query: f.Query, ByActivity: true, Limit: f.Limit}
	if f.Cursor != "" {
		var c linkCursor
		if err := decodeCursor(f.Cursor, &c); err != nil || c.ID == "" || c.Live < 0 || c.Live > 1 ||
			!validMicro(c.LastPaid, -1) || !validMicro(c.Now, 0) {
			return LinkList{}, at(invalid("cursor is not valid"), "cursor")
		}
		if c.Now > 0 {
			now = time.UnixMicro(c.Now)
		}
		key := store.LinkKey{Live: c.Live == 1, ID: c.ID}
		if c.LastPaid >= 0 {
			t := time.UnixMicro(c.LastPaid)
			key.LastPaidAt = &t
		}
		q.AfterKey = &key
	}
	page, more, err := s.store.PageLinks(ctx, a.MerchantID, a.Livemode, q, now)
	if err != nil {
		return LinkList{}, err
	}
	links, err := s.linksWithProducts(ctx, a, listedLinks(page))
	if err != nil {
		return LinkList{}, err
	}
	out := LinkList{Data: make([]Link, 0, len(links))}
	for _, l := range links {
		out.Data = append(out.Data, dashLink(l))
	}
	if more && len(page) > 0 {
		last := page[len(page)-1]
		c := linkCursor{LastPaid: -1, ID: last.ID, Now: now.UnixMicro()}
		if last.Live {
			c.Live = 1
		}
		if last.LastPaidAt != nil {
			c.LastPaid = last.LastPaidAt.UnixMicro()
		}
		next := encodeCursor(c)
		out.NextCursor = &next
	}
	return out, nil
}

// listedLinks drops the query's liveness: a page is rendered with Liveness, which agrees with it
// and also gives the reason.
func listedLinks(page []store.ListedLink) []store.PaymentLink {
	out := make([]store.PaymentLink, len(page))
	for i, l := range page {
		out[i] = l.PaymentLink
	}
	return out
}

// linkStatusArg is a link status filter as PageLinks takes it: all is no filter.
func linkStatusArg(status string) string {
	if status == "all" {
		return ""
	}
	return status
}

// DashProductLinks lists one product's links (404 for an unknown product).
func (s *Service) DashProductLinks(ctx context.Context, a Auth, productID string, f LinkFilter) (LinkList, error) {
	if _, err := s.store.GetProduct(ctx, a.MerchantID, a.Livemode, productID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return LinkList{}, productNotFound()
		}
		return LinkList{}, err
	}
	f.ProductID = productID
	return s.DashListLinks(ctx, a, f)
}

// DashProduct is a product as the dashboard shows it: the catalog item, its payout per network,
// its stats over every session of the product (from any link or the API) and its link counts.
type DashProduct struct {
	ID               string            `json:"id"`
	Livemode         bool              `json:"livemode"`
	Name             string            `json:"name"`
	Description      *string           `json:"description"`
	PriceType        string            `json:"price_type"`
	Amount           *string           `json:"amount"`
	MinAmount        *string           `json:"min_amount"`
	MaxAmount        *string           `json:"max_amount"`
	SuggestedAmounts []string          `json:"suggested_amounts"`
	Currency         string            `json:"currency"`
	Active           bool              `json:"active"`
	SuccessURL       *string           `json:"success_url"`
	Metadata         map[string]string `json:"metadata"`
	Payout           view.Payout       `json:"payout"`
	// Networks is the product's stablecoin choice per network it takes (D60); PaymentMethods what a
	// checkout opened now offers (the account's Settings narrowed by Networks), as the merchant API
	// product has them.
	Networks       []view.ProductNetwork `json:"networks"`
	PaymentMethods []view.PaymentMethod  `json:"payment_methods"`
	Stats          ProductStats          `json:"stats"`
	Links          view.LinkTotals       `json:"links"`
	CreatedAt      string                `json:"created_at"`
	UpdatedAt      string                `json:"updated_at"`
}

// ProductStats are a link's stats over every session of the product, plus Open: its sessions
// still waiting for (more) money, which keep the price and payout they opened with.
type ProductStats struct {
	LinkStats
	Open int64 `json:"open"`
}

func (s *Service) renderProducts(ctx context.Context, a Auth, products []store.Product) ([]DashProduct, error) {
	methods, err := s.productMethods(ctx, a)
	if err != nil {
		return nil, err
	}
	idList := make([]string, len(products))
	for i, p := range products {
		idList[i] = p.ID
	}
	counts, err := s.store.ProductCounts(ctx, idList)
	if err != nil {
		return nil, err
	}
	totals, err := s.linkTotals(ctx, products)
	if err != nil {
		return nil, err
	}
	out := make([]DashProduct, 0, len(products))
	for _, p := range products {
		stats, err := statsOf(counts[p.ID])
		if err != nil {
			return nil, err
		}
		v := s.view.Product(p, totals[p.ID])
		d := DashProduct{ID: p.ID, Livemode: p.Livemode, Name: p.Name, Description: p.Description, PriceType: string(p.PriceType),
			Amount: optUSD(p.AmountCents), MinAmount: optUSD(p.MinAmountCents), MaxAmount: optUSD(p.MaxAmountCents),
			SuggestedAmounts: []string{}, Currency: "USD", Active: p.Active, SuccessURL: p.SuccessURL, Metadata: v.Metadata,
			Payout: v.Payout, Networks: v.Networks, PaymentMethods: methods(p), Stats: ProductStats{LinkStats: stats, Open: counts[p.ID].Open},
			Links: v.Links, CreatedAt: rfc3339(p.CreatedAt), UpdatedAt: rfc3339(p.UpdatedAt)}
		for _, u := range p.SuggestedCents {
			d.SuggestedAmounts = append(d.SuggestedAmounts, money.FormatUSD(u))
		}
		out = append(out, d)
	}
	return out, nil
}
