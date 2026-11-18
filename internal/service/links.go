package service

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/prismaport/prismaport/internal/apikey"
	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/store"
)

// PublicLink is what a payment link page may show: the product's name and description and the
// effective price (a link that sets the amount of an open price answers a fixed price of that
// amount). Active equals Live (the page reads it to decide whether the link can be used);
// UnavailableReason says why a link is not live. The link's label, metadata and return URL are
// never shown, nor the merchant's id; Merchant names it only once it uploaded a logo.
type PublicLink struct {
	ID                string   `json:"id"`
	Livemode          bool     `json:"livemode"`
	Name              string   `json:"name"`
	Description       *string  `json:"description"`
	PriceType         string   `json:"price_type"`
	Amount            *string  `json:"amount"`
	MinAmount         *string  `json:"min_amount"`
	MaxAmount         *string  `json:"max_amount"`
	SuggestedAmounts  []string `json:"suggested_amounts"`
	Currency          string   `json:"currency"`
	Active            bool     `json:"active"`
	Live              bool     `json:"live"`
	UnavailableReason *string  `json:"unavailable_reason"`
	// Merchant brands the page once the merchant uploaded a logo; nil until then.
	Merchant *PublicMerchant `json:"merchant"`
}

// loadPublicLink reads a link and its product by the link id alone. Anything that is not a
// payment link id (old product links included) is not found.
func (s *Service) loadPublicLink(ctx context.Context, id string) (store.PaymentLink, store.Product, error) {
	if !strings.HasPrefix(id, "plink_") || len(id) > 64 {
		return store.PaymentLink{}, store.Product{}, linkNotFound()
	}
	l, err := s.store.LoadLink(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return store.PaymentLink{}, store.Product{}, linkNotFound()
	}
	if err != nil {
		return store.PaymentLink{}, store.Product{}, err
	}
	p, err := s.store.GetProduct(ctx, l.MerchantID, l.Livemode, l.ProductID)
	if err != nil {
		return store.PaymentLink{}, store.Product{}, err
	}
	return l, p, nil
}

func optUSD(u *money.Units) *string {
	if u == nil {
		return nil
	}
	v := money.FormatUSD(*u)
	return &v
}

// PublicLink serves the public view of a payment link. A link that is not live is still
// returned, with live=false, its reason and no price, so the page can say it is no longer
// available.
func (s *Service) PublicLink(ctx context.Context, id string) (PublicLink, error) {
	l, p, err := s.loadPublicLink(ctx, id)
	if err != nil {
		return PublicLink{}, err
	}
	counts, err := s.store.LinkCounts(ctx, []string{l.ID})
	if err != nil {
		return PublicLink{}, err
	}
	live, reason := Liveness(l, p.Active, counts[l.ID], s.store.Now())
	out := PublicLink{ID: l.ID, Livemode: l.Livemode, Name: p.Name, Description: p.Description, PriceType: string(p.PriceType),
		Currency: "USD", Active: live, Live: live, SuggestedAmounts: []string{}}
	if out.Merchant, err = s.PublicMerchant(ctx, l.MerchantID); err != nil {
		return PublicLink{}, err
	}
	if !live {
		out.UnavailableReason = &reason
		return out, nil
	}
	if p.PriceType == store.PriceOpen && l.AmountCents != nil {
		out.PriceType, out.Amount = string(store.PriceFixed), optUSD(l.AmountCents)
		return out, nil
	}
	out.Amount, out.MinAmount, out.MaxAmount = optUSD(p.AmountCents), optUSD(p.MinAmountCents), optUSD(p.MaxAmountCents)
	for _, u := range p.SuggestedCents {
		out.SuggestedAmounts = append(out.SuggestedAmounts, money.FormatUSD(u))
	}
	return out, nil
}

type linkSessionResult struct {
	SessionID string `json:"session_id"`
}

// OpenLinkSession creates a session from a live payment link on behalf of an anonymous payer, at
// most once per (link, Idempotency-Key). rawBody is the request body as received; the same key
// with a different body (another amount or reference) is a conflict. clientReference comes from
// the link URL: it is stored with source "payer", for correlation only. It returns the session
// id.
func (s *Service) OpenLinkSession(ctx context.Context, linkID, idemKey string, rawBody []byte, amount, clientReference *string) (string, bool, error) {
	if err := checkIdempotencyKey(idemKey); err != nil {
		return "", false, err
	}
	l, _, err := s.loadPublicLink(ctx, linkID)
	if err != nil {
		return "", false, err
	}
	// The payer acts with the merchant's authority for exactly one operation: opening a session
	// from this link at its product's price rules. Nothing else from the request is used, except
	// an amount the price leaves to the payer and a client reference that stays marked as the
	// payer's.
	a := Auth{KeyID: "checkout_link", MerchantID: l.MerchantID, Livemode: l.Livemode, Scopes: []string{string(apikey.SessionsWrite)}}
	// The key lives in the merchant's idempotency namespace; the prefix keeps it apart from keys
	// the merchant chooses for its own API calls.
	scope := store.IdempotencyScope{MerchantID: l.MerchantID, Livemode: l.Livemode, Key: "checkout_link/" + l.ID + "/" + idemKey}
	req := idemRequest{scope: scope, route: "POST /v1/checkout/links/" + l.ID + "/sessions", body: rawBody}
	out, _, replayed, err := idempotent(ctx, s.store, req, func(ctx context.Context, tx pgx.Tx) (linkSessionResult, int, error) {
		b := s.bind(tx)
		link, product, err := b.checkLink(ctx, tx, a, l.ID)
		if err != nil {
			return linkSessionResult{}, 0, err
		}
		in := SessionInput{ProductID: &product.ID, Amount: amount, ClientReferenceID: clientReference, referenceSource: ReferencePayer,
			link: &link, product: &product}
		ses, err := b.buildSession(ctx, a, in)
		if err != nil {
			return linkSessionResult{}, 0, err
		}
		if err := b.store.InsertSession(ctx, tx, ses); err != nil {
			return linkSessionResult{}, 0, err
		}
		return linkSessionResult{SessionID: ses.ID}, statusOK, nil
	})
	if err != nil {
		return "", false, err
	}
	if out.SessionID == "" {
		return "", false, errors.New("stored link session response is malformed")
	}
	return out.SessionID, replayed, nil
}

// checkLink locks the link for the rest of tx and refuses a session it may not open: the link
// must be live (its product active, itself active, not expired, a free max_payments slot among
// its own sessions). Every refusal is 410 link_unavailable. It returns the link and the product
// as read under the lock, which the session is built from.
func (s *Service) checkLink(ctx context.Context, tx pgx.Tx, a Auth, linkID string) (store.PaymentLink, store.Product, error) {
	l, p, counts, err := s.store.LockLink(ctx, tx, a.MerchantID, a.Livemode, linkID)
	if errors.Is(err, store.ErrNotFound) {
		return l, p, linkNotFound()
	}
	if err != nil {
		return l, p, err
	}
	if live, reason := Liveness(l, p.Active, counts, s.store.Now()); !live {
		return l, p, linkUnavailable(reason)
	}
	return l, p, nil
}
