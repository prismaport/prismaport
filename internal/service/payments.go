package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/prismaport/prismaport/internal/ids"
	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/session"
	"github.com/prismaport/prismaport/internal/store"
)

// Observation is a token transfer into a session address, as reported by the detector after
// its providers agree.
type Observation struct {
	SessionID string
	Chain     string
	Asset     string
	Address   string
	TxHash    string
	LogIndex  uint32
	Amount    money.Units
	// Simulated marks a synthetic test-mode payment from the sandbox simulator.
	Simulated bool
}

// ErrPaymentOnOtherSession is a log already credited to another session. Sessions on one
// customer address share its deposit addresses (B11), so a late check of an earlier session can
// rescan a transfer the open session already recorded: the detectors skip it.
var ErrPaymentOnOtherSession = errors.New("service: payment already recorded on another session")

// ErrSessionSuperseded is a transfer found for a session on a customer address after a newer
// session opened on the address: the newer session owns the watch (static-addresses.md, "Watch
// ownership") and reads the address again, so the detectors leave it to that session.
var ErrSessionSuperseded = store.ErrSessionSuperseded

// RecordPayment registers a detected payment. Re-reporting the same log is a no-op that
// returns the existing payment id. A chain observation for a session on a customer address is
// credited only while the session is the address's current one (ErrSessionSuperseded).
func (s *Service) RecordPayment(ctx context.Context, o Observation) (string, *store.Session, error) {
	id := ids.New(ids.Payment)
	now := s.store.Now()
	apply := s.store.ApplyOwnerCommand
	if o.Simulated {
		apply = s.store.ApplySessionCommand
	}
	ses, _, err := apply(ctx, o.SessionID, func(st session.State) (session.State, []session.Event, error) {
		return st.DetectPayment(session.NewPayment{ID: id, Chain: o.Chain, Asset: o.Asset, Address: o.Address,
			TxHash: o.TxHash, LogIndex: o.LogIndex, Amount: o.Amount, Simulated: o.Simulated}, now)
	})
	if errors.Is(err, session.ErrDuplicatePayment) {
		ses, gerr := s.store.LoadSession(ctx, o.SessionID)
		if gerr != nil {
			return "", nil, gerr
		}
		for _, p := range ses.State.Payments {
			if p.Chain == o.Chain && p.TxHash == o.TxHash && p.LogIndex == o.LogIndex && p.Status != session.Reversed {
				return p.ID, ses, nil
			}
		}
		// The on-chain identity is unique among live payments of every session.
		return "", nil, ErrPaymentOnOtherSession
	}
	if err != nil {
		return "", nil, err
	}
	return id, ses, nil
}

func (s *Service) paymentCommand(ctx context.Context, sessionID string, f func(session.State, time.Time) (session.State, []session.Event, error)) (*store.Session, error) {
	now := s.store.Now()
	ses, _, err := s.store.ApplySessionCommand(ctx, sessionID, func(st session.State) (session.State, []session.Event, error) {
		return f(st, now)
	})
	return ses, err
}

// ConfirmPayment marks a detected payment of the session as confirmed and records the resulting
// events.
func (s *Service) ConfirmPayment(ctx context.Context, sessionID, paymentID string) (*store.Session, error) {
	return s.paymentCommand(ctx, sessionID, func(st session.State, now time.Time) (session.State, []session.Event, error) {
		return st.ConfirmPayment(paymentID, now)
	})
}

// FinalizePayment marks a confirmed payment of the session as final and records the resulting
// events.
func (s *Service) FinalizePayment(ctx context.Context, sessionID, paymentID string) (*store.Session, error) {
	return s.paymentCommand(ctx, sessionID, func(st session.State, now time.Time) (session.State, []session.Event, error) {
		return st.FinalizePayment(paymentID, now)
	})
}

// ReversePayment removes a detected or confirmed payment from the session after a chain
// reorganisation, rolling its status back.
func (s *Service) ReversePayment(ctx context.Context, sessionID, paymentID string) (*store.Session, error) {
	return s.paymentCommand(ctx, sessionID, func(st session.State, now time.Time) (session.State, []session.Event, error) {
		return st.ReversePayment(paymentID, now)
	})
}

// FlagForReview puts the session under review with the given reason and records the resulting
// events.
func (s *Service) FlagForReview(ctx context.Context, sessionID, reason string) (*store.Session, error) {
	return s.paymentCommand(ctx, sessionID, func(st session.State, now time.Time) (session.State, []session.Event, error) {
		return st.FlagForReview(reason, now)
	})
}

// ClearReview takes the session out of review and records the resulting events.
func (s *Service) ClearReview(ctx context.Context, sessionID string) (*store.Session, error) {
	return s.paymentCommand(ctx, sessionID, func(st session.State, now time.Time) (session.State, []session.Event, error) {
		return st.ClearReview(now)
	})
}

// ExpireDue expires every session past its expiry, limit at a time. Sessions with a pending
// detected payment are not listed (they expire once it resolves), nor, with Config.ExpiryHold,
// sessions whose deposit addresses the scanner has not read since expiry
// (store.SessionsDueForExpiry). A session that fails to expire is logged and skipped so it cannot
// hold back the others: it stays first in the list, so a full batch that left one unexpired is
// followed by the batch after it, until a batch is not full or expired all it listed (sessions
// failing every time would otherwise fill every batch, ADR-0008 A-2). Failing sessions are retried
// on every call. The first error is returned once done, so the loop still reports it.
func (s *Service) ExpireDue(ctx context.Context, limit int) (int, error) {
	n, _, err := s.ExpirePass(ctx, limit)
	return n, err
}

// ExpirePass is ExpireDue that also reports whether any session was open or partially paid when
// it started (store.ExpiryScan, no extra statement): the worker's expiry loop keeps the process
// active while one is.
func (s *Service) ExpirePass(ctx context.Context, limit int) (int, bool, error) {
	now := s.store.Now()
	n := 0
	open := false
	var first error
	after := ""
	for {
		idsDue, anyOpen, err := s.store.ExpiryScan(ctx, now, s.cfg.ExpiryHold, after, limit)
		if err != nil {
			return n, open, err
		}
		if after == "" {
			open = anyOpen
		}
		done := 0
		for _, id := range idsDue {
			_, _, err := s.store.ApplySessionCommand(ctx, id, func(st session.State) (session.State, []session.Event, error) {
				return st.Expire(now)
			})
			switch {
			case err == nil:
				done++
			case errors.Is(err, session.ErrPendingPayments), errors.Is(err, session.ErrInvalidTransition), errors.Is(err, session.ErrNotYetExpired):
			case ctx.Err() != nil:
				return n + done, open, err
			default:
				s.Logger().Error("session expiry failed; skipped", "session", id, "err", err)
				if first == nil {
					first = fmt.Errorf("expire session %s: %w", id, err)
				}
			}
		}
		n += done
		if len(idsDue) < limit || done == len(idsDue) {
			return n, open, first
		}
		after = idsDue[len(idsDue)-1]
	}
}
