package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/ids"
)

var (
	// ErrWalletInUse refuses to archive a wallet that a product still pays. The error is a
	// *WalletInUseError naming how many products.
	ErrWalletInUse = errors.New("store: the wallet is used by a product")
	// ErrWalletLimit refuses a wallet above the active-wallet limit of its family.
	ErrWalletLimit = errors.New("store: too many active wallets in this family")
	// ErrWalletExists refuses a second active wallet with the same address.
	ErrWalletExists = errors.New("store: an active wallet already has this address")
	// ErrWalletUnavailable is a payout item naming a wallet that is unknown, archived or of
	// another family, merchant or mode.
	ErrWalletUnavailable = errors.New("store: the wallet is unknown, archived or of another network family")
)

// WalletInUseError is ErrWalletInUse with the number of products paying the wallet.
type WalletInUseError struct{ Products int }

func (e *WalletInUseError) Error() string {
	return fmt.Sprintf("store: the wallet is used by %d products", e.Products)
}

func (e *WalletInUseError) Unwrap() error { return ErrWalletInUse }

// Wallet is a merchant's payout address in one network family and mode. Its address never
// changes; a wallet that no product pays can be archived. Adding one never changes what any
// product pays; the family's default wallet is where a new product pays unless told otherwise.
type Wallet struct {
	ID         string
	MerchantID string
	Livemode   bool
	Family     chain.Family
	Label      string
	Address    string
	Verified   bool
	CreatedAt  time.Time
	ArchivedAt *time.Time
	// UsedByProducts counts the products (active or not) that pay this wallet.
	UsedByProducts int
	// Default marks the family's default wallet (D57): the one marked, or without a mark the
	// family's oldest active wallet. Only an active wallet is the default.
	Default bool
}

// PayoutItem is one recipient of a product's payout on a network, in payout order.
type PayoutItem struct {
	WalletID string
	Bps      uint16
	// Family is the network's family; the wallet must be of it.
	Family chain.Family
	// Label and Address are the wallet's, read with the product.
	Label   string
	Address string
}

const walletColumns = `w.id, w.merchant_id, w.livemode, w.family, w.label, w.address, w.verified, w.created_at, w.archived_at,
	(SELECT count(DISTINCT i.product_id) FROM product_payout_items i WHERE i.wallet_id = w.id),
	w.archived_at IS NULL AND w.id = (` + defaultWalletQuery + `)`

// defaultWalletQuery is the id of the default wallet of w's merchant, mode and family: the marked
// one, else the oldest active one. defaultOrder is its order.
const (
	defaultOrder       = `d.is_default DESC, d.created_at, d.seq`
	defaultWalletQuery = `SELECT d.id FROM wallets d WHERE d.merchant_id = w.merchant_id AND d.livemode = w.livemode
		AND d.family = w.family AND d.archived_at IS NULL ORDER BY ` + defaultOrder + ` LIMIT 1`
)

func scanWallet(r pgx.Row) (Wallet, error) {
	var w Wallet
	var fam string
	var used int64
	if err := r.Scan(&w.ID, &w.MerchantID, &w.Livemode, &fam, &w.Label, &w.Address, &w.Verified, &w.CreatedAt, &w.ArchivedAt, &used,
		&w.Default); err != nil {
		return w, notFound(err)
	}
	w.Family, w.UsedByProducts = chain.Family(fam), int(used)
	return w, nil
}

// lockMerchant serializes every wallet write and every product payout write of a merchant, so
// the active-wallet limit and archive-while-used see each other's results.
//
// FOR NO KEY UPDATE, not FOR UPDATE: every insert that references the merchant (a session, a
// product, an idempotency key, an audit entry) takes KEY SHARE on it through its foreign key, and
// a payout write takes this lock after its product. A FOR UPDATE here would wait for those inserts
// while they wait for the product, a deadlock; NO KEY UPDATE never conflicts with KEY SHARE.
func lockMerchant(ctx context.Context, tx pgx.Tx, merchantID string) error {
	var id string
	return notFound(tx.QueryRow(ctx, `SELECT id FROM merchants WHERE id = $1 FOR NO KEY UPDATE`, merchantID).Scan(&id))
}

// WalletFilter narrows ListWallets; an empty Family lists both families.
type WalletFilter struct {
	Family          chain.Family
	IncludeArchived bool
}

