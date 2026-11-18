package service

import (
	"context"
	"errors"
	"net/url"
	"time"

	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/session"
	"github.com/prismaport/prismaport/internal/store"
	"github.com/prismaport/prismaport/internal/view"
)

// explorers are the block explorers of the pilot chains: transaction and address URL prefixes,
// plus a query suffix for test clusters.
var explorers = map[string]struct{ tx, address, suffix string }{
	"base":          {"https://basescan.org/tx/", "https://basescan.org/address/", ""},
	"base-sepolia":  {"https://sepolia.basescan.org/tx/", "https://sepolia.basescan.org/address/", ""},
	"solana":        {"https://explorer.solana.com/tx/", "https://explorer.solana.com/address/", ""},
	"solana-devnet": {"https://explorer.solana.com/tx/", "https://explorer.solana.com/address/", "?cluster=devnet"},
}

// ExplorerTx is the explorer page of a transaction, or "" for a chain without one.
func ExplorerTx(chainID, hash string) string {
	e, ok := explorers[chainID]
	if !ok || hash == "" {
		return ""
	}
	return e.tx + url.PathEscape(hash) + e.suffix
}

// ExplorerAddress is the explorer page of an address, or "" for a chain without one.
func ExplorerAddress(chainID, addr string) string {
	e, ok := explorers[chainID]
	if !ok || addr == "" {
		return ""
	}
	return e.address + url.PathEscape(addr) + e.suffix
}

// DashSessionItem is the merchant API session plus the names it is listed under: link_name is
// the label of the link it was opened from, else that link's product name (null for API
// sessions); product_name is its product's current name.
type DashSessionItem struct {
	view.Session
	LinkName    *string `json:"link_name"`
	ProductName *string `json:"product_name"`
}

// SessionList is one page of dashboard sessions and the cursor of the next page, if any.
type SessionList struct {
	Data       []DashSessionItem `json:"data"`
	NextCursor *string           `json:"next_cursor"`
}

// sessionNames are the current product names and link display names of a set of sessions.
type sessionNames struct {
	products, links map[string]string
}

// sessionNames looks up the names of the sessions' products and links, in two queries.
func (s *Service) sessionNames(ctx context.Context, a Auth, list []*store.Session) (sessionNames, error) {
	var productIDs, linkIDs []string
	for _, ses := range list {
		if ses.ProductID != nil {
			productIDs = append(productIDs, *ses.ProductID)
		}
		if ses.LinkID != nil {
			linkIDs = append(linkIDs, *ses.LinkID)
		}
	}
	out := sessionNames{products: map[string]string{}, links: map[string]string{}}
	var err error
	if len(productIDs) > 0 {
		if out.products, err = s.store.ProductNames(ctx, a.MerchantID, a.Livemode, productIDs); err != nil {
			return out, err
		}
	}
	if len(linkIDs) > 0 {
		if out.links, err = s.store.LinkNames(ctx, a.MerchantID, a.Livemode, linkIDs); err != nil {
			return out, err
		}
	}
	return out, nil
}

func lookupName(names map[string]string, id *string) *string {
	if id == nil {
		return nil
	}
	if n, ok := names[*id]; ok {
		return &n
	}
	return nil
}

func (n sessionNames) item(v view.Session, ses *store.Session) DashSessionItem {
	return DashSessionItem{Session: v, LinkName: lookupName(n.links, ses.LinkID), ProductName: lookupName(n.products, ses.ProductID)}
}

// DashListSessions returns one page of the merchant's sessions matching the filter.
func (s *Service) DashListSessions(ctx context.Context, a Auth, f store.SessionFilter, cursor string, limit int) (SessionList, error) {
	if f.Status != "" && !session.Status(f.Status).Valid() {
		return SessionList{}, invalid("status must be one of open, partially_paid, paid, overpaid, expired, late_payment, under_review")
	}
	if len(f.Query) > 200 || len(f.ProductID) > 64 || len(f.LinkID) > 64 || len(cursor) > 64 {
		return SessionList{}, invalid("a filter is too long")
	}
	if f.ClientReferenceID != "" {
		if err := validateClientReference(f.ClientReferenceID); err != nil {
			return SessionList{}, err
		}
	}
	list, more, err := s.store.ListSessions(ctx, a.MerchantID, a.Livemode, f, cursor, limit)
	if err != nil {
		return SessionList{}, err
	}
	names, err := s.sessionNames(ctx, a, list)
	if err != nil {
		return SessionList{}, err
	}
	out := SessionList{Data: []DashSessionItem{}}
	for _, ses := range list {
		out.Data = append(out.Data, names.item(s.view.Session(ses), ses))
	}
	if more && len(list) > 0 {
		next := list[len(list)-1].ID
		out.NextCursor = &next
	}
	return out, nil
}

