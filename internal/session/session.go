// Package session is the pure session/payment state machine. It does no I/O: every command takes
// a State and returns a new State plus the events it produced. Callers persist both atomically.
//
// Rules:
//   - A session is paid only when the confirmed sum equals the amount exactly (no tolerance).
//   - An open-amount session (static customer addresses, B11) has no amount: it is paid once the
//     confirmed sum on one pay_to entry settles after that entry's fee (D35: dust never pays),
//     and it is never partially paid or overpaid.
//   - Only confirmed and finalized payments count; detected ones are pending, reversed ones leave
//     the sum and the status is recomputed.
//   - Every state change increments Version by exactly one.
package session

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/fee"
	"github.com/prismaport/prismaport/internal/money"
)

// Status is the lifecycle state of a session.
type Status string

// Session statuses.
const (
	Open          Status = "open"
	PartiallyPaid Status = "partially_paid"
	Paid          Status = "paid"
	Overpaid      Status = "overpaid"
	Expired       Status = "expired"
	LatePayment   Status = "late_payment"
	UnderReview   Status = "under_review"
)

// AllStatuses lists every session status.
var AllStatuses = []Status{Open, PartiallyPaid, Paid, Overpaid, Expired, LatePayment, UnderReview}

// Valid reports whether s is a known session status.
func (s Status) Valid() bool { return slices.Contains(AllStatuses, s) }

// PaymentStatus is the state of one payment on a session.
type PaymentStatus string

// Payment statuses.
const (
	Detected  PaymentStatus = "detected"
	Confirmed PaymentStatus = "confirmed"
	Finalized PaymentStatus = "finalized"
	Reversed  PaymentStatus = "reversed"
)

// AllPaymentStatuses lists every payment status.
var AllPaymentStatuses = []PaymentStatus{Detected, Confirmed, Finalized, Reversed}

// Valid reports whether p is a known payment status.
func (p PaymentStatus) Valid() bool { return slices.Contains(AllPaymentStatuses, p) }

// Counts reports whether a payment in this status is part of the received sum.
func (p PaymentStatus) Counts() bool { return p == Confirmed || p == Finalized }

// EventType names an event a session emits.
type EventType string

// Session event types.
const (
	EventPaymentDetected EventType = "session.payment_detected"
	EventPartiallyPaid   EventType = "session.partially_paid"
	EventPaid            EventType = "session.paid"
	EventOverpaid        EventType = "session.overpaid"
	EventExpired         EventType = "session.expired"
	EventLatePayment     EventType = "session.late_payment"
	EventUnderReview     EventType = "session.under_review"
	EventPaymentReversed EventType = "payment.reversed"
)

// AllEventTypes lists every type a webhook endpoint may subscribe to.
var AllEventTypes = []EventType{
	EventPaymentDetected, EventPartiallyPaid, EventPaid, EventOverpaid,
	EventExpired, EventLatePayment, EventUnderReview, EventPaymentReversed,
}

// Valid reports whether e is a known event type.
func (e EventType) Valid() bool {
	for _, t := range AllEventTypes {
		if t == e {
			return true
		}
	}
	return false
}

// Errors returned when a session rejects a transition or payment.
var (
	ErrInvalidTransition = errors.New("session: invalid transition")
	ErrUnknownPayment    = errors.New("session: unknown payment")
	ErrDuplicatePayment  = errors.New("session: payment already recorded")
	ErrUnknownPayTo      = errors.New("session: payment does not match any pay_to entry")
	ErrZeroPayment       = errors.New("session: payment amount is zero")
	ErrNotYetExpired     = errors.New("session: expiry time not reached")
	ErrPendingPayments   = errors.New("session: detected payments still pending")
	ErrAmountOutOfRange  = errors.New("session: payment amount out of range")
	// ErrInvalidFee is a pay_to entry whose fee rule cannot be applied to its payments.
	ErrInvalidFee = errors.New("session: pay_to fee rule is invalid")
)

