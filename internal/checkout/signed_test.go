package checkout

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/service"
	"github.com/prismaport/prismaport/internal/session"
	"github.com/prismaport/prismaport/internal/store"
	"github.com/prismaport/prismaport/internal/txjob"
)

// jobsReader answers the clock and a session's jobs; nothing else is read by these tests.
type jobsReader struct {
	Reader
	now  time.Time
	jobs []store.TxJob
}

func (r jobsReader) Now() time.Time { return r.now }

func (r jobsReader) SessionTxJobs(context.Context, string) ([]store.TxJob, error) { return r.jobs, nil }

func notPayable(t *testing.T, err error) {
	t.Helper()
	var e *service.Error
	if !errors.As(err, &e) || e.Kind != service.KindConflict || e.Code != CodeSessionNotPayable {
		t.Fatalf("error %#v", err)
	}
}

// A settle of the session still in flight under another authorization (a second payer wallet:
// EIP-3009 nonces are per authorizer) refuses a new one; the same authorization, or one after
// the other ended, is accepted.
func TestOtherSettleInFlightRefusesASecondWallet(t *testing.T) {
	const mine, theirs = "0xaaa|0x01", "0xbbb|0x01"
	for _, status := range []string{store.JobPending, store.JobBlocked, store.JobSubmitted, store.JobMined} {
		c := &Checkout{st: jobsReader{jobs: []store.TxJob{{Kind: store.JobSettle, Ref: theirs, Status: status}}}}
		notPayable(t, c.noOtherSettle(t.Context(), "ses_1", mine))
		if err := c.noOtherSettle(t.Context(), "ses_1", theirs); err != nil {
			t.Fatalf("same authorization while %s: %v", status, err)
		}
	}
	for _, status := range []string{store.JobDone, store.JobFailed} {
		c := &Checkout{st: jobsReader{jobs: []store.TxJob{{Kind: store.JobSettle, Ref: theirs, Status: status}}}}
		if err := c.noOtherSettle(t.Context(), "ses_1", mine); err != nil {
			t.Fatalf("other settle %s: %v", status, err)
		}
	}
	c := &Checkout{st: jobsReader{jobs: []store.TxJob{{Kind: store.JobFlush, Ref: theirs, Status: store.JobSubmitted}}}}
	if err := c.noOtherSettle(t.Context(), "ses_1", mine); err != nil {
		t.Fatalf("flush in flight: %v", err)
	}
}

// After a deposit in an 18-decimal token, what is left of the session may not end on a whole
// unit of a 6-decimal token: it is rounded up, as the simulator does, instead of refused.
func TestPayableRoundsUpAfterAnEighteenDecimalDeposit(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	st := session.State{Status: session.PartiallyPaid, AmountCents: money.FromUint64(2500), ExpiresAt: now.Add(time.Hour),
		Payments: []session.Payment{{ID: "pay_1", Asset: "USDe", Amount: money.MustParseInteger("10123456789000000000"), Decimals: 18,
			Status: session.Confirmed}}}
	c := &Checkout{st: jobsReader{now: now}}
	v, err := c.payable(signedTarget{ses: &store.Session{State: st}, pt: store.PayToRecord{Asset: "USDC", Decimals: 6}})
	if err != nil || v.String() != "14876544" {
		t.Fatalf("payable %s, %v", v, err)
	}
}

// An authorization is accepted only while it outlasts the sender's margin plus a retry: one the
// sender would refuse as expired at its first try is refused at submit.
func TestSignedWindowOutlastsTheSenderMargin(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	expires := now.Add(30 * time.Minute)
	at := func(d time.Duration) *big.Int { return big.NewInt(now.Add(d).Unix()) }
	if txjob.SettleSubmitMargin < txjob.SettleMinedMargin+txjob.DefaultBaseBackoff {
		t.Fatalf("submit margin %s does not cover a retry", txjob.SettleSubmitMargin)
	}
	for _, d := range []time.Duration{txjob.SettleMinedMargin + time.Second, txjob.SettleSubmitMargin} {
		var e *service.Error
		if err := signedWindow(big.NewInt(0), at(d), expires, now); !errors.As(err, &e) || e.Code != service.CodeInvalidRequest {
			t.Fatalf("valid_before now+%s: %v", d, err)
		}
	}
	if err := signedWindow(big.NewInt(0), at(txjob.SettleSubmitMargin+time.Second), expires, now); err != nil {
		t.Fatalf("valid_before past the margin: %v", err)
	}
	if err := signedWindow(big.NewInt(0), at(31*time.Minute), expires, now); err == nil {
		t.Fatal("valid_before after the session expiry accepted")
	}
	if err := signedWindow(at(0), at(10*time.Minute), expires, now); err == nil {
		t.Fatal("window not open yet accepted")
	}
}
