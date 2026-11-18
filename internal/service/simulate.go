package service

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prismaport/prismaport/internal/apikey"
	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/chainsettle"
	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/payout"
	"github.com/prismaport/prismaport/internal/session"
	"github.com/prismaport/prismaport/internal/store"
	"github.com/prismaport/prismaport/internal/view"
)

// Simulator actions.
const (
	SimPayExact      = "pay_exact"
	SimPayPartial    = "pay_partial"
	SimOverpay       = "overpay"
	SimExpire        = "expire"
	SimLatePayment   = "late_payment"
	SimReverseLast   = "reverse_last_payment"
	SimUnderReview   = "under_review"
	SimSettle        = "settle"
	CodeInvalidState = "invalid_state"
	simReviewReason  = "simulated"
)

// SimActions lists the simulator's actions in the order the docs present them.
var SimActions = []string{SimPayExact, SimPayPartial, SimOverpay, SimExpire, SimLatePayment, SimReverseLast, SimUnderReview, SimSettle}

// SimulateInput names the test-mode action to simulate on a session and its parameters.
type SimulateInput struct {
	Action string
	// Amount is the synthetic payment's amount in the token's units ("10.50"), for pay_partial,
	// overpay and late_payment. Empty picks a default.
	Amount string
	// Chain and Asset pick the pay_to entry to pay; empty picks the first one.
	Chain, Asset string
}

// SimEvent is one event a simulated action emitted.
type SimEvent struct {
	ID             string `json:"id"`
	Type           string `json:"type"`
	SessionVersion int64  `json:"session_version"`
}

// SimulateResult is the outcome of a simulated action: the events it emitted and the session after
// it.
type SimulateResult struct {
	Action  string       `json:"action"`
	Events  []SimEvent   `json:"events"`
	Session view.Session `json:"session"`
}

func invalidState(format string, args ...any) *Error {
	return newErr(KindConflict, CodeInvalidState, format, args...)
}

// Simulate drives a test-mode session through the same commands the detectors and the expiry
// sweeper use (RecordPayment, ConfirmPayment, ReversePayment, the expiry command, FlagForReview),
// one transaction per step (a simulated payment is recorded and confirmed in one), so the
// session, its events, the feed and the webhooks are exactly what production produces for that
// scenario. Synthetic payments are flagged simulated and have
// synthetic transaction hashes; they never get a chain record, so nothing is ever flushed.
func (s *Service) Simulate(ctx context.Context, a Auth, sessionID string, in SimulateInput) (SimulateResult, error) {
	if err := a.Require(apikey.SessionsWrite); err != nil {
		return SimulateResult{}, err
	}
	if err := RequireTestMode(a); err != nil {
		return SimulateResult{}, err
	}
	ses, err := s.store.GetSession(ctx, a.MerchantID, false, sessionID)
	if errors.Is(err, store.ErrNotFound) {
		return SimulateResult{}, sessionNotFound()
	}
	if err != nil {
		return SimulateResult{}, err
	}
	start := ses.State.Version
	// An open-amount session has no amount to pay exactly: pay_exact pays the amount given.
	openPay := ses.State.OpenAmount && in.Action == SimPayExact
	if in.Amount != "" && in.Action != SimPayPartial && in.Action != SimOverpay && in.Action != SimLatePayment && !openPay {
		return SimulateResult{}, invalid("amount applies only to pay_partial, overpay and late_payment, and to pay_exact on an open-amount session")
	}
	switch in.Action {
	case SimPayExact, SimPayPartial, SimOverpay:
		err = s.simPay(ctx, ses, in)
	case SimExpire:
		err = s.simExpire(ctx, a, ses)
	case SimLatePayment:
		if ses.State.Status == session.Open || ses.State.Status == session.PartiallyPaid {
			if err = s.simExpire(ctx, a, ses); err == nil {
				ses, err = s.store.LoadSession(ctx, ses.ID)
			}
		}
		if err == nil {
			err = s.simPay(ctx, ses, in)
		}
	case SimReverseLast:
		err = s.simReverse(ctx, ses)
	case SimUnderReview:
		if ses.State.Status == session.UnderReview {
			return SimulateResult{}, invalidState("the session is already under review")
		}
		_, err = s.FlagForReview(ctx, ses.ID, simReviewReason)
	case SimSettle:
		err = s.simSettle(ctx, ses)
	case "":
		return SimulateResult{}, invalid("action is required: one of pay_exact, pay_partial, overpay, expire, late_payment, reverse_last_payment, under_review, settle")
	default:
		return SimulateResult{}, invalid("unknown action %q", in.Action)
	}
	if err != nil {
		return SimulateResult{}, simError(err)
	}
	return s.simResult(ctx, in.Action, ses.ID, start)
}

