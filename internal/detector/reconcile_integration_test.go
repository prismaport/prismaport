//go:build integration

package detector_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/prismaport/prismaport/internal/alert"
	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/chainsettle"
	"github.com/prismaport/prismaport/internal/contracts"
	"github.com/prismaport/prismaport/internal/detector"
	"github.com/prismaport/prismaport/internal/evm"
	"github.com/prismaport/prismaport/internal/evmfake"
	"github.com/prismaport/prismaport/internal/evmrpc"
	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/payout"
	"github.com/prismaport/prismaport/internal/scan"
	"github.com/prismaport/prismaport/internal/store"
	"github.com/prismaport/prismaport/internal/worker"
)

// flush is a forwarder's flush of amount of token as the chain shows it: the token's Transfer
// out of the forwarder and the forwarder's Settled event (no fee).
func flush(token, fwd evm.Address, amount uint64) []evmfake.LogSpec {
	treasury := evm.AddressWord(evm.Address{0x7e})
	data := append(append(append(word(amount), word(0)...), word(amount)...), treasury[:]...)
	return []evmfake.LogSpec{
		transfer(token, fwd, evm.Address{0x3e}, amount),
		{Address: fwd, Topics: []evm.Word{contracts.SettledTopic, evm.AddressWord(token)}, Data: data},
	}
}

// storedBalance is the USDC balance the watch row keeps for the address.
func (e *env) storedBalance(to evm.Address) string {
	e.t.Helper()
	var bal *string
	if err := e.app.Store.Pool().QueryRow(e.t.Context(), `SELECT token_balances ->> $2 FROM chain_watch WHERE address = $1`,
		strings.ToLower(to.Hex()), strings.ToLower(usdc.Hex())).Scan(&bal); err != nil {
		e.t.Fatal(err)
	}
	if bal == nil {
		return ""
	}
	return *bal
}

// H-1: the balance already shows the deposit but both providers' log index still trails the
// head, so the rescan finds no transfer. The risen balance is not stored (it is not explained by
// the logs) and the next ticks rescan the same range until the logs show up. Before, the rescan
// stored the balance and moved checked_block past the deposit: every later tick read "unchanged"
// and the session was never credited.
func TestRiseTheLogsDoNotShowYetIsRescannedUntilTheyDo(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	s := e.create("25.00", "a")
	to := addr(t, s, "USDC")
	e.tick()
	e.pa.SetLogLag(3)
	e.pb.SetLogLag(3)
	e.chain.Mine(transfer(usdc, payer, to, 25_000_000))
	e.chain.MineN(2)
	e.tick()
	if got := e.get(s.ID); len(got.Payments) != 0 {
		t.Fatalf("logs hidden by the index lag were found: %+v", got)
	}
	if bal := e.storedBalance(to); bal == "25000000" {
		t.Fatal("the rise was stored while the logs do not explain it")
	}
	e.tick() // still lagging: retried, nothing stored
	e.pa.SetLogLag(0)
	e.pb.SetLogLag(0)
	e.tick()
	e.tick()
	if got := e.get(s.ID); got.Status != "paid" || len(got.Payments) != 1 || got.Received != "25.00" {
		t.Fatalf("deposit not credited once the logs caught up: %+v", got)
	}
	if bal := e.storedBalance(to); bal != "25000000" {
		t.Fatalf("stored balance %q, want 25000000", bal)
	}
	if e.alerts.Has("balance_unexplained") {
		t.Fatal("a lag that resolved raised an alert")
	}
}

// A balance the logs never explain is retried only for ReconcileWait: then it is stored, so the
// address is not rescanned on every tick for good, and a Critical alert says the session may be
// missing a payment ("Check again" recovers it once the logs show it, see
// TestCheckAgainCreditsADepositStoredUnexplained).
func TestRiseNeverExplainedIsStoredAfterTheWaitWithAnAlert(t *testing.T) {
	t.Parallel()
	e := setup(t, func(c *detector.Config) { c.ReconcileWait = 10 * time.Minute })
	s := e.create("25.00", "a")
	to := addr(t, s, "USDC")
	e.tick()
	e.pa.SetLogLag(1_000)
	e.pb.SetLogLag(1_000)
	e.chain.Mine(transfer(usdc, payer, to, 25_000_000))
	e.chain.MineN(2)
	e.tick()
	e.app.Clock.Advance(5 * time.Minute)
	e.chain.Mine()
	e.tick()
	if bal := e.storedBalance(to); bal == "25000000" || e.alerts.Has("balance_unexplained") {
		t.Fatalf("within the wait: stored %q, alerts %v", bal, e.alerts.Kinds())
	}
	e.app.Clock.Advance(6 * time.Minute)
	e.chain.Mine()
	e.tick()
	if bal := e.storedBalance(to); bal != "25000000" || !e.raised("balance_unexplained", alert.Critical) {
		t.Fatalf("after the wait: stored %q, alerts %v", bal, e.alerts.All())
	}
	logs := e.method("eth_getLogs")
	e.chain.Mine()
	e.tick()
	if e.method("eth_getLogs") != logs {
		t.Fatal("a balance stored after the wait is still rescanned")
	}
}