// PayTo is one place a payer may send funds, with the exact amount in the asset's base units
// (zero on an open-amount session) and the fee its forwarder takes.
type PayTo struct {
	Chain    string
	Asset    string
	Address  string
	Decimals uint8
	Amount   money.Units
	Fee      fee.Params
}

// Payment is one observed on-chain payment applied to a session.
type Payment struct {
	ID          string
	Chain       string
	Asset       string
	Address     string
	TxHash      string
	LogIndex    uint32
	Amount      money.Units
	Decimals    uint8
	Status      PaymentStatus
	Late        bool
	DetectedAt  time.Time
	ConfirmedAt *time.Time
	FinalizedAt *time.Time
	ReversedAt  *time.Time
	// Simulated marks a synthetic test-mode payment made by the sandbox simulator.
	Simulated bool
}

// State is everything the machine needs. AmountCents is the USD amount snapshot; it is zero
// when OpenAmount is set (the session has no amount: the payer decides how much to send).
type State struct {
	Version      int64
	Status       Status
	AmountCents  money.Units
	OpenAmount   bool
	PayTo        []PayTo
	Payments     []Payment
	ExpiresAt    time.Time
	ExpiredAt    *time.Time
	PaidAt       *time.Time
	ReviewReason string
}

// Event is produced by a command. Version is the session version it describes.
type Event struct {
	Type      EventType
	Version   int64
	PaymentID string
}

// New returns the initial state of a session.
func New(amountCents money.Units, payTo []PayTo, expiresAt time.Time) State {
	return State{
		Version:     1,
		Status:      Open,
		AmountCents: amountCents,
		PayTo:       append([]PayTo(nil), payTo...),
		ExpiresAt:   expiresAt,
	}
}

// NewOpenAmount returns the initial state of an open-amount session.
func NewOpenAmount(payTo []PayTo, expiresAt time.Time) State {
	st := New(money.Zero, payTo, expiresAt)
	st.OpenAmount = true
	return st
}

func (s State) clone() State {
	c := s
	c.PayTo = append([]PayTo(nil), s.PayTo...)
	c.Payments = append([]Payment(nil), s.Payments...)
	return c
}

// Validate checks every amount the state's value methods compute: the session amount and each
// payment in the canonical scale, the sum of all payments, and, on an open-amount session, each
// pay_to entry's sum after its fee. The store validates a state it reads or writes and
// DetectPayment the state it returns, so AmountCanonical, Received and the status never meet
// an amount out of range; they panic if one gets past (a broken invariant, not an input error).
func (s State) Validate() error {
	if _, err := money.Rescale(s.AmountCents, money.USDDecimals, chain.CanonicalDecimals); err != nil {
		return fmt.Errorf("%w: amount %s: %v", ErrAmountOutOfRange, s.AmountCents, err)
	}
	total := money.Zero
	for _, p := range s.Payments {
		c, err := chain.ToCanonical(p.Amount, p.Decimals)
		if err == nil {
			total, err = total.Add(c)
		}
		if err != nil {
			return fmt.Errorf("%w: payment %s: %v", ErrAmountOutOfRange, p.ID, err)
		}
	}
	if !s.OpenAmount {
		return nil
	}
	for _, pt := range s.PayTo {
		sum, err := s.entrySum(pt, func(Payment) bool { return true })
		if err != nil {
			return fmt.Errorf("%w: %s/%s: %v", ErrAmountOutOfRange, pt.Chain, pt.Asset, err)
		}
		if sum.IsZero() {
			continue
		}
		if err := pt.Fee.Validate(); err != nil {
			return fmt.Errorf("%w: %s/%s: %v", ErrInvalidFee, pt.Chain, pt.Asset, err)
		}
		if _, err := fee.Compute(sum, pt.Decimals, pt.Fee); err != nil {
			return fmt.Errorf("%w: %s/%s fee: %v", ErrAmountOutOfRange, pt.Chain, pt.Asset, err)
		}
	}
	return nil
}

