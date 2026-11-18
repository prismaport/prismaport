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

// The public side of static customer addresses (B11, "Checkout
// (public)"): the address page /a/{id} reads the address and opens (or resumes) its session with
// the address id as the only scope, like a payment link.

// Why an address page cannot open a session now. ReasonProductInactive is shared with links.
const (
	// ReasonAddressRotated: the merchant rotated the address; replaced_by is the new one.
	ReasonAddressRotated = "rotated"
	// ReasonNoNetworks: none of the address's (chain, asset) pairs is still accepted with the same
	// token; the merchant must rotate it.
	ReasonNoNetworks = "no_networks"
)

// PublicAddress is what the page of a customer address may show: the product's name and
// description, the price rule of the sessions the page opens, the deposit address per network
// (only while a session can open on it) and the session the page's button resumes. The
// merchant's customer id, metadata and payout are never shown; Merchant names it only once it
// uploaded a logo.
type PublicAddress struct {
	ID       string `json:"id"`
	Livemode bool   `json:"livemode"`
	// Status is active or rotated; ReplacedBy names the address that replaced a rotated one.
	Status     string  `json:"status"`
	ReplacedBy *string `json:"replaced_by"`
	// Live is true when the page's button can open a session; UnavailableReason says why not.
	Live              bool          `json:"live"`
	UnavailableReason *string       `json:"unavailable_reason"`
	Product           PublicProduct `json:"product"`
	// PriceType is the product's. A fixed price sets Amount; an open price opens an open-amount
	// session from this page (OpenAmount, Amount null): the payer sends any amount.
	PriceType  string  `json:"price_type"`
	Amount     *string `json:"amount"`
	OpenAmount bool    `json:"open_amount"`
	Currency   string  `json:"currency"`
	// PayTo is empty when the address is not live: funds sent there would not be watched (D37).
	PayTo []PublicAddressPayTo `json:"pay_to"`
	// Session is the session the button resumes (open or partially paid, before its expires_at);
	// null when the button opens a new one.
	Session  *PublicAddressSession `json:"session"`
	Merchant *PublicMerchant       `json:"merchant"`
}

// PublicProduct is a product as the payer sees it: its current name and description.
type PublicProduct struct {
	Name        string  `json:"name"`
	Description *string `json:"description"`
}

// PublicAddressPayTo is the customer's deposit address on one (chain, asset): always the same.
type PublicAddressPayTo struct {
	Chain   string `json:"chain"`
	Asset   string `json:"asset"`
	Address string `json:"address"`
	// TokenAccount is the Solana deposit token account (the PDA's ATA for the mint).
	TokenAccount string `json:"token_account,omitempty"`
	// Paused says payments of this asset on this network are paused (D64). The address stays
	// the customer's and is still watched, so what is sent to it is credited; absent when not
	// paused.
	Paused bool `json:"paused,omitempty"`
}

// PublicAddressSession names the session the page resumes.
type PublicAddressSession struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	ExpiresAt string `json:"expires_at"`
}

// loadPublicAddress reads an address by its id alone. Anything that is not a customer address id
// is not found.
func (s *Service) loadPublicAddress(ctx context.Context, id string) (store.CustomerAddress, error) {
	if !strings.HasPrefix(id, "caddr_") || len(id) > 64 {
		return store.CustomerAddress{}, customerAddressNotFound()
	}
	ca, err := s.store.LoadCustomerAddress(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return ca, customerAddressNotFound()
	}
	return ca, err
}

// publicAddressAuth is the authority the payer acts with on an address page: opening a session
// on this address, nothing else.
func publicAddressAuth(ca store.CustomerAddress) Auth {
	return Auth{KeyID: "checkout_address", MerchantID: ca.MerchantID, Livemode: ca.Livemode,
		Scopes: []string{string(apikey.SessionsWrite)}, Actor: "checkout:address"}
}

