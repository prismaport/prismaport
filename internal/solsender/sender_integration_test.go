//go:build integration

package solsender_test

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/alert"
	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/chainwatch"
	"github.com/prismaport/prismaport/internal/derive"
	"github.com/prismaport/prismaport/internal/fee"
	"github.com/prismaport/prismaport/internal/payout"
	"github.com/prismaport/prismaport/internal/service"
	"github.com/prismaport/prismaport/internal/solana"
	"github.com/prismaport/prismaport/internal/soldetector"
	"github.com/prismaport/prismaport/internal/solfake"
	"github.com/prismaport/prismaport/internal/solrpc"
	"github.com/prismaport/prismaport/internal/solsender"
	"github.com/prismaport/prismaport/internal/solwatch"
	"github.com/prismaport/prismaport/internal/store"
)

const chainID = "solana-local"

var (
	program = solfake.Key("program")
	usdc    = solfake.Key("usdc")
	usdt    = solfake.Key("usdt")
	rent    = solfake.Key("rent")
	payer   = solfake.Key("payer")
)

type env struct {
	t      *testing.T
	app    *apptest.App
	cl     *solfake.Cluster
	pa, pb *solfake.Provider
	det    *soldetector.Detector
	snd    *solsender.Sender
	alerts *alert.Recorder
}

func merchant() solana.PublicKey { return solana.MustPublicKey(apptest.SolanaDestination) }
func treasury() solana.PublicKey { return solana.MustPublicKey(apptest.SolanaTreasury) }

func ata(t *testing.T, owner solana.PublicKey) solana.PublicKey {
	t.Helper()
	return ataOf(t, owner, usdc)
}

