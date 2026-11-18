package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prismaport/prismaport/internal/money"
)

// SessionFilter narrows a merchant's session list. Empty fields do not filter.
type SessionFilter struct {
	Status    string
	ProductID string
	LinkID    string
	// Query matches, see ParseSessionSearch: a session id or its start, a payment transaction
	// hash or its start, a deposit address exactly, or a USD or token amount exactly.
	Query string
	// ClientReferenceID matches exactly, whoever set it.
	ClientReferenceID string
	// CustomerAddressID keeps the sessions opened on one customer address.
	CustomerAddressID string
}

// MinHashPrefix is the shortest transaction hash start the search matches, in characters after
// any "0x": shorter ones would select too many payments across every merchant before the
// merchant filter applies.
const MinHashPrefix = 8

var (
	searchToken  = regexp.MustCompile(`^[0-9A-Za-z_]+$`)
	searchAmount = regexp.MustCompile(`^[0-9]{1,15}(\.[0-9]{1,18})?$`)
	ulidChars    = regexp.MustCompile(`^[0-9A-Z]*$`)
)

// SessionSearch is how a search query is matched. Empty fields do not match.
type SessionSearch struct {
	// Exact matches a session id or a deposit address as typed (EVM hex in any case).
	Exact string
	// IDPrefix matches the start of a session id ("ses_" plus uppercase ULID characters).
	IDPrefix string
	// HashPrefix matches the start of a payment transaction hash; lowercase and compared
	// case-insensitively when HashFold (EVM "0x" hashes), as typed otherwise (Solana signatures).
	HashPrefix string
	HashFold   bool
	// Amount matches a session amount in USD or a payment amount in token units, exactly.
	Amount string
}

// ParseSessionSearch decides how q is matched. Every way that fits applies; the session matches
// if any of them does.
func ParseSessionSearch(q string) SessionSearch {
	q = strings.TrimSpace(q)
	if q == "" {
		return SessionSearch{}
	}
	out := SessionSearch{Exact: q}
	if searchAmount.MatchString(q) {
		out.Amount = q
	}
	if !searchToken.MatchString(q) {
		return out
	}
	if len(q) >= 4 && strings.EqualFold(q[:4], "ses_") {
		if rest := strings.ToUpper(q[4:]); ulidChars.MatchString(rest) {
			out.IDPrefix = "ses_" + rest
		}
		return out
	}
	if strings.HasPrefix(q, "0x") || strings.HasPrefix(q, "0X") {
		if len(q) >= 2+MinHashPrefix {
			out.HashPrefix, out.HashFold = strings.ToLower(q), true
		}
	} else if len(q) >= MinHashPrefix {
		out.HashPrefix = q
	}
	return out
}

// prefixEnd is the smallest string above every string that starts with prefix, for a byte-wise
// range scan. prefix is ASCII letters, digits and underscores (checked by ParseSessionSearch).
func prefixEnd(prefix string) string {
	return prefix[:len(prefix)-1] + string(prefix[len(prefix)-1]+1)
}

// where renders the search as one SQL condition on sessions s, appending its parameters to args.
// Prefixes use the byte-wise pattern operators so a btree text_pattern_ops index can serve them
// with bound parameters (LIKE could not, once the plan is generic).
func (q SessionSearch) where(args *[]any) string {
	if q.Exact == "" {
		return "true"
	}
	arg := func(v any) string {
		*args = append(*args, v)
		return fmt.Sprintf("$%d", len(*args))
	}
	exact := arg(q.Exact)
	or := []string{
		"s.id = " + exact,
		`EXISTS (SELECT 1 FROM session_pay_to t WHERE t.session_id = s.id AND (t.address = ` + exact + ` OR lower(t.address) = lower(` + exact + `)))`,
	}
	if q.IDPrefix != "" {
		or = append(or, fmt.Sprintf("(s.id ~>=~ %s AND s.id ~<~ %s)", arg(q.IDPrefix), arg(prefixEnd(q.IDPrefix))))
	}
	if q.HashPrefix != "" {
		col := "p.tx_hash"
		if q.HashFold {
			col = "lower(p.tx_hash)"
		}
		or = append(or, fmt.Sprintf("s.id IN (SELECT p.session_id FROM payments p WHERE %s ~>=~ %s AND %s ~<~ %s)",
			col, arg(q.HashPrefix), col, arg(prefixEnd(q.HashPrefix))))
	}
	if q.Amount != "" {
		amt := arg(q.Amount)
		or = append(or, "s.amount_cents = "+amt+"::numeric * 100",
			`EXISTS (SELECT 1 FROM payments p WHERE p.session_id = s.id AND p.amount = `+amt+`::numeric * power(10::numeric, p.decimals))`)
	}
	return "(" + strings.Join(or, " OR ") + ")"
}