// deployedByAThirdParty credits a first deposit of 10 and lets someone else deployAndFlush it:
// the forwarder is then deployed and seen.
func (e *env) deployedByAThirdParty(s sessionView, to evm.Address) {
	e.t.Helper()
	e.chain.Mine(transfer(usdc, payer, to, 10_000_000))
	e.chain.MineN(2)
	e.tick()
	b := e.chain.Mine(flush(usdc, to, 10_000_000)...)
	e.chain.Deploy(to, b.Number)
	e.chain.MineN(2)
	e.tick()
	if got := e.get(s.ID); got.Status != "partially_paid" || len(got.Payments) != 1 {
		e.t.Fatalf("first deposit: %+v", got)
	}
}

// H-4: once the forwarder is deployed anyone can flush() each new deposit before the next balance
// read: the balance reads 0 -> 0. The tick's single-provider read of the deployed forwarders'
// Settled events sees the flush and rescans, which credits the deposit. Before, the deposit was
// never seen.
func TestThirdPartyFlushOnADeployedForwarderIsFound(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	s := e.create("25.00", "a")
	to := addr(t, s, "USDC")
	e.deployedByAThirdParty(s, to)
	e.chain.Mine(transfer(usdc, payer, to, 15_000_000))
	e.chain.Mine(flush(usdc, to, 15_000_000)...)
	e.chain.MineN(2)
	e.tick()
	e.tick()
	if got := e.get(s.ID); got.Status != "paid" || len(got.Payments) != 2 || got.Received != "25.00" {
		t.Fatalf("deposit flushed by someone else on a deployed forwarder: %+v", got)
	}
	// Nothing new: the aggregate, the agreed head (one header per provider) and one
	// single-provider Settled read per tick, no rescan.
	e.chain.Mine()
	logs := e.method("eth_getLogs")
	if n := e.cost(e.tick); n != 4 || e.method("eth_getLogs") != logs+1 {
		t.Fatalf("idle tick on a deployed forwarder: %d requests, %d getLogs", n, e.method("eth_getLogs")-logs)
	}
}

// H-4, the balance that falls to zero: a deposit of 5 is read (not flushed yet), then another
// deposit and a flush of both land between two reads. A fall to zero on a deployed forwarder is
// rescanned (the flush may have carried a deposit no read saw), not stored as a plain flush.
func TestFallToZeroOnADeployedForwarderRescans(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	s := e.create("25.00", "a")
	to := addr(t, s, "USDC")
	e.deployedByAThirdParty(s, to)
	e.chain.Mine(transfer(usdc, payer, to, 5_000_000))
	e.chain.MineN(2)
	e.tick()
	e.chain.Mine(transfer(usdc, payer, to, 10_000_000))
	e.chain.Mine(flush(usdc, to, 15_000_000)...)
	e.chain.MineN(2)
	e.tick()
	e.tick()
	if got := e.get(s.ID); got.Status != "paid" || len(got.Payments) != 3 || got.Received != "25.00" {
		t.Fatalf("deposit carried by a flush the balance fell to zero with: %+v", got)
	}
}

// ourFlush mines our flush of amount on the address's forwarder as the sender sees it: a flush
// job of ours (so the deployment is ours), the flush's logs and then extra logs in the same block,
// the deployment, and the zero the receipt stores at the flush's block (FlushMined).
func (e *env) ourFlush(s sessionView, to evm.Address, amount uint64, extra ...evmfake.LogSpec) *evmfake.Block {
	e.t.Helper()
	e.ownFlushJob(s, to, true)
	f := e.chain.Mine(append(flush(usdc, to, amount), extra...)...)
	e.chain.Deploy(to, f.Number)
	if err := e.app.Store.FlushMined(e.t.Context(), "anvil", to.Hex(), usdc.Hex(), f.Number, e.det.BaselineReach()); err != nil {
		e.t.Fatal(err)
	}
	return f
}

