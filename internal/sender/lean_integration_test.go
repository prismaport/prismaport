//go:build integration

package sender_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/chainsched"
	"github.com/prismaport/prismaport/internal/chainwatch"
	"github.com/prismaport/prismaport/internal/contracts"
	"github.com/prismaport/prismaport/internal/detector"
	"github.com/prismaport/prismaport/internal/evm"
	"github.com/prismaport/prismaport/internal/evmfake"
	"github.com/prismaport/prismaport/internal/evmrpc"
	"github.com/prismaport/prismaport/internal/scan"
	"github.com/prismaport/prismaport/internal/sender"
	"github.com/prismaport/prismaport/internal/store"
	"github.com/prismaport/prismaport/internal/worker"
)

func (e *env) calls() int { return e.pa.Total() + e.pb.Total() }

func (e *env) method(m string) int { return e.pa.Calls(m) + e.pb.Calls(m) }

func (e *env) cost(f func()) int {
	before := e.calls()
	f()
	return e.calls() - before
}

func mainWallet() evm.Address {
	a, _ := evm.ParseAddress(apptest.EVMDestination)
	return a
}

// The flush precheck reads the latest head only (one header per provider, no safe or finalized
// tag), a submitted transaction is followed with one receipt request per poll from one provider,
// and a mined flush waits for its finality check with no request at all: at the check one
// finalized header, then the agreed block hash and receipt.
func TestSenderLeanReads(t *testing.T) {
	t.Parallel()
	e := setup(t, nil, nil)
	id, fwd := e.paid()
	headers := e.method("eth_getBlockByNumber")
	e.step()
	// precheck: latest from both providers; fee quote: one latest header.
	if n := e.method("eth_getBlockByNumber") - headers; n != 3 {
		t.Fatalf("sending made %d header reads, want 3", n)
	}
	j := e.job(id)
	if j.Status != store.JobSubmitted {
		t.Fatalf("job %+v", j)
	}
	receipts, headers := e.method("eth_getTransactionReceipt"), e.method("eth_getBlockByNumber")
	for range 3 {
		e.app.Clock.Advance(2 * time.Second)
		e.step()
	}
	if n := e.method("eth_getTransactionReceipt") - receipts; n != 3 || e.method("eth_getBlockByNumber") != headers {
		t.Fatalf("3 polls: %d receipt requests, %d header reads", n, e.method("eth_getBlockByNumber")-headers)
	}
	e.mineFlush(j, fwd, 25_000_000, 225_000, map[evm.Address]uint64{mainWallet(): 24_775_000}, []evm.Address{mainWallet()})
	e.chain.MineN(5)
	if n := e.cost(e.step); n != 1 || e.job(id).Status != store.JobMined {
		t.Fatalf("the mined poll made %d requests: %+v", n, e.job(id))
	}
	// Mined: 14 minutes of 2 s polls cost nothing.
	if n := e.cost(func() {
		for range 420 {
			e.app.Clock.Advance(2 * time.Second)
			e.step()
		}
	}); n != 0 {
		t.Fatalf("waiting for finality made %d requests", n)
	}
	e.app.Clock.Advance(2 * time.Minute)
	// finalized header 1 + block hash 2 + receipt 2
	if n := e.cost(e.step); n != 5 {
		t.Fatalf("the finality check made %d requests, want 5", n)
	}
	if j := e.job(id); j.Status != store.JobDone || j.Outcome != "settled" {
		t.Fatalf("job %+v", j)
	}
}

// A finality check that finds the block not final yet reads one header and waits one retry
// interval for the next.
func TestMinedFlushFinalityRetry(t *testing.T) {
	t.Parallel()
	e := setup(t, nil, func(c *sender.Config) { c.FinalityDelay, c.FinalityRetry = time.Minute, time.Minute })
	e.chain.FinalDepth = 100
	id, fwd := e.paid()
	e.step()
	e.mineFlush(e.job(id), fwd, 25_000_000, 225_000, map[evm.Address]uint64{mainWallet(): 24_775_000}, []evm.Address{mainWallet()})
	e.step()
	e.app.Clock.Advance(time.Minute)
	if n := e.cost(e.step); n != 1 || e.job(id).Status != store.JobMined {
		t.Fatalf("an early finality check made %d requests", n)
	}
	for range 25 {
		e.app.Clock.Advance(2 * time.Second)
		if n := e.cost(e.step); n != 0 {
			t.Fatalf("polled before the retry was due: %d requests", n)
		}
	}
	e.chain.MineN(100)
	e.app.Clock.Advance(10 * time.Second)
	e.step()
	if j := e.job(id); j.Status != store.JobDone {
		t.Fatalf("job %+v", j)
	}
}