// ListSessions returns a page of a merchant's sessions in one mode, newest first. after is the
// last id of the previous page.
func (s *Store) ListSessions(ctx context.Context, merchantID string, livemode bool, f SessionFilter, after string, limit int) ([]*Session, bool, error) {
	args := []any{merchantID, livemode, after, f.Status, f.ProductID, limit + 1, f.ClientReferenceID, f.LinkID, f.CustomerAddressID}
	search := ParseSessionSearch(f.Query).where(&args)
	rows, err := s.db.Query(ctx, `SELECT id FROM sessions s
		WHERE merchant_id = $1 AND livemode = $2 AND ($3 = '' OR id < $3)
			AND ($4 = '' OR status = $4) AND ($5 = '' OR product_id = $5) AND ($7 = '' OR client_reference_id = $7)
			AND ($8 = '' OR link_id = $8) AND ($9 = '' OR customer_address_id = $9) AND `+search+`
		ORDER BY id DESC LIMIT $6`, args...)
	if err != nil {
		return nil, false, opErr("list sessions", err)
	}
	idList, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, false, opErr("list sessions", err)
	}
	more := len(idList) > limit
	if more {
		idList = idList[:limit]
	}
	out := make([]*Session, 0, len(idList))
	for _, id := range idList {
		ses, err := loadSession(ctx, s.db, id, false)
		if err != nil {
			return nil, false, opErr("list sessions", err)
		}
		out = append(out, ses)
	}
	return out, more, nil
}

// ProductNames maps product ids of the merchant in one mode to their current names; ids of other
// merchants or modes are left out.
func (s *Store) ProductNames(ctx context.Context, merchantID string, livemode bool, ids []string) (map[string]string, error) {
	rows, err := s.db.Query(ctx, `SELECT id, name FROM products WHERE merchant_id = $1 AND livemode = $2 AND id = ANY($3)`,
		merchantID, livemode, ids)
	if err != nil {
		return nil, opErr("product names", err)
	}
	out := map[string]string{}
	var id, name string
	_, err = pgx.ForEachRow(rows, []any{&id, &name}, func() error {
		out[id] = name
		return nil
	})
	return out, opErr("product names", err)
}

// LinkNames maps link ids of the merchant in one mode to the name the dashboard shows for them:
// the link's label, else its product's current name. Ids of other merchants or modes are left out.
func (s *Store) LinkNames(ctx context.Context, merchantID string, livemode bool, ids []string) (map[string]string, error) {
	rows, err := s.db.Query(ctx, `SELECT l.id, coalesce(l.name, p.name) FROM payment_links l JOIN products p ON p.id = l.product_id
		WHERE l.merchant_id = $1 AND l.livemode = $2 AND l.id = ANY($3)`, merchantID, livemode, ids)
	if err != nil {
		return nil, opErr("link names", err)
	}
	out := map[string]string{}
	var id, name string
	_, err = pgx.ForEachRow(rows, []any{&id, &name}, func() error {
		out[id] = name
		return nil
	})
	return out, opErr("link names", err)
}