// H-1 on a customer address: the next session takes the watch row over with the address's
// stored balances (here the zero our flush's receipt stored at its block F), so its rescans have
// a known baseline. The first session is credited, our flush empties the address at F (after the
// address's cursor), the next session opens, and the payer's deposit shows in the balance while
// both providers' log index lags. Before, the takeover cleared the balances and the window from
// the cursor held our flush's Settled at F, which "explained" any non-zero balance of a taken-over
// address: the deposit's balance was stored, the cursor moved past it, and it was never credited.
func TestTakenOverCustomerAddressRescansFromTheStoredBalance(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	addrID, to := e.customerAddress("user_h1")
	s1 := e.openOn(addrID, "25.00", "a")
	e.chain.MineN(2)
	e.drain()
	e.chain.Mine(transfer(usdc, payer, to, 25_000_000))
	e.chain.MineN(2)
	e.drain()
	if got := e.get(s1.ID); got.Status != "paid" {
		t.Fatalf("first session: %+v", got)
	}
	e.ourFlush(s1, to, 25_000_000)
	e.chain.Mine()
	e.tick() // unchanged at zero: our deployment is remembered, nothing rescanned

	s2 := e.openOn(addrID, "", "b")
	e.pa.SetLogLag(3)
	e.pb.SetLogLag(3)
	e.chain.Mine(transfer(usdc, payer, to, 10_000_000))
	e.chain.MineN(2)
	e.tick()
	if owner, _ := e.watchOwner(to); owner != s2.ID {
		t.Fatalf("watch row of %s, want %s", owner, s2.ID)
	}
	if bal := e.storedBalance(to); bal != "0" {
		t.Fatalf("stored balance %q while the logs do not explain the deposit, want the zero kept", bal)
	}
	e.tick() // still lagging: retried
	e.pa.SetLogLag(0)
	e.pb.SetLogLag(0)
	e.tick()
	e.tick()
	if got := e.get(s2.ID); len(got.Payments) != 1 || got.Received != "10.00" {
		t.Fatalf("deposit on the taken-over address: %+v", got)
	}
	if bal := e.storedBalance(to); bal != "10000000" || e.alerts.Has("balance_unexplained") {
		t.Fatalf("stored balance %q, alerts %v", bal, e.alerts.Kinds())
	}
	if got := e.get(s1.ID); len(got.Payments) != 1 {
		t.Fatalf("first session: %+v", got)
	}
}

// A deposit mined in our flush's block after the flush is not in the zero the receipt stored at
// that block: the rescan counts the deposits of that block after its last flush, so the reading
// is explained at once. Before, the window started after the flush's block and, on a customer
// address (no retry from a first block), the address was held for ReconcileWait before a false
// Critical balance_unexplained (the deposit itself was credited).
func TestDepositInOurFlushBlockAfterTheFlushIsExplained(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	addrID, to := e.customerAddress("user_h1c")
	s := e.openOn(addrID, "50.00", "a")
	e.chain.MineN(2)
	e.drain()
	e.chain.Mine(transfer(usdc, payer, to, 25_000_000))
	e.chain.MineN(2)
	e.drain()
	e.ourFlush(s, to, 25_000_000, transfer(usdc, payer, to, 5_000_000))
	e.chain.MineN(2)
	e.tick()
	e.tick()
	if got := e.get(s.ID); len(got.Payments) != 2 || got.Received != "30.00" {
		t.Fatalf("deposit in our flush's block: %+v", got)
	}
	if bal := e.storedBalance(to); bal != "5000000" {
		t.Fatalf("stored balance %q, want the deposit explained and stored", bal)
	}
}

