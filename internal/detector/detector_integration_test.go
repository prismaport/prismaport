//go:build integration

package detector_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/alert"
	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/chainwatch"
	"github.com/prismaport/prismaport/internal/derive"
	"github.com/prismaport/prismaport/internal/detector"
	"github.com/prismaport/prismaport/internal/evm"
	"github.com/prismaport/prismaport/internal/evmfake"
	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/store"
)

var (
	usdc    = evm.Address{0xa0, 0x01}
	usdt    = evm.Address{0xa0, 0x02}
	gateway = evm.Address{0x6a, 0x7e}
	factory = evm.Address{0xfa, 0xc7}
	payer   = evm.Address{0x9a, 0x7e, 0x01}
)

func registry(t *testing.T) *chain.Registry {
	t.Helper()
	r, err := chain.NewRegistry([]chain.Chain{{ID: "anvil", Family: chain.EVM, Livemode: false, EVMID: 31337, Fee: chain.PilotFee(),
		Assets: []chain.Asset{{Symbol: "USDC", Token: usdc.Hex(), Decimals: 6, EIP3009: true}, {Symbol: "USDT", Token: usdt.Hex(), Decimals: 6}}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

type env struct {
	t      *testing.T
	app    *apptest.App
	chain  *evmfake.Chain
	pa, pb *evmfake.Provider
	q      *chainwatch.Quorum
	det    *detector.Detector
	alerts *alert.Recorder
	cfg    detector.Config
}

// setup builds a detector over two fake providers of one chain. Balances are answered from the
// chain's Transfer logs through the fake's Multicall3. Finality checks are due one minute after
// detection; the reveal margin (30 blocks) covers the blocks a test mines between a reveal and
// the first tick without moving the clock.
func setup(t *testing.T, mutate func(*detector.Config)) *env {
	t.Helper()
	return setupHold(t, 0, mutate)
}

// setupHold is setup with an expiry hold (service.Config.ExpiryHold, ADR-0008 H-3).
func setupHold(t *testing.T, hold time.Duration, mutate func(*detector.Config)) *env {
	t.Helper()
	reg := registry(t)
	app := apptest.New(t, apptest.Options{Registry: reg, Deriver: derive.Forwarders(factory), ExpiryHold: hold})
	c := evmfake.New(31337)
	c.SafeDepth, c.FinalDepth = 2, 4
	e := &env{t: t, app: app, chain: c, pa: c.Provider("a"), pb: c.Provider("b"), alerts: &alert.Recorder{}}
	e.q = chainwatch.NewQuorum("anvil", []chainwatch.Provider{e.pa, e.pb}, chainwatch.Options{})
	anvil, _ := reg.Get("anvil")
	gw := gateway
	e.cfg = detector.Config{Chain: anvil, Gateway: &gw, Policy: chainwatch.CreditPolicy{Confirmations: 2}, MaxRange: 5,
		ReanchorMargin: 30, FinalityDelay: time.Minute, FinalityRetry: time.Minute}
	if mutate != nil {
		mutate(&e.cfg)
	}
	e.det = e.newDetector()
	return e
}

func (e *env) newDetector() *detector.Detector {
	e.t.Helper()
	d, err := detector.New(e.cfg, e.q, e.app.Svc, e.app.Store, e.alerts, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	return d
}

// tick runs one detector tick.
func (e *env) tick() {
	e.t.Helper()
	if _, err := e.det.Step(e.t.Context()); err != nil {
		e.t.Fatalf("tick: %v", err)
	}
}

// drain runs two ticks (detection, then credit) and a third one past the finality delay.
func (e *env) drain() {
	e.t.Helper()
	e.tick()
	e.tick()
	e.app.Clock.Advance(e.cfg.FinalityDelay + time.Second)
	e.tick()
}

// calls is the number of requests both providers received.
func (e *env) calls() int { return e.pa.Total() + e.pb.Total() }

func (e *env) method(m string) int { return e.pa.Calls(m) + e.pb.Calls(m) }

type sessionView struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Received string `json:"amount_received"`
	PayTo    []struct {
		Chain, Asset, Address, Amount string
	} `json:"pay_to"`
	Payments []struct {
		ID, Status, TxHash, Amount string
	} `json:"payments"`
	Settlement *struct {
		Gross, Fee, Net string
		Payouts         []struct {
			WalletID string `json:"wallet_id"`
			Label    string `json:"label"`
			Address  string `json:"address"`
			Amount   string `json:"amount"`
		} `json:"payouts"`
	} `json:"settlement"`
}

func (e *env) create(amount, key string) sessionView {
	e.t.Helper()
	r := e.app.RevealedSession(e.app.TestKey, `{"amount":"`+amount+`"}`, "Idempotency-Key", key)
	if r.Status != 200 {
		e.t.Fatalf("create: %d %s", r.Status, r.Body)
	}
	var s sessionView
	_ = json.Unmarshal(r.Body, &s)
	return s
}

func (e *env) get(id string) sessionView {
	e.t.Helper()
	r := e.app.Do("GET", "/v1/sessions/"+id, e.app.TestKey, nil)
	var s sessionView
	_ = json.Unmarshal(r.Body, &s)
	return s
}

func addr(t *testing.T, s sessionView, asset string) evm.Address {
	t.Helper()
	for _, p := range s.PayTo {
		if p.Asset == asset {
			a, err := evm.ParseAddress(p.Address)
			if err != nil {
				t.Fatal(err)
			}
			return a
		}
	}
	t.Fatalf("no %s pay_to", asset)
	return evm.Address{}
}

func word(n uint64) []byte { w := evm.Uint64Word(n); return w[:] }

func transfer(token, from, to evm.Address, amount uint64) evmfake.LogSpec {
	return evmfake.LogSpec{Address: token, Topics: []evm.Word{detector.TransferTopic, evm.AddressWord(from), evm.AddressWord(to)}, Data: word(amount)}
}

// cost runs f and returns the requests both providers received meanwhile.
func (e *env) cost(f func()) int {
	before := e.calls()
	f()
	return e.calls() - before
}

// A deposit is found by the balance change of the next tick, credited once a later tick's head
// is CONFIRMATIONS deep and its block hash agreed, and finalized on schedule. Every step's
// requests are counted.
func TestExactPaymentDetectedConfirmedFinalized(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	s := e.create("25.00", "a")
	to := addr(t, s, "USDC")
	e.chain.MineN(2)
	e.chain.Mine(transfer(usdc, payer, to, 25_000_000))
	// aggregate 1 + agreed head 2 + getLogs 2 + the log's block hash 2
	if n := e.cost(e.tick); n != 7 {
		t.Fatalf("detecting tick made %d requests, want 7", n)
	}
	got := e.get(s.ID)
	if len(got.Payments) != 1 || got.Payments[0].Status != "detected" || got.Status != "open" {
		t.Fatalf("after detection: %+v", got)
	}
	// The head the payment waits on is stored for the checkout's confirmation count.
	detectedAt, found, err := e.app.Store.CreditHead(t.Context(), "anvil")
	if err != nil || !found {
		t.Fatalf("credit head after detection: %d %v %v", detectedAt, found, err)
	}
	e.chain.Mine()
	// aggregate 1 + the agreed head 2 (the aggregate's head is one provider's) + the payment's
	// block hash 2
	if n := e.cost(e.tick); n != 5 {
		t.Fatalf("crediting tick made %d requests, want 5", n)
	}
	if h, _, _ := e.app.Store.CreditHead(t.Context(), "anvil"); h != detectedAt+1 {
		t.Fatalf("credit head after one more block: %d, want %d", h, detectedAt+1)
	}
	got = e.get(s.ID)
	if got.Status != "paid" || got.Payments[0].Status != "confirmed" || got.Received != "25.00" {
		t.Fatalf("after 2 confirmations: %+v", got)
	}
	e.chain.MineN(3)
	if n := e.cost(e.tick); n != 1 {
		t.Fatalf("a tick before the finality check is due made %d requests, want 1", n)
	}
	e.app.Clock.Advance(time.Minute)
	// aggregate 1 + finalized header 1 + block hash 2
	if n := e.cost(e.tick); n != 4 {
		t.Fatalf("finality tick made %d requests, want 4", n)
	}
	if got = e.get(s.ID); got.Payments[0].Status != "finalized" {
		t.Fatalf("after finality: %+v", got)
	}
	// Paid exactly and final: the session leaves the watch (D47) and the chain reads nothing.
	if n := e.cost(e.tick); n != 0 {
		t.Fatalf("a settled session still cost %d requests", n)
	}
	if e.method("eth_getLogs") != 2 {
		t.Fatalf("%d getLogs over the session, want one agreed rescan", e.method("eth_getLogs"))
	}
}

// One aggregate per tick for any number of entries up to 500, whatever their sessions; the
// 501st entry adds a second aggregate. Nothing else is read while nothing changes.
func TestOneRequestPerTickForUpTo500Entries(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	for i := range 250 { // two entries (USDC, USDT) each
		e.create("1.00", fmt.Sprint("k", i))
	}
	e.chain.MineN(3)
	for i := range 4 {
		if n := e.cost(e.tick); n != 1 {
			t.Fatalf("tick %d over 500 entries made %d requests, want 1", i, n)
		}
	}
	if e.method("eth_call") != 4 || e.method("eth_getLogs") != 0 || e.method("eth_getBlockByNumber") != 0 {
		t.Fatalf("calls %d, getLogs %d, headers %d", e.method("eth_call"), e.method("eth_getLogs"), e.method("eth_getBlockByNumber"))
	}
	e.create("1.00", "one-more")
	if n := e.cost(e.tick); n != 2 {
		t.Fatalf("tick over 502 entries made %d requests, want 2", n)
	}
}

// Ticks alternate between the first two providers; a provider that fails is replaced by the
// other in the same tick and cooled down.
func TestProviderAlternationAndFailover(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	e.create("25.00", "a")
	e.chain.MineN(2)
	for range 4 {
		e.tick()
	}
	if e.pa.Calls("eth_call") != 2 || e.pb.Calls("eth_call") != 2 {
		t.Fatalf("aggregates a %d, b %d; want 2 each", e.pa.Calls("eth_call"), e.pb.Calls("eth_call"))
	}
	e.pa.SetDown(true)
	for range 4 {
		e.tick()
	}
	// a was asked once (and failed over to b), then cooled down: b served all four ticks.
	if e.pa.Calls("eth_call") != 3 || e.pb.Calls("eth_call") != 6 {
		t.Fatalf("with a down: a %d, b %d", e.pa.Calls("eth_call"), e.pb.Calls("eth_call"))
	}
	e.pb.SetDown(true)
	if _, err := e.det.Step(t.Context()); err == nil {
		t.Fatal("a tick with every provider down succeeded")
	}
}

func TestPartialTopUpOverpaidAndNoise(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	s := e.create("25.00", "a")
	o := e.create("10.00", "b")
	to, toO := addr(t, s, "USDC"), addr(t, o, "USDT")
	other := evm.Address{0x01, 0x02}
	nft := evmfake.LogSpec{Address: usdc, Topics: []evm.Word{detector.TransferTopic, evm.AddressWord(payer), evm.AddressWord(to), evm.Uint64Word(1)}}
	e.chain.Mine(
		transfer(usdc, payer, to, 10_000_000),
		transfer(other, payer, to, 15_000_000),   // unknown token contract
		transfer(usdc, payer, to, 0),             // poisoning
		nft,                                      // ERC-721 shape
		transfer(usdt, payer, to, 1),             // USDT to the USDC session: still the same session, asset USDT
		transfer(usdc, payer, other, 99_000_000), // not our address
		transfer(usdt, payer, toO, 10_000_001),   // overpay by one base unit
	)
	e.chain.MineN(2)
	e.drain()
	got := e.get(s.ID)
	if got.Status != "partially_paid" || len(got.Payments) != 2 || got.Received != "10.000001" {
		t.Fatalf("partial: %+v", got)
	}
	if g := e.get(o.ID); g.Status != "overpaid" {
		t.Fatalf("overpaid: %+v", g)
	}
	logs := e.method("eth_getLogs")
	e.chain.Mine(transfer(usdc, payer, to, 14_999_999))
	e.chain.MineN(2)
	e.drain()
	if got = e.get(s.ID); got.Status != "paid" || got.Received != "25.00" {
		t.Fatalf("top up: %+v", got)
	}
	// The top-up rescan starts after the block the first one reached.
	if r := e.pa.LogRanges(); e.method("eth_getLogs") != logs+2 || r[len(r)-1][0] <= 2 {
		t.Fatalf("top-up rescan: %d getLogs, last range %v", e.method("eth_getLogs")-logs, r[len(r)-1])
	}
}

// Every asset of a session shares its address: each token's balance is kept on its own, so a
// paid USDC entry next to an empty USDT one never rescans again.
func TestBalancesAreKeptPerToken(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	s := e.create("25.00", "a")
	to := addr(t, s, "USDC")
	e.chain.Mine(transfer(usdc, payer, to, 10_000_000))
	e.chain.MineN(2)
	e.tick()
	logs := e.method("eth_getLogs")
	for range 3 {
		e.chain.Mine()
		e.tick()
	}
	if e.method("eth_getLogs") != logs {
		t.Fatalf("%d more getLogs with no balance change", e.method("eth_getLogs")-logs)
	}
	e.chain.Mine(transfer(usdt, payer, to, 5_000_000))
	e.tick()
	// USDT has no stored balance yet, so its rescan starts at the address's first block (the
	// movements since must explain the reading, H-1): two ranges of MaxRange 5, both providers.
	if got := e.get(s.ID); len(got.Payments) != 2 || e.method("eth_getLogs") != logs+4 {
		t.Fatalf("USDT deposit: %d getLogs, %+v", e.method("eth_getLogs")-logs, got)
	}
}

// Our own flush lowers the balance: the tick stores it and rescans nothing, and the next
// deposit counts as a change even when it equals the flushed amount.
func TestOwnFlushDoesNotRescan(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	s := e.create("25.00", "a")
	to := addr(t, s, "USDC")
	e.chain.Mine(transfer(usdc, payer, to, 25_000_000))
	e.chain.MineN(2)
	e.tick()
	if got := e.get(s.ID); got.Status != "paid" {
		t.Fatalf("paid: %+v", got)
	}
	// Paid, its payment not final yet: still watched.
	merchant := evm.Address{0x3e}
	logs := e.method("eth_getLogs")
	e.chain.Mine(transfer(usdc, to, merchant, 25_000_000))
	if n := e.cost(e.tick); n != 1 || e.method("eth_getLogs") != logs {
		t.Fatalf("the flush tick made %d requests (%d getLogs)", n, e.method("eth_getLogs")-logs)
	}
	e.chain.Mine(transfer(usdc, payer, to, 25_000_000))
	e.tick()
	if got := e.get(s.ID); len(got.Payments) != 2 || e.method("eth_getLogs") != logs+2 {
		t.Fatalf("deposit after the flush: %d getLogs, %+v", e.method("eth_getLogs")-logs, got)
	}
}

// A payment whose block is replaced before it is credited is reversed by the hash re-check of
// the crediting tick, and the transfer mined again is found by the rescan that follows.
func TestReorgBeforeCreditReversedAndReminedLogCreditedAgain(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	s := e.create("25.00", "a")
	to := addr(t, s, "USDC")
	e.chain.MineN(2)
	spec := transfer(usdc, payer, to, 25_000_000)
	spec.TxHash = evm.Word{0x77}
	e.chain.Mine(spec)
	e.tick()
	if got := e.get(s.ID); len(got.Payments) != 1 || got.Payments[0].Status != "detected" {
		t.Fatalf("detected: %+v", got)
	}
	e.chain.Reorg(1)
	e.chain.MineN(2)
	e.tick()
	got := e.get(s.ID)
	if got.Status != "open" || got.Payments[0].Status != "reversed" || got.Received != "0.00" || !e.alerts.Has("reorg") {
		t.Fatalf("after reorg: %+v, alerts %v", got, e.alerts.Kinds())
	}
	e.chain.Mine(spec) // the same transaction mined again
	e.chain.Mine()
	e.drain()
	got = e.get(s.ID)
	if got.Status != "paid" || len(got.Payments) != 2 || got.Payments[1].TxHash != got.Payments[0].TxHash {
		t.Fatalf("re-mined: %+v", got)
	}
}

// With the default policy a payment counts at 10 blocks, far ahead of finality. A reorg deeper
// than those 10 blocks but above finality is caught by the finality check's hash re-check, after
// session.paid was already emitted.
func TestCreditedAtTenBlocksThenReversedAtTheFinalityCheck(t *testing.T) {
	t.Parallel()
	e := setup(t, func(c *detector.Config) { c.Policy = chainwatch.CreditPolicy{} })
	e.chain.SafeDepth, e.chain.FinalDepth = 40, 64
	s := e.create("25.00", "a")
	to := addr(t, s, "USDC")
	e.chain.MineN(2)
	e.chain.Mine(transfer(usdc, payer, to, 25_000_000))
	e.chain.MineN(chainwatch.DefaultConfirmations - 2)
	e.tick()
	e.tick()
	if got := e.get(s.ID); got.Status != "open" || got.Payments[0].Status != "detected" {
		t.Fatalf("credited before %d blocks: %+v", chainwatch.DefaultConfirmations, got)
	}
	e.chain.Mine()
	e.tick()
	got := e.get(s.ID)
	if got.Status != "paid" || got.Payments[0].Status != "confirmed" || got.Received != "25.00" {
		t.Fatalf("not credited at %d blocks: %+v", chainwatch.DefaultConfirmations, got)
	}
	e.chain.Reorg(chainwatch.DefaultConfirmations + 2)
	e.chain.MineN(chainwatch.DefaultConfirmations + 4)
	e.app.Clock.Advance(time.Minute)
	headers := e.method("eth_getBlockByNumber")
	e.tick() // due, but the finalized head is still below the block: one header, checked again later
	if got := e.get(s.ID); got.Payments[0].Status != "confirmed" || e.method("eth_getBlockByNumber") != headers+1 {
		t.Fatalf("before finality: %d header reads, %+v", e.method("eth_getBlockByNumber")-headers, got)
	}
	e.tick()
	if e.method("eth_getBlockByNumber") != headers+1 {
		t.Fatal("the finalized head was read again before the retry was due")
	}
	e.chain.MineN(64)
	e.app.Clock.Advance(time.Minute)
	e.tick()
	got = e.get(s.ID)
	if got.Status != "open" || got.Payments[0].Status != "reversed" || got.Received != "0.00" {
		t.Fatalf("after a reorg past the credit depth: %+v", got)
	}
	evs, _ := e.app.Store.SessionEvents(t.Context(), s.ID)
	if evs[len(evs)-1].Type != "payment.reversed" || evs[len(evs)-2].Type != "session.paid" {
		t.Fatalf("events %s, %s", evs[len(evs)-2].Type, evs[len(evs)-1].Type)
	}
	if !e.alerts.Has("reorg") {
		t.Fatalf("no reorg alert: %v", e.alerts.Kinds())
	}
}

// A provider on another fork answers its own balances, but the rescan needs both providers to
// return the same logs: nothing is credited and the chain halts until they agree again.
func TestProviderDisagreementHaltsCredit(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	s := e.create("25.00", "a")
	to := addr(t, s, "USDC")
	e.chain.MineN(2)
	e.tick()
	fork := e.chain.Fork()
	e.chain.Mine(transfer(usdc, payer, to, 25_000_000))
	fork.Mine()
	e.chain.MineN(2)
	fork.MineN(2)
	e.pb.Point(fork)
	disagreed := 0
	for range 4 {
		if _, err := e.det.Step(t.Context()); errors.Is(err, chainwatch.ErrDisagreement) {
			disagreed++
		} else if err != nil {
			t.Fatalf("tick: %v", err)
		}
	}
	if disagreed == 0 {
		t.Fatal("no tick saw the providers disagree")
	}
	if got := e.get(s.ID); len(got.Payments) != 0 {
		t.Fatalf("credited while providers disagree: %+v", got)
	}
	if st := e.q.Status(); st.State != chainwatch.Halted {
		t.Fatalf("status %+v", st)
	}
	e.pb.Point(e.chain) // the provider catches up with the canonical chain
	e.drain()
	if got := e.get(s.ID); got.Status != "paid" {
		t.Fatalf("after providers agree again: %+v", got)
	}
}

// Transfers made while the worker was down are all found by the first tick after it: the rescan
// starts at the block the last one reached.
func TestRestartWithoutGaps(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	s := e.create("25.00", "a")
	to := addr(t, s, "USDC")
	e.chain.Mine(transfer(usdc, payer, to, 10_000_000))
	e.tick()
	for i := range 20 {
		if i%7 == 3 {
			e.chain.Mine(transfer(usdc, payer, to, 5_000_000))
			continue
		}
		e.chain.Mine()
	}
	e.app.Clock.Advance(40 * time.Second)
	e.det = e.newDetector()
	e.drain()
	if got := e.get(s.ID); got.Status != "paid" || len(got.Payments) != 4 {
		t.Fatalf("after restart: %+v", got)
	}
}

func TestOneProviderDownFailsOverToThird(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	pc := e.chain.Provider("c")
	e.q = chainwatch.NewQuorum("anvil", []chainwatch.Provider{e.pa, e.pb, pc}, chainwatch.Options{})
	e.det = e.newDetector()
	s := e.create("25.00", "a")
	e.pa.SetDown(true)
	e.chain.Mine(transfer(usdc, payer, addr(t, s, "USDC"), 25_000_000))
	e.chain.MineN(2)
	e.drain()
	if got := e.get(s.ID); got.Status != "paid" {
		t.Fatalf("failover: %+v", got)
	}
	o := e.create("25.00", "b")
	e.chain.Mine(transfer(usdc, payer, addr(t, o, "USDC"), 25_000_000))
	e.pb.SetDown(true)
	if _, err := e.det.Step(t.Context()); !errors.Is(err, chainwatch.ErrNoQuorum) {
		t.Fatalf("single provider must not credit: %v", err)
	}
	if got := e.get(o.ID); len(got.Payments) != 0 {
		t.Fatalf("recorded through one provider: %+v", got)
	}
}

// A detected payment of a session that expired keeps the chain reading its head: an aggregate
// without calls (one request) until it is credited.
func TestDetectedPaymentOfAnExpiredSessionReadsTheHeadAlone(t *testing.T) {
	t.Parallel()
	e := setup(t, func(c *detector.Config) { c.FinalityDelay = 3 * time.Hour })
	s := e.create("25.00", "a")
	e.chain.Mine(transfer(usdc, payer, addr(t, s, "USDC"), 25_000_000))
	e.tick()
	e.app.Clock.Advance(time.Hour)
	e.exec(`UPDATE sessions SET expires_at = $2 WHERE id = $1`, s.ID, e.app.Clock.Now().Add(-time.Minute))
	if n := e.cost(e.tick); n != 1 || e.get(s.ID).Payments[0].Status != "detected" {
		t.Fatalf("waiting tick: %d requests", n)
	}
	e.chain.Mine()
	if n := e.cost(e.tick); n != 5 || e.get(s.ID).Payments[0].Status != "confirmed" { // aggregate, agreed head, hash
		t.Fatalf("crediting tick: %d requests, %+v", n, e.get(s.ID))
	}
	// Nothing watched and nothing due: no request until the finality check.
	if n := e.cost(e.tick); n != 0 {
		t.Fatalf("idle tick: %d requests", n)
	}
}

// A pay_to entry whose stored token does not parse is skipped with a critical alert, never read
// as the zero address and never dropped in silence; the other sessions are still read.
func TestUnparseableEntryAlertsAndIsSkipped(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	bad := e.create("25.00", "bad")
	good := e.create("25.00", "good")
	e.exec(`UPDATE session_pay_to SET token = 'not-a-token' WHERE session_id = $1`, bad.ID)
	to := addr(t, good, "USDC")
	e.chain.MineN(2)
	e.chain.Mine(transfer(usdc, payer, to, 25_000_000))
	e.tick()
	var crit bool
	for _, a := range e.alerts.All() {
		crit = crit || (a.Kind == "watch_entry_unparseable" && a.Severity == alert.Critical && a.Fields["session"] == bad.ID)
	}
	if !crit {
		t.Fatalf("alerts %v", e.alerts.Kinds())
	}
	if got := e.get(good.ID); len(got.Payments) != 1 {
		t.Fatalf("the other session was not read: %+v", got)
	}
}

func (e *env) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.app.Store.Pool().Exec(e.t.Context(), sql, args...); err != nil {
		e.t.Fatal(err)
	}
}

// gatewayLogs builds the logs of a Gateway.settle transaction: the token Transfer into the
// Gateway, the fee leg, one Transfer and one Payout per non-zero share in recipient order, then
// PaymentReceived. shares are the amounts per recipient of the session's snapshot.
func gatewayLogs(t *testing.T, e *env, s sessionView, gross, feeAmt uint64, shares ...uint64) []evmfake.LogSpec {
	t.Helper()
	ses, err := e.app.Store.LoadSession(t.Context(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	pt := ses.PayTo[0]
	rcpts, bps, err := derive.EVMRecipients(pt.Recipients)
	if err != nil {
		t.Fatal(err)
	}
	if len(shares) != len(rcpts) {
		t.Fatalf("%d shares for %d recipients", len(shares), len(rcpts))
	}
	tre, _ := evm.ParseAddress(pt.Treasury)
	nonce := evm.GatewayNonce(31337, gateway, ses.Salt, tre, pt.Fee.FeeBps, pt.Fee.FixedFeeMicros, rcpts, bps)
	tx := evm.Word{0x5e, byte(gross), byte(feeAmt)}
	net := uint64(0)
	logs := []evmfake.LogSpec{transfer(usdc, payer, gateway, gross), transfer(usdc, gateway, tre, feeAmt)}
	for i, amount := range shares {
		net += amount
		if amount == 0 {
			continue
		}
		logs = append(logs, transfer(usdc, gateway, rcpts[i], amount), payoutLog(gateway, rcpts[i], amount))
	}
	data := append(append(word(gross), word(feeAmt)...), word(net)...)
	logs = append(logs, evmfake.LogSpec{Address: gateway, Topics: []evm.Word{detector.PaymentReceivedTopic, nonce, evm.AddressWord(usdc),
		evm.AddressWord(payer)}, Data: data})
	for i := range logs {
		logs[i].TxHash = tx
	}
	return logs
}

func payoutLog(emitter, recipient evm.Address, amount uint64) evmfake.LogSpec {
	return evmfake.LogSpec{Address: emitter, Topics: []evm.Word{detector.PayoutTopic, evm.AddressWord(usdc), evm.AddressWord(recipient)},
		Data: word(amount)}
}

// settled hands a settle job's mined block to the detector, as the sender does.
func (e *env) settled(s sessionView, block uint64, reverted bool) int {
	e.t.Helper()
	n, err := e.det.GatewaySettled(e.t.Context(), store.TxJob{ID: "txj_test", SessionID: s.ID}, block, reverted)
	if err != nil {
		e.t.Fatalf("gateway settled: %v", err)
	}
	return n
}

// An EIP-3009 payment is found from its settle job's block (agreed hash, Gateway logs and the
// token transfers into the Gateway: 6 requests), never by a per-tick Gateway scan, and its split
// is recorded as the settlement at finality.
func TestGatewayPaymentRecordedFromTheSettleBlock(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	s := e.create("25.00", "a")
	b := e.chain.Mine(gatewayLogs(t, e, s, 25_000_000, 225_000, 24_775_000)...)
	e.chain.MineN(2)
	e.tick()
	if got := e.get(s.ID); len(got.Payments) != 0 || e.method("eth_getLogs") != 0 {
		t.Fatalf("a tick read the Gateway: %d getLogs, %+v", e.method("eth_getLogs"), got)
	}
	var found int
	if n := e.cost(func() { found = e.settled(s, b.Number, false) }); n != 6 || found != 1 {
		t.Fatalf("settle block read: %d requests, %d found", n, found)
	}
	e.tick()
	got := e.get(s.ID)
	if got.Status != "paid" || got.Settlement != nil {
		t.Fatalf("confirmed gateway payment: %+v", got)
	}
	e.chain.MineN(3)
	e.app.Clock.Advance(time.Minute)
	e.tick()
	got = e.get(s.ID)
	if got.Payments[0].Status != "finalized" || got.Settlement == nil || got.Settlement.Fee != "0.225000" || got.Settlement.Net != "24.775000" ||
		len(got.Settlement.Payouts) != 1 || got.Settlement.Payouts[0].Amount != "24.775000" || got.Settlement.Payouts[0].Address != apptest.EVMDestination {
		t.Fatalf("finalized gateway payment: %+v", got)
	}
	if e.method("eth_getLogs") != 4 {
		t.Fatalf("%d getLogs over the session, want the settle block's 4", e.method("eth_getLogs"))
	}
	// A reverted settle (someone settled the authorization first) looks back to the address's
	// first block; the payment already recorded is not recorded twice.
	if found := e.settled(s, e.chain.Tip().Number, true); found != 1 || len(e.get(s.ID).Payments) != 1 {
		t.Fatalf("reverted settle lookback: %d found, %+v", found, e.get(s.ID))
	}
}

// A signed-path payment whose block is reorged out is reversed and its settle job reopened (no
// rescan reads the Gateway): the sender follows the transaction again, and the block it is mined
// in again credits the payment again.
func TestReorgedGatewayPaymentReopensItsSettleJob(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	s := e.create("25.00", "a")
	logs := gatewayLogs(t, e, s, 25_000_000, 225_000, 24_775_000)
	e.chain.MineN(2)
	b := e.chain.Mine(logs...)
	tx := logs[0].TxHash.Hex()
	j, _, err := e.app.Store.InsertTxJob(t.Context(), store.TxJob{Chain: "anvil", Kind: store.JobSettle, Ref: "payer|nonce",
		SessionID: s.ID, Asset: "USDC", To: gateway.Hex(), Calldata: []byte{1}})
	if err != nil {
		t.Fatal(err)
	}
	n, nonce := b.Number, uint64(0)
	j.Status, j.Outcome, j.Nonce, j.TxHashes, j.RawTx = store.JobDone, "settled", &nonce, []string{tx}, []byte{2}
	j.MinedTxHash, j.MinedBlock, j.MinedBlockHash = tx, &n, b.Hash.Hex()
	if err := e.app.Store.UpdateTxJob(t.Context(), &j); err != nil {
		t.Fatal(err)
	}
	if found := e.settled(s, b.Number, false); found != 1 {
		t.Fatalf("settle block: %d found", found)
	}
	e.tick()
	if got := e.get(s.ID); len(got.Payments) != 1 || got.Payments[0].Status != "detected" {
		t.Fatalf("detected: %+v", got)
	}
	e.chain.Reorg(1)
	e.chain.MineN(2)
	e.tick()
	got := e.get(s.ID)
	if got.Status != "open" || got.Payments[0].Status != "reversed" || !e.alerts.Has("reorg") {
		t.Fatalf("after reorg: %+v, alerts %v", got, e.alerts.Kinds())
	}
	re, err := e.app.Store.GetTxJob(t.Context(), j.ID)
	if err != nil || re.Status != store.JobSubmitted || re.Outcome != "" || re.MinedTxHash != "" || re.MinedBlock != nil ||
		len(re.TxHashes) != 1 || re.TxHashes[0] != tx || len(re.RawTx) == 0 {
		t.Fatalf("settle job after reorg: %+v %v", re, err)
	}
	nb := e.chain.Mine(logs...) // the same transaction mined again; the sender reads its receipt
	e.chain.Mine()
	if found := e.settled(s, nb.Number, false); found != 1 {
		t.Fatalf("re-mined settle block: %d found", found)
	}
	e.drain()
	got = e.get(s.ID)
	if got.Status != "paid" || len(got.Payments) != 2 || got.Payments[1].Status != "confirmed" {
		t.Fatalf("re-mined: %+v", got)
	}
}

// A signed-path settlement moves the payer's tokens through the Gateway, never the deposit
// address: a partial deposit still there keeps its watched balance (a zero stamped over it would
// read as an unexplained rise and rescan the address).
func TestGatewaySettlementLeavesTheDepositBalance(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	s := e.create("25.00", "a")
	to := addr(t, s, "USDC")
	e.chain.MineN(2)
	e.chain.Mine(transfer(usdc, payer, to, 10_000_000))
	e.chain.MineN(2)
	e.tick()
	if got := e.storedBalance(to); got != "10000000" {
		t.Fatalf("stored balance after the deposit: %q", got)
	}
	b := e.chain.Mine(gatewayLogs(t, e, s, 15_000_000, 175_000, 14_825_000)...)
	e.chain.MineN(5)
	if found := e.settled(s, b.Number, false); found != 1 {
		t.Fatalf("settle block: %d found, alerts %v", found, e.alerts.All())
	}
	e.drain()
	got := e.get(s.ID)
	if got.Status != "paid" || got.Settlement == nil || e.storedBalance(to) != "10000000" || e.alerts.Has("balance_unexplained") {
		t.Fatalf("after the gateway settlement: %+v, stored %q, alerts %v", got, e.storedBalance(to), e.alerts.Kinds())
	}
}

// The head a tick's aggregate reports comes from one provider: a payment it would confirm waits
// until the agreed head (both providers) is as deep.
func TestCreditWaitsForTheAgreedHead(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	s := e.create("25.00", "a")
	e.chain.MineN(2)
	e.chain.Mine(transfer(usdc, payer, addr(t, s, "USDC"), 25_000_000))
	e.tick()
	if got := e.get(s.ID); len(got.Payments) != 1 || got.Payments[0].Status != "detected" {
		t.Fatalf("detected: %+v", got)
	}
	e.pb.SetLag(1)
	e.chain.Mine()
	e.tick()
	e.tick()
	if got := e.get(s.ID); got.Payments[0].Status != "detected" {
		t.Fatalf("credited on one provider's head: %+v", got)
	}
	e.pb.SetLag(0)
	e.tick()
	if got := e.get(s.ID); got.Payments[0].Status != "confirmed" || got.Status != "paid" {
		t.Fatalf("not credited at the agreed head: %+v", got)
	}
}

// splitDefault makes the test merchant's EVM default a 70/30 split between the seeded wallet and
// a second one, and returns the second wallet's address.
func splitDefault(t *testing.T, e *env) evm.Address {
	t.Helper()
	partner := "0x0000000000000000000000000000000000000B0b"
	w := e.app.Wallet(e.app.TestKey, chain.EVM, "Partner", partner)
	list := e.app.Do("GET", "/v1/wallets?family=evm", e.app.TestKey, nil).JSON(t)["data"].([]any)
	var main string
	for _, x := range list {
		if m := x.(map[string]any); m["address"] == apptest.EVMDestination {
			main = m["id"].(string)
		}
	}
	e.app.SplitLink(e.app.TestKey, chain.EVM, main, "70.00", w.ID, "30.00")
	a, _ := evm.ParseAddress(partner)
	return a
}

func TestGatewayPaymentSplitAmongRecipients(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	splitDefault(t, e)
	s := e.create("25.00", "a")
	b := e.chain.Mine(gatewayLogs(t, e, s, 25_000_000, 225_000, 17_342_500, 7_432_500)...)
	e.chain.MineN(5)
	e.settled(s, b.Number, false)
	e.drain()
	got := e.get(s.ID)
	if got.Status != "paid" || got.Settlement == nil || len(got.Settlement.Payouts) != 2 ||
		got.Settlement.Payouts[0].Amount != "17.342500" || got.Settlement.Payouts[1].Amount != "7.432500" ||
		got.Settlement.Payouts[1].Label != "Partner" || got.Settlement.Payouts[0].Address != apptest.EVMDestination {
		t.Fatalf("split gateway payment: %+v", got)
	}
}

func TestGatewayEventsThatDoNotAddUpAreRejected(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	splitDefault(t, e)
	s := e.create("25.00", "a")
	bad := gatewayLogs(t, e, s, 25_000_000, 100_000, 17_395_000, 7_505_000) // fee below the rule
	noTransfer := gatewayLogs(t, e, s, 30_000_000, 300_000, 20_790_000, 8_910_000)[1:]
	forged := gatewayLogs(t, e, s, 25_000_000, 225_000, 17_342_500, 7_432_500)
	forged[len(forged)-1].Address = evm.Address{0xba, 0xd0} // emitted by a contract that is not the Gateway
	wrongShares := gatewayLogs(t, e, s, 25_000_000, 225_000, 17_343_500, 7_431_500)
	swapped := gatewayLogs(t, e, s, 25_000_000, 225_000, 17_342_500, 7_432_500)
	swapped[2], swapped[3], swapped[4], swapped[5] = swapped[4], swapped[5], swapped[2], swapped[3]
	stranger := gatewayLogs(t, e, s, 25_000_000, 225_000, 17_342_500, 7_432_500)
	stranger[5].Topics[2] = evm.AddressWord(evm.Address{0x5e})
	spoofed := gatewayLogs(t, e, s, 25_000_000, 225_000, 17_342_500, 7_432_500)
	spoofed[5].Address = evm.Address{0xba, 0xd1} // the second Payout comes from another contract
	var blocks []uint64
	for _, logs := range [][]evmfake.LogSpec{bad, noTransfer, forged, wrongShares, swapped, stranger, spoofed} {
		blocks = append(blocks, e.chain.Mine(logs...).Number)
	}
	e.chain.MineN(2)
	for _, b := range blocks {
		if n := e.settled(s, b, false); n != 0 {
			t.Fatalf("block %d: %d payments recorded", b, n)
		}
	}
	e.drain()
	if got := e.get(s.ID); len(got.Payments) != 0 {
		t.Fatalf("credited a bad gateway event: %+v", got)
	}
	kinds := e.alerts.Kinds()
	if len(kinds) != 6 {
		t.Fatalf("alerts %v", kinds)
	}
	for _, k := range kinds {
		if k != "gateway_event_rejected" {
			t.Fatalf("alerts %v", kinds)
		}
	}
}

// A long gap is rescanned through providers that cap eth_getLogs (an Alchemy-like 10 blocks and a
// sepolia.base.org-like 200): every range fits the stricter cap, both providers are asked for
// identical ranges, and payments on chunk edges are all credited.
func TestRescanWithinProviderLogRangeCaps(t *testing.T) {
	t.Parallel()
	e := setup(t, func(c *detector.Config) { c.MaxRange = 10; c.ReanchorMargin = 0 })
	e.pa.SetMaxLogRange(10)
	e.pb.SetMaxLogRange(200)
	s := e.create("25.00", "a")
	to := addr(t, s, "USDC")
	e.tick() // registers the address at the current head
	pays := 0
	for e.chain.Tip().Number < 95 {
		if n := e.chain.Tip().Number + 1; n == 10 || n == 11 || n == 20 || n == 61 || n == 95 {
			e.chain.Mine(transfer(usdc, payer, to, 5_000_000))
			pays++
			continue
		}
		e.chain.Mine()
	}
	e.chain.MineN(2)
	e.drain()
	got := e.get(s.ID)
	if got.Status != "paid" || got.Received != "25.00" || len(got.Payments) != pays {
		t.Fatalf("after the gap: %d payments, want %d: %+v", len(got.Payments), pays, got)
	}
	ra, rb := e.pa.LogRanges(), e.pb.LogRanges()
	if len(ra) < 9 || len(ra) != len(rb) {
		t.Fatalf("providers asked for %d and %d ranges", len(ra), len(rb))
	}
	for i := range ra {
		if ra[i] != rb[i] {
			t.Fatalf("range %d differs: a %v, b %v", i, ra[i], rb[i])
		}
		if ra[i][1] < ra[i][0] || ra[i][1]-ra[i][0]+1 > 10 {
			t.Fatalf("range %v exceeds the 10-block cap", ra[i])
		}
	}
}

// Without a configured limit a rescan asks for 500 blocks per range, which a provider capped
// below that rejects: nothing is recorded and the entry's balance and block stay, so the next
// tick tries again.
func TestDefaultLogRangeIs500(t *testing.T) {
	t.Parallel()
	e := setup(t, func(c *detector.Config) { c.MaxRange = 0; c.ReanchorMargin = 600 })
	s := e.create("25.00", "a")
	e.chain.MineN(600)
	e.chain.Mine(transfer(usdc, payer, addr(t, s, "USDC"), 25_000_000))
	e.tick()
	if r := e.pa.LogRanges(); len(r) != 2 || r[0] != [2]uint64{1, 500} || r[1] != [2]uint64{501, 601} {
		t.Fatalf("ranges %v", r)
	}

	capped := setup(t, func(c *detector.Config) { c.MaxRange = 0; c.ReanchorMargin = 600 })
	capped.pa.SetMaxLogRange(10)
	cs := capped.create("25.00", "a")
	capped.chain.MineN(600)
	capped.chain.Mine(transfer(usdc, payer, addr(t, cs, "USDC"), 25_000_000))
	for range 2 {
		if _, err := capped.det.Step(t.Context()); err == nil {
			t.Fatal("a 500-block range passed a 10-block provider")
		}
	}
	if got := capped.get(cs.ID); len(got.Payments) != 0 {
		t.Fatalf("recorded past a rejected range: %+v", got)
	}
}

// Factory.deployAndFlush is permissionless: someone else flushing a deposit within one tick of
// its arrival leaves no balance to see. Their flush deploys the forwarder, which the same
// aggregate probes (no extra request): a deployment without a flush job of ours rescans the
// address once and records the transfer.
func TestForeignFlushBetweenTwoReadsIsFound(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	s := e.create("25.00", "a")
	to := addr(t, s, "USDC")
	e.tick()
	e.chain.Mine(transfer(usdc, payer, to, 25_000_000))
	b := e.chain.Mine(flush(usdc, to, 25_000_000)...)
	e.chain.Deploy(to, b.Number)
	e.chain.MineN(2)
	// aggregate 1 (balance 0, forwarder deployed) + agreed head 2 + transfer getLogs 2 + the
	// forwarder's Settled getLogs 2 + the two logs' block hashes 4, then the credit in the same
	// tick (deep enough): agreed head 2 + hash 2
	if n := e.cost(e.tick); n != 15 {
		t.Fatalf("tick after a foreign flush made %d requests, want 15", n)
	}
	if got := e.get(s.ID); len(got.Payments) != 1 || got.Received != "25.00" || got.Status != "paid" {
		t.Fatalf("deposit flushed by someone else not found: %+v", got)
	}
	logs := e.method("eth_getLogs")
	e.tick()
	if e.method("eth_getLogs") != logs+1 { // the deployed forwarder's Settled read (H-4)
		t.Fatalf("a deployment already handled rescanned again: %d getLogs, stored %q", e.method("eth_getLogs")-logs, e.storedBalance(to))
	}
}

// ownFlushJob inserts a flush job of ours for the address; sent gives it a transaction (as the
// sender does once it submits it), else it is only planned.
func (e *env) ownFlushJob(s sessionView, to evm.Address, sent bool) {
	e.t.Helper()
	ctx := e.t.Context()
	total := money.FromUint64(25_000_000)
	j, _, err := e.app.Store.InsertTxJob(ctx, store.TxJob{Chain: "anvil", Kind: store.JobFlush, Ref: "own",
		SessionID: s.ID, Asset: "USDC", Address: to.Hex(), PlannedTotal: &total, To: evm.Address{0xfa}.Hex(), Calldata: []byte{1}})
	if err != nil {
		e.t.Fatal(err)
	}
	if sent {
		nonce := uint64(0)
		j.Status, j.Nonce, j.TxHashes = store.JobSubmitted, &nonce, []string{evm.Word{0x0f}.Hex()}
		if err := e.app.Store.UpdateTxJob(ctx, &j); err != nil {
			e.t.Fatal(err)
		}
	}
}

// Our own flush deploys the forwarder too: no rescan, the deployment is only remembered.
func TestOwnFlushDeploymentDoesNotRescan(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	s := e.create("25.00", "a")
	to := addr(t, s, "USDC")
	e.tick()
	e.ownFlushJob(s, to, true)
	e.chain.Deploy(to, e.chain.Tip().Number)
	e.chain.Mine()
	logs := e.method("eth_getLogs")
	if n := e.cost(e.tick); n != 1 || e.method("eth_getLogs") != logs {
		t.Fatalf("own deployment: %d requests", n)
	}
	var seen *time.Time
	if err := e.app.Store.Pool().QueryRow(t.Context(), `SELECT forwarder_seen_at FROM chain_watch WHERE address = $1`,
		strings.ToLower(to.Hex())).Scan(&seen); err != nil || seen == nil {
		t.Fatalf("deployment not remembered: %v %v", seen, err)
	}
}

// The tick's aggregate3 lets the forwarder probes fail on their own: a forwarder of an
// implementation without tokens() (before D41) reverts its probe, which counts as deployed, and
// every other entry of the batch is still read, in the same single request. With a plain
// aggregate the whole batch reverted and no entry of the chain was read.
func TestRevertingForwarderProbeDoesNotBreakTheBatch(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	old := e.create("25.00", "a")
	oldTo := addr(t, old, "USDC")
	s := e.create("25.00", "b")
	to := addr(t, s, "USDC")
	e.tick()
	e.ownFlushJob(old, oldTo, true)
	e.chain.DeployLegacy(oldTo, e.chain.Tip().Number)
	e.chain.Mine()
	if n := e.cost(e.tick); n != 1 {
		t.Fatalf("a tick with a reverting probe made %d requests, want 1", n)
	}
	var seen *time.Time
	if err := e.app.Store.Pool().QueryRow(t.Context(), `SELECT forwarder_seen_at FROM chain_watch WHERE address = $1`,
		strings.ToLower(oldTo.Hex())).Scan(&seen); err != nil || seen == nil {
		t.Fatalf("legacy forwarder not seen deployed: %v %v", seen, err)
	}
	e.chain.Mine(transfer(usdc, payer, to, 25_000_000))
	e.chain.MineN(2)
	e.tick()
	if got := e.get(s.ID); got.Status != "paid" || len(got.Payments) != 1 {
		t.Fatalf("the other entry of the batch: %+v", got)
	}
}

// A flush job of ours only planned (not sent: the gas price above the cap, say) deployed nothing:
// a deployment next to it is someone else's deployAndFlush, which rescans the address and records
// the deposit that came and left with it. Before, any job of ours for the address counted as the
// deployer and the deposit was never recorded.
func TestForeignFlushNextToOurPlannedJobIsFound(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	s := e.create("25.00", "a")
	to := addr(t, s, "USDC")
	e.chain.Mine(transfer(usdc, payer, to, 25_000_000))
	e.chain.MineN(2)
	e.tick()
	if got := e.get(s.ID); got.Status != "paid" {
		t.Fatalf("paid: %+v", got)
	}
	e.ownFlushJob(s, to, false)
	e.chain.Mine(transfer(usdc, payer, to, 10_000_000))
	b := e.chain.Mine(flush(usdc, to, 35_000_000)...)
	e.chain.Deploy(to, b.Number)
	e.chain.MineN(2)
	e.tick()
	e.tick()
	if got := e.get(s.ID); len(got.Payments) != 2 || got.Received != "35.00" || got.Status != "overpaid" {
		t.Fatalf("deposit flushed by someone else next to our planned job: %+v", got)
	}
}

// D1 is credited and our flush empties the address at block F. Provider b then stays stuck before
// F: its reading (the old, higher balance) is older than the balance stored after the flush and
// is ignored, neither stored nor rescanned. A partial deposit D2 therefore still reads as a rise
// and is credited once both providers serve its block. Storing b's reading would have made D2
// look like a decrease, never rescanned.
func TestStaleReadingBeforeOurFlushDoesNotHideTheNextDeposit(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	s := e.create("25.00", "a")
	to := addr(t, s, "USDC")
	e.chain.Mine(transfer(usdc, payer, to, 10_000_000)) // D1
	e.chain.MineN(2)
	e.tick()
	e.tick()
	if got := e.get(s.ID); got.Status != "partially_paid" || len(got.Payments) != 1 {
		t.Fatalf("D1: %+v", got)
	}
	e.chain.Mine(transfer(usdc, to, evm.Address{0x3e}, 10_000_000)) // our flush
	f := e.chain.Tip().Number
	e.tick() // a fresh reading stores 0 at a block >= F
	stuck := func() { e.pb.SetLag(e.chain.Tip().Number - (f - 1)) }
	for range 3 {
		e.chain.Mine()
		stuck()
		e.tick()
	}
	e.chain.Mine(transfer(usdc, payer, to, 5_000_000)) // D2, partial
	for range 3 {
		e.chain.Mine()
		stuck()
		e.tick()
	}
	e.pb.SetLag(0)
	e.chain.MineN(2)
	e.tick()
	e.tick()
	if got := e.get(s.ID); len(got.Payments) != 2 || got.Received != "15.00" {
		t.Fatalf("D2 not credited after a stale reading: %+v", got)
	}
}

// A flush that emptied the address and a smaller deposit after it, both between two reads (here
// before the sender saw the flush's receipt): the balance reads lower than the stored one but not
// zero, which rescans and credits the deposit. Before, a lower reading was stored as a decrease
// and never rescanned.
func TestLowerNonZeroBalanceRescans(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	s := e.create("25.00", "a")
	to := addr(t, s, "USDC")
	e.chain.Mine(transfer(usdc, payer, to, 25_000_000))
	e.chain.MineN(2)
	e.tick()
	b := e.chain.Mine(flush(usdc, to, 25_000_000)...) // someone else's deployAndFlush
	e.chain.Deploy(to, b.Number)
	e.chain.Mine(transfer(usdc, payer, to, 10_000_000))
	e.chain.MineN(2)
	e.tick()
	e.tick()
	if got := e.get(s.ID); len(got.Payments) != 2 || got.Received != "35.00" || got.Status != "overpaid" {
		t.Fatalf("deposit after a flush, below the stored balance: %+v", got)
	}
	// 25 + 10 in, 25 flushed out: the reading is explained and stored.
	if bal := e.storedBalance(to); bal != "10000000" {
		t.Fatalf("stored balance %q, want 10000000", bal)
	}
}

// Our flush's receipt stores a zero balance at its block F. If F is then reorged out, the zero
// hides nothing: readings from before F are ignored only until the chain passes F again, and a
// reading past it shows the funds (the first deposit back, plus a new one) as a rise, which
// rescans and credits the new deposit.
func TestFlushReorgedOutDoesNotHideTheFunds(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	s := e.create("25.00", "a")
	to := addr(t, s, "USDC")
	e.chain.Mine(transfer(usdc, payer, to, 25_000_000))
	e.chain.MineN(2)
	e.tick()
	f := e.chain.Mine(transfer(usdc, to, evm.Address{0x3e}, 25_000_000)) // our flush
	if err := e.app.Store.FlushMined(t.Context(), "anvil", to.Hex(), usdc.Hex(), f.Number, e.det.BaselineReach()); err != nil {
		t.Fatal(err)
	}
	e.chain.Mine()
	e.tick()
	e.chain.Reorg(2) // F is gone; the first deposit is back on the address
	e.tick()         // read below F: ignored
	e.chain.Mine(transfer(usdc, payer, to, 5_000_000))
	e.chain.MineN(2)
	e.tick()
	e.tick()
	if got := e.get(s.ID); len(got.Payments) != 2 || got.Received != "30.00" {
		t.Fatalf("deposit after our flush was reorged out: %+v", got)
	}
	var bal string
	if err := e.app.Store.Pool().QueryRow(t.Context(), `SELECT token_balances ->> $2 FROM chain_watch WHERE address = $1`,
		strings.ToLower(to.Hex()), strings.ToLower(usdc.Hex())).Scan(&bal); err != nil || bal != "30000000" {
		t.Fatalf("stored balance %q (%v), want the funds back on the address", bal, err)
	}
}

// checked_block is the address's: a rescan triggered by one asset records the other asset's
// transfers in its range too. Here our USDT flush and a new USDT deposit land between two reads
// (the USDT balance reads lower, no rescan of its own) while a USDC deposit triggers the rescan
// of the same range: the USDT deposit is recorded by it, not skipped for good.
func TestRescanOfOneAssetRecordsTheOtherAssetsTransfers(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	s := e.create("25.00", "a")
	to := addr(t, s, "USDC")
	e.chain.Mine(transfer(usdt, payer, to, 5_000_000))
	e.chain.MineN(2)
	e.tick()
	if got := e.get(s.ID); len(got.Payments) != 1 {
		t.Fatalf("first USDT deposit: %+v", got)
	}
	e.chain.Mine(transfer(usdt, to, evm.Address{0x3e}, 5_000_000)) // our USDT flush
	e.chain.Mine(transfer(usdt, payer, to, 3_000_000), transfer(usdc, payer, to, 10_000_000))
	e.chain.MineN(2)
	e.tick()
	for range 3 {
		e.chain.Mine()
		e.tick()
	}
	got := e.get(s.ID)
	if len(got.Payments) != 3 || got.Received != "18.00" {
		t.Fatalf("both assets paid in one rescan range: %+v", got)
	}
}

// The idle heartbeat is one latest header from one provider, alternating; a provider down is
// failed over (the chain shows Degraded), and only every provider down fails it.
func TestHeartbeatIsOneHeaderRequestAlternating(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	ctx := t.Context()
	for range 2 {
		if n := e.cost(func() {
			if err := e.det.Heartbeat(ctx); err != nil {
				t.Fatal(err)
			}
		}); n != 1 {
			t.Fatalf("heartbeat made %d requests", n)
		}
	}
	if e.pa.Total() != 1 || e.pb.Total() != 1 {
		t.Fatalf("not alternating: a %d, b %d", e.pa.Total(), e.pb.Total())
	}
	e.pa.SetDown(true)
	for range 2 {
		if err := e.det.Heartbeat(ctx); err != nil {
			t.Fatalf("one provider down: %v", err)
		}
	}
	if st := e.q.Status(); st.State != chainwatch.Degraded {
		t.Fatalf("status with a provider down: %+v", st)
	}
	e.pb.SetDown(true)
	if err := e.det.Heartbeat(ctx); err == nil {
		t.Fatal("every provider down and the heartbeat succeeded")
	}
}