// EventLogEntry is one event of a session's history.
type EventLogEntry struct {
	ID             string
	Type           string
	SessionVersion int64
	Seq            *int64
	CreatedAt      time.Time
}

// SessionEventLog returns the events of a session in the order they were recorded.
func (s *Store) SessionEventLog(ctx context.Context, sessionID string) ([]EventLogEntry, error) {
	rows, err := s.db.Query(ctx, `SELECT id, type, session_version, seq, created_at FROM events WHERE session_id = $1
		ORDER BY session_version, pk`, sessionID)
	if err != nil {
		return nil, opErr("session event log", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (EventLogEntry, error) {
		var e EventLogEntry
		err := r.Scan(&e.ID, &e.Type, &e.SessionVersion, &e.Seq, &e.CreatedAt)
		return e, err
	})
	return out, opErr("session event log", err)
}

// ChainAssetTotal sums one (chain, asset) of a merchant's mode.
type ChainAssetTotal struct {
	Chain    string
	Asset    string
	Decimals uint8
	Received money.Units
	Fee      money.Units
	Net      money.Units
}

// Totals sums what a merchant received (confirmed and finalized payments) and what was settled
// (fee and net of every settlement) per chain and asset.
func (s *Store) Totals(ctx context.Context, merchantID string, livemode bool) ([]ChainAssetTotal, error) {
	rows, err := s.db.Query(ctx, `WITH received AS (
			SELECT p.chain, p.asset, p.decimals, sum(p.amount) AS amount FROM payments p JOIN sessions s ON s.id = p.session_id
			WHERE s.merchant_id = $1 AND s.livemode = $2 AND p.status IN ('confirmed', 'finalized')
			GROUP BY p.chain, p.asset, p.decimals),
		settled AS (
			SELECT x.chain, x.asset, t.decimals, sum(x.fee) AS fee, sum(x.net) AS net FROM settlements x
			JOIN sessions s ON s.id = x.session_id
			JOIN session_pay_to t ON t.session_id = x.session_id AND t.chain = x.chain AND t.asset = x.asset
			WHERE s.merchant_id = $1 AND s.livemode = $2
			GROUP BY x.chain, x.asset, t.decimals)
		SELECT coalesce(r.chain, d.chain), coalesce(r.asset, d.asset), coalesce(r.decimals, d.decimals),
			coalesce(r.amount, 0)::text, coalesce(d.fee, 0)::text, coalesce(d.net, 0)::text
		FROM received r FULL JOIN settled d ON d.chain = r.chain AND d.asset = r.asset AND d.decimals = r.decimals
		ORDER BY 1, 2, 3`, merchantID, livemode)
	if err != nil {
		return nil, opErr("totals", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ChainAssetTotal, error) {
		var t ChainAssetTotal
		var dec int16
		var recv, fee, net string
		if err := r.Scan(&t.Chain, &t.Asset, &dec, &recv, &fee, &net); err != nil {
			return t, err
		}
		t.Decimals = uint8(dec)
		var err error
		if t.Received, err = parseUnits("received", recv); err != nil {
			return t, err
		}
		if t.Fee, err = parseUnits("fee", fee); err != nil {
			return t, err
		}
		t.Net, err = parseUnits("net", net)
		return t, err
	})
	return out, opErr("totals", err)
}

// WalletTotal is what the settlements of a merchant's mode paid one wallet in one (chain, asset).
type WalletTotal struct {
	WalletID *string
	Label    *string
	Family   string
	Address  string
	Chain    string
	Asset    string
	Decimals uint8
	Amount   money.Units
}

// TotalsByWallet sums the settlement payouts of a merchant's mode per wallet, chain and asset.
func (s *Store) TotalsByWallet(ctx context.Context, merchantID string, livemode bool) ([]WalletTotal, error) {
	rows, err := s.db.Query(ctx, `SELECT sp.wallet_id, w.label, t.family, sp.address, x.chain, x.asset, t.decimals, sum(sp.amount)::text
		FROM settlement_payouts sp
		JOIN settlements x ON x.id = sp.settlement_id
		JOIN sessions s ON s.id = x.session_id
		JOIN session_pay_to t ON t.session_id = x.session_id AND t.chain = x.chain AND t.asset = x.asset
		LEFT JOIN wallets w ON w.id = sp.wallet_id
		WHERE s.merchant_id = $1 AND s.livemode = $2
		GROUP BY sp.wallet_id, w.label, t.family, sp.address, x.chain, x.asset, t.decimals
		ORDER BY t.family, sp.address, x.chain, x.asset`, merchantID, livemode)
	if err != nil {
		return nil, opErr("totals by wallet", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (WalletTotal, error) {
		var t WalletTotal
		var dec int16
		var amt string
		if err := r.Scan(&t.WalletID, &t.Label, &t.Family, &t.Address, &t.Chain, &t.Asset, &dec, &amt); err != nil {
			return t, err
		}
		t.Decimals = uint8(dec)
		var err error
		t.Amount, err = parseUnits("amount", amt)
		return t, err
	})
	return out, opErr("totals by wallet", err)
}

// RecentPayment is a payment with the session and link it belongs to.
type RecentPayment struct {
	ID          string
	SessionID   string
	ProductID   *string
	ProductName *string
	// LinkID is the link that opened the session; LinkName its label, else the product name.
	LinkID      *string
	LinkName    *string
	Chain       string
	Asset       string
	Decimals    uint8
	Amount      money.Units
	TxHash      string
	Status      string
	Late        bool
	DetectedAt  time.Time
	ConfirmedAt *time.Time
	Simulated   bool
}

// recentPaymentsSQL: payments_detected (migration 00106) lets the planner read payments newest
// first and stop after $3 of the merchant's mode, instead of sorting all of the merchant's.
const recentPaymentsSQL = `SELECT p.id, p.session_id, s.product_id, pr.name, s.link_id, coalesce(l.name, pr.name), p.chain, p.asset,
		p.decimals, p.amount::text, p.tx_hash, p.status, p.late, p.detected_at, p.confirmed_at, p.simulated
	FROM payments p JOIN sessions s ON s.id = p.session_id LEFT JOIN products pr ON pr.id = s.product_id
	LEFT JOIN payment_links l ON l.id = s.link_id
	WHERE s.merchant_id = $1 AND s.livemode = $2 ORDER BY p.detected_at DESC, p.id DESC LIMIT $3`

// RecentPayments returns up to limit of a merchant's most recent payments in the given mode.
func (s *Store) RecentPayments(ctx context.Context, merchantID string, livemode bool, limit int) ([]RecentPayment, error) {
	rows, err := s.db.Query(ctx, recentPaymentsSQL, merchantID, livemode, limit)
	if err != nil {
		return nil, opErr("recent payments", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (RecentPayment, error) {
		var p RecentPayment
		var dec int16
		var amt string
		if err := r.Scan(&p.ID, &p.SessionID, &p.ProductID, &p.ProductName, &p.LinkID, &p.LinkName, &p.Chain, &p.Asset, &dec, &amt, &p.TxHash, &p.Status,
			&p.Late, &p.DetectedAt, &p.ConfirmedAt, &p.Simulated); err != nil {
			return p, err
		}
		p.Decimals = uint8(dec)
		var err error
		p.Amount, err = parseUnits("payments.amount", amt)
		return p, err
	})
	return out, opErr("recent payments", err)
}

// DeliveryRecord is a webhook delivery as the dashboard lists it.
type DeliveryRecord struct {
	ID             string
	EndpointID     string
	EventID        string
	EventType      string
	SessionID      *string
	Status         string
	Attempts       int
	NextAttemptAt  time.Time
	LastAttemptAt  *time.Time
	LastStatusCode *int
	LastError      *string
	DeliveredAt    *time.Time
	CreatedAt      time.Time
	// Test marks a "Send test event" delivery: its event is a sample that is not in the feed.
	Test bool
}

// A test delivery has no events row: its sample event id and type live on the delivery.
const deliveryColumns = `d.id, d.endpoint_id, coalesce(d.event_id, d.test_event_id), coalesce(ev.type, d.test_event_type), ev.session_id,
	d.status, d.attempts, d.next_attempt_at, d.last_attempt_at, d.last_status_code, d.last_error, d.delivered_at, d.created_at, d.test`

const deliveryFrom = ` FROM webhook_deliveries d LEFT JOIN events ev ON ev.id = d.event_id`

// listDeliveriesSQL pages an endpoint's deliveries in the order of webhook_deliveries_endpoint
// (endpoint_id, id DESC; migration 00105), so a page stops after its rows.
const listDeliveriesSQL = `SELECT ` + deliveryColumns + deliveryFrom + `
	WHERE d.endpoint_id = $1 AND ($2 = '' OR d.id < $2) ORDER BY d.id DESC LIMIT $3`

func scanDelivery(r pgx.Row) (DeliveryRecord, error) {
	var d DeliveryRecord
	err := r.Scan(&d.ID, &d.EndpointID, &d.EventID, &d.EventType, &d.SessionID, &d.Status, &d.Attempts, &d.NextAttemptAt,
		&d.LastAttemptAt, &d.LastStatusCode, &d.LastError, &d.DeliveredAt, &d.CreatedAt, &d.Test)
	return d, notFound(err)
}

// ListDeliveries pages an endpoint's deliveries newest first; the endpoint must belong to the
// merchant and mode (deleted endpoints included, so their log stays readable).
func (s *Store) ListDeliveries(ctx context.Context, merchantID string, livemode bool, endpointID, after string, limit int) ([]DeliveryRecord, bool, error) {
	var n int
	if err := s.db.QueryRow(ctx, `SELECT count(*) FROM webhook_endpoints WHERE id = $1 AND merchant_id = $2 AND livemode = $3`,
		endpointID, merchantID, livemode).Scan(&n); err != nil {
		return nil, false, opErr("list deliveries", err)
	}
	if n == 0 {
		return nil, false, ErrNotFound
	}
	rows, err := s.db.Query(ctx, listDeliveriesSQL, endpointID, after, limit+1)
	if err != nil {
		return nil, false, opErr("list deliveries", err)
	}
	defer rows.Close()
	var out []DeliveryRecord
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, false, opErr("list deliveries", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, false, opErr("list deliveries", err)
	}
	more := len(out) > limit
	if more {
		out = out[:limit]
	}
	return out, more, nil
}

var (
	// ErrEndpointDeleted refuses a resend to a deleted endpoint.
	ErrEndpointDeleted = errors.New("store: webhook endpoint is deleted")
	// ErrEndpointDisabled refuses a resend to a disabled (paused or failing) endpoint.
	ErrEndpointDisabled = errors.New("store: webhook endpoint is disabled")
	// ErrTestDelivery refuses a resend of a test delivery: its sample event is not stored.
	ErrTestDelivery = errors.New("store: test deliveries are not resent")
)

// ResendDelivery queues a delivery again now with a fresh retry budget. The consumer dedupes by
// event id, so a resend of a delivered event is safe.
func (s *Store) ResendDelivery(ctx context.Context, merchantID string, livemode bool, deliveryID, actor string) (DeliveryRecord, error) {
	var out DeliveryRecord
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var deleted *time.Time
		var status string
		var test bool
		// The endpoint is read with a share lock, so a pause or delete waits for this resend and
		// then dead-letters it, or lands first and is seen here.
		err := tx.QueryRow(ctx, `SELECT e.deleted_at, e.status, d.test FROM webhook_endpoints e JOIN webhook_deliveries d ON e.id = d.endpoint_id
			WHERE d.id = $1 AND e.merchant_id = $2 AND e.livemode = $3 FOR SHARE OF e`, deliveryID, merchantID, livemode).Scan(&deleted, &status, &test)
		if err != nil {
			return notFound(err)
		}
		if _, err := tx.Exec(ctx, `SELECT 1 FROM webhook_deliveries WHERE id = $1 FOR UPDATE`, deliveryID); err != nil {
			return err
		}
		switch {
		case deleted != nil:
			return ErrEndpointDeleted
		case status != EndpointEnabled:
			return ErrEndpointDisabled
		}
		if test {
			return ErrTestDelivery
		}
		if _, err := tx.Exec(ctx, `UPDATE webhook_deliveries SET status = 'pending', attempts = 0, next_attempt_at = $2
			WHERE id = $1`, deliveryID, s.now()); err != nil {
			return err
		}
		if out, err = scanDelivery(tx.QueryRow(ctx, `SELECT `+deliveryColumns+deliveryFrom+`
			WHERE d.id = $1`, deliveryID)); err != nil {
			return err
		}
		return audit(ctx, tx, merchantID, actor, "webhook_delivery.resent", map[string]any{"delivery_id": deliveryID, "event_id": out.EventID})
	})
	return out, opErr("resend delivery", err)
}

// LockAcceptedAssets serializes read-modify-write updates of one merchant's accepted assets in a
// mode for the rest of tx and returns the current set.
func (s *Store) LockAcceptedAssets(ctx context.Context, tx pgx.Tx, merchantID string, livemode bool) ([]ChainAsset, error) {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "merchant_assets/"+merchantID+"/"+boolKey(livemode)); err != nil {
		return nil, opErr("lock accepted assets", err)
	}
	out, err := s.Bind(tx).AcceptedAssets(ctx, merchantID, livemode)
	return out, opErr("lock accepted assets", err)
}

