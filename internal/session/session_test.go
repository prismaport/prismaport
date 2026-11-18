package session

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/money"
	"pgregory.net/rapid"
)

var t0 = time.Date(2026, 10, 23, 18, 0, 0, 0, time.UTC)

func usd(s string) money.Units {
	u, err := money.ParseUSD(s)
	if err != nil {
		panic(err)
	}
	return u
}

func units(s string, dec uint8) money.Units {
	u, err := money.ParseDecimal(s, dec)
	if err != nil {
		panic(err)
	}
	return u
}

func founder() State {
	return New(usd("25.00"), []PayTo{
		{Chain: "base", Asset: "USDC", Address: "0xabc", Decimals: 6, Amount: units("25", 6)},
		{Chain: "solana", Asset: "USDT", Address: "9xQ", Decimals: 6, Amount: units("25", 6)},
		{Chain: "bsc", Asset: "USDT", Address: "0xabc", Decimals: 18, Amount: units("25", 18)},
	}, t0.Add(time.Hour))
}

type step func(State) (State, []Event, error)

func must(t *testing.T, s State, f step, wantStatus Status, wantEvents ...EventType) State {
	t.Helper()
	n, evs, err := f(s)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n.Status != wantStatus {
		t.Fatalf("status %s, want %s", n.Status, wantStatus)
	}
	if n.Version != s.Version+1 {
		t.Fatalf("version %d after %d", n.Version, s.Version)
	}
	if len(evs) != len(wantEvents) {
		t.Fatalf("events %v, want %v", evs, wantEvents)
	}
	for i, e := range evs {
		if e.Type != wantEvents[i] || e.Version != n.Version {
			t.Fatalf("event %d = %+v, want %s@%d", i, e, wantEvents[i], n.Version)
		}
	}
	return n
}

func detect(id, chainID, asset, addr, amount string, dec uint8, at time.Time) step {
	return func(s State) (State, []Event, error) {
		return s.DetectPayment(NewPayment{ID: id, Chain: chainID, Asset: asset, Address: addr, TxHash: "tx-" + id, Amount: units(amount, dec)}, at)
	}
}

func confirm(id string, at time.Time) step {
	return func(s State) (State, []Event, error) { return s.ConfirmPayment(id, at) }
}

func reverse(id string, at time.Time) step {
	return func(s State) (State, []Event, error) { return s.ReversePayment(id, at) }
}

func expire(at time.Time) step { return func(s State) (State, []Event, error) { return s.Expire(at) } }

func TestExactPaymentIsPaid(t *testing.T) {
	s := founder()
	s = must(t, s, detect("p1", "base", "USDC", "0xabc", "25", 6, t0), Open, EventPaymentDetected)
	s = must(t, s, confirm("p1", t0.Add(time.Minute)), Paid, EventPaid)
	if s.PaidAt == nil || !s.PaidAt.Equal(t0.Add(time.Minute)) {
		t.Fatalf("paid_at %v", s.PaidAt)
	}
	s = must(t, s, func(s State) (State, []Event, error) { return s.FinalizePayment("p1", t0.Add(time.Hour)) }, Paid)
	if _, _, err := s.Expire(t0.Add(2 * time.Hour)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("paid session expired: %v", err)
	}
	if _, _, err := s.ReversePayment("p1", t0); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("finalized payment reversed: %v", err)
	}
}

func TestOneBaseUnitShortIsPartial(t *testing.T) {
	s := founder()
	s = must(t, s, detect("p1", "base", "USDC", "0xabc", "24.999999", 6, t0), Open, EventPaymentDetected)
	s = must(t, s, confirm("p1", t0), PartiallyPaid, EventPartiallyPaid)
	if got := money.FormatDecimal(s.Remaining(), 18); got != "0.000001000000000000" {
		t.Fatalf("remaining %s", got)
	}
	s = must(t, s, detect("p2", "base", "USDC", "0xabc", "0.000001", 6, t0), PartiallyPaid, EventPaymentDetected)
	must(t, s, confirm("p2", t0), Paid, EventPaid)
}

func TestMixedDecimalsSumExactly(t *testing.T) {
	s := founder()
	s = must(t, s, detect("p1", "solana", "USDT", "9xQ", "10", 6, t0), Open, EventPaymentDetected)
	s = must(t, s, confirm("p1", t0), PartiallyPaid, EventPartiallyPaid)
	s = must(t, s, detect("p2", "bsc", "USDT", "0xabc", "15.000000000000000001", 18, t0), PartiallyPaid, EventPaymentDetected)
	s = must(t, s, confirm("p2", t0), Overpaid, EventOverpaid)
	if s.PaidAt == nil {
		t.Fatal("overpaid without paid_at")
	}
}