type recorder struct {
	blocks   []uint64
	reverted []bool
	found    int
	rescans  []uint64
}

func (r *recorder) GatewaySettled(_ context.Context, _ store.TxJob, block uint64, reverted bool) (int, error) {
	r.blocks, r.reverted = append(r.blocks, block), append(r.reverted, reverted)
	return r.found, nil
}

func (r *recorder) RescanSession(_ context.Context, _ string, block uint64) (int, error) {
	r.rescans = append(r.rescans, block)
	return 0, nil
}

func (r *recorder) BaselineReach() uint64 { return 0 }

// settleJob queues a settle job for a revealed session, as the EIP-3009 submit endpoint does.
func (e *env) settleJob() store.TxJob {
	e.t.Helper()
	r := e.app.RevealedSession(e.app.TestKey, `{"amount":"25.00"}`, "Idempotency-Key", "settle")
	id := r.JSON(e.t)["id"].(string)
	j, _, err := e.app.Store.InsertTxJob(e.t.Context(), store.TxJob{Chain: "anvil", Kind: store.JobSettle, Ref: "payer|nonce",
		SessionID: id, Asset: "USDC", To: evm.Address{0x6a, 0x7e}.Hex(), Calldata: contracts.BalanceOf(evm.Address{1}),
		Details: map[string]string{"valid_before": "99999999999"}})
	if err != nil {
		e.t.Fatal(err)
	}
	return j
}

// mineReceipt mines the job's transaction with the given status.
func (e *env) mineReceipt(j store.TxJob, status uint64) uint64 {
	e.t.Helper()
	j, err := e.app.Store.GetTxJob(e.t.Context(), j.ID)
	if err != nil || len(j.TxHashes) == 0 {
		e.t.Fatalf("job %+v %v", j, err)
	}
	hash, _ := evm.ParseWord(j.TxHashes[0])
	b := e.chain.Mine()
	e.chain.SetReceipt(evmrpc.Receipt{TxHash: hash, Status: status, BlockNumber: b.Number, BlockHash: b.Hash, GasUsed: 90_000})
	return b.Number
}

// A mined settle hands its block to the detector, which records the Gateway payment from that
// block; the job is done once a payment was recorded. A settle whose block holds no payment of
// the session ends only on a receipt both providers agree on, with an alert; a reverted one asks
// the detector to look back for someone else's settlement of the same authorization.
func TestSettleJobHandsItsBlockToTheDetector(t *testing.T) {
	t.Parallel()
	rec := &recorder{found: 1}
	e := setup(t, nil, func(c *sender.Config) { c.Recorder = rec })
	j := e.settleJob()
	e.step()
	block := e.mineReceipt(j, 1)
	e.step()
	if got, _ := e.app.Store.GetTxJob(t.Context(), j.ID); got.Status != store.JobDone || got.Outcome != "settled" ||
		len(rec.blocks) != 1 || rec.blocks[0] != block || rec.reverted[0] {
		t.Fatalf("job %+v, recorder %+v", got, rec)
	}

	unmatched := &recorder{}
	u := setup(t, nil, func(c *sender.Config) { c.Recorder = unmatched })
	uj := u.settleJob()
	u.step()
	u.mineReceipt(uj, 1)
	receipts := u.method("eth_getTransactionReceipt")
	u.step()
	got, _ := u.app.Store.GetTxJob(t.Context(), uj.ID)
	if got.Status != store.JobDone || got.Outcome != "settled_unmatched" || !u.alerts.Has("settle_unmatched") ||
		u.method("eth_getTransactionReceipt")-receipts != 3 { // the poll, then the agreed receipt
		t.Fatalf("job %+v, alerts %v, %d receipt requests", got, u.alerts.Kinds(), u.method("eth_getTransactionReceipt")-receipts)
	}

	front := &recorder{}
	r := setup(t, nil, func(c *sender.Config) { c.Recorder = front })
	rj := r.settleJob()
	r.step()
	rb := r.mineReceipt(rj, 0)
	r.step()
	if got, _ := r.app.Store.GetTxJob(t.Context(), rj.ID); got.Status != store.JobFailed || got.Outcome != "reverted" ||
		len(front.blocks) != 1 || front.blocks[0] != rb || !front.reverted[0] {
		t.Fatalf("job %+v, recorder %+v", got, front)
	}
}

