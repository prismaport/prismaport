package service

import (
	"context"
	"errors"

	"github.com/prismaport/prismaport/internal/apikey"
	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/session"
	"github.com/prismaport/prismaport/internal/store"
)

// Refusals of a reveal (D48), shared with the checkout's wallet endpoints.
const (
	CodeSessionNotPayable = "session_not_payable"
	CodeSessionExpired    = "session_expired"
)

// revealCheck refuses a reveal the session cannot take: a network it does not offer (400), a
// session paid, overpaid or under review (409), or one past its expiry (410, as D45). A network
// already revealed passes: a repeat answers the same details.
func (s *Service) revealCheck(chainID string) func(*store.Session) error {
	return func(ses *store.Session) error {
		if !offers(ses, chainID) {
			return at(invalid("this session does not offer the network %q", chainID), "chain")
		}
		st := ses.State
		switch st.Status {
		case session.Open, session.PartiallyPaid:
		case session.Expired, session.LatePayment:
			return sessionExpired()
		default:
			return newErr(KindConflict, CodeSessionNotPayable, "this session no longer accepts payments")
		}
		if !s.store.Now().Before(st.ExpiresAt) {
			return sessionExpired()
		}
		return nil
	}
}

func sessionExpired() *Error {
	return newErr(KindGone, CodeSessionExpired,
		"this payment session has closed. Do not send funds to it; ask the merchant for a new payment")
}

func offers(ses *store.Session, chainID string) bool {
	for _, p := range ses.PayTo {
		if p.Chain == chainID {
			return true
		}
	}
	return false
}

// checkRevealBlacklist checks the USDC blacklist of the session's EVM recipients on a network
// about to be revealed for the first time: one frozen recipient would block the whole split on
// chain, so the payer must not see an address that can never settle. It runs before the reveal's
// transaction, so a slow provider holds no row lock, and answers from the validator's cache of
// clean verdicts.
func (s *Service) checkRevealBlacklist(ctx context.Context, ses *store.Session, chainID string) error {
	if ses.Revealed(chainID) != nil {
		return nil
	}
	for _, p := range ses.Payouts {
		if p.Chain != chainID || p.Family != chain.EVM {
			continue
		}
		if err := s.checkPayoutBlacklist(ctx, ses.Livemode, p.Family, p.Address); err != nil {
			label := p.Address
			if p.Label != nil {
				label = *p.Label
			}
			return blacklistError(err, KindConflict, label, p.Address)
		}
	}
	return nil
}

// reveal shows the deposit details of one network of ses (D48): the checks, the pause of a first
// reveal (D64), the blacklist of a first EVM reveal, then the reveal row under the session lock
// (which starts the pay window on the session's first reveal). It answers the session as revealed.
func (s *Service) reveal(ctx context.Context, ses *store.Session, in store.RevealInput) (*store.Session, error) {
	check := s.revealCheck(in.Chain)
	// Checked once without the lock, so a refused reveal costs no RPC.
	if err := check(ses); err != nil {
		return nil, err
	}
	if err := s.checkRevealable(ctx, ses, in.Chain); err != nil {
		return nil, err
	}
	if err := s.checkRevealBlacklist(ctx, ses, in.Chain); err != nil {
		return nil, err
	}
	in.SessionID = ses.ID
	out, _, err := s.store.RevealNetwork(ctx, in, check)
	if errors.Is(err, store.ErrNotFound) {
		return nil, sessionNotFound()
	}
	return out, err
}

// RevealSession reveals a network of a session through the merchant API, for merchants with
// their own payment page (POST /v1/sessions/{id}/reveal). The first reveal of a network writes
// the session.revealed audit row.
func (s *Service) RevealSession(ctx context.Context, a Auth, id, chainID string) (*store.Session, error) {
	if err := a.Require(apikey.SessionsWrite); err != nil {
		return nil, err
	}
	ses, err := s.store.GetSession(ctx, a.MerchantID, a.Livemode, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, sessionNotFound()
	}
	if err != nil {
		return nil, err
	}
	return s.reveal(ctx, ses, store.RevealInput{Chain: chainID, Source: store.RevealMerchantAPI, MerchantID: a.MerchantID, Actor: a.actor()})
}

// RevealForPayer reveals a network of ses for the payer: source is store.RevealCheckout ("Generate
// payment" on the hosted checkout) or store.RevealWallet (a wallet endpoint, which also moves
// wallet_tx_at).
func (s *Service) RevealForPayer(ctx context.Context, ses *store.Session, chainID, source string) (*store.Session, error) {
	return s.reveal(ctx, ses, store.RevealInput{Chain: chainID, Source: source})
}