// DashLinkSessions lists the sessions opened from one link of the merchant.
func (s *Service) DashLinkSessions(ctx context.Context, a Auth, linkID, cursor string, limit int) (SessionList, error) {
	if _, err := s.store.GetLink(ctx, a.MerchantID, a.Livemode, linkID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return SessionList{}, linkNotFound()
		}
		return SessionList{}, err
	}
	return s.DashListSessions(ctx, a, store.SessionFilter{LinkID: linkID}, cursor, limit)
}

// SessionEvent is one entry of a session's event log as the dashboard shows it.
type SessionEvent struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Version   int64  `json:"session_version"`
	Seq       *int64 `json:"seq"`
	CreatedAt string `json:"created_at"`
}

// ExplorerLink points at a transaction or an address of the session on a block explorer.
type ExplorerLink struct {
	// Type is payment (a payment's transaction), settlement (the forwarding transaction) or
	// address (a deposit address).
	Type      string  `json:"type"`
	Chain     string  `json:"chain"`
	Asset     string  `json:"asset"`
	PaymentID *string `json:"payment_id,omitempty"`
	Value     string  `json:"value"`
	URL       string  `json:"url"`
}

// SessionDetail is the dashboard session (with its link and product names) plus its event
// history and explorer links.
type SessionDetail struct {
	DashSessionItem
	Events        []SessionEvent `json:"events"`
	ExplorerLinks []ExplorerLink `json:"explorer_links"`
	// WatchUntil is when the worker stops checking the session on its own (for a session paid
	// exactly, when its payments became final, D47); from then on RecheckAvailable is true and
	// "Check again" (POST .../recheck) may queue one more check (never on a session of a customer
	// address that a newer session on the address superseded).
	// RecheckRequestedAt is set while such a check waits for the worker.
	WatchUntil         string  `json:"watch_until"`
	RecheckAvailable   bool    `json:"recheck_available"`
	RecheckRequestedAt *string `json:"recheck_requested_at"`
	// SupersededBy is, on a session of a customer address, the newer session on the address that
	// owns its watch now; null on the address's current session and on other sessions.
	SupersededBy *string `json:"superseded_by"`
	// SettledWith lists the flushes of this session that found its funds already moved by the
	// flush of another session of the same customer address (outcome covered): that session holds
	// the settlement, once.
	SettledWith []SettledWith `json:"settled_with"`
}

// SettledWith names the session and transaction that settled this session's funds on one
// (chain, asset).
type SettledWith struct {
	Chain     string `json:"chain"`
	Asset     string `json:"asset"`
	SessionID string `json:"session_id"`
	TxHash    string `json:"tx_hash"`
}

