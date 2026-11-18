package service

import (
	"context"
	"time"

	"github.com/prismaport/prismaport/internal/apikey"
	"github.com/prismaport/prismaport/internal/store"
)

// The hosted checkout (internal/checkout) builds the chain-specific data of a payment (the
// signed EIP-3009 settle call, the Solana transaction) and reads what it shows the payer; every
// write it makes goes through these methods.

// RecordCheckoutSeen records that a payer is on the session's checkout, throttled in the store.
func (s *Service) RecordCheckoutSeen(ctx context.Context, sessionID string) error {
	_, err := s.store.TouchCheckoutSeen(ctx, sessionID)
	return err
}

// QueueSettle queues a settle transaction the checkout built and checked for the session: the
// gas wallet's sender submits it. A job with the same (chain, kind, ref) is answered as stored (a
// failed one re-armed with the new calldata), so a payer retrying a submit queues it once.
func (s *Service) QueueSettle(ctx context.Context, j store.TxJob) (store.TxJob, error) {
	j.Kind = store.JobSettle
	out, _, err := s.store.InsertTxJob(ctx, j)
	return out, err
}

// ReserveWalletTx records the wallet transaction the checkout built for the session unless the
// one recorded is still outstanding at height (store.ReserveWalletTx); false writes nothing.
func (s *Service) ReserveWalletTx(ctx context.Context, w store.WalletTx, height uint64, window time.Duration) (bool, error) {
	return s.store.ReserveWalletTx(ctx, w, height, window)
}

// testPanelActor names the hosted checkout's test panel in the audit log.
const testPanelActor = "checkout:test_panel"

// SimulateFromTestPanel runs one simulator step on merchantID's test-mode session for the hosted
// checkout's test panel, and records it in the merchant's audit log.
func (s *Service) SimulateFromTestPanel(ctx context.Context, merchantID, sessionID string, in SimulateInput) (SimulateResult, error) {
	a := Auth{KeyID: "checkout", MerchantID: merchantID, Livemode: false, Scopes: []string{string(apikey.SessionsWrite)},
		Actor: testPanelActor}
	res, err := s.Simulate(ctx, a, sessionID, in)
	if err != nil {
		return SimulateResult{}, err
	}
	if err := s.store.Audit(ctx, merchantID, a.Actor, "session.simulated", map[string]any{"session_id": sessionID, "action": in.Action}); err != nil {
		return SimulateResult{}, err
	}
	return res, nil
}
