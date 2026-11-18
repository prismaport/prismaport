package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prismaport/prismaport/internal/money"
)

// A session holds one of its link's max_payments slots from the moment it opens until it
// expires unpaid: open and paid sessions both count, so two payers can never both pay the last
// slot. Expired sessions release theirs; a late payment on one takes a slot back.
const (
	heldStatuses = `('open', 'partially_paid', 'paid', 'overpaid', 'under_review', 'late_payment')`
	paidStatuses = `('paid', 'overpaid')`
)

// PaymentLink is a public URL that sells one product. Its availability (active, expiry, payment
// limit) is its own; the price and payout are the product's, except that a link may set the
// amount of an open-price product (inside the product's range when it is set; it keeps it when the
// range moves later).
type PaymentLink struct {
	ID         string
	MerchantID string
	Livemode   bool
	ProductID  string
	// Name is an internal label the payer never sees; nil means the product name is shown.
	Name *string
	// AmountCents, on an open-price product only, is the amount of every session of the link.
	AmountCents *money.Units
	Active      bool
	// ExpiresAt stops the link opening sessions after that time; nil never expires.
	ExpiresAt *time.Time
	// MaxPayments caps the paid sessions of the link; nil is unlimited.
	MaxPayments *int
	// SuccessURL overrides the product's; Metadata is merged over the product's, per key.
	SuccessURL *string
	Metadata   map[string]string
	CreatedAt  time.Time
	UpdatedAt  time.Time
	// LastPaidAt is the paid time of the link's latest paid or overpaid session; nil when none is.
	// The session writer keeps it (keepLastPaid); only the dashboard's order reads it.
	LastPaidAt *time.Time
}

const linkColumns = `id, merchant_id, livemode, product_id, name, amount_cents::text, active, expires_at, max_payments,
	success_url, metadata, created_at, updated_at, last_paid_at`

func scanLink(row pgx.Row) (PaymentLink, error) {
	var l PaymentLink
	var amount *string
	if err := row.Scan(&l.ID, &l.MerchantID, &l.Livemode, &l.ProductID, &l.Name, &amount, &l.Active, &l.ExpiresAt, &l.MaxPayments,
		&l.SuccessURL, &l.Metadata, &l.CreatedAt, &l.UpdatedAt, &l.LastPaidAt); err != nil {
		return l, notFound(err)
	}
	var err error
	l.AmountCents, err = parseOptUnits("payment_links.amount_cents", amount)
	return l, err
}