// invariant panics on an amount Validate rules out.
func invariant(what string, err error) {
	panic(fmt.Sprintf("session: %s: %v (State.Validate admits no such state)", what, err))
}

// AmountCanonical is the session amount in the canonical scale.
func (s State) AmountCanonical() money.Units {
	u, err := money.Rescale(s.AmountCents, money.USDDecimals, chain.CanonicalDecimals)
	if err != nil {
		invariant("amount "+s.AmountCents.String()+" does not fit the canonical scale", err)
	}
	return u
}

// Received is the confirmed sum in the canonical scale.
func (s State) Received() money.Units {
	sum := money.Zero
	for _, p := range s.Payments {
		if !p.Status.Counts() {
			continue
		}
		c, err := chain.ToCanonical(p.Amount, p.Decimals)
		if err != nil {
			invariant("payment "+p.ID+" does not fit the canonical scale", err)
		}
		if sum, err = sum.Add(c); err != nil {
			invariant("received sum overflow", err)
		}
	}
	return sum
}

// Remaining is max(0, amount - received) in the canonical scale; always zero on an open-amount
// session.
func (s State) Remaining() money.Units {
	r, err := s.AmountCanonical().Sub(s.Received())
	if err != nil {
		return money.Zero
	}
	return r
}

// Settled reports whether the session was paid exactly and every payment is final (finalized or
// reversed), and since when: the last payment's finality, or the paid time if it is later. Such a
// session leaves every automatic check (D47); only "Check again" still reaches it.
func (s State) Settled() (time.Time, bool) {
	if s.Status != Paid {
		return time.Time{}, false
	}
	var at time.Time
	if s.PaidAt != nil {
		at = *s.PaidAt
	}
	for _, p := range s.Payments {
		var final *time.Time
		switch p.Status {
		case Finalized:
			final = p.FinalizedAt
		case Reversed:
			final = p.ReversedAt
		default:
			return time.Time{}, false
		}
		if final != nil && final.After(at) {
			at = *final
		}
	}
	return at, true
}

func (s State) paymentIndex(id string) int {
	for i, p := range s.Payments {
		if p.ID == id {
			return i
		}
	}
	return -1
}

// NewPayment is an observation reported by the detector.
type NewPayment struct {
	ID       string
	Chain    string
	Asset    string
	Address  string
	TxHash   string
	LogIndex uint32
	Amount   money.Units
	// Simulated marks a synthetic payment from the test-mode simulator, not from a chain.
	Simulated bool
}

// DetectPayment records a payment seen on-chain but not yet confirmed. It does not change the
// received sum. A payment detected after the session expired is marked late.
func (s State) DetectPayment(np NewPayment, now time.Time) (State, []Event, error) {
	if np.Amount.IsZero() {
		return s, nil, ErrZeroPayment
	}
	var target *PayTo
	for i := range s.PayTo {
		pt := s.PayTo[i]
		if pt.Chain == np.Chain && pt.Asset == np.Asset && pt.Address == np.Address {
			target = &pt
			break
		}
	}
	if target == nil {
		return s, nil, ErrUnknownPayTo
	}
	for _, p := range s.Payments {
		// A reversed log may legitimately be mined again after a reorg.
		sameLog := p.Chain == np.Chain && p.TxHash == np.TxHash && p.LogIndex == np.LogIndex
		if p.ID == np.ID || (sameLog && p.Status != Reversed) {
			return s, nil, ErrDuplicatePayment
		}
	}
	// Bound every live payment in the canonical scale up front so Received can never overflow.
	total, err := chain.ToCanonical(np.Amount, target.Decimals)
	if err != nil {
		return s, nil, ErrAmountOutOfRange
	}
	for _, p := range s.Payments {
		if p.Status == Reversed {
			continue
		}
		c, err := chain.ToCanonical(p.Amount, p.Decimals)
		if err == nil {
			total, err = total.Add(c)
		}
		if err != nil {
			return s, nil, ErrAmountOutOfRange
		}
	}
	n := s.clone()
	n.Payments = append(n.Payments, Payment{
		ID: np.ID, Chain: np.Chain, Asset: np.Asset, Address: np.Address, TxHash: np.TxHash,
		LogIndex: np.LogIndex, Amount: np.Amount, Decimals: target.Decimals, Status: Detected,
		Late: n.ExpiredAt != nil, DetectedAt: now, Simulated: np.Simulated,
	})
	// Every later transition only changes statuses: a state valid now stays valid.
	if err := n.Validate(); err != nil {
		return s, nil, err
	}
	n.Version++
	return n, []Event{{Type: EventPaymentDetected, Version: n.Version, PaymentID: np.ID}}, nil
}

