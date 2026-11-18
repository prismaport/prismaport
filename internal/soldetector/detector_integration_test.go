//go:build integration

package soldetector_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/alert"
	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/chainwatch"
	"github.com/prismaport/prismaport/internal/derive"
	"github.com/prismaport/prismaport/internal/service"
	"github.com/prismaport/prismaport/internal/solana"
	"github.com/prismaport/prismaport/internal/soldetector"
	"github.com/prismaport/prismaport/internal/solfake"
	"github.com/prismaport/prismaport/internal/solrpc"
	"github.com/prismaport/prismaport/internal/solwatch"
)

const chainID = "solana-local"

var (
	program = solfake.Key("program")
	usdc    = solfake.Key("usdc")
	rent    = solfake.Key("rent")
	payer   = solfake.Key("payer")
)

type env struct {
	t      *testing.T
	app    *apptest.App
	cl     *solfake.Cluster
	a, b   *solfake.Provider
	q      *solwatch.Quorum
	det    *soldetector.Detector
	alerts *alert.Recorder
	chain  chain.Chain
	// rentLamports is the rent receiver's balance the fake holds.
	rentLamports uint64
}

func setup(t *testing.T, credit solrpc.Commitment) *env {
	t.Helper()
	return setupHold(t, credit, 0)
}