func collectLinks(rows pgx.Rows, err error) ([]PaymentLink, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PaymentLink
	for rows.Next() {
		l, err := scanLink(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// LockProductShared locks a product of the merchant's mode against changes for the rest of tx
// and returns it. Link writes take it before the link row, so a link write never passes a product
// edit (a link amount is checked against the range as it stands) and never deadlocks with one.
func (s *Store) LockProductShared(ctx context.Context, tx pgx.Tx, merchantID string, livemode bool, id string) (Product, error) {
	var locked int
	if err := tx.QueryRow(ctx, `SELECT 1 FROM products WHERE id = $1 AND merchant_id = $2 AND livemode = $3 FOR SHARE`,
		id, merchantID, livemode).Scan(&locked); err != nil {
		return Product{}, opErr("lock product shared", notFound(err))
	}
	// Read after the lock, in a new statement: see LockProductForSession.
	out, err := scanProduct(tx.QueryRow(ctx, `SELECT `+productColumns+` FROM products WHERE id = $1`, id))
	return out, opErr("lock product shared", err)
}

// CreateLinkTx inserts a link inside tx and audits it as actor. The caller holds
// LockProductShared on its product.
func (s *Store) CreateLinkTx(ctx context.Context, tx pgx.Tx, l PaymentLink, actor string) error {
	now := s.now()
	if _, err := tx.Exec(ctx, `INSERT INTO payment_links (id, merchant_id, livemode, product_id, name, amount_cents, active,
		expires_at, max_payments, success_url, metadata, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6::numeric, $7, $8, $9, $10, $11, $12, $12)`,
		l.ID, l.MerchantID, l.Livemode, l.ProductID, l.Name, optUnitsArg(l.AmountCents), l.Active, l.ExpiresAt, l.MaxPayments,
		l.SuccessURL, metadataArg(l.Metadata), now); err != nil {
		return opErr("create link tx", err)
	}
	return opErr("create link tx", audit(ctx, tx, l.MerchantID, actor, "payment_link.created", map[string]any{"link_id": l.ID, "product_id": l.ProductID,
		"livemode": l.Livemode}))
}

// GetLink returns a merchant's payment link by id in the given mode, or ErrNotFound when none matches.
func (s *Store) GetLink(ctx context.Context, merchantID string, livemode bool, id string) (PaymentLink, error) {
	out, err := scanLink(s.db.QueryRow(ctx, `SELECT `+linkColumns+` FROM payment_links WHERE id = $1 AND merchant_id = $2 AND livemode = $3`,
		id, merchantID, livemode))
	return out, opErr("get link", err)
}

// LoadLink reads a link by id alone, across merchants and modes. Only the public checkout uses
// it: the link id is the whole scope of a payment link page.
func (s *Store) LoadLink(ctx context.Context, id string) (PaymentLink, error) {
	out, err := scanLink(s.db.QueryRow(ctx, `SELECT `+linkColumns+` FROM payment_links WHERE id = $1`, id))
	return out, opErr("load link", err)
}

// AllLinks returns every link of a merchant's mode, newest first; a non-empty productID keeps
// that product's only.
func (s *Store) AllLinks(ctx context.Context, merchantID string, livemode bool, productID string) ([]PaymentLink, error) {
	out, err := collectLinks(s.db.Query(ctx, `SELECT `+linkColumns+` FROM payment_links
		WHERE merchant_id = $1 AND livemode = $2 AND ($3 = '' OR product_id = $3) ORDER BY id DESC`, merchantID, livemode, productID))
	return out, opErr("all links", err)
}

// LinksOfProducts returns the links of the given products, newest first.
func (s *Store) LinksOfProducts(ctx context.Context, productIDs []string) ([]PaymentLink, error) {
	out, err := collectLinks(s.db.Query(ctx, `SELECT `+linkColumns+` FROM payment_links WHERE product_id = ANY($1) ORDER BY id DESC`, productIDs))
	return out, opErr("links of products", err)
}

// linkProductID reads the product of a link of the merchant's mode without locking anything; a
// link's product never changes, so it is safe to lock the product by it before the link.
func linkProductID(ctx context.Context, tx pgx.Tx, merchantID string, livemode bool, id string) (string, error) {
	var productID string
	err := tx.QueryRow(ctx, `SELECT product_id FROM payment_links WHERE id = $1 AND merchant_id = $2 AND livemode = $3`,
		id, merchantID, livemode).Scan(&productID)
	return productID, notFound(err)
}

// UpdateLinkAs locks the link's product (shared) and then the link, lets fn modify the link,
// writes it back and audits the change as actor. fn returning an error aborts the update. The
// product goes first because a product edit locks it and then deactivates its links.
func (s *Store) UpdateLinkAs(ctx context.Context, merchantID string, livemode bool, id, actor string, fn func(*PaymentLink, Product) error) (PaymentLink, error) {
	var out PaymentLink
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		productID, err := linkProductID(ctx, tx, merchantID, livemode, id)
		if err != nil {
			return err
		}
		p, err := s.LockProductShared(ctx, tx, merchantID, livemode, productID)
		if err != nil {
			return err
		}
		l, err := scanLink(tx.QueryRow(ctx, `SELECT `+linkColumns+` FROM payment_links
			WHERE id = $1 AND merchant_id = $2 AND livemode = $3 FOR UPDATE`, id, merchantID, livemode))
		if err != nil {
			return err
		}
		if err := fn(&l, p); err != nil {
			return err
		}
		l.UpdatedAt = s.now()
		if _, err := tx.Exec(ctx, `UPDATE payment_links SET name = $2, amount_cents = $3::numeric, active = $4, expires_at = $5,
			max_payments = $6, success_url = $7, metadata = $8, updated_at = $9 WHERE id = $1`,
			l.ID, l.Name, optUnitsArg(l.AmountCents), l.Active, l.ExpiresAt, l.MaxPayments, l.SuccessURL, metadataArg(l.Metadata),
			l.UpdatedAt); err != nil {
			return err
		}
		out = l
		return audit(ctx, tx, merchantID, actor, "payment_link.updated", map[string]any{"link_id": id, "product_id": l.ProductID,
			"livemode": livemode, "active": l.Active})
	})
	return out, opErr("update link as", err)
}

// AssetAmount is a sum of base units of one asset (symbol and decimals).
type AssetAmount struct {
	Asset    string
	Decimals uint8
	Amount   money.Units
}

// LinkCounts are the numbers of a payment link or a product, computed from its sessions and
// payments on every read, so they cannot drift from them.
type LinkCounts struct {
	// Opened is every session; Paid those paid in full (or overpaid); Held those holding a
	// max_payments slot; Open those still waiting for (more) money.
	Opened, Paid, Held, Open int64
	LastPaidAt               *time.Time
	// Received sums the counted (confirmed or finalized) payments of every session, late and
	// partial ones included: that money reached the merchant.
	Received []AssetAmount
}

// LinkCounts returns the counts of each link id, from the sessions opened from it; links
// without sessions get zero counts.
func (s *Store) LinkCounts(ctx context.Context, linkIDs []string) (map[string]LinkCounts, error) {
	out, err := sessionCounts(ctx, s.db, "link_id", linkIDs)
	return out, opErr("link counts", err)
}

// ProductCounts returns the counts of each product id over every session of the product, opened
// from any of its links or through the API.
func (s *Store) ProductCounts(ctx context.Context, productIDs []string) (map[string]LinkCounts, error) {
	out, err := sessionCounts(ctx, s.db, "product_id", productIDs)
	return out, opErr("product counts", err)
}

// sessionCounts groups sessions by col, which is link_id, product_id or customer_address_id (never
// input).
func sessionCounts(ctx context.Context, q querier, col string, keys []string) (map[string]LinkCounts, error) {
	out := make(map[string]LinkCounts, len(keys))
	for _, id := range keys {
		out[id] = LinkCounts{}
	}
	if len(keys) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `SELECT `+col+`, count(*),
			count(*) FILTER (WHERE status IN `+paidStatuses+`),
			count(*) FILTER (WHERE status IN `+heldStatuses+`),
			count(*) FILTER (WHERE status IN ('open', 'partially_paid')),
			max(paid_at) FILTER (WHERE status IN `+paidStatuses+`)
		FROM sessions WHERE `+col+` = ANY($1) GROUP BY `+col, keys)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var c LinkCounts
		if err := rows.Scan(&id, &c.Opened, &c.Paid, &c.Held, &c.Open, &c.LastPaidAt); err != nil {
			return nil, err
		}
		out[id] = c
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = q.Query(ctx, `SELECT s.`+col+`, p.asset, p.decimals, sum(p.amount)::text
		FROM payments p JOIN sessions s ON s.id = p.session_id
		WHERE s.`+col+` = ANY($1) AND p.status IN ('confirmed', 'finalized')
		GROUP BY s.`+col+`, p.asset, p.decimals ORDER BY s.`+col+`, p.asset, p.decimals`, keys)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, asset, sum string
		var dec int16
		if err := rows.Scan(&id, &asset, &dec, &sum); err != nil {
			return nil, err
		}
		u, err := parseUnits("payments.amount sum", sum)
		if err != nil {
			return nil, err
		}
		c := out[id]
		c.Received = append(c.Received, AssetAmount{Asset: asset, Decimals: uint8(dec), Amount: u})
		out[id] = c
	}
	return out, rows.Err()
}