func simError(err error) error {
	switch {
	case errors.Is(err, session.ErrInvalidTransition):
		return invalidState("the session cannot do that in its current state")
	case errors.Is(err, session.ErrAmountOutOfRange):
		return invalid("amount is out of range")
	case errors.Is(err, session.ErrPendingPayments):
		return invalidState("a detected payment is still pending confirmation")
	}
	return err
}

func (s *Service) simResult(ctx context.Context, action, sessionID string, after int64) (SimulateResult, error) {
	ses, err := s.store.LoadSession(ctx, sessionID)
	if err != nil {
		return SimulateResult{}, err
	}
	evs, err := s.store.SessionEvents(ctx, sessionID)
	if err != nil {
		return SimulateResult{}, err
	}
	out := SimulateResult{Action: action, Events: []SimEvent{}, Session: s.view.Session(ses)}
	for _, e := range evs {
		if e.SessionVersion > after {
			out.Events = append(out.Events, SimEvent{ID: e.ID, Type: e.Type, SessionVersion: e.SessionVersion})
		}
	}
	return out, nil
}

// payTarget picks the pay_to entry a synthetic payment goes to.
func payTarget(ses *store.Session, in SimulateInput) (store.PayToRecord, error) {
	for _, pt := range ses.PayTo {
		if (in.Chain == "" || pt.Chain == in.Chain) && (in.Asset == "" || pt.Asset == in.Asset) {
			return pt, nil
		}
	}
	return store.PayToRecord{}, invalid("the session has no pay_to entry for chain %q and asset %q", in.Chain, in.Asset)
}

// remainingIn is what is still due, in the entry's token units (rounded up to a whole unit).
func remainingIn(st session.State, pt store.PayToRecord) (money.Units, error) {
	return chain.FromCanonicalUp(st.Remaining(), pt.Decimals)
}

func (s *Service) simPay(ctx context.Context, ses *store.Session, in SimulateInput) error {
	pt, err := payTarget(ses, in)
	if err != nil {
		return err
	}
	st := ses.State
	if err := checkSimPayState(st, in.Action); err != nil {
		return err
	}
	rem, err := remainingIn(st, pt)
	if err != nil {
		return err
	}
	given, err := parseSimAmount(in, pt)
	if err != nil {
		return err
	}
	var amount money.Units
	if st.OpenAmount {
		amount, err = simOpenAmount(in, given)
	} else {
		amount, err = simFixedAmount(in, pt, rem, given)
	}
	if err != nil {
		return err
	}
	return s.recordSimPayment(ctx, ses, pt, amount)
}

// checkSimPayState refuses a simulated payment the session cannot take: a late payment needs an
// expired session (or one already late), any other payment an open or partially paid one.
func checkSimPayState(st session.State, action string) error {
	if action == SimLatePayment {
		if st.Status != session.Expired && st.Status != session.LatePayment {
			return invalidState("a late payment needs an open, partially paid or expired session; this one is %s", st.Status)
		}
		return nil
	}
	if st.Status != session.Open && st.Status != session.PartiallyPaid {
		return invalidState("the session is %s: only open and partially paid sessions take payments before expiry", st.Status)
	}
	return nil
}

