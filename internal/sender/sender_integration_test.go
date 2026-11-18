//go:build integration

package sender_test

import (
	"encoding/json"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/alert"
	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/chainwatch"
	"github.com/prismaport/prismaport/internal/contracts"
	"github.com/prismaport/prismaport/internal/derive"
	"github.com/prismaport/prismaport/internal/detector"
	"github.com/prismaport/prismaport/internal/evm"
	"github.com/prismaport/prismaport/internal/evmfake"
	"github.com/prismaport/prismaport/internal/evmrpc"
	"github.com/prismaport/prismaport/internal/sender"
	"github.com/prismaport/prismaport/internal/store"
)

var (
	usdc    = evm.Address{0xa0, 0x01}
	factory = evm.Address{0xfa, 0xc7}
	payer   = evm.Address{0x9a}
)

const gasKey = "0x59c6995e998f97a5a0044966f0945389dc9e86dae88c7a8412f4603b6b78690d"

type env struct {
	t      *testing.T
	app    *apptest.App
	chain  *evmfake.Chain
	q      *chainwatch.Quorum
	det    *detector.Detector
	snd    *sender.Sender
	pa, pb *evmfake.Provider
	alerts *alert.Recorder
	bal    map[evm.Address]uint64
	sent   [][]byte
}

func setup(t *testing.T, deriver derive.AddressDeriver, mutate func(*sender.Config)) *env {
	t.Helper()
	reg, err := chain.NewRegistry([]chain.Chain{{ID: "anvil", Family: chain.EVM, EVMID: 31337, Fee: chain.PilotFee(),
		Assets: []chain.Asset{{Symbol: "USDC", Token: usdc.Hex(), Decimals: 6}}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if deriver == nil {
		deriver = derive.Forwarders(factory)
	}
	e := &env{t: t, app: apptest.New(t, apptest.Options{Registry: reg, Deriver: deriver}), chain: evmfake.New(31337),
		alerts: &alert.Recorder{}, bal: map[evm.Address]uint64{}}
	e.chain.CallHandler = func(m evmrpc.CallMsg, _ uint64) ([]byte, error) {
		holder, _ := evm.Word(m.Data[4:36]).Address()
		w := evm.Uint64Word(e.bal[holder])
		return w[:], nil
	}
	e.chain.SendHandler = func(raw []byte) (evm.Word, error) {
		e.sent = append(e.sent, raw)
		var h evm.Word
		copy(h[:], evm.Keccak256(raw))
		return h, nil
	}
	e.pa, e.pb = e.chain.Provider("a"), e.chain.Provider("b")
	e.q = chainwatch.NewQuorum("anvil", []chainwatch.Provider{e.pa, e.pb}, chainwatch.Options{})
	anvil, _ := reg.Get("anvil")
	if e.det, err = detector.New(detector.Config{Chain: anvil, Policy: chainwatch.CreditPolicy{Confirmations: 1}, ReanchorMargin: 30}, e.q, e.app.Svc, e.app.Store,
		e.alerts, nil); err != nil {
		t.Fatal(err)
	}
	cfg := sender.Config{Chain: anvil, Factory: factory, BaseBackoff: time.Second, AlertAfter: 2, MaxAttempts: 3, Recorder: e.det}
	if mutate != nil {
		mutate(&cfg)
	}
	key, _ := evm.ParsePrivateKey(gasKey)
	e.snd = sender.New(cfg, e.q, e.app.Store, key, e.alerts, nil)
	return e
}

// paid creates a session and pays it on the fake chain, leaving one planned flush job.
func (e *env) paid() (string, evm.Address) {
	e.t.Helper()
	r := e.app.RevealedSession(e.app.TestKey, `{"amount":"25.00"}`, "Idempotency-Key", "k")
	var s struct {
		ID    string                     `json:"id"`
		PayTo []struct{ Address string } `json:"pay_to"`
	}
	_ = json.Unmarshal(r.Body, &s)
	to, _ := evm.ParseAddress(s.PayTo[0].Address)
	w := evm.Uint64Word(25_000_000)
	e.chain.Mine(evmfake.LogSpec{Address: usdc, Topics: []evm.Word{contracts.TransferTopic, evm.AddressWord(payer), evm.AddressWord(to)}, Data: w[:]})
	e.chain.MineN(1)
	e.bal[to] = 25_000_000
	for range 2 {
		if _, err := e.det.Step(e.t.Context()); err != nil {
			e.t.Fatal(err)
		}
	}
	return s.ID, to
}

func (e *env) step() {
	e.t.Helper()
	if _, err := e.snd.Plan(e.t.Context()); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.snd.Step(e.t.Context()); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) job(id string) store.TxJob {
	e.t.Helper()
	js, err := e.app.Store.SessionTxJobs(e.t.Context(), id)
	if err != nil || len(js) != 1 {
		e.t.Fatalf("jobs %+v %v", js, err)
	}
	return js[0]
}

func TestFlushSentWithCappedFees(t *testing.T) {
	t.Parallel()
	e := setup(t, nil, func(c *sender.Config) { c.MaxFeePerGas = big.NewInt(1_500_000) })
	id, _ := e.paid()
	e.step()
	j := e.job(id)
	if j.Status != store.JobSubmitted || len(e.sent) != 2 || j.Nonce == nil || *j.Nonce != 0 {
		t.Fatalf("job %+v sent %d", j, len(e.sent))
	}
	// 2 x base fee (1,000,000) + tip (1,000) exceeds the 1,500,000 cap, so the cap applies.
	if j.MaxFee.String() != "1500000" || j.Tip.String() != "1000" || j.GasLimit != 187_500 {
		t.Fatalf("fees max=%s tip=%s gas=%d", j.MaxFee, j.Tip, j.GasLimit)
	}
	e.step()
	if len(e.sent) != 4 {
		t.Fatalf("unmined transaction not re-broadcast: %d", len(e.sent))
	}
}

func TestBaseFeeAboveCapWaits(t *testing.T) {
	t.Parallel()
	e := setup(t, nil, func(c *sender.Config) { c.MaxFeePerGas = big.NewInt(500_000) })
	id, _ := e.paid()
	e.step()
	if j := e.job(id); j.Status != store.JobPending || len(e.sent) != 0 || !e.alerts.Has("gas_price_above_cap") {
		t.Fatalf("job %+v alerts %v", j, e.alerts.Kinds())
	}
}

func TestEmptyGasWalletAlerts(t *testing.T) {
	t.Parallel()
	e := setup(t, nil, nil)
	key, _ := evm.ParsePrivateKey(gasKey)
	e.chain.Balances[key.Address()] = big.NewInt(1000)
	id, _ := e.paid()
	e.step()
	if j := e.job(id); j.Status != store.JobPending || len(e.sent) != 0 {
		t.Fatalf("job %+v", j)
	}
	if !e.alerts.Has("gas_wallet_empty") || !e.alerts.Has("gas_wallet_low") {
		t.Fatalf("alerts %v", e.alerts.Kinds())
	}
}

func TestRevertingFlushRetriesWithBackoffThenGivesUp(t *testing.T) {
	t.Parallel()
	e := setup(t, nil, nil)
	e.chain.EstimateHandler = func(evmrpc.CallMsg) (uint64, error) {
		return 0, &evmrpc.RPCError{Code: 3, Message: "execution reverted: paused"}
	}
	id, _ := e.paid()
	e.step()
	j := e.job(id)
	if j.Status != store.JobPending || j.Attempts != 1 || !j.NextAttemptAt.After(e.app.Clock.Now()) {
		t.Fatalf("first failure %+v", j)
	}
	e.step()
	if e.job(id).Attempts != 1 {
		t.Fatal("retried before the backoff elapsed")
	}
	for range 2 {
		e.app.Clock.Advance(time.Minute)
		e.step()
	}
	j = e.job(id)
	if j.Status != store.JobFailed || j.Outcome != "gave_up" || j.Attempts != 3 {
		t.Fatalf("after max attempts %+v", j)
	}
	if !e.alerts.Has("flush_failing") || !e.alerts.Has("flush_gave_up") {
		t.Fatalf("alerts %v", e.alerts.Kinds())
	}
	if len(e.sent) != 0 {
		t.Fatal("a reverting flush was broadcast")
	}
}

func TestEmptyAndForeignAddresses(t *testing.T) {
	t.Parallel()
	e := setup(t, nil, nil)
	id, to := e.paid()
	e.bal[to] = 0 // someone else flushed before us
	e.step()
	// No final Settled event yet: the job waits for one to become final (D49) ...
	if j := e.job(id); j.Status != store.JobPending || !j.NextAttemptAt.After(e.app.Clock.Now()) {
		t.Fatalf("empty forwarder job not waiting %+v", j)
	}
	// ... and ends empty once any settlement would be final.
	e.app.Clock.Advance(time.Hour)
	e.step()
	if j := e.job(id); j.Status != store.JobDone || j.Outcome != "empty" {
		t.Fatalf("empty forwarder job %+v", j)
	}

	stub := setup(t, apptest.AnyMode{}, nil)
	sid, _ := stub.paid()
	stub.step()
	if js, _ := stub.app.Store.SessionTxJobs(t.Context(), sid); len(js) != 0 {
		t.Fatalf("flush planned for an address the factory does not derive: %+v", js)
	}
	if !stub.alerts.Has("flush_address_mismatch") {
		t.Fatalf("alerts %v", stub.alerts.Kinds())
	}
}

// A flush job whose from_block cannot be read fails as bad_job instead of rescanning the chain
// from block 0.
func TestEmptyForwarderWithAMalformedFromBlockFailsAsBadJob(t *testing.T) {
	t.Parallel()
	e := setup(t, nil, nil)
	id, to := e.paid()
	ctx := t.Context()
	if _, err := e.snd.Plan(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.app.Store.Pool().Exec(ctx, `UPDATE chain_tx_jobs SET details = jsonb_set(details, '{from_block}', '"12x"') WHERE id = $1`,
		e.job(id).ID); err != nil {
		t.Fatal(err)
	}
	e.bal[to] = 0
	logsBefore := e.pa.Calls("eth_getLogs") + e.pb.Calls("eth_getLogs")

	if _, err := e.snd.Step(ctx); err != nil {
		t.Fatal(err)
	}

	j := e.job(id)
	if j.Status != store.JobFailed || j.Outcome != "bad_job" || !strings.Contains(j.LastError, `from_block "12x"`) {
		t.Fatalf("job %+v", j)
	}
	if n := e.pa.Calls("eth_getLogs") + e.pb.Calls("eth_getLogs") - logsBefore; n != 0 {
		t.Fatalf("%d eth_getLogs calls for a job with no readable from_block", n)
	}
	if !e.alerts.Has("flush_bad_job") {
		t.Fatalf("alerts %v", e.alerts.Kinds())
	}
}

// An emptied forwarder's Settled events are read in ranges no wider than MaxLogRange, the same
// ranges from both providers, so a provider with a small eth_getLogs cap still answers.
// A job whose stored address cannot be read fails as bad_job with a critical alert. It is never
// read as the zero address: nothing is simulated, estimated or broadcast for it.
func TestMalformedJobAddressFailsWithoutSending(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ name, set string }{
		{"to", `to_address = 'not-an-address'`},
		{"address", `address = '0x1234'`},
		{"token", `details = jsonb_set(details, '{token}', '"0xzz"')`},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := setup(t, nil, nil)
			id, _ := e.paid()
			ctx := t.Context()
			if _, err := e.snd.Plan(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := e.app.Store.Pool().Exec(ctx, `UPDATE chain_tx_jobs SET `+c.set+` WHERE id = $1`, e.job(id).ID); err != nil {
				t.Fatal(err)
			}
			calls := func() int {
				n := 0
				for _, m := range []string{"eth_call", "eth_estimateGas", "eth_sendRawTransaction"} {
					n += e.pa.Calls(m) + e.pb.Calls(m)
				}
				return n
			}
			before := calls()

			if _, err := e.snd.Step(ctx); err != nil {
				t.Fatal(err)
			}

			j := e.job(id)
			if j.Status != store.JobFailed || j.Outcome != "bad_job" || !strings.Contains(j.LastError, c.name) {
				t.Fatalf("job %+v", j)
			}
			if n := calls() - before; n != 0 || len(e.sent) != 0 {
				t.Fatalf("%d calls, %d sent for a malformed job", n, len(e.sent))
			}
			if !e.alerts.Has("flush_bad_job") {
				t.Fatalf("alerts %v", e.alerts.Kinds())
			}
		})
	}
	t.Run("settle", func(t *testing.T) {
		t.Parallel()
		e := setup(t, nil, nil)
		j := e.settleJob()
		ctx := t.Context()
		if _, err := e.app.Store.Pool().Exec(ctx, `UPDATE chain_tx_jobs SET to_address = '' WHERE id = $1`, j.ID); err != nil {
			t.Fatal(err)
		}
		before := e.pa.Calls("eth_call") + e.pb.Calls("eth_call")

		if _, err := e.snd.Step(ctx); err != nil {
			t.Fatal(err)
		}

		got := e.job(j.SessionID)
		if got.Status != store.JobFailed || got.Outcome != "bad_job" || len(e.sent) != 0 {
			t.Fatalf("job %+v sent %d", got, len(e.sent))
		}
		if n := e.pa.Calls("eth_call") + e.pb.Calls("eth_call") - before; n != 0 {
			t.Fatalf("%d eth_call for a settle job with no readable target", n)
		}
		if !e.alerts.Has("settle_bad_job") {
			t.Fatalf("alerts %v", e.alerts.Kinds())
		}
	})
}

func TestForeignSettlementScanWithinLogRangeCap(t *testing.T) {
	t.Parallel()
	e := setup(t, nil, func(c *sender.Config) { c.MaxLogRange = 3 })
	e.pa.SetMaxLogRange(3)
	id, to := e.paid()
	ctx := t.Context()
	if _, err := e.snd.Plan(ctx); err != nil {
		t.Fatal(err)
	}
	e.chain.MineN(20)
	e.bal[to] = 0
	if _, err := e.snd.Step(ctx); err != nil {
		t.Fatal(err)
	}
	e.app.Clock.Advance(time.Hour) // past the wait for a settlement to become final
	before := [2]int{len(e.pa.LogRanges()), len(e.pb.LogRanges())}
	if _, err := e.snd.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if j := e.job(id); j.Status != store.JobDone || j.Outcome != "empty" {
		t.Fatalf("job %+v", j)
	}
	ra, rb := e.pa.LogRanges()[before[0]:], e.pb.LogRanges()[before[1]:]
	if len(ra) < 2 || len(ra) != len(rb) {
		t.Fatalf("asked a %v and b %v", ra, rb)
	}
	for i := range ra {
		if ra[i] != rb[i] || ra[i][1]-ra[i][0]+1 > 3 || (i > 0 && ra[i][0] != ra[i-1][1]+1) {
			t.Fatalf("range %d: a %v, b %v", i, ra[i], rb[i])
		}
	}
}

func word(n uint64) []byte { w := evm.Uint64Word(n); return w[:] }

// flushReceipt mines the job's flush: the forwarder's Payout events (one per given recipient
// and amount) then Settled, as ForwarderImpl.flush logs them.
func (e *env) flushReceipt(j store.TxJob, fwd evm.Address, gross, feeAmt uint64, payouts map[evm.Address]uint64, order []evm.Address) {
	e.t.Helper()
	e.mineFlush(j, fwd, gross, feeAmt, payouts, order)
	e.chain.MineN(5)
	e.step() // the receipt: mined, the finality check scheduled
	e.app.Clock.Advance(16 * time.Minute)
	e.step()
	e.step()
}

// mineFlush mines the job's flush transaction with its logs and registers its receipt.
func (e *env) mineFlush(j store.TxJob, fwd evm.Address, gross, feeAmt uint64, payouts map[evm.Address]uint64, order []evm.Address) {
	e.t.Helper()
	hash, _ := evm.ParseWord(j.TxHashes[0])
	treasury, _ := evm.ParseAddress(apptest.EVMTreasury)
	b := e.chain.Mine()
	var logs []evmrpc.Log
	net := uint64(0)
	for _, r := range order {
		net += payouts[r]
		logs = append(logs, evmrpc.Log{Address: fwd, Topics: []evm.Word{contracts.PayoutTopic, evm.AddressWord(usdc), evm.AddressWord(r)},
			Data: word(payouts[r])})
	}
	tw := evm.AddressWord(treasury)
	logs = append(logs, evmrpc.Log{Address: fwd, Topics: []evm.Word{contracts.SettledTopic, evm.AddressWord(usdc)},
		Data: append(append(append(word(gross), word(feeAmt)...), word(net)...), tw[:]...)})
	for i := range logs {
		logs[i].TxHash, logs[i].BlockNumber, logs[i].BlockHash, logs[i].LogIndex = hash, b.Number, b.Hash, uint32(i+1)
	}
	e.chain.SetReceipt(evmrpc.Receipt{TxHash: hash, Status: 1, BlockNumber: b.Number, BlockHash: b.Hash, GasUsed: 210_000, Logs: logs})
}

// splitDefault makes the test link pay its EVM chains 70/30 to the seeded wallet and a partner.
func (e *env) splitDefault() (main, partner evm.Address) {
	e.t.Helper()
	partner, _ = evm.ParseAddress("0x0000000000000000000000000000000000000B0b")
	w := e.app.Wallet(e.app.TestKey, chain.EVM, "Partner", partner.Hex())
	var mainID string
	for _, x := range e.app.Do("GET", "/v1/wallets?family=evm", e.app.TestKey, nil).JSON(e.t)["data"].([]any) {
		if m := x.(map[string]any); m["address"] == apptest.EVMDestination {
			mainID = m["id"].(string)
		}
	}
	e.app.SplitLink(e.app.TestKey, chain.EVM, mainID, "70", w.ID, "30")
	main, _ = evm.ParseAddress(apptest.EVMDestination)
	return main, partner
}

func TestFlushRecordsEachRecipientsPayout(t *testing.T) {
	t.Parallel()
	e := setup(t, nil, nil)
	main, partner := e.splitDefault()
	id, fwd := e.paid()
	e.step()
	j := e.job(id)
	if j.Details["recipients"] != main.Hex()+":7000,"+partner.Hex()+":3000" || j.Details["fee_bps"] != "50" {
		t.Fatalf("job details %v", j.Details)
	}
	words, _ := evm.Words(j.Calldata[4:])
	// salt, two offsets, then the params tuple (treasury, fee, fixed fee, three offsets, recipients,
	// bps, bound tokens) and the tokens to flush.
	if len(words) < 19 || words[9].Big().Uint64() != 2 || words[12].Big().Uint64() != 2 {
		t.Fatalf("deployAndFlush does not carry the two recipients: %x", j.Calldata)
	}
	if words[15].Big().Uint64() != 1 || words[17].Big().Uint64() != 1 || words[16] != words[18] {
		t.Fatalf("deployAndFlush does not bind and flush exactly the session asset: %x", j.Calldata)
	}
	e.flushReceipt(j, fwd, 25_000_000, 225_000, map[evm.Address]uint64{main: 17_342_500, partner: 7_432_500}, []evm.Address{main, partner})
	if j := e.job(id); j.Status != store.JobDone || j.Outcome != "settled" {
		t.Fatalf("job %+v alerts %v", j, e.alerts.Kinds())
	}
	ses, _ := e.app.Store.LoadSession(t.Context(), id)
	st := ses.Settlements[0]
	if len(ses.Settlements) != 1 || len(st.Payouts) != 2 || st.Payouts[0].Address != main.Hex() || st.Payouts[0].Amount.String() != "17342500" ||
		st.Payouts[1].Amount.String() != "7432500" || st.Payouts[1].Label == nil || *st.Payouts[1].Label != "Partner" {
		t.Fatalf("settlement %+v", ses.Settlements)
	}
}

func TestFlushWithForeignPayoutIsNotRecorded(t *testing.T) {
	t.Parallel()
	e := setup(t, nil, nil)
	main, _ := e.splitDefault()
	id, fwd := e.paid()
	e.step()
	stranger := evm.Address{0x5e}
	e.flushReceipt(e.job(id), fwd, 25_000_000, 225_000, map[evm.Address]uint64{main: 17_342_500, stranger: 7_432_500}, []evm.Address{main, stranger})
	ses, _ := e.app.Store.LoadSession(t.Context(), id)
	if len(ses.Settlements) != 0 || !e.alerts.Has("settlement_mismatch") {
		t.Fatalf("a settlement paying a stranger was recorded: %+v %v", ses.Settlements, e.alerts.Kinds())
	}
	if j := e.job(id); j.Outcome != "no_settlement" {
		t.Fatalf("job %+v", j)
	}
}