// settle records our flush of 25.00 mined at block for s, as the sender does at its finality.
func (e *env) settle(s sessionView, block uint64) {
	e.t.Helper()
	ctx := e.t.Context()
	ses, err := e.app.Store.LoadSession(ctx, s.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	pt := ses.PayTo[0]
	for _, p := range ses.PayTo {
		if p.Asset == "USDC" {
			pt = p
		}
	}
	gross := money.FromUint64(25_000_000)
	rule, err := payout.Compute(gross, pt.Decimals, pt.Fee, payout.Bps(pt.Recipients))
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.app.Store.RecordSettlement(ctx, s.ID, store.Settlement{Chain: "anvil", Asset: "USDC", Gross: gross, Fee: rule.Fee,
		Net: rule.Net, Shares: rule.Shares, TxHash: "0x" + strings.Repeat("f1", 32), SettledAt: e.app.Clock.Now(), Block: block}, chainsettle.Verify); err != nil {
		e.t.Fatal(err)
	}
}

// The settlement of the first session's flush is recorded at the flush's finality, after the next
// session on the customer address was paid and its balance stored. That balance is the address's,
// read after the flush: the settlement's zero is not stamped over it. Before, the zero replaced it
// (at the later block), the next reading looked like a rise no log explained, and the address was
// held for ReconcileWait before a false Critical balance_unexplained.
func TestSettlementAfterTheNextDepositKeepsItsBalance(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	addrID, to := e.customerAddress("user_h1b")
	s1 := e.openOn(addrID, "25.00", "a")
	e.chain.MineN(2)
	e.drain()
	e.chain.Mine(transfer(usdc, payer, to, 25_000_000))
	e.chain.MineN(2)
	e.drain()
	f := e.ourFlush(s1, to, 25_000_000)
	e.chain.Mine()
	e.tick()
	s2 := e.openOn(addrID, "", "b")
	e.chain.Mine(transfer(usdc, payer, to, 10_000_000))
	e.chain.MineN(2)
	e.tick()
	if bal := e.storedBalance(to); bal != "10000000" {
		t.Fatalf("second session's balance %q", bal)
	}
	e.settle(s1, f.Number)
	logs := e.method("eth_getLogs")
	e.chain.Mine()
	e.tick()
	if bal := e.storedBalance(to); bal != "10000000" || e.method("eth_getLogs") != logs+1 { // the Settled read only
		t.Fatalf("after the first session's settlement: stored %q, %d getLogs", bal, e.method("eth_getLogs")-logs)
	}
	e.tick()
	if got := e.get(s2.ID); len(got.Payments) != 1 || got.Received != "10.00" {
		t.Fatalf("second session: %+v", got)
	}
}

// The flush probe reads up to the agreed head, not the head of the provider that answered the
// aggregate: a provider behind the other one is never asked for blocks past its own head (an
// error that cools it down and costs a second request on the other provider). Whichever provider
// answers the aggregate, every probe is one request.
func TestFlushProbeStaysWithinTheAgreedHead(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	s := e.create("25.00", "a")
	to := addr(t, s, "USDC")
	e.deployedByAThirdParty(s, to)
	for _, behind := range []*evmfake.Provider{e.pb, e.pa} {
		e.pa.SetLag(0)
		e.pb.SetLag(0)
		behind.SetLag(2)
		e.chain.MineN(3)
		logs := e.method("eth_getLogs")
		e.tick()
		if n := e.method("eth_getLogs") - logs; n != 1 {
			t.Fatalf("probe with provider %s behind: %d getLogs, want 1", behind.Name(), n)
		}
	}
}

// H-1, the first tick of a new customer address: the second provider is one block behind, so the
// agreed head the takeover's rescan reaches is below the block the balance was read at, and no
// balance is stored. The address's cursor stays unset until every watched token has a stored
// balance, so the zero before the address's creation stays the baseline. Two deposits arrive and
// the second one's log lags: the reading is not explained and waits for that log. Before, the
// cursor moved on the first tick, the baseline became unknown, and the first deposit's movement
// "explained" the reading: the balance was stored, the cursor passed the lagging deposit, and it
// was never credited, with no alert.
func TestFirstTickBehindKeepsTheCustomerAddressBaseline(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	addrID, to := e.customerAddress("user_h1d")
	s := e.openOn(addrID, "", "a")
	e.chain.MineN(2)
	e.pb.SetLag(1)
	e.tick() // the takeover: the aggregate from a, the agreed head one block below it
	if bal := e.storedBalance(to); bal != "" {
		t.Fatalf("the first tick stored %q: the scenario needs the agreed head below the reading", bal)
	}
	e.pb.SetLag(0)
	e.chain.Mine(transfer(usdc, payer, to, 10_000_000))
	e.chain.Mine(transfer(usdc, payer, to, 5_000_000))
	e.pa.SetLogLag(1)
	e.pb.SetLogLag(1)
	e.tick()
	if bal := e.storedBalance(to); bal == "15000000" {
		t.Fatal("a reading one deposit's log does not explain was stored")
	}
	e.pa.SetLogLag(0)
	e.pb.SetLogLag(0)
	e.chain.MineN(2)
	e.tick()
	e.tick()
	if got := e.get(s.ID); len(got.Payments) != 2 || got.Received != "15.00" {
		t.Fatalf("both deposits on a new customer address: %+v", got)
	}
	if bal := e.storedBalance(to); bal != "15000000" || e.alerts.Has("balance_unexplained") {
		t.Fatalf("stored %q, alerts %v", bal, e.alerts.Kinds())
	}
}

// storedBlock is the block of the balance the watch row keeps for the address's token.
func (e *env) storedBlock(to, token evm.Address) uint64 {
	e.t.Helper()
	var b *int64
	if err := e.app.Store.Pool().QueryRow(e.t.Context(), `SELECT (token_blocks ->> $2)::bigint FROM chain_watch WHERE address = $1`,
		strings.ToLower(to.Hex()), strings.ToLower(token.Hex())).Scan(&b); err != nil || b == nil {
		e.t.Fatalf("stored block of %s: %v %v", token.Hex(), b, err)
	}
	return uint64(*b)
}

// A stored balance far older than where a customer address's rescan starts (here USDT's zero,
// left as it was while the address was read for USDC) is not a baseline past BaselineReach: the
// next session's rescan starts at the cursor rather than back at that block, and the token's
// reading is checked against the window's movements. Before, the rescan (the tick's, for every
// changed address) went back to that block: months of MAX_LOG_RANGE chunks for a token the
// address's sessions left alone, again on every tick while the reading was unexplained.
func TestOldStoredBalanceDoesNotPullTheRescanBack(t *testing.T) {
	t.Parallel()
	e := setup(t, func(c *detector.Config) { c.BaselineReach = 20 * 2 * time.Second }) // 20 blocks
	addrID, to := e.customerAddress("user_h1e")
	s1 := e.openOn(addrID, "25.00", "a")
	e.chain.MineN(2)
	e.drain()
	old := e.storedBlock(to, usdt)
	e.chain.MineN(40)
	e.chain.Mine(transfer(usdc, payer, to, 25_000_000))
	e.chain.MineN(2)
	e.drain()
	if got := e.get(s1.ID); got.Status != "paid" || e.storedBlock(to, usdt) != old {
		t.Fatalf("first session %+v, USDT stored at %d (was %d)", got, e.storedBlock(to, usdt), old)
	}
	s2 := e.openOn(addrID, "", "b")
	e.chain.Mine(transfer(usdt, payer, to, 7_000_000))
	e.chain.MineN(2)
	seen := map[*evmfake.Provider]int{e.pa: len(e.pa.LogRanges()), e.pb: len(e.pb.LogRanges())}
	e.tick()
	for p, n := range seen {
		for _, r := range p.LogRanges()[n:] {
			if r[0] <= old+20 {
				t.Fatalf("the rescan went back to block %d, USDT's stored balance is from %d", r[0], old)
			}
		}
	}
	e.tick()
	if got := e.get(s2.ID); len(got.Payments) != 1 {
		t.Fatalf("second session: %+v", got)
	}
}

// A flush's zero (our receipt's, FlushMined) replaces the balance stored before it, and the
// sender's rescan after a flush that moved other than was credited (RescanSession) starts after
// checked_block. A deposit between that earlier balance and checked_block whose log lagged (here
// a rescan for the other asset moved checked_block past it before its log was served) was then
// read by nobody: later windows start at the flush. The zero now moves checked_block back to the
// block of the balance it replaces, so the sender's rescan reads those blocks again.
func TestFlushRescanReadsFromTheBalanceBeforeTheFlush(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	ctx := t.Context()
	s := e.create("50.00", "a")
	to := addr(t, s, "USDC")
	e.chain.Mine(transfer(usdc, payer, to, 25_000_000))
	e.chain.MineN(2)
	e.tick()
	if bal := e.storedBalance(to); bal != "25000000" {
		t.Fatalf("first deposit stored %q", bal)
	}
	e.chain.Mine(transfer(usdc, payer, to, 10_000_000))
	e.exec(`UPDATE chain_watch SET checked_block = $2 WHERE address = $1`, strings.ToLower(to.Hex()), int64(e.chain.Tip().Number))
	f := e.ourFlush(s, to, 35_000_000) // moved 35, 25 credited: the sender asks for a rescan
	e.chain.MineN(2)
	e.tick() // unchanged at zero: nothing rescanned
	if _, err := e.det.RescanSession(ctx, s.ID, f.Number); err != nil {
		t.Fatal(err)
	}
	e.tick()
	e.tick()
	if got := e.get(s.ID); len(got.Payments) != 2 || got.Received != "35.00" {
		t.Fatalf("deposit before our flush, after checked_block moved past it: %+v", got)
	}
}

// checkedBlock is the address's checked_block.
func (e *env) checkedBlock(to evm.Address) uint64 {
	e.t.Helper()
	var b *int64
	if err := e.app.Store.Pool().QueryRow(e.t.Context(), `SELECT checked_block FROM chain_watch WHERE address = $1`,
		strings.ToLower(to.Hex())).Scan(&b); err != nil || b == nil {
		e.t.Fatalf("checked_block: %v %v", b, err)
	}
	return uint64(*b)
}

// The zero a flush stores moves checked_block back to the balance it replaces by at most
// BaselineReach: a balance stored long before (a token idle for months) would otherwise send the
// sender's rescan, and the tick's rescan with it, back to that block on every tick while a reading
// is held, the cost the reach bounds.
func TestFlushMovesCheckedBlockBackWithinTheReach(t *testing.T) {
	t.Parallel()
	e := setup(t, func(c *detector.Config) { c.BaselineReach = 20 * 2 * time.Second }) // 20 blocks
	ctx := t.Context()
	s := e.create("50.00", "a")
	to := addr(t, s, "USDC")
	e.chain.Mine(transfer(usdc, payer, to, 25_000_000))
	e.chain.MineN(2)
	e.tick()
	old := e.storedBlock(to, usdc)
	e.chain.MineN(60)
	e.exec(`UPDATE chain_watch SET checked_block = $2 WHERE address = $1`, strings.ToLower(to.Hex()), int64(e.chain.Tip().Number))
	checked := e.checkedBlock(to)
	f := e.ourFlush(s, to, 25_000_000)
	if got := e.checkedBlock(to); got != checked-20 {
		t.Fatalf("checked_block %d after the flush, want %d (moved back 20 blocks, not to the balance's %d)", got, checked-20, old)
	}
	seen := map[*evmfake.Provider]int{e.pa: len(e.pa.LogRanges()), e.pb: len(e.pb.LogRanges())}
	if _, err := e.det.RescanSession(ctx, s.ID, f.Number); err != nil {
		t.Fatal(err)
	}
	for p, n := range seen {
		for _, r := range p.LogRanges()[n:] {
			if r[0] <= checked-20 {
				t.Fatalf("the sender's rescan went back to block %d", r[0])
			}
		}
	}
}

// A token whose balance never reads (its balanceOf answers no amount) keeps a new customer
// address's cursor unset only for ReconcileWait: then the cursor moves with a Warning, instead of
// every rescan of the address starting at its creation for good.
func TestUnreadableTokenLetsTheCursorMoveAfterTheWait(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	e.chain.CallHandler = func(m evmrpc.CallMsg, n uint64) ([]byte, error) {
		if m.To == usdt {
			return []byte{1, 2, 3}, nil
		}
		holder, _ := evm.Word(m.Data[4:36]).Address()
		w := evm.BigWord(e.chain.TokenBalance(m.To, holder, n))
		return w[:], nil
	}
	addrID, to := e.customerAddress("user_h1f")
	s := e.openOn(addrID, "", "a")
	e.chain.MineN(2)
	e.tick()
	cursor := func() *int64 {
		t.Helper()
		var b *int64
		err := e.app.Store.Pool().QueryRow(t.Context(), `SELECT block_number FROM customer_address_cursors WHERE customer_address_id = $1`,
			addrID).Scan(&b)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			t.Fatal(err)
		}
		return b
	}
	if c := cursor(); c != nil {
		t.Fatalf("cursor %d while USDT has no stored balance", *c)
	}
	e.app.Clock.Advance(11 * time.Minute)
	e.chain.Mine(transfer(usdc, payer, to, 10_000_000))
	e.chain.MineN(2)
	e.tick()
	if c := cursor(); c == nil || !e.alerts.Has("balance_unexplained") {
		t.Fatalf("after the wait: cursor %v, alerts %v", c, e.alerts.Kinds())
	}
	e.tick()
	if got := e.get(s.ID); len(got.Payments) != 1 {
		t.Fatalf("USDC deposit: %+v", got)
	}
}