func TestOverpaidAndReversalRollsBack(t *testing.T) {
	s := founder()
	s = must(t, s, detect("p1", "base", "USDC", "0xabc", "25", 6, t0), Open, EventPaymentDetected)
	s = must(t, s, confirm("p1", t0), Paid, EventPaid)
	s = must(t, s, detect("p2", "base", "USDC", "0xabc", "1", 6, t0), Paid, EventPaymentDetected)
	s = must(t, s, confirm("p2", t0), Overpaid, EventOverpaid)
	s = must(t, s, reverse("p2", t0), Paid, EventPaymentReversed)
	s = must(t, s, reverse("p1", t0), Open, EventPaymentReversed)
	if s.PaidAt != nil || !s.Received().IsZero() {
		t.Fatalf("rollback incomplete: paid_at=%v received=%s", s.PaidAt, s.Received())
	}
}

func TestExpiryAndLatePayment(t *testing.T) {
	s := founder()
	if _, _, err := s.Expire(t0); !errors.Is(err, ErrNotYetExpired) {
		t.Fatalf("expired early: %v", err)
	}
	s = must(t, s, detect("p1", "base", "USDC", "0xabc", "10", 6, t0), Open, EventPaymentDetected)
	if _, _, err := s.Expire(t0.Add(2 * time.Hour)); !errors.Is(err, ErrPendingPayments) {
		t.Fatalf("expired with a pending payment: %v", err)
	}
	s = must(t, s, confirm("p1", t0.Add(2*time.Hour)), PartiallyPaid, EventPartiallyPaid)
	s = must(t, s, expire(t0.Add(2*time.Hour)), Expired, EventExpired)
	s = must(t, s, detect("p2", "base", "USDC", "0xabc", "15", 6, t0.Add(3*time.Hour)), Expired, EventPaymentDetected)
	if !s.Payments[1].Late {
		t.Fatal("payment after expiry not marked late")
	}
	s = must(t, s, confirm("p2", t0.Add(3*time.Hour)), LatePayment, EventLatePayment)
	if s.PaidAt != nil {
		t.Fatal("late payment must not set paid_at")
	}
	if !s.Received().Equal(s.AmountCanonical()) {
		t.Fatal("received should equal amount")
	}
	s = must(t, s, reverse("p2", t0.Add(4*time.Hour)), Expired, EventPaymentReversed)
}

func TestReview(t *testing.T) {
	s := founder()
	s = must(t, s, detect("p1", "base", "USDC", "0xabc", "25", 6, t0), Open, EventPaymentDetected)
	s = must(t, s, func(s State) (State, []Event, error) { return s.FlagForReview("screening", t0) }, UnderReview, EventUnderReview)
	s = must(t, s, confirm("p1", t0), UnderReview)
	if s.PaidAt != nil {
		t.Fatal("paid_at set while under review")
	}
	if _, _, err := s.FlagForReview("again", t0); !errors.Is(err, ErrInvalidTransition) {
		t.Fatal("double review accepted")
	}
	s = must(t, s, func(s State) (State, []Event, error) { return s.ClearReview(t0) }, Paid, EventPaid)
}