// parseSimAmount reads the request's amount in the pay target's decimals; zero when none was
// given.
func parseSimAmount(in SimulateInput, pt store.PayToRecord) (money.Units, error) {
	if in.Amount == "" {
		return money.Zero, nil
	}
	amount, err := money.ParseDecimal(in.Amount, pt.Decimals)
	if err != nil {
		return money.Zero, invalid("amount must be a positive decimal with at most %d places, in %s", pt.Decimals, pt.Asset)
	}
	if amount.IsZero() {
		return money.Zero, invalid("amount must be greater than zero")
	}
	return amount, nil
}

// simOpenAmount is the payment of an open-amount session: what was sent is what was paid, the
// amount as given, which is required. Such a session is never partially paid or overpaid.
func simOpenAmount(in SimulateInput, given money.Units) (money.Units, error) {
	switch {
	case in.Action == SimPayPartial || in.Action == SimOverpay:
		return money.Zero, invalidState("an open-amount session is never partially paid or overpaid; use pay_exact with an amount")
	case in.Amount == "":
		return money.Zero, invalid("amount is required on an open-amount session: the payer decides how much to send")
	}
	return given, nil
}

// simFixedAmount is the payment of a fixed-amount session with rem still due: exact pays rem;
// partial and overpay take the amount given, below or above rem, or default to half of rem and
// rem plus one unit; a late payment defaults to rem, or the whole amount when nothing is due.
func simFixedAmount(in SimulateInput, pt store.PayToRecord, rem, given money.Units) (money.Units, error) {
	switch in.Action {
	case SimPayExact:
		if rem.IsZero() {
			return money.Zero, invalidState("nothing is left to pay on this session")
		}
		return rem, nil
	case SimPayPartial:
		if rem.Cmp(money.FromUint64(2)) < 0 {
			return money.Zero, invalidState("too little is left to pay to make a partial payment")
		}
		if in.Amount == "" {
			return money.MulDivDown(rem, 1, 2)
		}
		if given.Cmp(rem) >= 0 {
			return money.Zero, invalid("a partial payment must be below the %s %s still due", money.FormatDecimal(rem, pt.Decimals), pt.Asset)
		}
	case SimOverpay:
		if in.Amount == "" {
			return rem.Add(money.Pow10(pt.Decimals))
		}
		if given.Cmp(rem) <= 0 {
			return money.Zero, invalid("an overpayment must be above the %s %s still due", money.FormatDecimal(rem, pt.Decimals), pt.Asset)
		}
	case SimLatePayment:
		if in.Amount == "" {
			if rem.IsZero() {
				return pt.Amount, nil
			}
			return rem, nil
		}
	}
	return given, nil
}

// recordSimPayment records a synthetic payment of amount to pt and confirms it.
func (s *Service) recordSimPayment(ctx context.Context, ses *store.Session, pt store.PayToRecord, amount money.Units) error {
	family := chain.EVM
	if c, ok := s.cfg.Registry.Get(pt.Chain); ok {
		family = c.Family
	}
	// Recorded and confirmed in one transaction (ADR-0008 A-3): no chain loop ever confirms a
	// synthetic payment, so a detected one left behind by a failed confirm would never resolve
	// and would keep the session from expiring.
	obs := Observation{SessionID: ses.ID, Chain: pt.Chain, Asset: pt.Asset, Address: pt.Address,
		TxHash: SyntheticTxHash(family), Amount: amount, Simulated: true}
	return pgx.BeginFunc(ctx, s.store.Pool(), func(tx pgx.Tx) error {
		b := s.bind(tx)
		pid, _, err := b.RecordPayment(ctx, obs)
		if err != nil {
			return err
		}
		_, err = b.ConfirmPayment(ctx, ses.ID, pid)
		return err
	})
}

