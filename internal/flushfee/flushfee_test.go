package flushfee

import (
	"testing"

	"github.com/prismaport/prismaport/internal/alert"
	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/session"
	"github.com/prismaport/prismaport/internal/store"
)

func u(n uint64) money.Units { return money.FromUint64(n) }

// pilot is a session paying USDC on base with the pilot fee (0.5% + 0.10).
func pilot(settlements ...store.Settlement) *store.Session {
	return &store.Session{ID: "ses_1", MerchantID: "mer_1", Livemode: true,
		PayTo: []store.PayToRecord{{Chain: "base", Asset: "USDC", Decimals: 6, Fee: chain.PilotFee()}},
		State: session.State{Payments: []session.Payment{
			{Chain: "base", Asset: "USDC", TxHash: "0xD1", Amount: u(10_000_000)},
			{Chain: "base", Asset: "USDC", TxHash: "0xd2", Amount: u(15_000_000)},
		}},
		Settlements: settlements}
}

func flush(tx string, gross, fee uint64) store.Settlement {
	return store.Settlement{Chain: "base", Asset: "USDC", TxHash: tx, Gross: u(gross), Fee: u(fee)}
}

func TestOneFlushIsNoExcess(t *testing.T) {
	if _, ok, err := Reconcile(pilot(flush("0xf1", 25_000_000, 225_000)), "base", "USDC"); ok || err != nil {
		t.Fatalf("ok %v err %v", ok, err)
	}
}

// A third party flushed after each part payment: 10 then 15 USDC paid 0.15 + 0.175 in fees where
// one flush of 25 USDC pays 0.225, so the merchant paid one extra fixed fee (0.10).
func TestTwoFlushesPayOneExtraFixedFee(t *testing.T) {
	ses := pilot(flush("0xf1", 10_000_000, 150_000), flush("0xf2", 15_000_000, 175_000))
	ex, ok, err := Reconcile(ses, "base", "USDC")
	if err != nil || !ok {
		t.Fatalf("ok %v err %v", ok, err)
	}
	if ex.Flushes != 2 || ex.Gross.String() != "25000000" || ex.Charged.String() != "325000" || ex.SingleFlush.String() != "225000" ||
		ex.Extra.String() != "100000" || ex.FixedFee.String() != "100000" {
		t.Fatalf("excess %+v", ex)
	}
	d := ex.Details()
	if d["extra_fixed_fee"] != "100000" || d["flush_settlements"] != "2" {
		t.Fatalf("details %v", d)
	}
	a := ex.Alert(ses)
	if a.Kind != AlertKind || a.Severity != alert.Warning || a.Chain != "base" || a.Fields["merchant"] != "mer_1" ||
		a.Fields["session"] != "ses_1" || a.Fields["flushes"] != "2" || a.Fields["extra_fee"] != "0.10" || a.Fields["livemode"] != "true" {
		t.Fatalf("alert %+v", a)
	}
}

// Settlements made in a payment's own transaction (signed path, connected wallet; matched case-
// insensitively), simulated ones and other assets or chains are not flushes.
func TestOnlyFlushSettlementsCount(t *testing.T) {
	signed := flush("0xd1", 10_000_000, 150_000)
	sim := flush("0xf3", 1_000_000, 105_000)
	sim.Simulated = true
	usdt := flush("0xf4", 1_000_000, 105_000)
	usdt.Asset = "USDT"
	other := flush("0xf5", 1_000_000, 105_000)
	other.Chain = "solana"
	ses := pilot(signed, sim, usdt, other, flush("0xf2", 15_000_000, 175_000))
	if ex, ok, err := Reconcile(ses, "base", "USDC"); ok || err != nil {
		t.Fatalf("excess %+v ok %v err %v", ex, ok, err)
	}
	if _, ok, err := Reconcile(ses, "base", "DAI"); ok || err != nil { // no pay_to for it
		t.Fatalf("ok %v err %v", ok, err)
	}
}

// Without a fixed fee two flushes can charge no more than one (the percentage rounds down per
// flush, so splitting it never costs more).
func TestNoFixedFeeIsNoExcess(t *testing.T) {
	ses := pilot(flush("0xf1", 10_000_001, 50_000), flush("0xf2", 15_000_001, 75_000))
	ses.PayTo[0].Fee.FixedFeeMicros = 0
	if ex, ok, err := Reconcile(ses, "base", "USDC"); ok || err != nil {
		t.Fatalf("excess %+v ok %v err %v", ex, ok, err)
	}
}