// ConfirmPayment counts a detected payment toward the sum and recomputes the status.
func (s State) ConfirmPayment(id string, now time.Time) (State, []Event, error) {
	i := s.paymentIndex(id)
	if i < 0 {
		return s, nil, ErrUnknownPayment
	}
	if s.Payments[i].Status != Detected {
		return s, nil, fmt.Errorf("%w: payment %s is %s", ErrInvalidTransition, id, s.Payments[i].Status)
	}
	n := s.clone()
	t := now
	n.Payments[i].Status = Confirmed
	n.Payments[i].ConfirmedAt = &t
	n.Version++
	n.recompute(now)
	// A paid open-amount session keeps crediting payments without a second session.paid; the
	// payment was announced when it was detected.
	if n.OpenAmount && s.Status == Paid && n.Status == Paid {
		return n, nil, nil
	}
	return n, n.statusEvent(id), nil
}

// FinalizePayment marks a confirmed payment as final. The sum and status do not change.
func (s State) FinalizePayment(id string, now time.Time) (State, []Event, error) {
	i := s.paymentIndex(id)
	if i < 0 {
		return s, nil, ErrUnknownPayment
	}
	if s.Payments[i].Status != Confirmed {
		return s, nil, fmt.Errorf("%w: payment %s is %s", ErrInvalidTransition, id, s.Payments[i].Status)
	}
	n := s.clone()
	t := now
	n.Payments[i].Status = Finalized
	n.Payments[i].FinalizedAt = &t
	n.Version++
	return n, nil, nil
}

// ReversePayment removes a detected or confirmed payment (reorg) and rolls the status back.
// Finalized payments cannot be reversed.
func (s State) ReversePayment(id string, now time.Time) (State, []Event, error) {
	i := s.paymentIndex(id)
	if i < 0 {
		return s, nil, ErrUnknownPayment
	}
	if st := s.Payments[i].Status; st != Detected && st != Confirmed {
		return s, nil, fmt.Errorf("%w: payment %s is %s", ErrInvalidTransition, id, st)
	}
	n := s.clone()
	t := now
	n.Payments[i].Status = Reversed
	n.Payments[i].ReversedAt = &t
	n.Version++
	n.recompute(now)
	return n, []Event{{Type: EventPaymentReversed, Version: n.Version, PaymentID: id}}, nil
}

// Expire closes an open or partially paid session once its expiry time has passed. It refuses
// while a detected payment is pending, so an on-time payment is never turned into a late one by
// slow confirmations.
func (s State) Expire(now time.Time) (State, []Event, error) {
	if s.Status != Open && s.Status != PartiallyPaid {
		return s, nil, fmt.Errorf("%w: cannot expire a %s session", ErrInvalidTransition, s.Status)
	}
	if now.Before(s.ExpiresAt) {
		return s, nil, ErrNotYetExpired
	}
	for _, p := range s.Payments {
		if p.Status == Detected {
			return s, nil, ErrPendingPayments
		}
	}
	n := s.clone()
	t := now
	n.ExpiredAt = &t
	n.Version++
	n.recompute(now)
	return n, []Event{{Type: EventExpired, Version: n.Version}}, nil
}