// LockLink locks a link row for the rest of tx and returns it with its product and its counts.
// Every session opened from a link checks the link under this lock, so concurrent opens of one
// link serialize and each sees the sessions the previous ones committed.
//
// The product is locked first, FOR KEY SHARE: the session insert takes that lock anyway (its
// foreign key), and taking it after the link would deadlock with a product edit that holds the
// product and deactivates the link. It never blocks other opens or link edits.
func (s *Store) LockLink(ctx context.Context, tx pgx.Tx, merchantID string, livemode bool, id string) (PaymentLink, Product, LinkCounts, error) {
	productID, err := linkProductID(ctx, tx, merchantID, livemode, id)
	if err != nil {
		return PaymentLink{}, Product{}, LinkCounts{}, opErr("lock link", err)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM products WHERE id = $1 FOR KEY SHARE`, productID); err != nil {
		return PaymentLink{}, Product{}, LinkCounts{}, opErr("lock link", err)
	}
	// Read after the lock, in a new statement: see LockProductForSession.
	p, err := scanProduct(tx.QueryRow(ctx, `SELECT `+productColumns+` FROM products WHERE id = $1`, productID))
	if err != nil {
		return PaymentLink{}, Product{}, LinkCounts{}, opErr("lock link", err)
	}
	l, err := scanLink(tx.QueryRow(ctx, `SELECT `+linkColumns+` FROM payment_links
		WHERE id = $1 AND merchant_id = $2 AND livemode = $3 FOR UPDATE`, id, merchantID, livemode))
	if err != nil {
		return PaymentLink{}, Product{}, LinkCounts{}, opErr("lock link", err)
	}
	counts, err := sessionCounts(ctx, tx, "link_id", []string{id})
	if err != nil {
		return PaymentLink{}, Product{}, LinkCounts{}, opErr("lock link", err)
	}
	return l, p, counts[id], nil
}