func TestRejections(t *testing.T) {
	s := founder()
	if _, _, err := s.DetectPayment(NewPayment{ID: "x", Chain: "base", Asset: "USDT", Address: "0xabc", Amount: units("1", 6)}, t0); !errors.Is(err, ErrUnknownPayTo) {
		t.Fatalf("unsupported asset accepted: %v", err)
	}
	if _, _, err := s.DetectPayment(NewPayment{ID: "x", Chain: "base", Asset: "USDC", Address: "0xdef", Amount: units("1", 6)}, t0); !errors.Is(err, ErrUnknownPayTo) {
		t.Fatalf("wrong address accepted: %v", err)
	}
	if _, _, err := s.DetectPayment(NewPayment{ID: "x", Chain: "base", Asset: "USDC", Address: "0xabc", Amount: money.MaxUnits()}, t0); !errors.Is(err, ErrAmountOutOfRange) {
		t.Fatalf("amount overflowing the canonical scale accepted: %v", err)
	}
	big, _ := mustDiv(money.MaxUnits(), 2).Add(money.FromUint64(1))
	s1, _, err := s.DetectPayment(NewPayment{ID: "y", Chain: "bsc", Asset: "USDT", Address: "0xabc", TxHash: "a", Amount: big}, t0)
	if err != nil {
		t.Fatalf("large payment rejected: %v", err)
	}
	if _, _, err := s1.DetectPayment(NewPayment{ID: "z", Chain: "bsc", Asset: "USDT", Address: "0xabc", TxHash: "b", Amount: big}, t0); !errors.Is(err, ErrAmountOutOfRange) {
		t.Fatalf("sum overflow accepted: %v", err)
	}
	if _, _, err := s.DetectPayment(NewPayment{ID: "x", Chain: "base", Asset: "USDC", Address: "0xabc"}, t0); !errors.Is(err, ErrZeroPayment) {
		t.Fatalf("zero payment accepted: %v", err)
	}
	s, _, _ = detect("p1", "base", "USDC", "0xabc", "1", 6, t0)(s)
	dup := NewPayment{ID: "p9", Chain: "base", Asset: "USDC", Address: "0xabc", TxHash: "tx-p1", Amount: units("1", 6)}
	if _, _, err := s.DetectPayment(dup, t0); !errors.Is(err, ErrDuplicatePayment) {
		t.Fatalf("duplicate log accepted: %v", err)
	}
	if r, _, err := s.ReversePayment("p1", t0); err != nil {
		t.Fatal(err)
	} else if _, _, err := r.DetectPayment(dup, t0); err != nil {
		t.Fatalf("re-mined log after reversal rejected: %v", err)
	}
	if _, _, err := s.ConfirmPayment("nope", t0); !errors.Is(err, ErrUnknownPayment) {
		t.Fatal("unknown payment confirmed")
	}
	if _, _, err := s.FinalizePayment("p1", t0); !errors.Is(err, ErrInvalidTransition) {
		t.Fatal("detected payment finalized directly")
	}
	s, _, _ = s.ConfirmPayment("p1", t0)
	if _, _, err := s.ConfirmPayment("p1", t0); !errors.Is(err, ErrInvalidTransition) {
		t.Fatal("double confirm accepted")
	}
}

func TestCommandsDoNotMutateInput(t *testing.T) {
	s := founder()
	s, _, _ = detect("p1", "base", "USDC", "0xabc", "25", 6, t0)(s)
	before := fmt.Sprintf("%+v", s.Payments)
	_, _, _ = s.ConfirmPayment("p1", t0)
	if fmt.Sprintf("%+v", s.Payments) != before {
		t.Fatal("ConfirmPayment mutated its input")
	}
}

// TestPropertyStateMachine runs random command sequences and checks the invariants after each.
func TestPropertyStateMachine(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		amountCents := rapid.Uint64Range(1, 10_000).Draw(t, "cents")
		s := New(money.FromUint64(amountCents), []PayTo{
			{Chain: "base", Asset: "USDC", Address: "A", Decimals: 6},
			{Chain: "bsc", Asset: "USDT", Address: "A", Decimals: 18},
		}, t0.Add(time.Hour))
		now := t0
		nextID := 0
		steps := rapid.IntRange(1, 40).Draw(t, "steps")
		for i := 0; i < steps; i++ {
			now = now.Add(time.Duration(rapid.IntRange(0, 30).Draw(t, "dt")) * time.Minute)
			var n State
			var evs []Event
			var err error
			pick := func() string {
				if len(s.Payments) == 0 {
					return "none"
				}
				return s.Payments[rapid.IntRange(0, len(s.Payments)-1).Draw(t, "pi")].ID
			}
			switch rapid.IntRange(0, 6).Draw(t, "cmd") {
			case 0:
				nextID++
				dec := uint8(6)
				ch := "base"
				if rapid.Bool().Draw(t, "bsc") {
					dec, ch = 18, "bsc"
				}
				amt, _ := money.Rescale(money.FromUint64(rapid.Uint64Range(1, amountCents*2).Draw(t, "pc")), 2, dec)
				switch rapid.IntRange(0, 2).Draw(t, "shape") {
				case 1:
					amt, _ = amt.Add(money.FromUint64(1))
				case 2:
					if r := s.Remaining(); !r.IsZero() {
						ch, amt = "bsc", r
					}
				}
				n, evs, err = s.DetectPayment(NewPayment{ID: fmt.Sprintf("p%d", nextID), Chain: ch, Asset: map[string]string{"base": "USDC", "bsc": "USDT"}[ch], Address: "A", TxHash: fmt.Sprint(nextID), Amount: amt}, now)
			case 1:
				n, evs, err = s.ConfirmPayment(pick(), now)
			case 2:
				n, evs, err = s.FinalizePayment(pick(), now)
			case 3:
				n, evs, err = s.ReversePayment(pick(), now)
			case 4:
				n, evs, err = s.Expire(now)
			case 5:
				n, evs, err = s.FlagForReview("x", now)
			case 6:
				n, evs, err = s.ClearReview(now)
			}
			if err != nil {
				if len(evs) != 0 {
					t.Fatalf("failed command produced events")
				}
				continue
			}
			if n.Version != s.Version+1 {
				t.Fatalf("version %d -> %d", s.Version, n.Version)
			}
			if len(evs) > 1 {
				t.Fatalf("more than one event: %v", evs)
			}
			for _, e := range evs {
				if e.Version != n.Version || !e.Type.Valid() {
					t.Fatalf("bad event %+v at version %d", e, n.Version)
				}
			}
			checkInvariants(t, s, n)
			s = n
		}
	})
}