// FlagForReview holds the session out of the paid flow after a screening hit.
func (s State) FlagForReview(reason string, now time.Time) (State, []Event, error) {
	if s.Status == UnderReview {
		return s, nil, fmt.Errorf("%w: already under review", ErrInvalidTransition)
	}
	n := s.clone()
	n.Status = UnderReview
	n.ReviewReason = reason
	n.Version++
	return n, []Event{{Type: EventUnderReview, Version: n.Version}}, nil
}

// ClearReview returns a reviewed session to the status its payments imply.
func (s State) ClearReview(now time.Time) (State, []Event, error) {
	if s.Status != UnderReview {
		return s, nil, fmt.Errorf("%w: not under review", ErrInvalidTransition)
	}
	n := s.clone()
	n.ReviewReason = ""
	n.Status = Open
	n.Version++
	n.recompute(now)
	return n, n.statusEvent(""), nil
}

// derivedStatus is the status implied by the payments, ignoring review.
func (s State) derivedStatus() Status {
	if s.ExpiredAt != nil {
		for _, p := range s.Payments {
			if p.Late && p.Status.Counts() {
				return LatePayment
			}
		}
		return Expired
	}
	if s.OpenAmount {
		if s.settles() {
			return Paid
		}
		return Open
	}
	recv, amt := s.Received(), s.AmountCanonical()
	switch c := recv.Cmp(amt); {
	case recv.IsZero() && !amt.IsZero():
		return Open
	case c < 0:
		return PartiallyPaid
	case c == 0:
		return Paid
	default:
		return Overpaid
	}
}

// settles reports whether the confirmed sum on some pay_to entry settles after its fee: a flush
// moves one (address, token) at a time, so sums on different entries are never added.
func (s State) settles() bool {
	for _, pt := range s.PayTo {
		sum, err := s.entrySum(pt, func(p Payment) bool { return p.Status.Counts() })
		if err != nil {
			invariant("received sum overflow", err)
		}
		if sum.IsZero() {
			continue
		}
		split, err := fee.Compute(sum, pt.Decimals, pt.Fee)
		if err != nil {
			invariant("fee rule of "+pt.Chain+"/"+pt.Asset, err)
		}
		if !split.Dust {
			return true
		}
	}
	return false
}

func (s *State) recompute(now time.Time) {
	if s.Status == UnderReview {
		return
	}
	s.Status = s.derivedStatus()
	switch s.Status {
	case Paid, Overpaid:
		if s.PaidAt == nil {
			t := now
			s.PaidAt = &t
		}
	default:
		s.PaidAt = nil
	}
}

// statusEvent is the event announcing the current status after a sum change, if any.
func (s State) statusEvent(paymentID string) []Event {
	var t EventType
	switch s.Status {
	case PartiallyPaid:
		t = EventPartiallyPaid
	case Paid:
		t = EventPaid
	case Overpaid:
		t = EventOverpaid
	case LatePayment:
		t = EventLatePayment
	case Expired:
		t = EventExpired
	default:
		return nil
	}
	return []Event{{Type: t, Version: s.Version, PaymentID: paymentID}}
}

// entrySum is the sum, in the asset's base units, of the payments to pt that count.
func (s State) entrySum(pt PayTo, counts func(Payment) bool) (money.Units, error) {
	sum := money.Zero
	for _, p := range s.Payments {
		if !counts(p) || p.Chain != pt.Chain || p.Asset != pt.Asset || p.Address != pt.Address {
			continue
		}
		var err error
		if sum, err = sum.Add(p.Amount); err != nil {
			return money.Zero, err
		}
	}
	return sum, nil
}