// raised reports whether an alert of kind was raised with severity sev.
func (e *env) raised(kind string, sev alert.Severity) bool {
	for _, a := range e.alerts.All() {
		if a.Kind == kind && a.Severity == sev {
			return true
		}
	}
	return false
}

// tokenBalance is the balance of token the watch row keeps for the address.
func (e *env) tokenBalance(to, token evm.Address) string {
	e.t.Helper()
	var bal *string
	if err := e.app.Store.Pool().QueryRow(e.t.Context(), `SELECT token_balances ->> $2 FROM chain_watch WHERE address = $1`,
		strings.ToLower(to.Hex()), strings.ToLower(token.Hex())).Scan(&bal); err != nil {
		e.t.Fatal(err)
	}
	if bal == nil {
		return ""
	}
	return *bal
}

// The Critical escape after ReconcileWait stores the balance (the ticks stop rescanning) but
// leaves checked_block before the unexplained window: once the logs catch up, "Check again"
// rescans the deposit's block and credits it, once. Before, the escape moved checked_block past
// the deposit, "Check again" started after it, and the payment was never credited. The address
// watches one asset (a chain with USDC only): another asset without a stored balance would pull
// "Check again" back to the address's first block and hide the gap.
func TestCheckAgainCreditsADepositStoredUnexplained(t *testing.T) {
	t.Parallel()
	e := setup(t, func(c *detector.Config) { c.ReconcileWait = 10 * time.Minute })
	s := e.create("25.00", "a")
	to := addr(t, s, "USDC")
	e.exec(`DELETE FROM session_pay_to WHERE session_id = $1 AND asset = 'USDT'`, s.ID)
	e.tick()
	e.pa.SetLogLag(1_000)
	e.pb.SetLogLag(1_000)
	e.chain.Mine(transfer(usdc, payer, to, 25_000_000))
	e.chain.MineN(2)
	e.tick()
	e.app.Clock.Advance(11 * time.Minute)
	e.chain.Mine()
	e.tick()
	if bal := e.storedBalance(to); bal != "25000000" || !e.raised("balance_unexplained", alert.Critical) {
		t.Fatalf("after the wait: stored %q, alerts %v", bal, e.alerts.All())
	}
	e.pa.SetLogLag(0)
	e.pb.SetLogLag(0)
	e.chain.Mine()
	e.tick()
	e.tick()
	if got := e.get(s.ID); len(got.Payments) != 0 {
		t.Fatalf("a tick rescanned a stored balance: %+v", got)
	}
	recheck := func() {
		t.Helper()
		e.exec(`UPDATE sessions SET recheck_requested_at = $2 WHERE id = $1`, s.ID, e.app.Clock.Now())
		e.late()
		e.drain()
		if e.recheckRequested(s.ID) {
			t.Fatal("recheck request not cleared")
		}
	}
	recheck()
	if got := e.get(s.ID); got.Status != "paid" || len(got.Payments) != 1 || got.Received != "25.00" {
		t.Fatalf("after check again: %+v", got)
	}
	e.app.Clock.Advance(time.Minute)
	recheck()
	if got := e.get(s.ID); len(got.Payments) != 1 || got.Received != "25.00" {
		t.Fatalf("a second check again: %+v", got)
	}
}

