//go:build integration

package detector_test

import (
	"strings"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/chainwatch"
	"github.com/prismaport/prismaport/internal/detector"
	"github.com/prismaport/prismaport/internal/service"
)

func (e *env) eventTypes(id string) string {
	e.t.Helper()
	evs, err := e.app.Store.SessionEvents(e.t.Context(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	var out []string
	for _, ev := range evs {
		out = append(out, ev.Type)
	}
	return strings.Join(out, ",")
}

func (e *env) simulate(id string, steps ...service.SimulateInput) {
	e.t.Helper()
	a := service.Auth{MerchantID: e.app.MerchantID, Scopes: []string{"sessions:write"}, KeyID: "test"}
	for _, in := range steps {
		if _, err := e.app.Svc.Simulate(e.t.Context(), a, id, in); err != nil {
			e.t.Fatalf("simulate %+v: %v", in, err)
		}
	}
}

// The simulator produces, for each scenario, the same event sequence and the same final session
// as the detector does from real chain logs.
func TestSimulatorMatchesDetector(t *testing.T) {
	t.Parallel()
	scenarios := []struct {
		name     string
		policy   *chainwatch.CreditPolicy
		detector func(e *env, s sessionView)
		simulate []service.SimulateInput
		events   string
	}{
		{"exact payment", nil, func(e *env, s sessionView) {
			e.chain.Mine(transfer(usdc, payer, addr(t, s, "USDC"), 25_000_000))
			e.chain.MineN(2)
			e.drain()
		}, []service.SimulateInput{{Action: service.SimPayExact, Asset: "USDC"}}, "session.payment_detected,session.paid"},
		{"partial payment", nil, func(e *env, s sessionView) {
			e.chain.Mine(transfer(usdc, payer, addr(t, s, "USDC"), 10_000_000))
			e.chain.MineN(2)
			e.drain()
		}, []service.SimulateInput{{Action: service.SimPayPartial, Amount: "10.00", Asset: "USDC"}}, "session.payment_detected,session.partially_paid"},
		{"partial then top up", nil, func(e *env, s sessionView) {
			e.chain.Mine(transfer(usdc, payer, addr(t, s, "USDC"), 10_000_000))
			e.chain.MineN(2)
			e.drain()
			e.chain.Mine(transfer(usdc, payer, addr(t, s, "USDC"), 15_000_000))
			e.chain.MineN(2)
			e.drain()
		}, []service.SimulateInput{{Action: service.SimPayPartial, Amount: "10.00", Asset: "USDC"}, {Action: service.SimPayExact, Asset: "USDC"}},
			"session.payment_detected,session.partially_paid,session.payment_detected,session.paid"},
		{"overpayment", nil, func(e *env, s sessionView) {
			e.chain.Mine(transfer(usdc, payer, addr(t, s, "USDC"), 30_000_000))
			e.chain.MineN(2)
			e.drain()
		}, []service.SimulateInput{{Action: service.SimOverpay, Amount: "30.00", Asset: "USDC"}}, "session.payment_detected,session.overpaid"},
		{"expiry", nil, func(e *env, s sessionView) {
			e.app.Clock.Advance(2 * time.Hour)
			if _, err := e.app.Svc.ExpireDue(t.Context(), 10); err != nil {
				t.Fatal(err)
			}
		}, []service.SimulateInput{{Action: service.SimExpire}}, "session.expired"},
		{"late payment", nil, func(e *env, s sessionView) {
			to := addr(t, s, "USDC")
			e.chain.MineN(2)
			e.drain()
			e.app.Clock.Advance(2 * time.Hour)
			if _, err := e.app.Svc.ExpireDue(t.Context(), 10); err != nil {
				t.Fatal(err)
			}
			e.chain.Mine(transfer(usdc, payer, to, 25_000_000))
			e.chain.MineN(2)
			e.late() // an expired session is reached by its late checks
			e.drain()
		}, []service.SimulateInput{{Action: service.SimLatePayment, Asset: "USDC"}}, "session.expired,session.payment_detected,session.late_payment"},
		{"reorg reversal", &chainwatch.CreditPolicy{Confirmations: 1}, func(e *env, s sessionView) {
			e.chain.MineN(2)
			e.chain.Mine(transfer(usdc, payer, addr(t, s, "USDC"), 25_000_000))
			e.chain.Mine()
			e.drain()
			e.chain.Reorg(2)
			e.chain.MineN(7) // the finality check's hash re-check catches it once finality covers the block
			e.drain()
		}, []service.SimulateInput{{Action: service.SimPayExact, Asset: "USDC"}, {Action: service.SimReverseLast}},
			"session.payment_detected,session.paid,payment.reversed"},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			e := setup(t, func(c *detector.Config) {
				if sc.policy != nil {
					c.Policy = *sc.policy
				}
			})
			onChain := e.create("25.00", "real")
			sim := e.create("25.00", "sim")
			e.simulate(sim.ID, sc.simulate...)
			sc.detector(e, onChain)
			got, want := e.eventTypes(sim.ID), e.eventTypes(onChain.ID)
			if want != sc.events || got != want {
				t.Fatalf("detector: %s\nsimulator: %s\nexpected: %s", want, got, sc.events)
			}
			r, s := e.get(onChain.ID), e.get(sim.ID)
			if r.Status != s.Status || r.Received != s.Received || len(r.Payments) != len(s.Payments) {
				t.Fatalf("final sessions differ:\ndetector %+v\nsimulator %+v", r, s)
			}
			for i := range r.Payments {
				if r.Payments[i].Status == "finalized" {
					r.Payments[i].Status = "confirmed"
				}
				if r.Payments[i].Status != s.Payments[i].Status || r.Payments[i].Amount != s.Payments[i].Amount {
					t.Fatalf("payment %d: detector %+v simulator %+v", i, r.Payments[i], s.Payments[i])
				}
			}
		})
	}
}