// AuditRecord is an audit entry with its details.
type AuditRecord struct {
	ID        int64
	Actor     string
	Action    string
	Details   json.RawMessage
	CreatedAt time.Time
}

// AuditRecords returns a merchant's audit log entries, with their details, in the order they were written.
func (s *Store) AuditRecords(ctx context.Context, merchantID string) ([]AuditRecord, error) {
	rows, err := s.db.Query(ctx, `SELECT id, actor, action, details::text, created_at FROM audit_log WHERE merchant_id = $1 ORDER BY id`, merchantID)
	if err != nil {
		return nil, opErr("audit records", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (AuditRecord, error) {
		var a AuditRecord
		var d string
		err := r.Scan(&a.ID, &a.Actor, &a.Action, &d, &a.CreatedAt)
		a.Details = json.RawMessage(d)
		return a, err
	})
	return out, opErr("audit records", err)
}

// Onboarding is where a merchant stands in Get started (D57), for one mode.
type Onboarding struct {
	// HasProduct is a product of the mode, active or not; AnyProduct one of either mode.
	HasProduct bool
	AnyProduct bool
	// AnyWallet is an active wallet of either mode; WalletFamilies the families with an active
	// wallet in the mode.
	AnyWallet      bool
	WalletFamilies []string
}

// Onboarding reads the merchant's Get started facts for a mode in one statement of index probes.
func (s *Store) Onboarding(ctx context.Context, merchantID string, livemode bool) (Onboarding, error) {
	var o Onboarding
	err := s.db.QueryRow(ctx, `SELECT
		EXISTS (SELECT 1 FROM products WHERE merchant_id = $1 AND livemode = $2),
		EXISTS (SELECT 1 FROM products WHERE merchant_id = $1),
		EXISTS (SELECT 1 FROM wallets WHERE merchant_id = $1 AND archived_at IS NULL),
		ARRAY(SELECT DISTINCT family FROM wallets WHERE merchant_id = $1 AND livemode = $2 AND archived_at IS NULL ORDER BY family)`,
		merchantID, livemode).Scan(&o.HasProduct, &o.AnyProduct, &o.AnyWallet, &o.WalletFamilies)
	return o, opErr("onboarding", err)
}