func (s *Service) simExpire(ctx context.Context, a Auth, ses *store.Session) error {
	st := ses.State
	if st.Status != session.Open && st.Status != session.PartiallyPaid {
		return invalidState("only open and partially paid sessions expire; this one is %s", st.Status)
	}
	for _, p := range st.Payments {
		if p.Status == session.Detected {
			return invalidState("a detected payment is still pending confirmation")
		}
	}
	// A session's expiry must stay after its creation, even when both fall in one microsecond.
	at := s.store.Now()
	if !at.After(ses.CreatedAt) {
		at = ses.CreatedAt.Add(time.Microsecond)
	}
	if err := s.store.ExpireNowForSimulation(ctx, a.MerchantID, ses.ID, at); err != nil {
		return err
	}
	_, _, err := s.store.ApplySessionCommand(ctx, ses.ID, func(st session.State) (session.State, []session.Event, error) {
		return st.Expire(at)
	})
	return err
}

func (s *Service) simReverse(ctx context.Context, ses *store.Session) error {
	for i := len(ses.State.Payments) - 1; i >= 0; i-- {
		p := ses.State.Payments[i]
		if p.Simulated && (p.Status == session.Detected || p.Status == session.Confirmed) {
			_, err := s.ReversePayment(ctx, ses.ID, p.ID)
			return err
		}
	}
	return invalidState("the session has no simulated payment that can be reversed")
}

// simSettle forwards a closed session's counted simulated payments as the flusher would: per
// pay_to entry, what arrived and was not settled yet is finalized and split by the fee rule and
// the session's payout snapshot, and recorded as a settlement (flagged simulated, with a
// synthetic transaction hash). Like a real settlement it emits no event; the session's version
// grows. An amount too small to settle after the fee stays unsettled, as dust does on chain.
func (s *Service) simSettle(ctx context.Context, ses *store.Session) error {
	switch ses.State.Status {
	case session.Paid, session.Overpaid, session.Expired, session.LatePayment:
	default:
		return invalidState("only closed sessions are settled (paid, overpaid, expired or late payment); this one is %s", ses.State.Status)
	}
	settled := 0
	for _, pt := range ses.PayTo {
		gross := money.Zero
		var ids []string
		for _, p := range ses.State.Payments {
			if p.Chain != pt.Chain || p.Asset != pt.Asset {
				continue
			}
			if p.Status == session.Detected {
				return invalidState("a detected payment is still pending confirmation")
			}
			if p.Status != session.Confirmed && p.Status != session.Finalized {
				continue
			}
			if !p.Simulated {
				return invalidState("the session holds a real payment; only the flusher settles it")
			}
			var err error
			if gross, err = gross.Add(p.Amount); err != nil {
				return err
			}
			if p.Status == session.Confirmed {
				ids = append(ids, p.ID)
			}
		}
		for _, st := range ses.Settlements {
			if st.Chain == pt.Chain && st.Asset == pt.Asset {
				var err error
				if gross, err = gross.Sub(st.Gross); err != nil {
					return err
				}
			}
		}
		if gross.IsZero() {
			continue
		}
		rule, err := payout.Compute(gross, pt.Decimals, pt.Fee, payout.Bps(pt.Recipients))
		if err != nil {
			return err
		}
		if rule.Dust {
			continue
		}
		for _, id := range ids {
			if _, err := s.FinalizePayment(ctx, ses.ID, id); err != nil {
				return err
			}
		}
		family := chain.EVM
		if c, ok := s.cfg.Registry.Get(pt.Chain); ok {
			family = c.Family
		}
		if _, err := s.store.RecordSettlement(ctx, ses.ID, store.Settlement{Chain: pt.Chain, Asset: pt.Asset, Gross: gross, Fee: rule.Fee,
			Net: rule.Net, Shares: rule.Shares, TxHash: SyntheticTxHash(family), SettledAt: s.store.Now(), Simulated: true}, chainsettle.Verify); err != nil {
			return err
		}
		settled++
	}
	if settled == 0 {
		return invalidState("nothing to settle: no counted payment large enough to settle after the fee is waiting to be forwarded")
	}
	return nil
}