// ListWallets lists a merchant's wallets of one mode: active ones first, oldest first.
func (s *Store) ListWallets(ctx context.Context, merchantID string, livemode bool, f WalletFilter) ([]Wallet, error) {
	rows, err := s.db.Query(ctx, `SELECT `+walletColumns+` FROM wallets w
		WHERE w.merchant_id = $1 AND w.livemode = $2 AND ($3 = '' OR w.family = $3) AND ($4 OR w.archived_at IS NULL)
		ORDER BY w.archived_at IS NOT NULL, w.family, w.created_at, w.seq`, merchantID, livemode, string(f.Family), f.IncludeArchived)
	if err != nil {
		return nil, opErr("list wallets", err)
	}
	defer rows.Close()
	var out []Wallet
	for rows.Next() {
		w, err := scanWallet(rows)
		if err != nil {
			return nil, opErr("list wallets", err)
		}
		out = append(out, w)
	}
	return out, opErr("list wallets", rows.Err())
}

// GetWallet returns a merchant's wallet by id in the given mode, or ErrNotFound when none matches.
func (s *Store) GetWallet(ctx context.Context, merchantID string, livemode bool, id string) (Wallet, error) {
	out, err := scanWallet(s.db.QueryRow(ctx, `SELECT `+walletColumns+` FROM wallets w
		WHERE w.id = $1 AND w.merchant_id = $2 AND w.livemode = $3`, id, merchantID, livemode))
	return out, opErr("get wallet", err)
}

// DefaultWallet is the family's default wallet, where a new product pays that family by
// default: the one marked default, else the oldest active one; ErrNotFound without an active
// wallet.
func (s *Store) DefaultWallet(ctx context.Context, merchantID string, livemode bool, family chain.Family) (Wallet, error) {
	out, err := scanWallet(s.db.QueryRow(ctx, `SELECT `+walletColumns+` FROM wallets w
		WHERE w.merchant_id = $1 AND w.livemode = $2 AND w.family = $3 AND w.archived_at IS NULL
		ORDER BY w.is_default DESC, w.created_at, w.seq LIMIT 1`, merchantID, livemode, string(family)))
	return out, opErr("default wallet", err)
}

// SetDefaultWallet makes an active wallet its family's default. It changes no product's payout:
// only products created afterwards pay it by default. Making the default the default again
// changes nothing. ErrWalletUnavailable for an archived wallet.
func (s *Store) SetDefaultWallet(ctx context.Context, merchantID string, livemode bool, id, actor string) (Wallet, error) {
	var out Wallet
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if err := lockMerchant(ctx, tx, merchantID); err != nil {
			return err
		}
		w, err := scanWallet(tx.QueryRow(ctx, `SELECT `+walletColumns+` FROM wallets w
			WHERE w.id = $1 AND w.merchant_id = $2 AND w.livemode = $3`, id, merchantID, livemode))
		if err != nil {
			return err
		}
		if w.ArchivedAt != nil {
			return ErrWalletUnavailable
		}
		out = w
		if w.Default {
			return nil
		}
		// Two statements: wallets_one_default is checked row by row, so one UPDATE that sets the
		// new default before it clears the old one fails on the index.
		if _, err := tx.Exec(ctx, `UPDATE wallets SET is_default = false
			WHERE merchant_id = $1 AND livemode = $2 AND family = $3 AND is_default`,
			merchantID, livemode, string(w.Family)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE wallets SET is_default = true WHERE id = $1`, id); err != nil {
			return err
		}
		out.Default = true
		return audit(ctx, tx, merchantID, actor, "wallet.default_set", map[string]any{"wallet_id": id, "livemode": livemode,
			"family": w.Family})
	})
	return out, opErr("set default wallet", err)
}

// HasActiveWallet reports whether the merchant has an active wallet in that mode.
func (s *Store) HasActiveWallet(ctx context.Context, merchantID string, livemode bool) (bool, error) {
	var has bool
	err := s.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM wallets
		WHERE merchant_id = $1 AND livemode = $2 AND archived_at IS NULL)`, merchantID, livemode).Scan(&has)
	return has, opErr("has active wallet", err)
}