// PublicAddress serves the public view of a customer address. active says the page is open and
// visible: it records the payer's presence on the session it resumes, as GET
// /v1/checkout/sessions/{id}?active=1 does. A rotated address, or one whose product was
// deactivated, is still answered, live false, without deposit addresses.
func (s *Service) PublicAddress(ctx context.Context, id string, active bool) (PublicAddress, error) {
	ca, err := s.loadPublicAddress(ctx, id)
	if err != nil {
		return PublicAddress{}, err
	}
	p, err := s.store.GetProduct(ctx, ca.MerchantID, ca.Livemode, ca.ProductID)
	if err != nil {
		return PublicAddress{}, err
	}
	out := PublicAddress{ID: ca.ID, Livemode: ca.Livemode, Status: ca.Status, ReplacedBy: ca.ReplacedBy,
		Product: PublicProduct{Name: p.Name, Description: p.Description}, PriceType: string(p.PriceType),
		Currency: "USD", PayTo: []PublicAddressPayTo{}}
	if p.PriceType == store.PriceFixed && p.AmountCents != nil {
		v := money.FormatUSD(*p.AmountCents)
		out.Amount = &v
	} else {
		out.OpenAmount = true
	}
	if out.Merchant, err = s.PublicMerchant(ctx, ca.MerchantID); err != nil {
		return PublicAddress{}, err
	}
	// The same rule as the open (D50): the address's current session while it still waits for payment.
	var open *store.Session
	err = pgx.BeginFunc(ctx, s.store.Pool(), func(tx pgx.Tx) error {
		var err error
		open, err = s.resumableOnAddress(ctx, tx, ca.ID)
		return err
	})
	if err != nil {
		return PublicAddress{}, err
	}
	if open != nil {
		out.Session = &PublicAddressSession{ID: open.ID, Status: string(open.State.Status), ExpiresAt: rfc3339(open.State.ExpiresAt)}
		if active {
			if _, err := s.store.TouchCheckoutSeen(ctx, open.ID); err != nil && ctx.Err() == nil {
				s.Logger().Warn("checkout presence not recorded", "session", open.ID, "err", err)
			}
		}
	}
	reason := ""
	var entries []store.PayToRecord
	switch {
	case ca.Status != store.AddressActive:
		reason = ReasonAddressRotated
	case !p.Active:
		reason = ReasonProductInactive
	default:
		// The networks a session opened now would offer (addressEntries): never one turned off since.
		entries, _, err = s.addressEntries(ctx, publicAddressAuth(ca), ca, &p)
		if e := (*Error)(nil); errors.As(err, &e) && e.Code == CodeInvalidRequest {
			reason, err = ReasonNoNetworks, nil
		}
		if err != nil {
			return PublicAddress{}, err
		}
	}
	if reason != "" {
		out.UnavailableReason = &reason
		return out, nil
	}
	pauses, err := s.ChainPauses(ctx)
	if err != nil {
		return PublicAddress{}, err
	}
	out.Live = true
	for _, e := range entries {
		out.PayTo = append(out.PayTo, PublicAddressPayTo{Chain: e.Chain, Asset: e.Asset, Address: e.Address, TokenAccount: e.TokenAccount(),
			Paused: pauses.Paused(e.Chain, e.Asset)})
	}
	return out, nil
}

// OpenAddressSessionPublic opens a session on a customer address from its public page, or
// answers the one the address already has (openOnAddress decides, under the address lock, so two
// clicks never open two sessions). There is no Idempotency-Key: the address itself makes a retry
// answer the same session. It returns the session id.
func (s *Service) OpenAddressSessionPublic(ctx context.Context, id string) (string, error) {
	ca, err := s.loadPublicAddress(ctx, id)
	if err != nil {
		return "", err
	}
	a := publicAddressAuth(ca)
	// The snapshot never changes: the blacklist check runs once, before any lock.
	if ca.Status == store.AddressActive {
		if err := s.precheckAddressPayouts(ctx, a, store.IdempotencyScope{}, ca); err != nil {
			return "", err
		}
	}
	var sessionID string
	err = pgx.BeginFunc(ctx, s.store.Pool(), func(tx pgx.Tx) error {
		ses, err := s.bind(tx).openOnAddress(ctx, tx, a, SessionInput{CustomerAddressID: &ca.ID})
		if err != nil {
			return err
		}
		sessionID = ses.ID
		return nil
	})
	if err != nil {
		// The request has no fields: an error about the address is not about a parameter.
		if e := (*Error)(nil); errors.As(err, &e) && e.Param == "customer_address_id" {
			e.Param = ""
		}
		return "", err
	}
	return sessionID, nil
}