func checkInvariants(t *rapid.T, prev, s State) {
	if !s.Status.Valid() {
		t.Fatalf("invalid status %q", s.Status)
	}
	recv, amt := s.Received(), s.AmountCanonical()
	if s.Status == Paid && !recv.Equal(amt) {
		t.Fatalf("paid with received %s != amount %s", recv, amt)
	}
	if s.Status == Overpaid && recv.Cmp(amt) <= 0 {
		t.Fatalf("overpaid with received %s <= amount %s", recv, amt)
	}
	if s.Status == PartiallyPaid && (recv.IsZero() || recv.Cmp(amt) >= 0) {
		t.Fatalf("partially_paid with received %s / %s", recv, amt)
	}
	if s.Status == Open && !recv.IsZero() {
		t.Fatalf("open with received %s", recv)
	}
	if s.ExpiredAt != nil && s.Status != Expired && s.Status != LatePayment && s.Status != UnderReview {
		t.Fatalf("expired session in %s", s.Status)
	}
	if s.Status != UnderReview {
		paidLike := s.Status == Paid || s.Status == Overpaid
		if paidLike != (s.PaidAt != nil) {
			t.Fatalf("paid_at=%v in status %s", s.PaidAt, s.Status)
		}
	}
	for i, p := range prev.Payments {
		cur := s.Payments[i]
		if p.Status == Finalized && cur.Status != Finalized {
			t.Fatalf("finalized payment %s moved to %s", p.ID, cur.Status)
		}
		if p.Status == Reversed && cur.Status != Reversed {
			t.Fatalf("reversed payment %s moved to %s", p.ID, cur.Status)
		}
	}
}

func mustDiv(u money.Units, d uint64) money.Units {
	q, err := money.MulDivDown(u, 1, d)
	if err != nil {
		panic(err)
	}
	return q
}

// A simulated payment follows exactly the rules of a detected one; the flag only travels with it.
func TestSimulatedPaymentFollowsTheSameRules(t *testing.T) {
	real, sim := founder(), founder()
	real = must(t, real, detect("p1", "base", "USDC", "0xabc", "25", 6, t0), Open, EventPaymentDetected)
	sim = must(t, sim, func(s State) (State, []Event, error) {
		return s.DetectPayment(NewPayment{ID: "p1", Chain: "base", Asset: "USDC", Address: "0xabc", TxHash: "tx-p1",
			Amount: units("25", 6), Simulated: true}, t0)
	}, Open, EventPaymentDetected)
	real = must(t, real, confirm("p1", t0), Paid, EventPaid)
	sim = must(t, sim, confirm("p1", t0), Paid, EventPaid)
	if !sim.Payments[0].Simulated || real.Payments[0].Simulated {
		t.Fatalf("flag: sim %v real %v", sim.Payments[0].Simulated, real.Payments[0].Simulated)
	}
	sim.Payments[0].Simulated = false
	if fmt.Sprintf("%+v", sim) != fmt.Sprintf("%+v", real) {
		t.Fatalf("states differ beyond the flag:\n%+v\n%+v", sim, real)
	}
}