// CreateWallet adds an active wallet. At most maxActive wallets of a family may be active.
func (s *Store) CreateWallet(ctx context.Context, w Wallet, maxActive int, actor string) (Wallet, error) {
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if err := lockMerchant(ctx, tx, w.MerchantID); err != nil {
			return err
		}
		var active, same int
		if err := tx.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE address = $4) FROM wallets
			WHERE merchant_id = $1 AND livemode = $2 AND family = $3 AND archived_at IS NULL`,
			w.MerchantID, w.Livemode, string(w.Family), w.Address).Scan(&active, &same); err != nil {
			return err
		}
		if same > 0 {
			return ErrWalletExists
		}
		if active >= maxActive {
			return ErrWalletLimit
		}
		now := s.now()
		// The family's first active wallet is its default; a later one leaves the default alone.
		w.ID, w.CreatedAt, w.ArchivedAt, w.UsedByProducts, w.Default = ids.NewAt(ids.Wallet, now), now, nil, 0, active == 0
		if _, err := tx.Exec(ctx, `INSERT INTO wallets (id, merchant_id, livemode, family, label, address, verified, created_at, is_default)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`, w.ID, w.MerchantID, w.Livemode, string(w.Family), w.Label, w.Address, w.Verified,
			w.CreatedAt, w.Default); err != nil {
			return err
		}
		return audit(ctx, tx, w.MerchantID, actor, "wallet.created", map[string]any{"wallet_id": w.ID, "livemode": w.Livemode,
			"family": w.Family, "label": w.Label, "address": w.Address, "verified": w.Verified})
	})
	if err != nil {
		return Wallet{}, opErr("create wallet", err)
	}
	return w, nil
}

// ArchiveWallet retires a wallet that no product pays. Sessions opened before keep paying it.
// Archiving the default makes the family's oldest remaining active wallet the default.
func (s *Store) ArchiveWallet(ctx context.Context, merchantID string, livemode bool, id, actor string) (Wallet, error) {
	var out Wallet
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if err := lockMerchant(ctx, tx, merchantID); err != nil {
			return err
		}
		w, err := scanWallet(tx.QueryRow(ctx, `SELECT `+walletColumns+` FROM wallets w
			WHERE w.id = $1 AND w.merchant_id = $2 AND w.livemode = $3`, id, merchantID, livemode))
		if err != nil {
			return err
		}
		if w.ArchivedAt != nil {
			out = w
			return nil
		}
		if w.UsedByProducts > 0 {
			return &WalletInUseError{Products: w.UsedByProducts}
		}
		now := s.now()
		if _, err := tx.Exec(ctx, `UPDATE wallets SET archived_at = $2, is_default = false WHERE id = $1`, id, now); err != nil {
			return err
		}
		if w.Default {
			if _, err := tx.Exec(ctx, `UPDATE wallets SET is_default = true WHERE id = (SELECT d.id FROM wallets d
				WHERE d.merchant_id = $1 AND d.livemode = $2 AND d.family = $3 AND d.archived_at IS NULL
				ORDER BY `+defaultOrder+` LIMIT 1)`, merchantID, livemode, string(w.Family)); err != nil {
				return err
			}
		}
		w.ArchivedAt, w.Default = &now, false
		out = w
		return audit(ctx, tx, merchantID, actor, "wallet.archived", map[string]any{"wallet_id": id, "livemode": livemode,
			"family": w.Family, "address": w.Address})
	})
	return out, opErr("archive wallet", err)
}

// checkPayoutWallets requires every item's wallet to be an active wallet of the product's
// merchant and mode in the item's family, and fills in each item's label and address.
func checkPayoutWallets(ctx context.Context, tx pgx.Tx, merchantID string, livemode bool, payout map[string][]PayoutItem) error {
	for _, items := range payout {
		for i := range items {
			fam := items[i].Family
			var label, address, f string
			var archived *time.Time
			err := tx.QueryRow(ctx, `SELECT label, address, family, archived_at FROM wallets WHERE id = $1 AND merchant_id = $2 AND livemode = $3`,
				items[i].WalletID, merchantID, livemode).Scan(&label, &address, &f, &archived)
			if errors.Is(err, pgx.ErrNoRows) || (err == nil && (archived != nil || f != string(fam))) {
				return fmt.Errorf("%w: %s", ErrWalletUnavailable, items[i].WalletID)
			}
			if err != nil {
				return err
			}
			items[i].Label, items[i].Address = label, address
		}
	}
	return nil
}