// A late pass that holds an address (its deposit's log lags) wrote no next_check_at and no
// "Check again" for it, so the address stays due: the pass fails once its other writes are done
// and the pacer retries it at the warm interval, not on every step. Before, it returned nil and
// the pacer ran it again every ModeCheck (a rescan through both providers each time) for as long
// as the logs lagged, up to ReconcileWait.
func TestHeldLateCheckIsRetriedAtTheWarmInterval(t *testing.T) {
	t.Parallel()
	e := setup(t, func(c *detector.Config) { c.MaxRange = 500 })
	ctx := t.Context()
	s := e.create("25.00", "a")
	to := addr(t, s, "USDC")
	e.chain.MineN(2)
	e.tick() // registers the address while the session is open
	e.app.Clock.Advance(time.Hour)
	e.expire()
	row := func() string { // the address's check state: checked_block, next_check_at
		t.Helper()
		var b *int64
		var next *time.Time
		if err := e.app.Store.Pool().QueryRow(ctx, `SELECT checked_block, next_check_at FROM chain_watch WHERE address = $1`,
			strings.ToLower(to.Hex())).Scan(&b, &next); err != nil {
			t.Fatal(err)
		}
		return fmt.Sprint(b, next)
	}
	before := row()
	e.pa.SetLogLag(3)
	e.pb.SetLogLag(3)
	e.chain.Mine(transfer(usdc, payer, to, 25_000_000))
	e.chain.MineN(2)
	if _, err := e.det.Late(ctx); err == nil || !strings.Contains(err.Error(), "held") {
		t.Fatalf("late check holding an address: err %v", err)
	}
	if bal, got := e.storedBalance(to), e.get(s.ID); bal == "25000000" || row() != before || len(got.Payments) != 0 {
		t.Fatalf("held address committed: stored %q, check state %s (was %s), %+v", bal, row(), before, got)
	}

	p := scan.NewPacer(scan.Config{Chain: "anvil", Intervals: fastHot, Now: e.app.Clock.Now},
		scan.FromStore(e.app.Store, "anvil", chain.EVM, 30*time.Second, e.cfgSchedule(), scan.DefaultWatchAfterExpiry), e.det)
	step := func() error {
		_, err := p.Step(ctx)
		if errors.Is(err, worker.ErrSkipped) {
			return nil
		}
		return err
	}
	logs := e.method("eth_getLogs")
	if err := step(); err == nil || e.method("eth_getLogs") == logs {
		t.Fatalf("the pacer's late pass: err %v, %d getLogs", err, e.method("eth_getLogs")-logs)
	}
	logs = e.method("eth_getLogs")
	for range 30 {
		e.app.Clock.Advance(time.Second)
		_ = step() // an idle heartbeat may run; no late pass
	}
	if n := e.method("eth_getLogs") - logs; n != 0 {
		t.Fatalf("a held late check ran again within the warm interval: %d getLogs", n)
	}
	e.pa.SetLogLag(0)
	e.pb.SetLogLag(0)
	e.app.Clock.Advance(fastHot.Warm)
	if err := step(); err != nil {
		t.Fatalf("late check once the logs caught up: %v", err)
	}
	e.drain()
	if got := e.get(s.ID); got.Status != "late_payment" || len(got.Payments) != 1 || got.Received != "25.00" {
		t.Fatalf("late deposit once the logs caught up: %+v", got)
	}
}

