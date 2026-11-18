package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/fee"
	"github.com/prismaport/prismaport/internal/payout"
)

// Customer address statuses.
const (
	AddressActive  = "active"
	AddressRotated = "rotated"
)

// ErrActiveAddressExists is an insert that met the customer's active address (a concurrent
// create won the partial unique index).
var ErrActiveAddressExists = errors.New("store: the customer already has an active address")

// CustomerAddress is a static deposit address of one customer of a merchant (B11): one salt and
// the derivation inputs of every chain, fixed when it was created. Sessions opened on it copy
// PayTo (with their own amount) and Payouts.
type CustomerAddress struct {
	ID         string
	MerchantID string
	Livemode   bool
	CustomerID string
	ProductID  string
	Salt       [32]byte
	Status     string
	ReplacedBy *string
	Metadata   map[string]string
	CreatedAt  time.Time
	RotatedAt  *time.Time
	// PayTo has no amounts: an address has none, its sessions do.
	PayTo   []PayToRecord
	Payouts []SessionPayout
}

// InsertCustomerAddress writes a new address with its pay_to and payout rows inside tx. A second
// active address of the customer is ErrActiveAddressExists.
func (s *Store) InsertCustomerAddress(ctx context.Context, tx pgx.Tx, ca *CustomerAddress, actor string) error {
	if _, err := tx.Exec(ctx, `INSERT INTO customer_addresses (id, merchant_id, livemode, customer_id, product_id, salt, status,
		metadata, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		ca.ID, ca.MerchantID, ca.Livemode, ca.CustomerID, ca.ProductID, ca.Salt[:], ca.Status, metadataArg(ca.Metadata),
		ca.CreatedAt); err != nil {
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == "23505" && pg.ConstraintName == "customer_addresses_active" {
			return ErrActiveAddressExists
		}
		return opErr("insert customer address", err)
	}
	for i, p := range ca.PayTo {
		if _, err := tx.Exec(ctx, `INSERT INTO customer_address_pay_to (customer_address_id, position, chain, asset, family, token,
			decimals, address, treasury, fee_bps, fixed_fee_micros, program, rent_receiver, bound_tokens)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
			ca.ID, i, p.Chain, p.Asset, string(p.Family), p.Token, int16(p.Decimals), p.Address, p.Treasury, int32(p.Fee.FeeBps),
			int64(p.Fee.FixedFeeMicros), optString(p.Program), optString(p.RentReceiver), p.BoundTokens); err != nil {
			return opErr("insert customer address", err)
		}
	}
	for _, p := range ca.Payouts {
		if _, err := tx.Exec(ctx, `INSERT INTO customer_address_payouts (customer_address_id, merchant_id, livemode, chain_id, family,
			position, wallet_id, address, bps) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`, ca.ID, ca.MerchantID, ca.Livemode, p.Chain,
			string(p.Family), p.Position, p.WalletID, p.Address, int32(p.Bps)); err != nil {
			return opErr("insert customer address", err)
		}
	}
	return opErr("insert customer address", audit(ctx, tx, ca.MerchantID, actor, "customer_address.created", map[string]any{"customer_address_id": ca.ID,
		"customer_id": ca.CustomerID, "product_id": ca.ProductID, "livemode": ca.Livemode}))
}

const customerAddressColumns = `id, merchant_id, livemode, customer_id, product_id, salt, status, replaced_by, metadata,
	created_at, rotated_at`

func scanCustomerAddress(r pgx.Row) (CustomerAddress, error) {
	var ca CustomerAddress
	var salt []byte
	if err := r.Scan(&ca.ID, &ca.MerchantID, &ca.Livemode, &ca.CustomerID, &ca.ProductID, &salt, &ca.Status, &ca.ReplacedBy,
		&ca.Metadata, &ca.CreatedAt, &ca.RotatedAt); err != nil {
		return ca, notFound(err)
	}
	if len(salt) != 32 {
		return ca, fmt.Errorf("store: customer address %s salt has %d bytes", ca.ID, len(salt))
	}
	copy(ca.Salt[:], salt)
	return ca, nil
}