func ataOf(t *testing.T, owner, mint solana.PublicKey) solana.PublicKey {
	t.Helper()
	a, err := solana.AssociatedTokenAddress(owner, mint)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// setup wires the detector and the flusher to a fake cluster on which the merchant's and the
// treasury's token accounts exist, unless a test closes them. The chain offers USDC only.
func setup(t *testing.T, mutate func(*solsender.Config)) *env {
	t.Helper()
	return setupMints(t, mutate, usdc)
}

// setupMints is setup on a chain offering the given mints (usdc as USDC, usdt as USDT).
func setupMints(t *testing.T, mutate func(*solsender.Config), mints ...solana.PublicKey) *env {
	t.Helper()
	var assets []chain.Asset
	for _, m := range mints {
		assets = append(assets, chain.Asset{Symbol: map[solana.PublicKey]string{usdc: "USDC", usdt: "USDT"}[m], Token: m.String(), Decimals: 6})
	}
	reg, err := chain.NewRegistry([]chain.Chain{{ID: chainID, Family: chain.Solana, Fee: chain.PilotFee(), Assets: assets}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := reg.Get(chainID)
	e := &env{t: t, cl: solfake.New(), alerts: &alert.Recorder{}}
	e.cl.MineN(5)
	for _, m := range mints {
		e.cl.SetTokenAccount(ataOf(t, merchant(), m), m, merchant(), 0)
		e.cl.SetTokenAccount(ataOf(t, treasury(), m), m, treasury(), 0)
	}
	e.pa, e.pb = e.cl.Provider("a"), e.cl.Provider("b")
	q := solwatch.NewQuorum(chainID, []solwatch.Provider{e.pa, e.pb}, chainwatch.Options{})
	e.app = apptest.New(t, apptest.Options{Registry: reg,
		Deriver: derive.PerChain{Chains: map[string]derive.AddressDeriver{chainID: derive.SolanaForwarders{Program: program}}},
		Solana:  map[string]service.SolanaChain{chainID: {Program: program.String(), RentReceiver: rent.String()}}})
	if e.det, err = soldetector.New(soldetector.Config{Chain: c, Credit: solrpc.Confirmed}, q, e.app.Svc, e.app.Store, e.alerts, nil); err != nil {
		t.Fatal(err)
	}
	cfg := solsender.Config{Chain: c, PriorityFee: 1_000, BaseBackoff: time.Second, AlertAfter: 2, MaxAttempts: 3, Recorder: e.det}
	if mutate != nil {
		mutate(&cfg)
	}
	gas, err := solana.KeypairFromSeed(bytes.Repeat([]byte{3}, 32))
	if err != nil {
		t.Fatal(err)
	}
	e.snd = solsender.New(cfg, q, e.app.Store, gas, e.alerts, nil)
	return e
}

// paid opens a 25.00 session, pays its Solana deposit address on the fake cluster and lets the
// detector credit it. It returns the session id, the forwarder and its token account.
func (e *env) paid() (string, solana.PublicKey, solana.PublicKey) {
	e.t.Helper()
	r := e.app.RevealedSession(e.app.TestKey, `{"amount":"25.00"}`, "Idempotency-Key", "k")
	var s struct {
		ID    string `json:"id"`
		PayTo []struct {
			Chain, Address string
		} `json:"pay_to"`
	}
	_ = json.Unmarshal(r.Body, &s)
	var pda solana.PublicKey
	for _, p := range s.PayTo {
		if p.Chain == chainID {
			pda = solana.MustPublicKey(p.Address)
		}
	}
	if pda.IsZero() {
		e.t.Fatalf("no %s pay_to: %s", chainID, r.Body)
	}
	return s.ID, pda, e.deposit(pda, "deposit")
}

// deposit pays 25.00 into the forwarder's empty token account on the fake cluster and lets the
// detector see it. It returns the token account.
func (e *env) deposit(pda solana.PublicKey, sig string) solana.PublicKey {
	e.t.Helper()
	return e.depositMint(pda, usdc, sig)
}

// depositMint pays 25.00 of mint into the forwarder's token account of that mint.
func (e *env) depositMint(pda, mint solana.PublicKey, sig string) solana.PublicKey {
	e.t.Helper()
	src, dst := ataOf(e.t, payer, mint), ataOf(e.t, pda, mint)
	e.cl.Mine(solfake.Tx(solfake.Signature(sig), payer, []solrpc.Instruction{solfake.Transfer(src, dst, payer, 25_000_000)}, nil,
		[]solfake.TokenBalance{{Account: src, Mint: mint, Owner: payer, Amount: 25_000_000}},
		[]solfake.TokenBalance{{Account: src, Mint: mint, Owner: payer, Amount: 0}, {Account: dst, Mint: mint, Owner: pda, Amount: 25_000_000}}))
	e.cl.SetTokenAccount(dst, mint, pda, 25_000_000)
	if _, err := e.det.Step(e.t.Context()); err != nil {
		e.t.Fatal(err)
	}
	return dst
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

func (e *env) settlements(id string) []store.Settlement {
	e.t.Helper()
	ses, err := e.app.Store.LoadSession(e.t.Context(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return ses.Settlements
}

type move struct {
	to     solana.PublicKey
	amount uint64
}

// land executes the flusher's last broadcast transaction: the flush instruction moves the given
// amounts out of the deposit account, signed by the forwarder, the program prints logs, and the
// slot becomes final. The flusher follows it and records the settlement.
func (e *env) land(pda, source solana.PublicKey, moves []move, logs []string) {
	e.t.Helper()
	sent := e.cl.Sent()
	if len(sent) == 0 {
		e.t.Fatal("nothing was broadcast")
	}
	raw := sent[len(sent)-1]
	probe, err := solfake.FromRaw(raw, nil, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	i := solfake.InstructionIndex(probe, program, solana.IxFlush.Discriminator)
	if i < 0 {
		e.t.Fatal("the broadcast transaction has no flush instruction")
	}
	var inner []solrpc.Instruction
	for _, m := range moves {
		inner = append(inner, solfake.Transfer(source, m.to, pda, m.amount))
	}
	tx, err := solfake.FromRaw(raw, map[int][]solrpc.Instruction{i: inner}, logs)
	if err != nil {
		e.t.Fatal(err)
	}
	slot := e.cl.Mine(tx)
	e.cl.CloseAccount(source)
	e.step()
	e.cl.Finalize(slot)
	e.app.Clock.Advance(solsender.DefaultFinalityDelay) // the finality check is scheduled (D49)
	e.step()
}

// split is the pilot rule on 25.00: 0.225 to the treasury, 24.775 to the merchant.
func split(t *testing.T) []move {
	t.Helper()
	return []move{{ata(t, treasury()), 225_000}, {ata(t, merchant()), 24_775_000}}
}

func events(t *testing.T, pda, source solana.PublicKey, gross, feeAmt, net uint64, payouts ...move) []string {
	logs := []string{solfake.SettledLog(solana.SettledEvent{Forwarder: pda, Mint: usdc, Source: source, Gross: gross, Fee: feeAmt, Net: net, Treasury: treasury()})}
	for _, p := range payouts {
		logs = append(logs, solfake.PayoutLog(solana.PayoutEvent{Forwarder: pda, Mint: usdc, Recipient: p.to, Amount: p.amount}))
	}
	return logs
}

func (e *env) critical(kind string) []alert.Alert {
	var out []alert.Alert
	for _, a := range e.alerts.All() {
		if a.Kind == kind && a.Severity == alert.Critical {
			out = append(out, a)
		}
	}
	return out
}

func TestFlushSettlesAndRecordsTheSplit(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	id, pda, source := e.paid()
	e.step()
	j := e.job(id)
	if j.Status != store.JobSubmitted || j.Details["owner"] != pda.String() || j.Address != source.String() || j.GasLimit != 62_000 {
		t.Fatalf("job %+v", j)
	}
	if n := len(e.cl.Sent()); n != 2 {
		t.Fatalf("broadcast to %d providers, want 2", n)
	}

	m := split(t)
	e.land(pda, source, m, events(t, pda, source, 25_000_000, 225_000, 24_775_000, m[1]))

	j = e.job(id)
	if j.Status != store.JobDone || j.Outcome != "settled" || j.MinedBlockHash == "" {
		t.Fatalf("job %+v alerts %v", j, e.alerts.Kinds())
	}
	st := e.settlements(id)
	if len(st) != 1 || st[0].Gross.String() != "25000000" || st[0].Fee.String() != "225000" || st[0].Net.String() != "24775000" ||
		st[0].TxHash != j.MinedTxHash || len(st[0].Payouts) != 1 || st[0].Payouts[0].Address != apptest.SolanaDestination ||
		st[0].Payouts[0].Amount.String() != "24775000" {
		t.Fatalf("settlement %+v", st)
	}
	if len(e.alerts.All()) != 0 {
		t.Fatalf("alerts on a clean flush: %v", e.alerts.Kinds())
	}
}

func TestFlushNotFollowingTheSplitIsNotRecorded(t *testing.T) {
	t.Parallel()
	stranger := solfake.Key("stranger")
	for _, tc := range []struct {
		name  string
		moves func(t *testing.T) []move
	}{
		{"foreign payout", func(t *testing.T) []move {
			return []move{{ata(t, treasury()), 225_000}, {ata(t, merchant()), 24_000_000}, {ata(t, stranger), 775_000}}
		}},
		{"wrong shares", func(t *testing.T) []move {
			return []move{{ata(t, treasury()), 500_000}, {ata(t, merchant()), 24_500_000}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t, nil)
			id, pda, source := e.paid()
			e.step()
			e.land(pda, source, tc.moves(t), nil)

			if st := e.settlements(id); len(st) != 0 {
				t.Fatalf("a flush that breaks the split was recorded: %+v", st)
			}
			if got := e.critical("settlement_mismatch"); len(got) != 1 || got[0].Fields["session"] != id {
				t.Fatalf("critical alerts %v", e.alerts.Kinds())
			}
			if j := e.job(id); j.Status != store.JobDone || j.Outcome != "no_settlement" {
				t.Fatalf("job %+v", j)
			}
		})
	}
}

func TestEventsDifferentFromTheTransfersAlert(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		logs func(t *testing.T, pda, source solana.PublicKey) []string
		want string
	}{
		{"settled amounts", func(t *testing.T, pda, source solana.PublicKey) []string {
			return events(t, pda, source, 26_000_000, 260_000, 25_740_000, split(t)[1])
		}, "Settled event differs from the token movements"},
		{"missing payout event", func(t *testing.T, pda, source solana.PublicKey) []string {
			return events(t, pda, source, 25_000_000, 225_000, 24_775_000)
		}, "Payout events differ from the token movements"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t, nil)
			id, pda, source := e.paid()
			e.step()
			e.land(pda, source, split(t), tc.logs(t, pda, source))

			got := e.critical("settled_event_mismatch")
			if len(got) != 1 || got[0].Message != tc.want || got[0].Fields["session"] != id {
				t.Fatalf("alerts %+v", e.alerts.All())
			}
			// Events are only a cross-check: the token movements are what is recorded.
			if st := e.settlements(id); len(st) != 1 || st[0].Net.String() != "24775000" {
				t.Fatalf("settlement %+v", st)
			}
			if j := e.job(id); j.Outcome != "settled" {
				t.Fatalf("job %+v", j)
			}
		})
	}
}

func TestFlushBlockedUntilRecipientTokenAccountExists(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	e.cl.CloseAccount(ata(t, merchant()))
	id, _, _ := e.paid()

	e.step()
	j := e.job(id)
	if j.Status != store.JobBlocked || !strings.Contains(j.LastError, ata(t, merchant()).String()) || len(e.cl.Sent()) != 0 {
		t.Fatalf("job %+v sent %d", j, len(e.cl.Sent()))
	}
	all := e.alerts.All()
	if len(all) != 1 || all[0].Kind != "recipient_ata_missing" || all[0].Severity != alert.Warning || all[0].Fields["session"] != id {
		t.Fatalf("alerts %+v", all)
	}

	e.step()
	e.app.Clock.Advance(10 * time.Minute)
	e.step()
	if j := e.job(id); j.Status != store.JobBlocked || len(e.cl.Sent()) != 0 || j.Attempts != 0 {
		t.Fatalf("still missing: job %+v sent %d", j, len(e.cl.Sent()))
	}
	if n := len(e.alerts.All()); n != 1 {
		t.Fatalf("blocked alert repeated before a day passed: %v", e.alerts.Kinds())
	}

	e.cl.SetTokenAccount(ata(t, merchant()), usdc, merchant(), 0)
	e.step()
	if j := e.job(id); j.Status != store.JobBlocked {
		t.Fatalf("rechecked before the interval: %+v", j)
	}
	e.app.Clock.Advance(10 * time.Minute)
	e.step()
	if j := e.job(id); j.Status != store.JobSubmitted || j.LastError != "" || len(e.cl.Sent()) == 0 {
		t.Fatalf("not resumed once the account exists: %+v", j)
	}
}

func TestDustBalanceStaysInTheForwarder(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	id, pda, source := e.paid()
	e.cl.SetTokenAccount(source, usdc, pda, 90_000)

	e.step()
	if j := e.job(id); j.Status != store.JobDone || j.Outcome != "dust" || len(e.cl.Sent()) != 0 {
		t.Fatalf("job %+v sent %d", j, len(e.cl.Sent()))
	}
	if !e.alerts.Has("flush_dust") || len(e.settlements(id)) != 0 {
		t.Fatalf("alerts %v", e.alerts.Kinds())
	}
}

// A flush job whose stored mint or account cannot be read fails as bad_job with a critical alert,
// and nothing is sent for it.
func TestMalformedJobFailsWithACriticalAlert(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ name, set string }{
		{"mint", `details = jsonb_set(details, '{mint}', '"not-a-key"')`},
		{"address", `address = 'not-a-key'`},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := setup(t, nil)
			id, _, _ := e.paid()
			ctx := t.Context()
			if _, err := e.snd.Plan(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := e.app.Store.Pool().Exec(ctx, `UPDATE chain_tx_jobs SET `+c.set+` WHERE id = $1`, e.job(id).ID); err != nil {
				t.Fatal(err)
			}
			if _, err := e.snd.Step(ctx); err != nil {
				t.Fatal(err)
			}
			if j := e.job(id); j.Status != store.JobFailed || j.Outcome != "bad_job" || len(e.cl.Sent()) != 0 {
				t.Fatalf("job %+v sent %d", j, len(e.cl.Sent()))
			}
			if len(e.critical("flush_bad_job")) != 1 {
				t.Fatalf("alerts %v", e.alerts.Kinds())
			}
		})
	}
}

func TestFailingSimulationRetriesWithBackoffThenGivesUp(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	e.cl.SimulateHandler = func([]byte) solrpc.Simulation {
		return solrpc.Simulation{Err: json.RawMessage(`{"InstructionError":[3,{"Custom":6001}]}`), Logs: []string{"paused"}}
	}
	id, _, _ := e.paid()

	e.step()
	j := e.job(id)
	if j.Status != store.JobPending || j.Attempts != 1 || !j.NextAttemptAt.After(e.app.Clock.Now()) || !strings.Contains(j.LastError, "simulation failed") {
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
	if len(e.critical("flush_failing")) == 0 || len(e.critical("flush_gave_up")) != 1 {
		t.Fatalf("alerts %v", e.alerts.Kinds())
	}
	if len(e.cl.Sent()) != 0 {
		t.Fatal("a failing flush was broadcast")
	}
}

// forwarder rebuilds the forwarder a flush job targets from its details.
func forwarder(t *testing.T, j store.TxJob) solana.Forwarder {
	t.Helper()
	salt, err := hex.DecodeString(j.Details["salt"])
	if err != nil || len(salt) != 32 {
		t.Fatalf("salt %q", j.Details["salt"])
	}
	bps, _ := strconv.ParseUint(j.Details["fee_bps"], 10, 16)
	fixedFee, _ := strconv.ParseUint(j.Details["fixed_fee_micros"], 10, 64)
	var rs []payout.Recipient
	for _, part := range strings.Split(j.Details["recipients"], ",") {
		addr, share, _ := strings.Cut(part, ":")
		n, _ := strconv.ParseUint(share, 10, 16)
		rs = append(rs, payout.Recipient{Address: addr, Bps: uint16(n)})
	}
	p, err := derive.SolanaParams(derive.Params{Treasury: j.Details["treasury"], RentReceiver: j.Details["rent_receiver"],
		Fee: fee.Params{FeeBps: uint16(bps), FixedFeeMicros: fixedFee}, Recipients: rs, Tokens: strings.Split(j.Details["mints"], ",")})
	if err != nil {
		t.Fatal(err)
	}
	f, err := solana.NewForwarder(program, [32]byte(salt), p)
	if err != nil || f.Address.String() != j.Details["owner"] {
		t.Fatalf("forwarder %v %v", f.Address, err)
	}
	return f
}

func TestEmptyAccountSettledByAnotherCaller(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	id, pda, source := e.paid()
	if _, err := e.snd.Plan(t.Context()); err != nil {
		t.Fatal(err)
	}
	f := forwarder(t, e.job(id))
	stranger := solfake.Key("stranger")
	ix, err := f.Flush(stranger, usdc, source, false)
	if err != nil {
		t.Fatal(err)
	}
	var inner []solrpc.Instruction
	for _, m := range split(t) {
		inner = append(inner, solfake.Transfer(source, m.to, pda, m.amount))
	}
	tx := solfake.Tx(solfake.Signature("third-party flush"), stranger, []solrpc.Instruction{solfake.Raw(ix)},
		map[int][]solrpc.Instruction{0: inner}, nil, nil)
	e.cl.Finalize(e.cl.Mine(tx))
	e.cl.CloseAccount(source)

	e.step()
	j := e.job(id)
	if j.Status != store.JobDone || j.Outcome != "settled_elsewhere" || len(e.cl.Sent()) != 0 {
		t.Fatalf("job %+v", j)
	}
	if st := e.settlements(id); len(st) != 1 || st[0].TxHash != tx.Signatures[0] || st[0].Net.String() != "24775000" {
		t.Fatalf("settlement %+v", st)
	}
}

func TestEmptyAccountWithoutAFlushFinishes(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	id, _, source := e.paid()
	e.cl.CloseAccount(source)

	e.step()
	if j := e.job(id); j.Status != store.JobDone || j.Outcome != "empty" || len(e.cl.Sent()) != 0 {
		t.Fatalf("job %+v", j)
	}
	if len(e.settlements(id)) != 0 {
		t.Fatal("a settlement was recorded for an account nobody flushed")
	}
}

// lastSent is the jsonParsed view of the flusher's last broadcast transaction.
func (e *env) lastSent() *solrpc.Tx {
	e.t.Helper()
	sent := e.cl.Sent()
	if len(sent) == 0 {
		e.t.Fatal("nothing was broadcast")
	}
	tx, err := solfake.FromRaw(sent[len(sent)-1], nil, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	return tx
}

// jobs lists a session's flush jobs in the order they were planned (by planned total).
func (e *env) jobs(id string) []store.TxJob {
	e.t.Helper()
	js, err := e.app.Store.SessionTxJobs(e.t.Context(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	slices.SortFunc(js, func(a, b store.TxJob) int { return a.PlannedTotal.Cmp(*b.PlannedTotal) })
	return js
}

// programIxs lists the instructions of tx addressed to the forwarder program.
func programIxs(tx *solrpc.Tx) []solrpc.Instruction {
	var out []solrpc.Instruction
	for _, ix := range tx.Instructions {
		if ix.ProgramID == program.String() {
			out = append(out, ix)
		}
	}
	return out
}

// TestFlushIsASingleFlushInstruction: the program is stateless (D41), so the transaction holds
// one flush of the deposit token account (it closes the account itself) and nothing else of the
// program: no initialize and no close_forwarder.
func TestFlushIsASingleFlushInstruction(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	id, _, source := e.paid()
	e.step()
	j := e.job(id)
	if j.Status != store.JobSubmitted || j.Details["mints"] != usdc.String() {
		t.Fatalf("job %+v", j)
	}
	ixs := programIxs(e.lastSent())
	if len(ixs) != 1 || solfake.InstructionIndex(e.lastSent(), program, solana.IxFlush.Discriminator) < 0 ||
		len(ixs[0].Accounts) < 4 || ixs[0].Accounts[3] != source.String() {
		t.Fatalf("program instructions %+v", ixs)
	}
}

// TestTwoMintSessionFlushesEachTokenAccountInItsOwnTransaction: a session offering USDC and USDT
// on Solana binds both mints in one deposit owner. Both entries re-derive with the stored list
// and each token account is flushed by its own transaction (two flushes do not fit in one).
func TestTwoMintSessionFlushesEachTokenAccountInItsOwnTransaction(t *testing.T) {
	t.Parallel()
	e := setupMints(t, nil, usdc, usdt)
	id, pda, usdcSource := e.paid()
	usdtSource := e.depositMint(pda, usdt, "usdt deposit")

	if n, err := e.snd.Plan(t.Context()); err != nil || n != 2 {
		t.Fatalf("planned %d: %v (alerts %v)", n, err, e.alerts.Kinds())
	}
	if got := e.critical("flush_address_mismatch"); len(got) != 0 {
		t.Fatalf("mismatch alerts %+v", got)
	}
	want, err := derive.SortTokens(chain.Solana, []string{usdc.String(), usdt.String()})
	if err != nil {
		t.Fatal(err)
	}
	js := e.jobs(id)
	if len(js) != 2 {
		t.Fatalf("jobs %+v", js)
	}
	sources := map[string]string{}
	for _, j := range js {
		if j.Details["mints"] != strings.Join(want, ",") || j.Details["owner"] != pda.String() {
			t.Fatalf("job details %v", j.Details)
		}
		sources[j.Details["mint"]] = j.Address
	}
	if sources[usdc.String()] != usdcSource.String() || sources[usdt.String()] != usdtSource.String() {
		t.Fatalf("token accounts per mint %v", sources)
	}

	flushed := map[string]string{}
	for range 2 {
		if _, err := e.snd.Step(t.Context()); err != nil {
			t.Fatal(err)
		}
		tx := e.lastSent()
		ixs := programIxs(tx)
		if len(ixs) != 1 || solfake.InstructionIndex(tx, program, solana.IxFlush.Discriminator) < 0 || len(ixs[0].Accounts) < 4 {
			t.Fatalf("program instructions %+v", ixs)
		}
		flushed[ixs[0].Accounts[2]] = ixs[0].Accounts[3]
	}
	if len(flushed) != 2 || flushed[usdc.String()] != usdcSource.String() || flushed[usdt.String()] != usdtSource.String() {
		t.Fatalf("flushed (mint -> source) %v", flushed)
	}
	for _, j := range e.jobs(id) {
		if j.Status != store.JobSubmitted {
			t.Fatalf("job %+v", j)
		}
	}
}

// TestSecondDepositIsFlushedAgain: nothing is left to initialize or close between flushes, so a
// later deposit into the same owner is flushed like the first.
func TestSecondDepositIsFlushedAgain(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	id, pda, source := e.paid()
	e.step()
	m := split(t)
	logs := events(t, pda, source, 25_000_000, 225_000, 24_775_000, m[1])
	e.land(pda, source, m, logs)

	e.deposit(pda, "second deposit")
	e.step()
	js := e.jobs(id)
	if len(js) != 2 || js[1].Status != store.JobSubmitted || len(programIxs(e.lastSent())) != 1 {
		t.Fatalf("jobs %+v", js)
	}
	e.land(pda, source, m, logs)
	js = e.jobs(id)
	if js[0].Outcome != "settled" || js[1].Status != store.JobDone || js[1].Outcome != "settled" {
		t.Fatalf("jobs %+v", js)
	}
	if st := e.settlements(id); len(st) != 2 || st[1].TxHash != js[1].MinedTxHash || st[1].Gross.String() != "25000000" {
		t.Fatalf("settlements %+v", st)
	}
}

// A deposit right after our flush closed the deposit account, of the same amount as the one
// credited or less, is credited although the account never reads higher than before the flush:
// our flush landing stores a zero balance at its slot, so the next read is a rise. Before, the read
// equal to the stored balance was not scanned (or a lower one stored as a decrease), the first
// payment became final, the session left the watch as settled (D47) and the deposit was lost.
func TestDepositRightAfterOurFlushIsCredited(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		second uint64
	}{{"equal", 25_000_000}, {"smaller", 10_000_000}} {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t, nil)
			ctx := t.Context()
			id, pda, source := e.paid()
			e.step()
			m := split(t)
			e.mineFlush(pda, source, m, events(t, pda, source, 25_000_000, 225_000, 24_775_000, m[1]))
			e.step() // the flush landed (confirmed)
			if j := e.job(id); j.Status != store.JobMined {
				t.Fatalf("flush not landed: %+v", j)
			}
			src := ata(t, payer)
			e.cl.Mine(solfake.Tx(solfake.Signature("second deposit"), payer, []solrpc.Instruction{solfake.Transfer(src, source, payer, tc.second)}, nil,
				[]solfake.TokenBalance{{Account: src, Mint: usdc, Owner: payer, Amount: tc.second}},
				[]solfake.TokenBalance{{Account: src, Mint: usdc, Owner: payer, Amount: 0}, {Account: source, Mint: usdc, Owner: pda, Amount: tc.second}}))
			e.cl.SetTokenAccount(source, usdc, pda, tc.second)
			for range 4 {
				e.cl.MineN(1)
				e.cl.Finalize(e.cl.Tip())
				e.app.Clock.Advance(time.Minute)
				if _, err := e.det.Step(ctx); err != nil {
					t.Fatal(err)
				}
				e.step()
			}
			ses, err := e.app.Store.LoadSession(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if ses.State.Status != "overpaid" || len(ses.State.Payments) != 2 || ses.State.Payments[1].Amount.String() != strconv.FormatUint(tc.second, 10) {
				t.Fatalf("second deposit after our flush: status %s, payments %+v", ses.State.Status, ses.State.Payments)
			}
		})
	}
}

// A deposit that lands after the credited one and before our flush is sent leaves with the flush
// (it moves the account's whole balance) and is never seen by a balance read. At the flush's
// finality the agreed transaction shows it moved more than was credited when the job was planned,
// and the detector reads the account's history again: the deposit is credited and the session
// ends overpaid. Before, it was never recorded.
func TestFlushOfMoreThanCreditedRescansTheAccount(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	ctx := t.Context()
	r := e.app.RevealedSession(e.app.TestKey, `{"amount":"100.00"}`, "Idempotency-Key", "k")
	var s struct {
		ID    string                            `json:"id"`
		PayTo []struct{ Chain, Address string } `json:"pay_to"`
	}
	_ = json.Unmarshal(r.Body, &s)
	var pda solana.PublicKey
	for _, p := range s.PayTo {
		if p.Chain == chainID {
			pda = solana.MustPublicKey(p.Address)
		}
	}
	src, source := ata(t, payer), ata(t, pda)
	pay := func(sig string, amount, before uint64) {
		e.cl.Mine(solfake.Tx(solfake.Signature(sig), payer, []solrpc.Instruction{solfake.Transfer(src, source, payer, amount)}, nil,
			[]solfake.TokenBalance{{Account: src, Mint: usdc, Owner: payer, Amount: amount}, {Account: source, Mint: usdc, Owner: pda, Amount: before}},
			[]solfake.TokenBalance{{Account: src, Mint: usdc, Owner: payer, Amount: 0}, {Account: source, Mint: usdc, Owner: pda, Amount: before + amount}}))
		e.cl.SetTokenAccount(source, usdc, pda, before+amount)
	}
	pay("deposit", 100_000_000, 0)
	if _, err := e.det.Step(ctx); err != nil {
		t.Fatal(err)
	}
	pay("second deposit", 50_000_000, 100_000_000) // just before our flush is sent
	e.step()
	if j := e.job(s.ID); j.Status != store.JobSubmitted || j.PlannedTotal.String() != "100000000" {
		t.Fatalf("flush job %+v", j)
	}
	m := []move{{ata(t, treasury()), 850_000}, {ata(t, merchant()), 149_150_000}}
	e.land(pda, source, m, events(t, pda, source, 150_000_000, 850_000, 149_150_000, m[1]))
	for range 2 {
		e.cl.MineN(1)
		e.cl.Finalize(e.cl.Tip())
		e.app.Clock.Advance(time.Minute)
		if _, err := e.det.Step(ctx); err != nil {
			t.Fatal(err)
		}
	}
	ses, err := e.app.Store.LoadSession(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if j := e.job(s.ID); j.Outcome != "settled" || len(ses.Settlements) != 1 || ses.Settlements[0].Gross.String() != "150000000" {
		t.Fatalf("job %+v, settlements %+v", j, ses.Settlements)
	}
	if ses.State.Status != "overpaid" || len(ses.State.Payments) != 2 || ses.State.Payments[1].Amount.String() != "50000000" {
		t.Fatalf("deposit that left with our flush: status %s, payments %+v", ses.State.Status, ses.State.Payments)
	}
}
