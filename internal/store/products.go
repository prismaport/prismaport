package store

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/money"
)

// PriceType says whether a product has a fixed price or lets the payer choose the amount.
type PriceType string

// Product price types.
const (
	PriceFixed PriceType = "fixed"
	PriceOpen  PriceType = "open"
)

// Product is a catalog item: what is sold, how it is priced and where the money goes. Payment
// links sell it; it has no URL or availability of its own.
type Product struct {
	ID         string
	MerchantID string
	Livemode   bool
	Name       string
	// Description is shown on the checkout under the name; nil has none.
	Description    *string
	PriceType      PriceType
	AmountCents    *money.Units
	MinAmountCents *money.Units
	MaxAmountCents *money.Units
	SuggestedCents []money.Units
	SuccessURL     *string
	Metadata       map[string]string
	Active         bool
	// Payout is where the product's money goes, per network (registry chain id of its mode), in
	// payout order: 1 to 5 items of the chain's family adding up to 10000 bps. A chain without
	// items (or absent) is a network the product does not take. Every link of the product pays it.
	Payout map[string][]PayoutItem
	// Networks is the stablecoin choice of every network the product takes (D60), keyed like
	// Payout and always with the same keys: a network it does not take has neither. What a
	// checkout offers is computed from it, never stored (service.effectiveMethods).
	Networks  map[string]NetworkChoice
	CreatedAt time.Time
	UpdatedAt time.Time
	// LastPaidAt is the paid time of the product's latest paid or overpaid session; nil when none
	// is. The session writer keeps it (keepLastPaid); only the dashboard's order reads it.
	LastPaidAt *time.Time
}

// Accept is how a product takes the stablecoins of one network (D60).
type Accept string

const (
	// AcceptAll takes every asset the merchant accepts on the network, turned on later included.
	AcceptAll Accept = "all"
	// AcceptSome takes only the listed assets; one the merchant turns off is kept but not offered.
	AcceptSome Accept = "some"
	// AcceptNone is a network the product does not take: never stored (no row, no payout).
	AcceptNone Accept = "none"
)

// NetworkChoice is a product's choice on one network it takes. Assets lists the chosen symbols for
// AcceptSome (1 to 4) and is nil for AcceptAll.
type NetworkChoice struct {
	Accept Accept
	Assets []string
}

// Takes reports whether the choice lets the product offer asset, before the merchant's own
// accepted assets narrow it.
func (n NetworkChoice) Takes(asset string) bool {
	return n.Accept != AcceptSome || slices.Contains(n.Assets, asset)
}

const productColumns = `id, merchant_id, livemode, name, description, price_type, amount_cents::text, min_amount_cents::text,
	max_amount_cents::text, suggested_amounts_cents::text[], success_url, metadata, active, created_at, updated_at,
	last_paid_at, (SELECT json_agg(json_build_object('c', i.chain_id, 'f', i.family, 'w', i.wallet_id, 'b', i.bps, 'l', w.label, 'a', w.address)
			ORDER BY i.chain_id, i.position)
		FROM product_payout_items i JOIN wallets w ON w.id = i.wallet_id WHERE i.product_id = products.id),
	(SELECT json_agg(json_build_object('c', n.chain_id, 'a', n.accept, 's', n.assets) ORDER BY n.chain_id)
		FROM product_networks n WHERE n.product_id = products.id)`

func scanProduct(row pgx.Row) (Product, error) {
	var p Product
	var pt string
	var amount, minA, maxA *string
	var suggested []string
	var payoutJSON, networksJSON []byte
	if err := row.Scan(&p.ID, &p.MerchantID, &p.Livemode, &p.Name, &p.Description, &pt, &amount, &minA, &maxA, &suggested,
		&p.SuccessURL, &p.Metadata, &p.Active, &p.CreatedAt, &p.UpdatedAt, &p.LastPaidAt, &payoutJSON, &networksJSON); err != nil {
		return p, notFound(err)
	}
	if payoutJSON != nil {
		var rows []struct {
			C string `json:"c"`
			F string `json:"f"`
			W string `json:"w"`
			B uint16 `json:"b"`
			L string `json:"l"`
			A string `json:"a"`
		}
		if err := json.Unmarshal(payoutJSON, &rows); err != nil {
			return p, fmt.Errorf("store: product %s payout: %w", p.ID, err)
		}
		p.Payout = map[string][]PayoutItem{}
		for _, r := range rows {
			p.Payout[r.C] = append(p.Payout[r.C], PayoutItem{WalletID: r.W, Bps: r.B, Family: chain.Family(r.F), Label: r.L, Address: r.A})
		}
	}
	if err := scanNetworks(&p, networksJSON); err != nil {
		return p, err
	}
	p.PriceType = PriceType(pt)
	var err error
	if p.AmountCents, err = parseOptUnits("amount_cents", amount); err != nil {
		return p, err
	}
	if p.MinAmountCents, err = parseOptUnits("min_amount_cents", minA); err != nil {
		return p, err
	}
	if p.MaxAmountCents, err = parseOptUnits("max_amount_cents", maxA); err != nil {
		return p, err
	}
	for _, s := range suggested {
		u, err := parseUnits("suggested_amounts_cents", s)
		if err != nil {
			return p, err
		}
		p.SuggestedCents = append(p.SuggestedCents, u)
	}
	return p, nil
}