// loadAddressRows reads an address's pay_to and payout rows into ca.
func loadAddressRows(ctx context.Context, q querier, ca *CustomerAddress) error {
	rows, err := q.Query(ctx, `SELECT sp.chain_id, sp.family, sp.position, sp.wallet_id, w.label, sp.address, sp.bps
		FROM customer_address_payouts sp LEFT JOIN wallets w ON w.id = sp.wallet_id
		WHERE sp.customer_address_id = $1 ORDER BY sp.chain_id, sp.position`, ca.ID)
	if err != nil {
		return err
	}
	ca.Payouts, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (SessionPayout, error) {
		var p SessionPayout
		var fam string
		var pos int16
		var bps int32
		if err := r.Scan(&p.Chain, &fam, &pos, &p.WalletID, &p.Label, &p.Address, &bps); err != nil {
			return p, err
		}
		p.Family, p.Position, p.Bps = chain.Family(fam), int(pos), uint16(bps)
		return p, nil
	})
	if err != nil {
		return err
	}
	rows, err = q.Query(ctx, `SELECT chain, asset, family, token, decimals, address, treasury, fee_bps, fixed_fee_micros,
		coalesce(program, ''), coalesce(rent_receiver, ''), bound_tokens FROM customer_address_pay_to
		WHERE customer_address_id = $1 ORDER BY position`, ca.ID)
	if err != nil {
		return err
	}
	ca.PayTo, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (PayToRecord, error) {
		var p PayToRecord
		var dec int16
		var bps int32
		var fixedFee int64
		var fam string
		if err := r.Scan(&p.Chain, &p.Asset, &fam, &p.Token, &dec, &p.Address, &p.Treasury, &bps, &fixedFee, &p.Program,
			&p.RentReceiver, &p.BoundTokens); err != nil {
			return p, err
		}
		p.Family, p.Decimals, p.Fee = chain.Family(fam), uint8(dec), fee.Params{FeeBps: uint16(bps), FixedFeeMicros: uint64(fixedFee)}
		return p, nil
	})
	if err != nil {
		return err
	}
	for i := range ca.PayTo {
		for _, p := range ca.Payouts {
			if p.Chain == ca.PayTo[i].Chain {
				ca.PayTo[i].Recipients = append(ca.PayTo[i].Recipients, payout.Recipient{Address: p.Address, Bps: p.Bps})
			}
		}
	}
	return nil
}

// GetCustomerAddress reads one address of a merchant in one mode, with its rows.
func (s *Store) GetCustomerAddress(ctx context.Context, merchantID string, livemode bool, id string) (CustomerAddress, error) {
	ca, err := scanCustomerAddress(s.db.QueryRow(ctx, `SELECT `+customerAddressColumns+` FROM customer_addresses
		WHERE id = $1 AND merchant_id = $2 AND livemode = $3`, id, merchantID, livemode))
	if err != nil {
		return ca, opErr("get customer address", err)
	}
	return ca, opErr("get customer address", loadAddressRows(ctx, s.db, &ca))
}

// LoadCustomerAddress reads an address by its id alone, with its rows, for the public checkout
// (the id is the only scope there).
func (s *Store) LoadCustomerAddress(ctx context.Context, id string) (CustomerAddress, error) {
	ca, err := scanCustomerAddress(s.db.QueryRow(ctx, `SELECT `+customerAddressColumns+` FROM customer_addresses WHERE id = $1`, id))
	if err != nil {
		return ca, opErr("load customer address", err)
	}
	return ca, opErr("load customer address", loadAddressRows(ctx, s.db, &ca))
}

// ActiveCustomerAddress reads the customer's active address inside tx, locked FOR UPDATE, with
// its rows. The caller holds the product lock first (lock order: product, then address).
func (s *Store) ActiveCustomerAddress(ctx context.Context, tx pgx.Tx, merchantID string, livemode bool, customerID string) (CustomerAddress, error) {
	ca, err := scanCustomerAddress(tx.QueryRow(ctx, `SELECT `+customerAddressColumns+` FROM customer_addresses
		WHERE merchant_id = $1 AND livemode = $2 AND customer_id = $3 AND status = 'active' FOR UPDATE`, merchantID, livemode, customerID))
	if err != nil {
		return ca, opErr("active customer address", err)
	}
	return ca, opErr("active customer address", loadAddressRows(ctx, tx, &ca))
}

