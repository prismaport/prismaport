package service

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prismaport/prismaport/internal/store"
)

func (s *Service) dashProduct(ctx context.Context, a Auth, id string) (DashProduct, error) {
	p, err := s.store.GetProduct(ctx, a.MerchantID, a.Livemode, id)
	if errors.Is(err, store.ErrNotFound) {
		return DashProduct{}, productNotFound()
	}
	if err != nil {
		return DashProduct{}, err
	}
	list, err := s.renderProducts(ctx, a, []store.Product{p})
	if err != nil {
		return DashProduct{}, err
	}
	return list[0], nil
}

// DashProduct returns one of the merchant's products with the extra detail the dashboard shows, or
// a not-found error.
func (s *Service) DashProduct(ctx context.Context, a Auth, id string) (DashProduct, error) {
	return s.dashProduct(ctx, a, id)
}

type productCreated struct {
	ID string `json:"id"`
}

// DashCreateProduct creates a product at most once per Idempotency-Key (scoped to the merchant
// and mode, apart from API keys' namespace). A replay answers with the product as it is now.
func (s *Service) DashCreateProduct(ctx context.Context, a Auth, idemKey string, rawBody []byte, in ProductInput) (DashProduct, bool, error) {
	if err := checkIdempotencyKey(idemKey); err != nil {
		return DashProduct{}, false, err
	}
	if err := checkProductCreate(in); err != nil {
		return DashProduct{}, false, err
	}
	if in.Active.Given() {
		return DashProduct{}, false, at(invalid("a new product is always active; deactivate it afterwards"), "active")
	}
	scope := store.IdempotencyScope{MerchantID: a.MerchantID, Livemode: a.Livemode, Key: "dashboard_product/" + idemKey}
	req := idemRequest{scope: scope, route: "POST /v1/dashboard/products", body: rawBody}
	created, _, replayed, err := idempotent(ctx, s.store, req, func(ctx context.Context, tx pgx.Tx) (productCreated, int, error) {
		b := s.bind(tx)
		p := newProduct(a)
		if err := b.applyProductInput(ctx, a, &p, in, true); err != nil {
			return productCreated{}, 0, err
		}
		if err := b.store.CreateProductTx(ctx, tx, p, a.actor()); err != nil {
			return productCreated{}, 0, storePayoutError(err)
		}
		return productCreated{ID: p.ID}, statusOK, nil
	})
	if err != nil {
		return DashProduct{}, false, err
	}
	if created.ID == "" {
		return DashProduct{}, false, errors.New("stored product response is malformed")
	}
	p, err := s.dashProduct(ctx, a, created.ID)
	return p, replayed, err
}

// DashUpdateProduct edits a product. Price, payout and default changes reach only sessions
// opened afterwards, from every link of the product (a money change with active links says
// whether they stay active); deactivating it stops all its links. Sessions already open are never
// touched.
func (s *Service) DashUpdateProduct(ctx context.Context, a Auth, id string, in ProductInput) (DashProduct, error) {
	if _, err := s.updateProduct(ctx, a, id, in); err != nil {
		return DashProduct{}, err
	}
	return s.dashProduct(ctx, a, id)
}

// ProductFilter narrows the dashboard product list.
type ProductFilter struct {
	// Status is active, inactive or all.
	Status string
	// Query matches the name (case-insensitive substring) or the exact id.
	Query  string
	Cursor string
	Limit  int
}

// DashProductList is one page of dashboard products and the cursor of the next page, if any.
type DashProductList struct {
	Data       []DashProduct `json:"data"`
	NextCursor *string       `json:"next_cursor"`
}

// productCursor is the sort key of the last product of a page: active first, then by last
// payment (never paid last), then newest.
type productCursor struct {
	Active   int    `json:"a"`
	LastPaid int64  `json:"p"`
	ID       string `json:"i"`
}

func productCursorOf(p store.Product) productCursor {
	out := productCursor{LastPaid: -1, ID: p.ID}
	if p.Active {
		out.Active = 1
	}
	if p.LastPaidAt != nil {
		out.LastPaid = p.LastPaidAt.UnixMicro()
	}
	return out
}

func (a productCursor) lastPaidAt() *time.Time {
	if a.LastPaid < 0 {
		return nil
	}
	t := time.UnixMicro(a.LastPaid)
	return &t
}

// DashListProducts returns one page of the merchant's products matching the filter's query and
// status.
func (s *Service) DashListProducts(ctx context.Context, a Auth, f ProductFilter) (DashProductList, error) {
	q := store.ProductQuery{Query: f.Query, Limit: f.Limit}
	switch f.Status {
	case "", "all":
	case "active", "inactive":
		q.Status = f.Status
	default:
		return DashProductList{}, at(invalid("status must be active, inactive or all"), "status")
	}
	if f.Cursor != "" {
		var c productCursor
		if err := decodeCursor(f.Cursor, &c); err != nil || c.ID == "" || c.Active < 0 || c.Active > 1 ||
			!validMicro(c.LastPaid, -1) {
			return DashProductList{}, at(invalid("cursor is not valid"), "cursor")
		}
		q.After = &store.ProductKey{Active: c.Active == 1, LastPaidAt: c.lastPaidAt(), ID: c.ID}
	}
	page, more, err := s.store.PageProducts(ctx, a.MerchantID, a.Livemode, q)
	if err != nil {
		return DashProductList{}, err
	}
	rendered, err := s.renderProducts(ctx, a, page)
	if err != nil {
		return DashProductList{}, err
	}
	out := DashProductList{Data: rendered}
	if more && len(page) > 0 {
		next := encodeCursor(productCursorOf(page[len(page)-1]))
		out.NextCursor = &next
	}
	return out, nil
}

// DashProductSessions lists every session of one product, from its links and the API.
func (s *Service) DashProductSessions(ctx context.Context, a Auth, productID, cursor string, limit int) (SessionList, error) {
	if _, err := s.store.GetProduct(ctx, a.MerchantID, a.Livemode, productID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return SessionList{}, productNotFound()
		}
		return SessionList{}, err
	}
	return s.DashListSessions(ctx, a, store.SessionFilter{ProductID: productID}, cursor, limit)
}