// scanNetworks reads the product's network rows into Networks, keyed like Payout: a row without
// payout items (left by a binary older than D60) is a network the product does not take, and a
// payout without a row takes its network as all.
func scanNetworks(p *Product, raw []byte) error {
	var rows []struct {
		C string   `json:"c"`
		A string   `json:"a"`
		S []string `json:"s"`
	}
	if raw != nil {
		if err := json.Unmarshal(raw, &rows); err != nil {
			return fmt.Errorf("store: product %s networks: %w", p.ID, err)
		}
	}
	stored := map[string]NetworkChoice{}
	for _, r := range rows {
		stored[r.C] = NetworkChoice{Accept: Accept(r.A), Assets: r.S}
	}
	p.Networks = map[string]NetworkChoice{}
	for id := range p.Payout {
		n, ok := stored[id]
		if !ok {
			n = NetworkChoice{Accept: AcceptAll}
		}
		p.Networks[id] = n
	}
	return nil
}

func collectProducts(rows pgx.Rows, err error) ([]Product, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Product
	for rows.Next() {
		p, err := scanProduct(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// CreateProduct inserts a product and, with a non-empty actor, audits it.
func (s *Store) CreateProduct(ctx context.Context, p Product, actor string) (Product, error) {
	if err := s.inTx(ctx, func(tx pgx.Tx) error { return s.CreateProductTx(ctx, tx, p, actor) }); err != nil {
		return Product{}, opErr("create product", err)
	}
	return s.GetProduct(ctx, p.MerchantID, p.Livemode, p.ID)
}

// CreateProductTx inserts a product inside tx (idempotent creates) and, with a non-empty actor,
// audits it.
func (s *Store) CreateProductTx(ctx context.Context, tx pgx.Tx, p Product, actor string) error {
	if err := s.insertProduct(ctx, tx, &p); err != nil {
		return opErr("create product tx", err)
	}
	if actor == "" {
		return nil
	}
	return opErr("create product tx", audit(ctx, tx, p.MerchantID, actor, "product.created", map[string]any{"product_id": p.ID, "livemode": p.Livemode,
		"payout": PayoutAudit(p.Payout), "networks": NetworksAudit(p.Networks)}))
}

// writePayout replaces a product's networks and payout items under the merchant lock (so a
// wallet cannot be archived meanwhile), after checking every wallet is an active wallet of the
// product's merchant, mode and family. Each network the payout names is written first with its
// choice (all when Networks has none for it), then its items; Networks ends up keyed like Payout.
// The whole product is checked once more at commit.
func writePayout(ctx context.Context, tx pgx.Tx, p *Product) error {
	if err := lockMerchant(ctx, tx, p.MerchantID); err != nil {
		return err
	}
	if err := checkPayoutWallets(ctx, tx, p.MerchantID, p.Livemode, p.Payout); err != nil {
		return err
	}
	// Deleting the networks deletes their items too (ON DELETE CASCADE).
	if _, err := tx.Exec(ctx, `DELETE FROM product_networks WHERE product_id = $1`, p.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM product_payout_items WHERE product_id = $1`, p.ID); err != nil {
		return err
	}
	networks := map[string]NetworkChoice{}
	for _, id := range PayoutChains(p.Payout) {
		n, ok := p.Networks[id]
		if !ok || n.Accept != AcceptSome {
			n = NetworkChoice{Accept: AcceptAll}
		}
		networks[id] = n
		if _, err := tx.Exec(ctx, `INSERT INTO product_networks (product_id, merchant_id, livemode, chain_id, accept, assets)
			VALUES ($1, $2, $3, $4, $5, $6)`, p.ID, p.MerchantID, p.Livemode, id, string(n.Accept), n.Assets); err != nil {
			return err
		}
		for i, it := range p.Payout[id] {
			if _, err := tx.Exec(ctx, `INSERT INTO product_payout_items (product_id, merchant_id, livemode, chain_id, family, position, wallet_id, bps)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, p.ID, p.MerchantID, p.Livemode, id, string(it.Family), i, it.WalletID, int32(it.Bps)); err != nil {
				if isForeignKeyViolation(err) {
					return fmt.Errorf("%w: %s", ErrWalletUnavailable, it.WalletID)
				}
				return err
			}
		}
	}
	p.Networks = networks
	return nil
}

// PayoutChains lists the chains a payout names, sorted.
func PayoutChains(payout map[string][]PayoutItem) []string {
	out := make([]string, 0, len(payout))
	for id := range payout {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// PayoutAudit renders a payout for the audit log: per chain the wallet ids, addresses and bps.
func PayoutAudit(payout map[string][]PayoutItem) map[string]any {
	out := map[string]any{}
	for id, list := range payout {
		items := []map[string]any{}
		for _, it := range list {
			items = append(items, map[string]any{"wallet_id": it.WalletID, "address": it.Address, "bps": it.Bps})
		}
		out[id] = items
	}
	return out
}

// NetworksAudit renders a product's network choices for the audit log: per chain the accept mode
// and, for some, the assets.
func NetworksAudit(networks map[string]NetworkChoice) map[string]any {
	out := map[string]any{}
	for id, n := range networks {
		v := map[string]any{"accept": string(n.Accept)}
		if n.Accept == AcceptSome {
			v["assets"] = n.Assets
		}
		out[id] = v
	}
	return out
}

// CloneNetworks copies network choices so a change to the copy never reaches the original.
func CloneNetworks(networks map[string]NetworkChoice) map[string]NetworkChoice {
	out := make(map[string]NetworkChoice, len(networks))
	for id, n := range networks {
		out[id] = NetworkChoice{Accept: n.Accept, Assets: slices.Clone(n.Assets)}
	}
	return out
}

// SameNetworks reports whether two products take the same networks with the same choices.
func SameNetworks(a, b map[string]NetworkChoice) bool {
	if len(a) != len(b) {
		return false
	}
	for id, x := range a {
		y, ok := b[id]
		if !ok || x.Accept != y.Accept || !slices.Equal(x.Assets, y.Assets) {
			return false
		}
	}
	return true
}

// ClonePayout copies a payout so a change to the copy never reaches the original.
func ClonePayout(payout map[string][]PayoutItem) map[string][]PayoutItem {
	out := map[string][]PayoutItem{}
	for id, items := range payout {
		out[id] = append([]PayoutItem(nil), items...)
	}
	return out
}

// SamePayout reports whether two payouts pay the same wallets the same shares in the same order
// on every chain.
func SamePayout(a, b map[string][]PayoutItem) bool {
	for _, m := range []map[string][]PayoutItem{a, b} {
		for id := range m {
			x, y := a[id], b[id]
			if len(x) != len(y) {
				return false
			}
			for i := range x {
				if x[i].WalletID != y[i].WalletID || x[i].Bps != y[i].Bps {
					return false
				}
			}
		}
	}
	return true
}

func (s *Store) insertProduct(ctx context.Context, tx pgx.Tx, p *Product) error {
	now := s.now()
	p.CreatedAt, p.UpdatedAt = now, now
	_, err := tx.Exec(ctx, `INSERT INTO products (id, merchant_id, livemode, name, description, price_type, amount_cents,
		min_amount_cents, max_amount_cents, suggested_amounts_cents, success_url, metadata, active, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7::numeric, $8::numeric, $9::numeric, $10::text[]::base_units[], $11, $12, $13, $14, $14)`,
		p.ID, p.MerchantID, p.Livemode, p.Name, p.Description, string(p.PriceType), optUnitsArg(p.AmountCents),
		optUnitsArg(p.MinAmountCents), optUnitsArg(p.MaxAmountCents), unitsArrayArg(p.SuggestedCents), p.SuccessURL,
		metadataArg(p.Metadata), p.Active, now)
	if err != nil {
		return err
	}
	return writePayout(ctx, tx, p)
}

// GetProduct returns a merchant's product by id in the given mode, or ErrNotFound when none matches.
func (s *Store) GetProduct(ctx context.Context, merchantID string, livemode bool, id string) (Product, error) {
	out, err := scanProduct(s.db.QueryRow(ctx, `SELECT `+productColumns+` FROM products
		WHERE id = $1 AND merchant_id = $2 AND livemode = $3`, id, merchantID, livemode))
	return out, opErr("get product", err)
}

// LockProductForSession reads a product for a session about to reference it, FOR KEY SHARE: the
// lock the session insert's foreign key takes anyway, taken before anything else so the session
// follows the product-first lock order and sees the product (and its payout) as of any edit that
// committed before it. The product is read by a second statement: a row re-read after waiting
// for a lock keeps the first statement's snapshot for the payout items it joins.
func (s *Store) LockProductForSession(ctx context.Context, merchantID string, livemode bool, id string) (Product, error) {
	var locked string
	if err := s.db.QueryRow(ctx, `SELECT id FROM products WHERE id = $1 AND merchant_id = $2 AND livemode = $3 FOR KEY SHARE`,
		id, merchantID, livemode).Scan(&locked); err != nil {
		return Product{}, opErr("lock product for session", notFound(err))
	}
	return s.GetProduct(ctx, merchantID, livemode, id)
}

// ListProducts returns products newest first. startingAfter is the last id of the previous page.
func (s *Store) ListProducts(ctx context.Context, merchantID string, livemode bool, limit int, startingAfter string) ([]Product, bool, error) {
	out, err := collectProducts(s.db.Query(ctx, `SELECT `+productColumns+` FROM products
		WHERE merchant_id = $1 AND livemode = $2 AND ($3 = '' OR id < $3)
		ORDER BY id DESC LIMIT $4`, merchantID, livemode, startingAfter, limit+1))
	if err != nil {
		return nil, false, opErr("list products", err)
	}
	more := len(out) > limit
	if more {
		out = out[:limit]
	}
	return out, more, nil
}

// ProductsByID returns the products of a merchant's mode with the given ids, by id; ids of
// other merchants or modes are left out.
func (s *Store) ProductsByID(ctx context.Context, merchantID string, livemode bool, ids []string) (map[string]Product, error) {
	list, err := collectProducts(s.db.Query(ctx, `SELECT `+productColumns+` FROM products
		WHERE merchant_id = $1 AND livemode = $2 AND id = ANY($3)`, merchantID, livemode, ids))
	if err != nil {
		return nil, opErr("products by id", err)
	}
	out := make(map[string]Product, len(list))
	for _, p := range list {
		out[p.ID] = p
	}
	return out, nil
}

// UpdateProduct locks the product, lets fn modify it, and writes it back. fn returning an error
// aborts the update. A non-empty actor also writes an audit entry.
func (s *Store) UpdateProduct(ctx context.Context, merchantID string, livemode bool, id string, fn func(*Product) error) (Product, error) {
	return s.UpdateProductAs(ctx, merchantID, livemode, id, "", fn)
}

// UpdateProductAs is UpdateProduct audited as actor; it never touches the product's links.
func (s *Store) UpdateProductAs(ctx context.Context, merchantID string, livemode bool, id, actor string, fn func(*Product) error) (Product, error) {
	return s.EditProductAs(ctx, merchantID, livemode, id, actor, func(p *Product, _ []PaymentLink) (bool, error) { return false, fn(p) })
}

// EditProductAs locks the product, lets fn modify it knowing its active links, writes it back
// and, when fn answers true, deactivates every one of those links in the same transaction (each
// audited as payment_link.updated with reason product_edit). fn returning an error aborts the
// whole edit; so does any later failure, links included.
//
// Lock order is the product, then its links. Every other writer of a link row (link create and
// edit, a session opened from a link) locks the product first, so while this holds the product
// FOR UPDATE nobody holds one of its link rows: deactivating them cannot deadlock, and the active
// links read here cannot change before commit.
func (s *Store) EditProductAs(ctx context.Context, merchantID string, livemode bool, id, actor string,
	fn func(p *Product, activeLinks []PaymentLink) (deactivate bool, err error)) (Product, error) {
	var out Product
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var locked int
		if err := tx.QueryRow(ctx, `SELECT 1 FROM products WHERE id = $1 AND merchant_id = $2 AND livemode = $3 FOR UPDATE`,
			id, merchantID, livemode).Scan(&locked); err != nil {
			return notFound(err)
		}
		// Read after the lock, in a new statement: see LockProductForSession.
		p, err := scanProduct(tx.QueryRow(ctx, `SELECT `+productColumns+` FROM products WHERE id = $1`, id))
		if err != nil {
			return err
		}
		active, err := collectLinks(tx.Query(ctx, `SELECT `+linkColumns+` FROM payment_links WHERE product_id = $1 AND active
			ORDER BY id`, p.ID))
		if err != nil {
			return err
		}
		previous := ClonePayout(p.Payout)
		previousNetworks := CloneNetworks(p.Networks)
		deactivate, err := fn(&p, active)
		if err != nil {
			return err
		}
		p.UpdatedAt = s.now()
		_, err = tx.Exec(ctx, `UPDATE products SET name = $2, description = $3, amount_cents = $4::numeric, min_amount_cents = $5::numeric,
			max_amount_cents = $6::numeric, suggested_amounts_cents = $7::text[]::base_units[], success_url = $8,
			metadata = $9, active = $10, updated_at = $11 WHERE id = $1`,
			p.ID, p.Name, p.Description, optUnitsArg(p.AmountCents), optUnitsArg(p.MinAmountCents), optUnitsArg(p.MaxAmountCents),
			unitsArrayArg(p.SuggestedCents), p.SuccessURL, metadataArg(p.Metadata), p.Active, p.UpdatedAt)
		if err != nil {
			return err
		}
		if deactivate && len(active) > 0 {
			if err := deactivateLinks(ctx, tx, actor, p, active); err != nil {
				return err
			}
		}
		changed := !SamePayout(previous, p.Payout)
		if changed || !SameNetworks(previousNetworks, p.Networks) {
			if err := writePayout(ctx, tx, &p); err != nil {
				return err
			}
		}
		// A network dropped or added as all is told by product.payout_changed; only a choice that
		// changed (or a network added as some) gets its own entry.
		networksChanged := false
		for id, n := range p.Networks {
			old, ok := previousNetworks[id]
			if !ok {
				old = NetworkChoice{Accept: AcceptAll}
			}
			if n.Accept != old.Accept || !slices.Equal(n.Assets, old.Assets) {
				networksChanged = true
			}
		}
		out = p
		if actor == "" {
			return nil
		}
		if changed {
			if err := audit(ctx, tx, merchantID, actor, "product.payout_changed", map[string]any{"product_id": id, "livemode": livemode,
				"previous": PayoutAudit(previous), "payout": PayoutAudit(p.Payout)}); err != nil {
				return err
			}
		}
		if networksChanged {
			if err := audit(ctx, tx, merchantID, actor, "product.networks_changed", map[string]any{"product_id": id, "livemode": livemode,
				"previous": NetworksAudit(previousNetworks), "networks": NetworksAudit(p.Networks)}); err != nil {
				return err
			}
		}
		return audit(ctx, tx, merchantID, actor, "product.updated", map[string]any{"product_id": id, "livemode": livemode, "active": p.Active})
	})
	return out, opErr("edit product as", err)
}

// deactivateLinks turns off links of p inside tx, which holds p's row lock, and audits each one
// as actor (when there is one).
func deactivateLinks(ctx context.Context, tx pgx.Tx, actor string, p Product, links []PaymentLink) error {
	idList := make([]string, len(links))
	for i, l := range links {
		idList[i] = l.ID
	}
	if _, err := tx.Exec(ctx, `UPDATE payment_links SET active = false, updated_at = $3 WHERE product_id = $1 AND id = ANY($2)`,
		p.ID, idList, p.UpdatedAt); err != nil {
		return err
	}
	if actor == "" {
		return nil
	}
	for _, id := range idList {
		if err := audit(ctx, tx, p.MerchantID, actor, "payment_link.updated", map[string]any{"link_id": id, "product_id": p.ID,
			"livemode": p.Livemode, "active": false, "reason": "product_edit"}); err != nil {
			return err
		}
	}
	return nil
}

// ProductNetwork is one network an active product takes, with the product's name: what Settings
// shows next to each stablecoin (D60).
type ProductNetwork struct {
	ProductID   string
	ProductName string
	Chain       string
	Choice      NetworkChoice
}

// ActiveProductNetworks lists, for the active products of a merchant's mode, every network they
// take (one with payout items), by product id then chain.
func (s *Store) ActiveProductNetworks(ctx context.Context, merchantID string, livemode bool) ([]ProductNetwork, error) {
	rows, err := s.db.Query(ctx, `SELECT p.id, p.name, n.chain_id, n.accept, n.assets
		FROM product_networks n JOIN products p ON p.id = n.product_id
		WHERE p.merchant_id = $1 AND p.livemode = $2 AND p.active
		  AND EXISTS (SELECT 1 FROM product_payout_items i WHERE i.product_id = n.product_id AND i.chain_id = n.chain_id)
		ORDER BY p.id, n.chain_id`, merchantID, livemode)
	if err != nil {
		return nil, opErr("active product networks", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ProductNetwork, error) {
		var n ProductNetwork
		var accept string
		err := r.Scan(&n.ProductID, &n.ProductName, &n.Chain, &accept, &n.Choice.Assets)
		n.Choice.Accept = Accept(accept)
		return n, err
	})
	return out, opErr("active product networks", err)
}
