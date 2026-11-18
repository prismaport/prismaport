//go:build integration

package sender_test

import (
	"math/big"
	"strconv"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/contracts"
	"github.com/prismaport/prismaport/internal/evm"
	"github.com/prismaport/prismaport/internal/evmrpc"
	"github.com/prismaport/prismaport/internal/sender"
	"github.com/prismaport/prismaport/internal/store"
)

var gateway = evm.Address{0x6a, 0x7e}

var errNonceTooLow = &evmrpc.RPCError{Code: -32000, Message: "nonce too low"}

// A settle whose transaction mined while a provider had not indexed its receipt yet, and whose
// fee-bumped replacement then finds the nonce used, is recognised as mined from the original hash
// on the other provider (ADR-0008 §A-6, the mechanism of §H-2): the job is not retried (which
// forgot the hash, and the retry's simulation reverted on the used authorization), the detector
// records the payment from the original's block.
func TestMinedSettleUnseenByOneProviderIsNotRetried(t *testing.T) {
	t.Parallel()
	rec := &recorder{found: 1}
	e := setup(t, nil, func(c *sender.Config) { c.Recorder = rec })
	j := e.settleJob()
	e.step()
	sent, _ := e.app.Store.GetTxJob(t.Context(), j.ID)
	block := e.mineReceipt(j, 1)
	e.pa.HideReceipts(1)
	e.pb.HideReceipts(1)
	e.chain.SendHandler = func([]byte) (evm.Word, error) { return evm.Word{}, errNonceTooLow }
	e.app.Clock.Advance(3 * time.Minute)
	e.step()
	got, _ := e.app.Store.GetTxJob(t.Context(), j.ID)
	if got.Status != store.JobDone || got.Outcome != "settled" || got.MinedTxHash != sent.TxHashes[0] || len(got.TxHashes) != 2 ||
		got.Attempts != 0 || len(rec.blocks) != 1 || rec.blocks[0] != block || rec.reverted[0] {
		t.Fatalf("job %+v, recorder %+v", got, rec)
	}
}

// A replacement that finds the nonce used while no provider shows a receipt of any of the job's
// hashes is retried, as before: the nonce went to another transaction.
func TestNonceUsedByAnotherTransactionRetries(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	e := setup(t, nil, func(c *sender.Config) { c.Recorder = rec })
	j := e.settleJob()
	e.step()
	e.chain.SendHandler = func([]byte) (evm.Word, error) { return evm.Word{}, errNonceTooLow }
	e.app.Clock.Advance(3 * time.Minute)
	e.step()
	got, _ := e.app.Store.GetTxJob(t.Context(), j.ID)
	if got.Status != store.JobPending || got.Attempts != 1 || len(got.TxHashes) != 0 || len(rec.blocks) != 0 {
		t.Fatalf("job %+v, recorder %+v", got, rec)
	}
}

// signedSettle queues a settle job as the EIP-3009 submit endpoint does: settle calldata for the
// Gateway, ref "payer|nonce". The fake chain answers the Gateway's simulation with simulate and
// the token's authorizationState with used.
func (e *env) signedSettle(simulate, estimate func() error, used *bool) store.TxJob {
	e.t.Helper()
	r := e.app.RevealedSession(e.app.TestKey, `{"amount":"25.00"}`, "Idempotency-Key", "signed")
	id := r.JSON(e.t)["id"].(string)
	nonce := evm.Word{0x0c, 0xe0}
	calldata := contracts.Settle(contracts.Authorization{Token: usdc, From: payer, Value: big.NewInt(25_000_000),
		ValidAfter: big.NewInt(0), ValidBefore: big.NewInt(99999999999), Recipients: []evm.Address{{0x11}}, Bps: []uint16{10000}})
	j, _, err := e.app.Store.InsertTxJob(e.t.Context(), store.TxJob{Chain: "anvil", Kind: store.JobSettle,
		Ref: payer.Lower() + "|" + nonce.Hex(), SessionID: id, Asset: "USDC", To: gateway.Hex(), Calldata: calldata,
		Details: map[string]string{"from": payer.Hex(), "value": "25000000", "valid_before": "99999999999"}})
	if err != nil {
		e.t.Fatal(err)
	}
	want := contracts.AuthorizationState(payer, nonce)
	e.chain.CallHandler = func(m evmrpc.CallMsg, _ uint64) ([]byte, error) {
		switch {
		case m.To == gateway:
			if err := simulate(); err != nil {
				return nil, err
			}
			return make([]byte, 64), nil
		case m.To == usdc && string(m.Data) == string(want):
			w := evm.Word{}
			if *used {
				w[31] = 1
			}
			return w[:], nil
		}
		return nil, &evmrpc.RPCError{Code: 3, Message: "execution reverted"}
	}
	e.chain.EstimateHandler = func(m evmrpc.CallMsg) (uint64, error) {
		if err := estimate(); err != nil {
			return 0, err
		}
		return 150_000, nil
	}
	return j
}

func reverts() error {
	return &evmrpc.RPCError{Code: 3, Message: "execution reverted: FiatTokenV2: authorization is used or canceled"}
}
func passes() error { return nil }