// LockCustomerAddress locks an address for a session open or a rotation, in the lock order every
// writer follows: its product FOR KEY SHARE (the lock a session insert's foreign key takes
// anyway), then the address FOR UPDATE. The product is read after its lock, so it is seen as of
// any edit committed before; the address's product never changes, so it is safe to lock it first.
func (s *Store) LockCustomerAddress(ctx context.Context, tx pgx.Tx, merchantID string, livemode bool, id string) (CustomerAddress, Product, error) {
	var productID string
	if err := tx.QueryRow(ctx, `SELECT product_id FROM customer_addresses WHERE id = $1 AND merchant_id = $2 AND livemode = $3`,
		id, merchantID, livemode).Scan(&productID); err != nil {
		return CustomerAddress{}, Product{}, opErr("lock customer address", notFound(err))
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM products WHERE id = $1 FOR KEY SHARE`, productID); err != nil {
		return CustomerAddress{}, Product{}, opErr("lock customer address", err)
	}
	// Read after the lock, in a new statement: see LockProductForSession.
	p, err := scanProduct(tx.QueryRow(ctx, `SELECT `+productColumns+` FROM products WHERE id = $1`, productID))
	if err != nil {
		return CustomerAddress{}, Product{}, opErr("lock customer address", err)
	}
	ca, err := scanCustomerAddress(tx.QueryRow(ctx, `SELECT `+customerAddressColumns+` FROM customer_addresses WHERE id = $1 FOR UPDATE`, id))
	if err != nil {
		return CustomerAddress{}, Product{}, opErr("lock customer address", err)
	}
	return ca, p, opErr("lock customer address", loadAddressRows(ctx, tx, &ca))
}

// MarkAddressRotated retires an active address in favour of replacedBy, which the caller inserts
// in the same transaction (the foreign key is checked at commit).
func (s *Store) MarkAddressRotated(ctx context.Context, tx pgx.Tx, ca CustomerAddress, replacedBy, actor string, now time.Time) error {
	tag, err := tx.Exec(ctx, `UPDATE customer_addresses SET status = 'rotated', replaced_by = $2, rotated_at = $3
		WHERE id = $1 AND status = 'active'`, ca.ID, replacedBy, now)
	if err != nil {
		return opErr("mark address rotated", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	return opErr("mark address rotated", audit(ctx, tx, ca.MerchantID, actor, "customer_address.rotated", map[string]any{"customer_address_id": ca.ID,
		"replaced_by": replacedBy, "livemode": ca.Livemode}))
}

// ListCustomerAddresses lists a mode's addresses newest first, of one customer when customerID is
// set, every status. startingAfter is the last id of the previous page.
func (s *Store) ListCustomerAddresses(ctx context.Context, merchantID string, livemode bool, customerID, startingAfter string, limit int) ([]CustomerAddress, bool, error) {
	rows, err := s.db.Query(ctx, `SELECT `+customerAddressColumns+` FROM customer_addresses
		WHERE merchant_id = $1 AND livemode = $2 AND ($3 = '' OR customer_id = $3) AND ($4 = '' OR id < $4)
		ORDER BY id DESC LIMIT $5`, merchantID, livemode, customerID, startingAfter, limit+1)
	if err != nil {
		return nil, false, opErr("list customer addresses", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (CustomerAddress, error) { return scanCustomerAddress(r) })
	if err != nil {
		return nil, false, opErr("list customer addresses", err)
	}
	more := len(out) > limit
	if more {
		out = out[:limit]
	}
	for i := range out {
		if err := loadAddressRows(ctx, s.db, &out[i]); err != nil {
			return nil, false, opErr("list customer addresses", err)
		}
	}
	return out, more, nil
}

// OpenAddressSession is the address's current session when it still waits for payment (open or
// partially paid, not expired), nil otherwise (D50): once that session is paid, overpaid, expired
// or otherwise closed, the next open makes a new one. Call it under the address lock
// (LockCustomerAddress) with the same transaction: sessions are opened one at a time.
func (s *Store) OpenAddressSession(ctx context.Context, tx pgx.Tx, addressID string, now time.Time) (*Session, error) {
	var id string
	err := tx.QueryRow(ctx, `SELECT s.id FROM customer_addresses a JOIN sessions s ON s.id = a.current_session_id
		WHERE a.id = $1 AND s.status IN ('open', 'partially_paid') AND s.expires_at > $2`, addressID, now).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, opErr("open address session", err)
	}
	out, err := loadSession(ctx, tx, id, false)
	return out, opErr("open address session", err)
}

// SetCurrentSession makes a session just inserted on the address (same transaction, under the
// address lock) its current one: the owner of the address's watch from now on.
func (s *Store) SetCurrentSession(ctx context.Context, tx pgx.Tx, addressID, sessionID string) error {
	tag, err := tx.Exec(ctx, `UPDATE customer_addresses SET current_session_id = $2 WHERE id = $1`, addressID, sessionID)
	if err != nil {
		return opErr("set current session", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrNotFound
	}
	return nil
}

// CurrentAddressSession is the id of the address's current session ("" when none was opened).
func (s *Store) CurrentAddressSession(ctx context.Context, addressID string) (string, error) {
	var id *string
	if err := s.db.QueryRow(ctx, `SELECT current_session_id FROM customer_addresses WHERE id = $1`, addressID).Scan(&id); err != nil {
		return "", opErr("current address session", notFound(err))
	}
	if id == nil {
		return "", nil
	}
	return *id, nil
}

// AddressSessions lists the sessions opened on an address, newest first. startingAfter is the
// last id of the previous page.
func (s *Store) AddressSessions(ctx context.Context, addressID, startingAfter string, limit int) ([]*Session, bool, error) {
	rows, err := s.db.Query(ctx, `SELECT id FROM sessions WHERE customer_address_id = $1 AND ($2 = '' OR id < $2)
		ORDER BY id DESC LIMIT $3`, addressID, startingAfter, limit+1)
	if err != nil {
		return nil, false, opErr("address sessions", err)
	}
	idList, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, false, opErr("address sessions", err)
	}
	more := len(idList) > limit
	if more {
		idList = idList[:limit]
	}
	out := make([]*Session, 0, len(idList))
	for _, id := range idList {
		ses, err := loadSession(ctx, s.db, id, false)
		if err != nil {
			return nil, false, opErr("address sessions", err)
		}
		out = append(out, ses)
	}
	return out, more, nil
}

// AddressCounts returns the counts of each customer address id over the sessions opened on it
// (LinkCounts: opened, paid, received per asset, last paid); addresses without sessions get zero
// counts.
func (s *Store) AddressCounts(ctx context.Context, addressIDs []string) (map[string]LinkCounts, error) {
	out, err := sessionCounts(ctx, s.db, "customer_address_id", addressIDs)
	return out, opErr("address counts", err)
}

// AddressSessionRef is a short reference to a customer address's current session.
type AddressSessionRef struct {
	ID        string
	Status    string
	CreatedAt time.Time
}

// LastAddressSessions maps each customer address id to its current (latest) session; addresses
// with no session yet are absent.
func (s *Store) LastAddressSessions(ctx context.Context, addressIDs []string) (map[string]AddressSessionRef, error) {
	out := map[string]AddressSessionRef{}
	if len(addressIDs) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(ctx, `SELECT a.id, s.id, s.status, s.created_at FROM customer_addresses a
		JOIN sessions s ON s.id = a.current_session_id WHERE a.id = ANY($1)`, addressIDs)
	if err != nil {
		return nil, opErr("last address sessions", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var ref AddressSessionRef
		if err := rows.Scan(&id, &ref.ID, &ref.Status, &ref.CreatedAt); err != nil {
			return nil, opErr("last address sessions", err)
		}
		out[id] = ref
	}
	return out, opErr("last address sessions", rows.Err())
}