// setupHold is setup with an expiry hold (service.Config.ExpiryHold, ADR-0008 H-3).
func setupHold(t *testing.T, credit solrpc.Commitment, hold time.Duration) *env {
	t.Helper()
	reg, err := chain.NewRegistry([]chain.Chain{{ID: chainID, Family: chain.Solana, Fee: chain.PilotFee(),
		Assets: []chain.Asset{{Symbol: "USDC", Token: usdc.String(), Decimals: 6}}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := reg.Get(chainID)
	e := &env{t: t, cl: solfake.New(), alerts: &alert.Recorder{}, chain: c}
	e.cl.MineN(5)
	e.a, e.b = e.cl.Provider("a"), e.cl.Provider("b")
	e.q = solwatch.NewQuorum(chainID, []solwatch.Provider{e.a, e.b}, chainwatch.Options{})
	e.app = apptest.New(t, apptest.Options{Registry: reg,
		Deriver: derive.PerChain{Chains: map[string]derive.AddressDeriver{chainID: derive.SolanaForwarders{Program: program}}},
		Solana:  map[string]service.SolanaChain{chainID: {Program: program.String(), RentReceiver: rent.String()}}, ExpiryHold: hold})
	if e.det, err = soldetector.New(soldetector.Config{Chain: c, Credit: credit}, e.q, e.app.Svc, e.app.Store, e.alerts, nil); err != nil {
		t.Fatal(err)
	}
	return e
}

type view struct {
	Status         string `json:"status"`
	AmountReceived string `json:"amount_received"`
	PayTo          []struct {
		Chain, Address string
	} `json:"pay_to"`
	Payments []struct {
		Status string
		TxHash string `json:"tx_hash"`
	} `json:"payments"`
}

func (e *env) get(id string) view {
	e.t.Helper()
	var v view
	if err := json.Unmarshal(e.app.Do("GET", "/v1/sessions/"+id, e.app.TestKey, nil).Body, &v); err != nil {
		e.t.Fatal(err)
	}
	return v
}

// open creates a 25.00 session and returns its id and Solana deposit address (the forwarder).
func (e *env) open() (string, solana.PublicKey) {
	e.t.Helper()
	return e.openKey("k")
}

// openKey is open with its own idempotency key (several sessions in one test).
func (e *env) openKey(key string) (string, solana.PublicKey) {
	e.t.Helper()
	r := e.app.RevealedSession(e.app.TestKey, `{"amount":"25.00"}`, "Idempotency-Key", key)
	var s struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(r.Body, &s)
	for _, p := range e.get(s.ID).PayTo {
		if p.Chain == chainID {
			return s.ID, solana.MustPublicKey(p.Address)
		}
	}
	e.t.Fatalf("no %s pay_to: %s", chainID, r.Body)
	return "", solana.PublicKey{}
}

func ata(t *testing.T, owner solana.PublicKey) solana.PublicKey {
	t.Helper()
	a, err := solana.AssociatedTokenAddress(owner, usdc)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// deposit is a payer's transfer of amount into the forwarder's token account.
func (e *env) deposit(seed string, pda solana.PublicKey, amount uint64) *solrpc.Tx {
	e.t.Helper()
	src, dst := ata(e.t, payer), ata(e.t, pda)
	return solfake.Tx(solfake.Signature(seed), payer, []solrpc.Instruction{solfake.Transfer(src, dst, payer, amount)}, nil,
		[]solfake.TokenBalance{{Account: src, Mint: usdc, Owner: payer, Amount: 1_000_000_000}},
		[]solfake.TokenBalance{{Account: src, Mint: usdc, Owner: payer, Amount: 1_000_000_000 - amount}, {Account: dst, Mint: usdc, Owner: pda, Amount: amount}})
}

func (e *env) step() error {
	_, err := e.det.Step(e.t.Context())
	return err
}

func (e *env) mustStep() {
	e.t.Helper()
	if err := e.step(); err != nil {
		e.t.Fatal(err)
	}
}

func TestCreditAtConfirmedThenFinalized(t *testing.T) {
	t.Parallel()
	e := setup(t, solrpc.Confirmed)
	id, pda := e.open()
	tx := e.deposit("pay", pda, 25_000_000)
	slot := e.cl.Mine(tx)

	e.mustStep()
	v := e.get(id)
	if v.Status != "paid" || len(v.Payments) != 1 || v.Payments[0].Status != "confirmed" || v.Payments[0].TxHash != tx.Signatures[0] {
		t.Fatalf("credit at confirmed: %+v", v)
	}

	e.cl.Finalize(slot)
	e.app.Clock.Advance(soldetector.DefaultFinalityDelay) // the finality check is scheduled (D49)
	e.mustStep()
	if v := e.get(id); v.Payments[0].Status != "finalized" {
		t.Fatalf("after finality: %+v", v)
	}
	e.mustStep()
	if v := e.get(id); len(v.Payments) != 1 {
		t.Fatalf("payment counted twice: %+v", v)
	}
}

func TestPaymentOffTheFinalizedForkIsReversed(t *testing.T) {
	t.Parallel()
	e := setup(t, solrpc.Confirmed)
	id, pda := e.open()
	slot := e.cl.Mine(e.deposit("pay", pda, 25_000_000))
	e.mustStep()
	if v := e.get(id); v.Status != "paid" {
		t.Fatalf("not credited at confirmed: %+v", v)
	}

	e.cl.Orphan(slot)
	e.cl.Finalize(slot)
	e.app.Clock.Advance(soldetector.DefaultFinalityDelay)
	e.mustStep()

	v := e.get(id)
	if len(v.Payments) != 1 || v.Payments[0].Status != "reversed" || v.Status == "paid" {
		t.Fatalf("payment on an orphaned slot not reversed: %+v", v)
	}
	if !e.alerts.Has("reorg") {
		t.Fatalf("alerts %v", e.alerts.Kinds())
	}
	e.mustStep()
	if v := e.get(id); len(v.Payments) != 1 || v.Payments[0].Status != "reversed" {
		t.Fatalf("orphaned payment credited again on the rescan: %+v", v)
	}
}

func TestProviderDisagreementHaltsCredit(t *testing.T) {
	t.Parallel()
	e := setup(t, solrpc.Confirmed)
	id, pda := e.open()
	e.b.Rewrite(func(tx *solrpc.Tx) *solrpc.Tx {
		for i := range tx.PostToken {
			tx.PostToken[i].Amount = strings.Replace(tx.PostToken[i].Amount, "25000000", "26000000", 1)
		}
		tx.Instructions[0].Info = strings.Replace(tx.Instructions[0].Info, "25000000", "26000000", 1)
		return tx
	})
	e.cl.Mine(e.deposit("pay", pda, 25_000_000))

	err := e.step()
	if !errors.Is(err, solwatch.ErrDisagreement) {
		t.Fatalf("step error %v, want a disagreement", err)
	}
	if v := e.get(id); len(v.Payments) != 0 || v.Status != "open" {
		t.Fatalf("credited while providers disagree: %+v", v)
	}
	if st := e.q.Status(); st.State != chainwatch.Halted {
		t.Fatalf("quorum status %+v", st)
	}

	e.b.Rewrite(nil)
	e.mustStep()
	if v := e.get(id); v.Status != "paid" || len(v.Payments) != 1 {
		t.Fatalf("not credited once the providers agree: %+v", v)
	}
	if st := e.q.Status(); st.State != chainwatch.OK {
		t.Fatalf("quorum status after recovery %+v", st)
	}
}

// A fresh detector (a restart) reads the finalized slot on its first scan, so the cursor of an
// account moves past a transaction that is already final instead of the account being listed from
// the start on every tick (ADR-0008 H-5).
func TestFirstScanReadsFinalizedSlot(t *testing.T) {
	t.Parallel()
	e := setup(t, solrpc.Confirmed)
	id, pda := e.open()
	tx := e.deposit("pay", pda, 25_000_000)
	e.cl.Finalize(e.cl.Mine(tx))

	e.mustStep()
	if v := e.get(id); v.Status != "paid" {
		t.Fatalf("not credited: %+v", v)
	}
	rows, err := e.app.Store.SolanaDepositRows(t.Context(), chainID, []string{ata(t, pda).String()})
	if err != nil || len(rows) != 1 {
		t.Fatalf("deposit rows %v %v", rows, err)
	}
	if rows[0].CursorSignature != tx.Signatures[0] {
		t.Fatalf("cursor %q, want the finalized deposit %q", rows[0].CursorSignature, tx.Signatures[0])
	}
}