// DashSession returns one of the merchant's sessions with its payments and event log, or a
// not-found error.
func (s *Service) DashSession(ctx context.Context, a Auth, id string) (SessionDetail, error) {
	ses, err := s.store.GetSession(ctx, a.MerchantID, a.Livemode, id)
	if errors.Is(err, store.ErrNotFound) {
		return SessionDetail{}, sessionNotFound()
	}
	if err != nil {
		return SessionDetail{}, err
	}
	evs, err := s.store.SessionEventLog(ctx, id)
	if err != nil {
		return SessionDetail{}, err
	}
	names, err := s.sessionNames(ctx, a, []*store.Session{ses})
	if err != nil {
		return SessionDetail{}, err
	}
	out := SessionDetail{DashSessionItem: names.item(s.view.Session(ses), ses), Events: []SessionEvent{}, ExplorerLinks: []ExplorerLink{}}
	for _, e := range evs {
		out.Events = append(out.Events, SessionEvent{ID: e.ID, Type: e.Type, Version: e.SessionVersion, Seq: e.Seq, CreatedAt: rfc3339(e.CreatedAt)})
	}
	for _, p := range ses.State.Payments {
		// A simulated payment's hash is synthetic: no explorer has it.
		if p.Simulated {
			continue
		}
		if u := ExplorerTx(p.Chain, p.TxHash); u != "" {
			id := p.ID
			out.ExplorerLinks = append(out.ExplorerLinks, ExplorerLink{Type: "payment", Chain: p.Chain, Asset: p.Asset, PaymentID: &id, Value: p.TxHash, URL: u})
		}
	}
	for _, st := range ses.Settlements {
		if u := ExplorerTx(st.Chain, st.TxHash); u != "" {
			out.ExplorerLinks = append(out.ExplorerLinks, ExplorerLink{Type: "settlement", Chain: st.Chain, Asset: st.Asset, Value: st.TxHash, URL: u})
		}
	}
	for _, p := range ses.PayTo {
		// An unrevealed network has no address to show (D48).
		if ses.Revealed(p.Chain) == nil {
			continue
		}
		if u := ExplorerAddress(p.Chain, p.Address); u != "" {
			out.ExplorerLinks = append(out.ExplorerLinks, ExplorerLink{Type: "address", Chain: p.Chain, Asset: p.Asset, Value: p.Address, URL: u})
		}
	}
	ends := s.WatchEndsAt(ses)
	// A session never revealed (D48) has nothing to check again.
	out.WatchUntil, out.RecheckAvailable = rfc3339(ends), !s.store.Now().Before(ends) && len(ses.Reveals) > 0
	out.SettledWith = []SettledWith{}
	if ses.CustomerAddressID != nil {
		// A newer session on the customer address watches it: "Check again" is refused here.
		current, err := s.store.CurrentAddressSession(ctx, *ses.CustomerAddressID)
		if err != nil {
			return SessionDetail{}, err
		}
		if current != ses.ID {
			out.RecheckAvailable = false
		}
		if current != "" && current != ses.ID {
			out.SupersededBy = &current
		}
		jobs, err := s.store.SessionTxJobs(ctx, ses.ID)
		if err != nil {
			return SessionDetail{}, err
		}
		for _, j := range jobs {
			if j.Kind == "flush" && j.Outcome == "covered" && j.Details["covered_by_session"] != "" {
				out.SettledWith = append(out.SettledWith, SettledWith{Chain: j.Chain, Asset: j.Asset,
					SessionID: j.Details["covered_by_session"], TxHash: j.Details["covered_by_tx"]})
			}
		}
	}
	pending, err := s.store.RecheckRequestedAt(ctx, id)
	if err != nil {
		return SessionDetail{}, err
	}
	if pending != nil {
		v := rfc3339(*pending)
		out.RecheckRequestedAt = &v
	}
	return out, nil
}

// OverviewTotal is the totals of one chain and asset. received is what arrived (confirmed and
// finalized payments); fees and net are what was settled (forwarded). received - fees - net is
// still in deposit addresses, waiting to be forwarded.
type OverviewTotal struct {
	Chain    string `json:"chain"`
	Asset    string `json:"asset"`
	Received string `json:"received"`
	Fees     string `json:"fees"`
	Net      string `json:"net"`
}

// RecentPayment is one recent payment on the dashboard overview.
type RecentPayment struct {
	ID          string  `json:"id"`
	SessionID   string  `json:"session_id"`
	ProductID   *string `json:"product_id"`
	ProductName *string `json:"product_name"`
	LinkID      *string `json:"link_id"`
	LinkName    *string `json:"link_name"`
	Chain       string  `json:"chain"`
	Asset       string  `json:"asset"`
	Amount      string  `json:"amount"`
	Status      string  `json:"status"`
	Late        bool    `json:"late"`
	TxHash      string  `json:"tx_hash"`
	ExplorerURL *string `json:"explorer_url"`
	DetectedAt  string  `json:"detected_at"`
	ConfirmedAt *string `json:"confirmed_at"`
	// Simulated is true for a synthetic test-mode payment made by the simulator.
	Simulated bool `json:"simulated"`
}

// LinkAttention is a link that needs the merchant: Name is its label, else its product's name.
type LinkAttention struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	ProductID string `json:"product_id"`
	Reason    string `json:"reason"`
}