// A flush that moved more than was credited when it was planned (a second deposit arrived
// between two balance reads and left with the flush) makes the detector rescan the address,
// which records the deposit.
func TestFlushOfMoreThanCreditedRescansTheAddress(t *testing.T) {
	t.Parallel()
	e := setup(t, nil, nil)
	id, fwd := e.paid()
	w := evm.Uint64Word(25_000_000)
	e.chain.Mine(evmfake.LogSpec{Address: usdc, Topics: []evm.Word{contracts.TransferTopic, evm.AddressWord(payer), evm.AddressWord(fwd)},
		Data: w[:]})
	e.bal[fwd] = 50_000_000
	e.step()
	e.flushReceipt(e.job(id), fwd, 50_000_000, 350_000, map[evm.Address]uint64{mainWallet(): 49_650_000}, []evm.Address{mainWallet()})
	if _, err := e.det.Step(t.Context()); err != nil { // credits the deposit the rescan found
		t.Fatal(err)
	}
	ses, err := e.app.Store.LoadSession(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(ses.Settlements) != 1 || len(ses.State.Payments) != 2 || ses.State.Status != "overpaid" {
		t.Fatalf("settlements %d, payments %+v, status %s", len(ses.Settlements), ses.State.Payments, ses.State.Status)
	}
}

// Someone else flushed the credited deposit and a new one between our precheck and our
// transaction: ours mines on an empty forwarder (no Settled event) and its receipt stores the
// zero at its block, after the stranger's flush, so the detector's later windows start past both.
// The finality check sees our flush moved less than was credited (nothing) and makes the detector
// rescan the address, which records the new deposit. Before, only a flush of more than was
// credited rescanned and the deposit was never credited.
func TestFlushOfLessThanCreditedRescansTheAddress(t *testing.T) {
	t.Parallel()
	e := setup(t, nil, nil)
	id, fwd := e.paid()
	e.step() // the precheck reads the 25 credited; our flush is sent
	j := e.job(id)
	w := evm.Uint64Word(10_000_000)
	e.chain.Mine(evmfake.LogSpec{Address: usdc, Topics: []evm.Word{contracts.TransferTopic, evm.AddressWord(payer), evm.AddressWord(fwd)},
		Data: w[:]})
	treasury, _ := evm.ParseAddress(apptest.EVMTreasury)
	tw := evm.AddressWord(treasury)
	e.chain.Mine(evmfake.LogSpec{TxHash: evm.Word{0xf1, 0x06}, Address: fwd, Topics: []evm.Word{contracts.SettledTopic, evm.AddressWord(usdc)},
		Data: append(append(append(word(35_000_000), word(275_000)...), word(34_725_000)...), tw[:]...)})
	e.bal[fwd] = 0
	e.mineReceipt(j, 1) // ours: nothing left to move
	e.chain.MineN(5)
	e.step() // the receipt: mined, the zero stored at its block
	e.app.Clock.Advance(16 * time.Minute)
	e.step()
	if j := e.job(id); j.Status != store.JobDone {
		t.Fatalf("job %+v", j)
	}
	if _, err := e.det.Step(t.Context()); err != nil { // credits the deposit the rescan found
		t.Fatal(err)
	}
	ses, err := e.app.Store.LoadSession(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(ses.State.Payments) != 2 || ses.State.Status != "overpaid" {
		t.Fatalf("payments %+v, status %s", ses.State.Payments, ses.State.Status)
	}
}

// Someone else's deployAndFlush moved a second deposit along with the credited one before our job
// was sent: the job finds the forwarder empty, records that flush's settlement (settled_elsewhere)
// and, since it moved more than was credited when the job was planned, makes the detector rescan
// the address, which records the second deposit. Before, the job ended there and the deposit was
// never credited.
func TestForeignFlushOfMoreThanCreditedRescansTheAddress(t *testing.T) {
	t.Parallel()
	e := setup(t, nil, nil)
	ctx := t.Context()
	id, fwd := e.paid()
	w := evm.Uint64Word(25_000_000)
	e.chain.Mine(evmfake.LogSpec{Address: usdc, Topics: []evm.Word{contracts.TransferTopic, evm.AddressWord(payer), evm.AddressWord(fwd)},
		Data: w[:]})
	treasury, _ := evm.ParseAddress(apptest.EVMTreasury)
	tw := evm.AddressWord(treasury)
	flush := evm.Word{0xf1, 0x05}
	e.chain.Mine(
		evmfake.LogSpec{TxHash: flush, Address: fwd, Topics: []evm.Word{contracts.PayoutTopic, evm.AddressWord(usdc), evm.AddressWord(mainWallet())},
			Data: word(49_650_000)},
		evmfake.LogSpec{TxHash: flush, Address: fwd, Topics: []evm.Word{contracts.SettledTopic, evm.AddressWord(usdc)},
			Data: append(append(append(word(50_000_000), word(350_000)...), word(49_650_000)...), tw[:]...)})
	e.bal[fwd] = 0
	e.chain.MineN(5) // the stranger's flush is final
	e.step()
	j := e.job(id)
	if j.Status != store.JobDone || j.Outcome != "settled_elsewhere" {
		t.Fatalf("job %+v", j)
	}
	if _, err := e.det.Step(ctx); err != nil { // credits the deposit the rescan found
		t.Fatal(err)
	}
	ses, err := e.app.Store.LoadSession(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(ses.Settlements) != 1 || ses.Settlements[0].Gross.String() != "50000000" || len(ses.State.Payments) != 2 ||
		ses.State.Status != "overpaid" {
		t.Fatalf("settlements %+v, payments %+v, status %s", ses.Settlements, ses.State.Payments, ses.State.Status)
	}
}

// failOnce is the detector with its first RescanSession failing (a provider error, say).
type failOnce struct {
	sender.Recorder
	failed bool
}

func (f *failOnce) RescanSession(ctx context.Context, sessionID string, block uint64) (int, error) {
	if !f.failed {
		f.failed = true
		return 0, errors.New("provider down")
	}
	return f.Recorder.RescanSession(ctx, sessionID, block)
}

// The rescan after someone else's flush of more than was credited fails once, after the
// settlement was recorded. The retry finds the settlement already on the job's session: it still
// counts as the session's, so the gross is compared again, the rescan runs and the deposit is
// credited. Before, the retry saw nothing new, waited out the finality window and ended empty,
// and the deposit was never recorded.
func TestForeignFlushRescanRetriedAfterAFailure(t *testing.T) {
	t.Parallel()
	rec := &failOnce{}
	e := setup(t, nil, func(c *sender.Config) { rec.Recorder = c.Recorder; c.Recorder = rec })
	ctx := t.Context()
	id, fwd := e.paid()
	w := evm.Uint64Word(25_000_000)
	e.chain.Mine(evmfake.LogSpec{Address: usdc, Topics: []evm.Word{contracts.TransferTopic, evm.AddressWord(payer), evm.AddressWord(fwd)},
		Data: w[:]})
	treasury, _ := evm.ParseAddress(apptest.EVMTreasury)
	tw := evm.AddressWord(treasury)
	flush := evm.Word{0xf1, 0x06}
	e.chain.Mine(
		evmfake.LogSpec{TxHash: flush, Address: fwd, Topics: []evm.Word{contracts.PayoutTopic, evm.AddressWord(usdc), evm.AddressWord(mainWallet())},
			Data: word(49_650_000)},
		evmfake.LogSpec{TxHash: flush, Address: fwd, Topics: []evm.Word{contracts.SettledTopic, evm.AddressWord(usdc)},
			Data: append(append(append(word(50_000_000), word(350_000)...), word(49_650_000)...), tw[:]...)})
	e.bal[fwd] = 0
	e.chain.MineN(5)
	if _, err := e.snd.Plan(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.snd.Step(ctx); err == nil || !rec.failed {
		t.Fatalf("the first rescan should have failed: %v", err)
	}
	e.step()
	if j := e.job(id); j.Status != store.JobDone || j.Outcome != "settled_elsewhere" {
		t.Fatalf("job after the retry %+v", j)
	}
	if _, err := e.det.Step(ctx); err != nil {
		t.Fatal(err)
	}
	ses, err := e.app.Store.LoadSession(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(ses.Settlements) != 1 || len(ses.State.Payments) != 2 || ses.State.Status != "overpaid" {
		t.Fatalf("settlements %d, payments %+v, status %s", len(ses.Settlements), ses.State.Payments, ses.State.Status)
	}
}

// Our flush moved more than was credited and the rescan after its settlement fails once. The next
// finality check finds the settlement already recorded on the session: the job ends settled (not
// no_settlement) and the rescan runs again, crediting the deposit.
func TestFinishRetryAfterRecordingEndsSettled(t *testing.T) {
	t.Parallel()
	rec := &failOnce{}
	e := setup(t, nil, func(c *sender.Config) { rec.Recorder = c.Recorder; c.Recorder = rec })
	ctx := t.Context()
	id, fwd := e.paid()
	w := evm.Uint64Word(25_000_000)
	e.chain.Mine(evmfake.LogSpec{Address: usdc, Topics: []evm.Word{contracts.TransferTopic, evm.AddressWord(payer), evm.AddressWord(fwd)},
		Data: w[:]})
	e.bal[fwd] = 50_000_000
	e.step()
	e.mineFlush(e.job(id), fwd, 50_000_000, 350_000, map[evm.Address]uint64{mainWallet(): 49_650_000}, []evm.Address{mainWallet()})
	e.chain.MineN(5)
	e.step() // mined
	e.app.Clock.Advance(16 * time.Minute)
	if _, err := e.snd.Step(ctx); err == nil || !rec.failed {
		t.Fatalf("the first rescan should have failed: %v", err)
	}
	if j := e.job(id); j.Status != store.JobMined {
		t.Fatalf("job after the failed rescan %+v", j)
	}
	e.step()
	if j := e.job(id); j.Status != store.JobDone || j.Outcome != "settled" {
		t.Fatalf("job after the retry %+v", j)
	}
	if _, err := e.det.Step(ctx); err != nil {
		t.Fatal(err)
	}
	ses, err := e.app.Store.LoadSession(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(ses.Settlements) != 1 || len(ses.State.Payments) != 2 || ses.State.Status != "overpaid" {
		t.Fatalf("settlements %d, payments %+v, status %s", len(ses.Settlements), ses.State.Payments, ses.State.Status)
	}
}

// countRescans is the detector, counting the rescans asked for.
type countRescans struct {
	sender.Recorder
	n int
}

func (c *countRescans) RescanSession(ctx context.Context, sessionID string, block uint64) (int, error) {
	c.n++
	return c.Recorder.RescanSession(ctx, sessionID, block)
}

// A receipt that repeats our flush's Settled log (same transaction and index) counts it once:
// the gross stays at the 25 planned and no rescan is asked for.
func TestRepeatedSettledLogCountsOnce(t *testing.T) {
	t.Parallel()
	rec := &countRescans{}
	e := setup(t, nil, func(c *sender.Config) { rec.Recorder = c.Recorder; c.Recorder = rec })
	id, fwd := e.paid()
	e.step()
	j := e.job(id)
	e.mineFlush(j, fwd, 25_000_000, 225_000, map[evm.Address]uint64{mainWallet(): 24_775_000}, []evm.Address{mainWallet()})
	hash, _ := evm.ParseWord(j.TxHashes[0])
	r, err := e.pa.Receipt(t.Context(), hash)
	if err != nil {
		t.Fatal(err)
	}
	r.Logs = append(r.Logs, r.Logs[len(r.Logs)-1]) // the Settled log twice
	e.chain.SetReceipt(r)
	e.chain.MineN(5)
	e.step() // mined
	e.app.Clock.Advance(16 * time.Minute)
	e.step()
	ses, err := e.app.Store.LoadSession(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if j := e.job(id); j.Outcome != "settled" || len(ses.Settlements) != 1 || rec.n != 0 {
		t.Fatalf("job %+v, settlements %d, rescans %d", j, len(ses.Settlements), rec.n)
	}
}

// settledLogs mines Payout and Settled events of fwd under tx into the chain (so eth_getLogs sees
// them) and, when receipt is set, registers the receipt of tx with those logs.
func (e *env) settledLogs(tx evm.Word, fwd evm.Address, gross, feeAmt uint64, receipt bool) *evmfake.Block {
	e.t.Helper()
	treasury, _ := evm.ParseAddress(apptest.EVMTreasury)
	tw := evm.AddressWord(treasury)
	net := gross - feeAmt
	b := e.chain.Mine(
		evmfake.LogSpec{TxHash: tx, Address: fwd, Topics: []evm.Word{contracts.PayoutTopic, evm.AddressWord(usdc), evm.AddressWord(mainWallet())},
			Data: word(net)},
		evmfake.LogSpec{TxHash: tx, Address: fwd, Topics: []evm.Word{contracts.SettledTopic, evm.AddressWord(usdc)},
			Data: append(append(append(word(gross), word(feeAmt)...), word(net)...), tw[:]...)})
	if receipt {
		e.chain.SetReceipt(evmrpc.Receipt{TxHash: tx, Status: 1, BlockNumber: b.Number, BlockHash: b.Hash, GasUsed: 210_000, Logs: b.Logs})
	}
	return b
}

// Our flush A settles the first 25 (S_A). A late deposit of 10 is credited, then a stranger's
// deployAndFlush moves it, not final yet. Job B (planned 35) finds the forwarder empty; its scan
// reads S_A again (already the session's): 25 is below the 35 planned, so B waits for a
// settlement to become final instead of ending at once, and records the stranger's 10 once it is.
// Counting S_A as "something recorded" ended B settled_elsewhere at once and lost the 10.
func TestForeignFlushAfterOurEarlierFlushIsWaitedFor(t *testing.T) {
	t.Parallel()
	e := setup(t, nil, nil)
	ctx := t.Context()
	id, fwd := e.paid()
	e.step()
	a := e.job(id)
	hash, _ := evm.ParseWord(a.TxHashes[0])
	e.settledLogs(hash, fwd, 25_000_000, 225_000, true)
	e.bal[fwd] = 0
	e.step() // A mined
	e.chain.MineN(5)
	w := evm.Uint64Word(10_000_000)
	e.chain.Mine(evmfake.LogSpec{Address: usdc, Topics: []evm.Word{contracts.TransferTopic, evm.AddressWord(payer), evm.AddressWord(fwd)},
		Data: w[:]})
	e.bal[fwd] = 10_000_000
	e.chain.MineN(1)
	if _, err := e.det.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if ses, _ := e.app.Store.LoadSession(ctx, id); len(ses.State.Payments) != 2 {
		t.Fatalf("late deposit not credited: %+v", ses.State.Payments)
	}
	e.settledLogs(evm.Word{0x57, 0x7a}, fwd, 10_000_000, 150_000, false) // the stranger's flush, not final
	e.bal[fwd] = 0
	e.app.Clock.Advance(16 * time.Minute)
	e.step() // A final and settled; B planned and found empty
	js, _ := e.app.Store.SessionTxJobs(ctx, id)
	if len(js) != 2 || js[0].Outcome != "settled" || js[1].Status != store.JobPending || js[1].PlannedTotal.String() != "35000000" {
		t.Fatalf("B should wait for the stranger's flush to become final: %+v", js)
	}
	e.chain.MineN(5)
	e.app.Clock.Advance(2 * time.Minute)
	e.step()
	js, _ = e.app.Store.SessionTxJobs(ctx, id)
	ses, err := e.app.Store.LoadSession(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if js[1].Status != store.JobDone || js[1].Outcome != "settled_elsewhere" || len(ses.Settlements) != 2 {
		t.Fatalf("job B %+v, settlements %+v", js[1], ses.Settlements)
	}
}

// A deposit right after our flush, of the same amount as the one credited or less, is credited
// although the address's balance never reads higher than before the flush: our flush's receipt
// stores a zero balance at its block, so the next read is a rise. Before, the read equal to the
// stored balance was skipped (or a lower one stored as a decrease), the first payment became
// final, the session left the watch as settled (D47) and the deposit was never credited.
func TestDepositRightAfterOurFlushIsCredited(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		second uint64
	}{{"equal", 25_000_000}, {"smaller", 10_000_000}} {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t, nil, nil)
			ctx := t.Context()
			id, fwd := e.paid()
			tick := func() {
				t.Helper()
				if _, err := e.det.Step(ctx); err != nil {
					t.Fatal(err)
				}
			}
			e.step()
			j := e.job(id)
			if j.Status != store.JobSubmitted {
				t.Fatalf("flush not sent: %+v", j)
			}
			e.mineFlush(j, fwd, 25_000_000, 225_000, map[evm.Address]uint64{mainWallet(): 24_775_000}, []evm.Address{mainWallet()})
			e.bal[fwd] = 0
			e.step() // the receipt: mined
			w := evm.Uint64Word(tc.second)
			e.chain.Mine(evmfake.LogSpec{Address: usdc, Topics: []evm.Word{contracts.TransferTopic, evm.AddressWord(payer), evm.AddressWord(fwd)},
				Data: w[:]})
			e.bal[fwd] = tc.second
			e.chain.MineN(1)
			tick()
			// The first payment becomes final (and the flush settles) well after the deposit.
			e.chain.MineN(10)
			e.app.Clock.Advance(16 * time.Minute)
			for range 3 {
				tick()
				e.step()
			}
			ses, err := e.app.Store.LoadSession(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if ses.State.Status != "overpaid" || len(ses.State.Payments) != 2 || ses.State.Payments[1].Amount.String() != fmt.Sprint(tc.second) {
				t.Fatalf("second deposit after our flush: status %s, received %s, payments %+v", ses.State.Status, ses.State.Received(),
					ses.State.Payments)
			}
		})
	}
}

// TestPaidOnBaseRequestBudget runs a session paid exactly in its first minute on a Base-like
// chain (2 s blocks, finality about 15 minutes after a block, CONFIRMATIONS 10, FINALITY_DELAY
// 15 min, hot = warm = 1 min) through the paced detector and the sender, and counts every request
// from the reveal until the session leaves every check.
func TestPaidOnBaseRequestBudget(t *testing.T) {
	t.Parallel()
	e := setup(t, nil, nil)
	e.chain.SafeDepth, e.chain.FinalDepth = 30, 450
	anvil, _ := e.app.Registry.Get("anvil")
	det, err := detector.New(detector.Config{Chain: anvil, Policy: chainwatch.CreditPolicy{}, ReanchorMargin: 30}, e.q, e.app.Svc, e.app.Store, e.alerts, nil)
	if err != nil {
		t.Fatal(err)
	}
	snd := sender.New(sender.Config{Chain: anvil, Factory: factory, Recorder: det}, e.q, e.app.Store, gasKeyOf(t), e.alerts, nil)
	sch, _ := scan.ParseSchedule(scan.DefaultLateSchedule)
	p := scan.NewPacer(scan.Config{Chain: "anvil", Intervals: scan.DefaultIntervals(), Now: e.app.Clock.Now},
		scan.FromStore(e.app.Store, "anvil", chain.EVM, 30*time.Second, sch, scan.DefaultWatchAfterExpiry), det)
	ctx := t.Context()
	// The worker's chain scheduler drives both, every 2 s here: one state query, then the
	// detector when its pacer is due and the sender when the state shows it work.
	var stepErrs []error
	keep := func(n int, err error) (int, error) {
		if err != nil && !errors.Is(err, worker.ErrSkipped) {
			stepErrs = append(stepErrs, err)
		}
		return n, err
	}
	q := scan.Query([]string{"anvil"}, 30*time.Second, sch, scan.DefaultWatchAfterExpiry)
	sched := chainsched.New(chainsched.Config{Now: e.app.Clock.Now,
		State: func(ctx context.Context) (map[string]store.ChainScanState, error) {
			q.Now = e.app.Clock.Now()
			return e.app.Store.ChainStates(ctx, q)
		},
		Chains: []chainsched.Chain{{ID: "anvil", Due: p.Due, SenderEvery: 2 * time.Second,
			Detector: func(ctx context.Context, st scan.State) (int, error) { return keep(p.StepState(ctx, st)) },
			Sender: func(ctx context.Context) (int, error) {
				n, err := snd.Plan(ctx)
				if err != nil {
					return keep(0, err)
				}
				m, err := snd.Step(ctx)
				return keep(n+m, err)
			}}}})
	tick := func() {
		t.Helper()
		if _, err := sched.Step(ctx); err != nil && !errors.Is(err, worker.ErrSkipped) {
			t.Fatal(err)
		}
		sched.Wait()
		if len(stepErrs) > 0 {
			t.Fatal(stepErrs)
		}
	}
	tick() // startup heartbeat, before the session
	e.chain.MineN(500)
	methods := []string{"eth_call", "eth_getBlockByNumber", "eth_getLogs", "eth_getTransactionReceipt", "eth_sendRawTransaction",
		"eth_getTransactionCount", "eth_estimateGas", "eth_maxPriorityFeePerGas", "eth_getBalance"}
	byMethod := map[string]int{}
	for _, m := range methods {
		byMethod[m] = -e.method(m)
	}
	before := e.calls()
	r := e.app.RevealedSession(e.app.TestKey, `{"amount":"25.00"}`, "Idempotency-Key", "budget")
	id := r.JSON(t)["id"].(string)
	ses, _ := e.app.Store.LoadSession(ctx, id)
	to, _ := evm.ParseAddress(ses.PayTo[0].Address)
	flushed := false
	var counts []int
	for i := range 40 * 30 { // 40 minutes of 2 s steps, one block each
		e.app.Clock.Advance(2 * time.Second)
		if i == 10 { // the payer pays 20 s after generating
			w := evm.Uint64Word(25_000_000)
			e.chain.Mine(evmfake.LogSpec{Address: usdc, Topics: []evm.Word{contracts.TransferTopic, evm.AddressWord(payer), evm.AddressWord(to)},
				Data: w[:]})
			e.bal[to] = 25_000_000
		} else {
			e.chain.Mine()
		}
		tick()
		if js, _ := e.app.Store.SessionTxJobs(ctx, id); !flushed && len(js) == 1 && js[0].Status == store.JobSubmitted {
			e.mineFlush(js[0], to, 25_000_000, 225_000, map[evm.Address]uint64{mainWallet(): 24_775_000}, []evm.Address{mainWallet()})
			e.bal[to] = 0
			flushed = true
		}
		if i%30 == 29 {
			counts = append(counts, e.calls()-before)
		}
	}
	got := e.calls() - before
	ses, _ = e.app.Store.LoadSession(ctx, id)
	js, _ := e.app.Store.SessionTxJobs(ctx, id)
	if ses.State.Status != "paid" || ses.State.Payments[0].Status != "finalized" || len(ses.Settlements) != 1 || len(js) != 1 ||
		js[0].Status != store.JobDone {
		t.Fatalf("session %s %+v, jobs %+v", ses.State.Status, ses.State.Payments, js)
	}
	t.Logf("paid exactly on Base in the first minute: %d requests (cumulative per minute: %v)", got, counts)
	for _, m := range methods {
		byMethod[m] += e.method(m)
	}
	t.Logf("by method: %v", byMethod)
	if got > 50 {
		t.Fatalf("a session paid exactly cost %d requests, budget 50", got)
	}
}

func gasKeyOf(t *testing.T) *evm.PrivateKey {
	t.Helper()
	k, err := evm.ParsePrivateKey(gasKey)
	if err != nil {
		t.Fatal(err)
	}
	return k
}