// A settle whose authorization was used already (someone submitted it first, or our own earlier
// transaction mined unseen) reverts in simulation; the detector looks back for the session's
// Gateway payment, and one found ends the job settled instead of rejected (ADR-0008
// §H-2). This holds for a revert in the precheck and in gas estimation.
func TestSettleOfAUsedAuthorizationFindsTheSettlement(t *testing.T) {
	t.Parallel()
	for name, sim := range map[string][2]func() error{"precheck": {reverts, passes}, "estimate": {passes, reverts}} {
		t.Run(name, func(t *testing.T) {
			rec := &recorder{found: 1}
			e := setup(t, nil, func(c *sender.Config) { c.Recorder = rec })
			used := true
			j := e.signedSettle(sim[0], sim[1], &used)
			e.chain.MineN(3)
			e.step()
			got, _ := e.app.Store.GetTxJob(t.Context(), j.ID)
			if got.Status != store.JobDone || got.Outcome != "settled" || len(e.sent) != 0 ||
				len(rec.blocks) != 1 || rec.blocks[0] != e.chain.Tip().Number || !rec.reverted[0] {
				t.Fatalf("job %+v, recorder %+v, sent %d", got, rec, len(e.sent))
			}
		})
	}
}

// A settle whose authorization the token reports unused is a plain rejection (balance, window):
// it fails at once, without a look back. A used one whose settlement is not found (yet) is tried
// once more, since the simulating provider may be ahead of the agreed head, then fails rejected.
func TestRejectedSettle(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	e := setup(t, nil, func(c *sender.Config) { c.Recorder = rec })
	used := false
	j := e.signedSettle(reverts, passes, &used)
	e.step()
	if got, _ := e.app.Store.GetTxJob(t.Context(), j.ID); got.Status != store.JobFailed || got.Outcome != "rejected" ||
		len(rec.blocks) != 0 {
		t.Fatalf("unused authorization: job %+v, recorder %+v", got, rec)
	}

	rec2 := &recorder{}
	u := setup(t, nil, func(c *sender.Config) { c.Recorder = rec2 })
	used2 := true
	uj := u.signedSettle(reverts, passes, &used2)
	u.step()
	if got, _ := u.app.Store.GetTxJob(t.Context(), uj.ID); got.Status != store.JobPending || got.Attempts != 1 ||
		len(rec2.blocks) != 1 {
		t.Fatalf("first look: job %+v, recorder %+v", got, rec2)
	}
	u.app.Clock.Advance(time.Minute)
	u.step()
	if got, _ := u.app.Store.GetTxJob(t.Context(), uj.ID); got.Status != store.JobFailed || got.Outcome != "rejected" ||
		len(rec2.blocks) != 2 || !rec2.reverted[1] {
		t.Fatalf("second look: job %+v, recorder %+v", got, rec2)
	}
}

// Our settle transaction reverting because someone settled the authorization first ends the job
// settled once the detector finds that settlement, without naming our reverted transaction as
// the payment.
func TestRevertedSettleOfAnAuthorizationSettledFirst(t *testing.T) {
	t.Parallel()
	rec := &recorder{found: 1}
	e := setup(t, nil, func(c *sender.Config) { c.Recorder = rec })
	used := false
	j := e.signedSettle(passes, passes, &used)
	e.step()
	used = true
	block := e.mineReceipt(j, 0)
	e.step()
	got, _ := e.app.Store.GetTxJob(t.Context(), j.ID)
	if got.Status != store.JobDone || got.Outcome != "settled" || got.MinedTxHash != "" || len(rec.blocks) != 1 ||
		rec.blocks[0] != block || !rec.reverted[0] {
		t.Fatalf("job %+v, recorder %+v", got, rec)
	}
}

// A settle job whose authorization is about to expire (a retry waited out its backoff) looks for
// a settlement of the authorization before failing: our earlier transaction may have mined
// unseen, or someone settled it first. One found ends the job settled; none fails it expired, and
// a token reporting the authorization unused needs no look back.
func TestExpiredSettleLooksForTheSettlementFirst(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		used     bool
		found    int
		status   string
		outcome  string
		lookback int
	}{
		"settled": {used: true, found: 1, status: store.JobDone, outcome: "settled", lookback: 1},
		"used":    {used: true, status: store.JobFailed, outcome: "expired", lookback: 1},
		"unused":  {status: store.JobFailed, outcome: "expired"},
	} {
		t.Run(name, func(t *testing.T) {
			rec := &recorder{found: c.found}
			e := setup(t, nil, func(cfg *sender.Config) { cfg.Recorder = rec })
			used := c.used
			j := e.signedSettle(passes, passes, &used)
			soon := strconv.FormatInt(e.app.Store.Now().Add(10*time.Second).Unix(), 10)
			if err := e.app.Store.AddTxJobDetails(t.Context(), j.ID, map[string]string{"valid_before": soon}); err != nil {
				t.Fatal(err)
			}
			e.chain.MineN(3)
			e.step()
			got, _ := e.app.Store.GetTxJob(t.Context(), j.ID)
			if got.Status != c.status || got.Outcome != c.outcome || len(e.sent) != 0 || len(rec.blocks) != c.lookback ||
				(c.lookback > 0 && !rec.reverted[0]) {
				t.Fatalf("job %+v, recorder %+v, sent %d", got, rec, len(e.sent))
			}
		})
	}
}
