package store

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// The dashboard lists products and links by activity: active (a link: live) first, then by last
// payment, never paid last, then newest. Pages are keyset queries over that order; last_paid_at
// is kept on the rows by the session writer (keepLastPaid), so no page aggregates sessions.

// ProductKey is a product's place in the activity order: a page continues after it.
type ProductKey struct {
	Active     bool
	LastPaidAt *time.Time
	ID         string
}

// ProductQuery filters the products page. Status is "", active or inactive; Query matches the
// exact id or, case-folded, a part of the name.
type ProductQuery struct {
	Status string
	Query  string
	After  *ProductKey
	Limit  int
}

// PageProducts returns a page of a mode's products in the activity order and whether more follow.
func (s *Store) PageProducts(ctx context.Context, merchantID string, livemode bool, q ProductQuery) ([]Product, bool, error) {
	args := []any{merchantID, livemode, q.Status, strings.ToLower(strings.TrimSpace(q.Query)), q.Query, q.Limit + 1}
	after := ""
	if q.After != nil {
		args = append(args, q.After.Active, sortTime(q.After.LastPaidAt), q.After.ID)
		after = ` AND (active, coalesce(last_paid_at, '-infinity'::timestamptz), id) < ($7, $8, $9)`
	}
	out, err := collectProducts(s.db.Query(ctx, `SELECT `+productColumns+` FROM products
		WHERE merchant_id = $1 AND livemode = $2 AND ($3 = '' OR active = ($3 = 'active'))
			AND ($4 = '' OR id = $5 OR strpos(lower(name), $4) > 0)`+after+`
		ORDER BY active DESC, coalesce(last_paid_at, '-infinity'::timestamptz) DESC, id DESC LIMIT $6`, args...))
	if err != nil {
		return nil, false, opErr("page products", err)
	}
	more := len(out) > q.Limit
	if more {
		out = out[:q.Limit]
	}
	return out, more, nil
}

// LinkKey is a link's place in the activity order: a page continues after it.
type LinkKey struct {
	Live       bool
	LastPaidAt *time.Time
	ID         string
}

// LinkQuery filters a links page. ProductID keeps one product's links; Status is "", live or
// inactive (every link that is not live); Query matches the exact link or product id or,
// case-folded, a part of the link's label or the product's name. ByActivity orders by activity
// and continues after AfterKey; otherwise links are newest first, continuing after AfterID.
type LinkQuery struct {
	ProductID  string
	Status     string
	Query      string
	ByActivity bool
	AfterKey   *LinkKey
	AfterID    string
	Limit      int
}

// ListedLink is a link with whether it was live at the query's time.
type ListedLink struct {
	PaymentLink
	Live bool
}

// linkLiveSQL is Liveness (service/linkstate.go) in SQL, with $3 the time: the product and the
// link active, not expired, and under max_payments. Paid sessions are held ones, so held below
// the cap covers both the exhausted and the reserved rule. The two must agree; the integration
// tests compare them.
const linkLiveSQL = `(p.active AND l.active AND (l.expires_at IS NULL OR l.expires_at > $3)
	AND (l.max_payments IS NULL OR (SELECT count(*) FROM sessions s
		WHERE s.link_id = l.id AND s.status IN ` + heldStatuses + `) < l.max_payments))`

// PageLinks returns a page of a mode's links and whether more follow; liveness is as of now.
// Whether a link is live depends on the time and its sessions, so each page computes it over
// the mode's links (or the product's): one pass without aggregating any other session.
func (s *Store) PageLinks(ctx context.Context, merchantID string, livemode bool, q LinkQuery, now time.Time) ([]ListedLink, bool, error) {
	args := []any{merchantID, livemode, now, q.ProductID, strings.ToLower(strings.TrimSpace(q.Query)), q.Query, q.Status, q.Limit + 1}
	after, order := "", `id DESC`
	switch {
	case q.ByActivity:
		order = `live DESC, coalesce(last_paid_at, '-infinity'::timestamptz) DESC, id DESC`
		if q.AfterKey != nil {
			args = append(args, q.AfterKey.Live, sortTime(q.AfterKey.LastPaidAt), q.AfterKey.ID)
			after = ` AND (live, coalesce(last_paid_at, '-infinity'::timestamptz), id) < ($9, $10, $11)`
		}
	case q.AfterID != "":
		args = append(args, q.AfterID)
		after = ` AND id < $9`
	}
	rows, err := s.db.Query(ctx, `SELECT `+linkColumns+`, live FROM (
		SELECT l.*, `+linkLiveSQL+` AS live FROM payment_links l JOIN products p ON p.id = l.product_id
		WHERE l.merchant_id = $1 AND l.livemode = $2 AND ($4 = '' OR l.product_id = $4)
			AND ($5 = '' OR l.id = $6 OR l.product_id = $6 OR strpos(lower(coalesce(l.name, '')), $5) > 0
				OR strpos(lower(p.name), $5) > 0)) x
		WHERE ($7 = '' OR live = ($7 = 'live'))`+after+`
		ORDER BY `+order+` LIMIT $8`, args...)
	if err != nil {
		return nil, false, opErr("page links", err)
	}
	defer rows.Close()
	var out []ListedLink
	for rows.Next() {
		var l ListedLink
		if l.PaymentLink, err = scanLink(withLive{rows, &l.Live}); err != nil {
			return nil, false, opErr("page links", err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, false, opErr("page links", err)
	}
	more := len(out) > q.Limit
	if more {
		out = out[:q.Limit]
	}
	return out, more, nil
}

// withLive scans the live column that follows linkColumns.
type withLive struct {
	pgx.Row
	live *bool
}

func (r withLive) Scan(dest ...any) error { return r.Row.Scan(append(dest, r.live)...) }

// sortTime is a last paid time as the activity order sorts it: never paid is -infinity.
func sortTime(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{InfinityModifier: pgtype.NegativeInfinity, Valid: true}
	}
	return pgtype.Timestamptz{Time: *t, Valid: true}
}
