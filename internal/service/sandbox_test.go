package service

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/base58"
	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/evm"
	"github.com/prismaport/prismaport/internal/ids"
	"github.com/prismaport/prismaport/internal/session"
	"github.com/prismaport/prismaport/internal/store"
	"github.com/prismaport/prismaport/internal/view"
)

func TestSyntheticTxHash(t *testing.T) {
	seen := map[string]bool{}
	for range 50 {
		h := SyntheticTxHash(chain.EVM)
		if len(h) != 66 || !strings.HasPrefix(h, SimulatedHashPrefix) || seen[h] {
			t.Fatalf("evm hash %s", h)
		}
		seen[h] = true
		s := SyntheticTxHash(chain.Solana)
		raw, err := base58.Decode(s)
		if err != nil || len(raw) != 64 || !strings.HasPrefix(s, "11111111") || seen[s] {
			t.Fatalf("solana signature %s: %v", s, err)
		}
		seen[s] = true
	}
}

// Every sample is a well-formed test-mode session in the state its event announces.
func TestSampleSessions(t *testing.T) {
	s := &Service{cfg: Config{Registry: chain.Pilot()}, view: view.Renderer{CheckoutBaseURL: "https://pay.test"}}
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	want := map[session.EventType]session.Status{
		session.EventPaymentDetected: session.Open, session.EventPartiallyPaid: session.PartiallyPaid,
		session.EventPaid: session.Paid, session.EventOverpaid: session.Overpaid, session.EventExpired: session.Expired,
		session.EventLatePayment: session.LatePayment, session.EventUnderReview: session.UnderReview,
		session.EventPaymentReversed: session.Open,
	}
	if len(want) != len(session.AllEventTypes) {
		t.Fatal("an event type has no sample expectation")
	}
	for typ, status := range want {
		ses, err := s.sampleSession("mer_1", typ, now)
		if err != nil {
			t.Fatalf("%s: %v", typ, err)
		}
		if ses.State.Status != status || ses.Livemode || !ids.Valid(ids.Session, ses.ID) {
			t.Errorf("%s: status %s livemode %v id %s", typ, ses.State.Status, ses.Livemode, ses.ID)
		}
		if len(ses.PayTo) != 2 {
			t.Fatalf("%s: pay_to %v", typ, ses.PayTo)
		}
		for _, pt := range ses.PayTo {
			c, _ := chain.Pilot().Get(pt.Chain)
			if c.Livemode {
				t.Fatalf("%s: live chain %s in a sample", typ, pt.Chain)
			}
			if c.Family == chain.EVM {
				if _, err := evm.ParseAddress(pt.Address); err != nil {
					t.Fatalf("%s: evm address %s: %v", typ, pt.Address, err)
				}
			} else if raw, err := base58.Decode(pt.Address); err != nil || len(raw) != 32 {
				t.Fatalf("%s: solana address %s", typ, pt.Address)
			}
		}
		for _, p := range ses.State.Payments {
			if !p.Simulated || !ids.Valid(ids.Payment, p.ID) || !strings.HasPrefix(p.TxHash, SimulatedHashPrefix) {
				t.Errorf("%s: payment %+v", typ, p)
			}
		}
		payload, err := s.view.EventPayload(store.EventMeta{ID: ids.NewAt(ids.Event, now), Type: string(typ), CreatedAt: now}, ses)
		if err != nil {
			t.Fatal(err)
		}
		var env struct {
			Type string `json:"type"`
			Data struct {
				Session view.Session `json:"session"`
			} `json:"data"`
		}
		if err := json.Unmarshal(payload, &env); err != nil || env.Type != string(typ) || env.Data.Session.Livemode {
			t.Fatalf("%s: %s %v", typ, payload, err)
		}
	}
	paid, _ := s.sampleSession("mer_1", session.EventPaid, now)
	if paid.State.Remaining().IsZero() == false || paid.State.PaidAt == nil {
		t.Fatalf("paid sample: %+v", paid.State)
	}
}