// A customer address's token without a known baseline (its stored balance older than
// BaselineReach) is explained by the window only. Someone else's deployAndFlush sweeps a first
// deposit, and a second deposit's log lags: the zero after the flush is a known baseline, so the
// reading must be what came in after it. Before, the flush alone "explained" any non-zero
// balance: it was stored, the cursor passed the deposit, and the deposit was never credited.
func TestUnknownBaselineNeedsTheDepositsAfterTheLastFlush(t *testing.T) {
	t.Parallel()
	e := setup(t, func(c *detector.Config) { c.BaselineReach = 20 * 2 * time.Second }) // 20 blocks
	addrID, to := e.customerAddress("user_h1g")
	s1 := e.openOn(addrID, "25.00", "a")
	e.chain.MineN(2)
	e.drain()
	e.chain.MineN(40) // USDT's stored zero falls out of the reach
	e.chain.Mine(transfer(usdc, payer, to, 25_000_000))
	e.chain.MineN(2)
	e.drain()
	if got := e.get(s1.ID); got.Status != "paid" {
		t.Fatalf("first session: %+v", got)
	}
	s2 := e.openOn(addrID, "", "b")
	e.chain.Mine(transfer(usdt, payer, to, 5_000_000))
	b := e.chain.Mine(flush(usdt, to, 5_000_000)...) // someone else's deployAndFlush
	e.chain.Deploy(to, b.Number)
	e.chain.Mine(transfer(usdt, payer, to, 7_000_000))
	e.pa.SetLogLag(1) // the last deposit's log is not served yet
	e.pb.SetLogLag(1)
	e.tick()
	if bal := e.tokenBalance(to, usdt); bal == "7000000" {
		t.Fatal("a reading the deposits after the last flush do not explain was stored")
	}
	e.pa.SetLogLag(0)
	e.pb.SetLogLag(0)
	e.chain.MineN(2)
	e.tick()
	e.tick()
	if got := e.get(s2.ID); len(got.Payments) != 2 || got.Received != "12.00" {
		t.Fatalf("deposits around a third-party flush: %+v", got)
	}
	if bal := e.tokenBalance(to, usdt); bal != "7000000" || e.alerts.Has("balance_unexplained") {
		t.Fatalf("stored %q, alerts %v", bal, e.alerts.Kinds())
	}
}