// Overview is the dashboard home: totals, totals per wallet, recent payments and links that need
// attention.
type Overview struct {
	Totals []OverviewTotal `json:"totals"`
	// TotalsByWallet is what settlements paid each wallet, per chain and asset.
	TotalsByWallet []WalletTotal   `json:"totals_by_wallet"`
	RecentPayments []RecentPayment `json:"recent_payments"`
	LinksAttention []LinkAttention `json:"links_attention"`
}

// WalletTotal is what settlements paid one wallet on one chain and asset.
type WalletTotal struct {
	WalletID *string `json:"wallet_id"`
	Label    *string `json:"label"`
	Family   string  `json:"family"`
	Address  string  `json:"address"`
	Chain    string  `json:"chain"`
	Asset    string  `json:"asset"`
	Amount   string  `json:"amount"`
}

// ExpiringWindow is how close to its expiry an active link is flagged as expiring.
const ExpiringWindow = 72 * time.Hour

// DashOverview builds the dashboard home for the merchant in the auth's mode.
func (s *Service) DashOverview(ctx context.Context, a Auth) (Overview, error) {
	out := Overview{Totals: []OverviewTotal{}, TotalsByWallet: []WalletTotal{}, RecentPayments: []RecentPayment{}, LinksAttention: []LinkAttention{}}
	totals, err := s.store.Totals(ctx, a.MerchantID, a.Livemode)
	if err != nil {
		return out, err
	}
	for _, t := range totals {
		out.Totals = append(out.Totals, OverviewTotal{Chain: t.Chain, Asset: t.Asset, Received: money.FormatDecimal(t.Received, t.Decimals),
			Fees: money.FormatDecimal(t.Fee, t.Decimals), Net: money.FormatDecimal(t.Net, t.Decimals)})
	}
	byWallet, err := s.store.TotalsByWallet(ctx, a.MerchantID, a.Livemode)
	if err != nil {
		return out, err
	}
	for _, t := range byWallet {
		out.TotalsByWallet = append(out.TotalsByWallet, WalletTotal{WalletID: t.WalletID, Label: t.Label, Family: t.Family, Address: t.Address,
			Chain: t.Chain, Asset: t.Asset, Amount: money.FormatDecimal(t.Amount, t.Decimals)})
	}
	recent, err := s.store.RecentPayments(ctx, a.MerchantID, a.Livemode, 10)
	if err != nil {
		return out, err
	}
	for _, p := range recent {
		rp := RecentPayment{ID: p.ID, SessionID: p.SessionID, ProductID: p.ProductID, ProductName: p.ProductName, LinkID: p.LinkID,
			LinkName: p.LinkName, Chain: p.Chain, Asset: p.Asset,
			Amount: money.FormatDecimal(p.Amount, p.Decimals), Status: p.Status, Late: p.Late, TxHash: p.TxHash,
			DetectedAt: rfc3339(p.DetectedAt), ConfirmedAt: optRFC3339(p.ConfirmedAt), Simulated: p.Simulated}
		if u := ExplorerTx(p.Chain, p.TxHash); u != "" && !p.Simulated {
			rp.ExplorerURL = &u
		}
		out.RecentPayments = append(out.RecentPayments, rp)
	}
	links, err := s.sortedLinks(ctx, a, "")
	if err != nil {
		return out, err
	}
	now := s.store.Now()
	for _, l := range links {
		why := ""
		if l.UnavailableReason != nil {
			why = *l.UnavailableReason
		}
		var reason string
		switch {
		case why == ReasonProductInactive && l.counts.Open > 0:
			// A deactivated product stops all its links on purpose; only open sessions need a look.
			reason = ReasonProductInactive
		case why == ReasonExhausted:
			reason = "exhausted"
		case (why == "" || why == ReasonReserved) && l.link.ExpiresAt != nil && l.link.ExpiresAt.Sub(now) <= ExpiringWindow:
			reason = "expiring"
		case why == ReasonInactive && l.counts.Open > 0:
			reason = "inactive_with_open_sessions"
		default:
			continue
		}
		out.LinksAttention = append(out.LinksAttention, LinkAttention{ID: l.ID, Name: l.displayName(), ProductID: l.ProductID, Reason: reason})
	}
	return out, nil
}