// D47: only a session paid exactly whose payments are all final is settled, from the moment the
// last payment became final.
func TestSettled(t *testing.T) {
	finalize := func(id string, at time.Time) step {
		return func(s State) (State, []Event, error) { return s.FinalizePayment(id, at) }
	}
	s := founder()
	if _, ok := s.Settled(); ok {
		t.Fatal("an open session is settled")
	}
	s = must(t, s, detect("p1", "base", "USDC", "0xabc", "25", 6, t0), Open, EventPaymentDetected)
	s = must(t, s, confirm("p1", t0.Add(time.Minute)), Paid, EventPaid)
	if _, ok := s.Settled(); ok {
		t.Fatal("paid with a confirmed payment is settled")
	}
	s = must(t, s, finalize("p1", t0.Add(16*time.Minute)), Paid)
	if at, ok := s.Settled(); !ok || !at.Equal(t0.Add(16*time.Minute)) {
		t.Fatalf("settled %v %v, want at the finality", at, ok)
	}
	// A duplicate send makes it overpaid: back on the schedule.
	o := must(t, s, detect("p2", "base", "USDC", "0xabc", "1", 6, t0.Add(time.Hour)), Paid, EventPaymentDetected)
	if _, ok := o.Settled(); ok {
		t.Fatal("a pending payment left the session settled")
	}
	o = must(t, o, confirm("p2", t0.Add(time.Hour)), Overpaid, EventOverpaid)
	o = must(t, o, finalize("p2", t0.Add(2*time.Hour)), Overpaid)
	if _, ok := o.Settled(); ok {
		t.Fatal("an overpaid session is settled")
	}
	// A partial payment never settles.
	p := must(t, founder(), detect("p1", "base", "USDC", "0xabc", "10", 6, t0), Open, EventPaymentDetected)
	p = must(t, p, confirm("p1", t0), PartiallyPaid, EventPartiallyPaid)
	p = must(t, p, finalize("p1", t0.Add(time.Minute)), PartiallyPaid)
	if _, ok := p.Settled(); ok {
		t.Fatal("a partially paid session is settled")
	}
}

// Validate rules out every amount the value methods cannot compute with: a session amount or a
// payment past the canonical scale, a sum of payments that overflows, and on an open-amount
// session a fee rule that cannot be applied. The value methods only panic past it, naming the
// broken invariant.
func TestValidate(t *testing.T) {
	if err := founder().Validate(); err != nil {
		t.Fatalf("a new session: %v", err)
	}
	s, _, err := detect("p1", "base", "USDC", "0xabc", "25", 6, t0)(founder())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("a detected payment: %v", err)
	}
	huge := founder()
	huge.AmountCents = money.MaxUnits()
	if err := huge.Validate(); !errors.Is(err, ErrAmountOutOfRange) {
		t.Fatalf("session amount: %v", err)
	}
	stored := s
	stored.Payments = append([]Payment(nil), s.Payments...)
	stored.Payments[0].Amount, stored.Payments[0].Status = money.MaxUnits(), Confirmed
	if err := stored.Validate(); !errors.Is(err, ErrAmountOutOfRange) {
		t.Fatalf("stored payment: %v", err)
	}
	half, _ := mustDiv(money.MaxUnits(), 2).Add(money.FromUint64(1))
	sum := s
	sum.Payments = []Payment{{ID: "a", Chain: "bsc", Asset: "USDT", Address: "0xabc", Amount: half, Decimals: 18, Status: Confirmed},
		{ID: "b", Chain: "bsc", Asset: "USDT", Address: "0xabc", Amount: half, Decimals: 18, Status: Reversed}}
	if err := sum.Validate(); !errors.Is(err, ErrAmountOutOfRange) {
		t.Fatalf("sum overflow: %v", err)
	}
	open := NewOpenAmount([]PayTo{{Chain: "base", Asset: "USDC", Address: "0xabc", Decimals: 6}}, t0.Add(time.Hour))
	open.PayTo[0].Fee.FeeBps = 10_000
	if err := open.Validate(); err != nil {
		t.Fatalf("a fee rule nothing was paid under: %v", err)
	}
	if _, _, err := open.DetectPayment(NewPayment{ID: "p1", Chain: "base", Asset: "USDC", Address: "0xabc", TxHash: "t", Amount: units("5", 6)}, t0); !errors.Is(err, ErrInvalidFee) {
		t.Fatalf("payment under a broken fee rule: %v", err)
	}
	defer func() {
		r := recover()
		if msg, _ := r.(string); !strings.Contains(msg, "State.Validate admits no such state") {
			t.Fatalf("panic: %v", r)
		}
	}()
	_ = stored.Received()
	t.Fatal("Received computed an amount past the canonical scale")
}
