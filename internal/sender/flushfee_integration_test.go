//go:build integration

package sender_test

import (
	"testing"

	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/contracts"
	"github.com/prismaport/prismaport/internal/evm"
	"github.com/prismaport/prismaport/internal/evmfake"
	"github.com/prismaport/prismaport/internal/flushfee"
	"github.com/prismaport/prismaport/internal/store"
)

// strangerFlush mines a third party's flush of the forwarder: one Payout to the main wallet and
// the Settled event, in its own transaction.
func (e *env) strangerFlush(tx evm.Word, fwd evm.Address, gross, feeAmt uint64) {
	treasury, _ := evm.ParseAddress(apptest.EVMTreasury)
	tw := evm.AddressWord(treasury)
	e.chain.Mine(
		evmfake.LogSpec{TxHash: tx, Address: fwd, Topics: []evm.Word{contracts.PayoutTopic, evm.AddressWord(usdc), evm.AddressWord(mainWallet())},
			Data: word(gross - feeAmt)},
		evmfake.LogSpec{TxHash: tx, Address: fwd, Topics: []evm.Word{contracts.SettledTopic, evm.AddressWord(usdc)},
			Data: append(append(append(word(gross), word(feeAmt)...), word(gross-feeAmt)...), tw[:]...)})
}

// ADR 0008 I-1: a third party flushed the 25 USDC of a session in two parts (10 + 15), so the
// forwarder charged the fixed fee twice (0.15 + 0.175 where one flush pays 0.225). The job records
// both settlements, raises flush_fixed_fee_excess with the merchant, session and amounts, and
// keeps the extra on its details.
func TestStrangerFlushesInPartsAlertTheExtraFixedFee(t *testing.T) {
	t.Parallel()
	e := setup(t, nil, nil)
	ctx := t.Context()
	id, fwd := e.paid()
	e.strangerFlush(evm.Word{0xf1, 0x11}, fwd, 10_000_000, 150_000)
	e.strangerFlush(evm.Word{0xf1, 0x12}, fwd, 15_000_000, 175_000)
	e.bal[fwd] = 0
	e.chain.MineN(5)
	e.step()
	j := e.job(id)
	if j.Status != store.JobDone || j.Outcome != "settled_elsewhere" {
		t.Fatalf("job %+v", j)
	}
	if j.Details["extra_fixed_fee"] != "100000" || j.Details["flush_settlements"] != "2" || j.Details["single_flush_fee"] != "225000" {
		t.Fatalf("job details %v", j.Details)
	}
	var got bool
	for _, a := range e.alerts.All() {
		if a.Kind != flushfee.AlertKind {
			continue
		}
		got = true
		if a.Fields["session"] != id || a.Fields["merchant"] == "" || a.Fields["extra_fee"] != "0.10" || a.Fields["flushes"] != "2" ||
			a.Chain == "" {
			t.Fatalf("alert %+v", a)
		}
	}
	ses, err := e.app.Store.LoadSession(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !got || len(ses.Settlements) != 2 {
		t.Fatalf("alerts %v settlements %+v", e.alerts.Kinds(), ses.Settlements)
	}
}

// One flush of the whole balance is the expected case: no alert.
func TestSingleStrangerFlushRaisesNoFeeAlert(t *testing.T) {
	t.Parallel()
	e := setup(t, nil, nil)
	id, fwd := e.paid()
	e.strangerFlush(evm.Word{0xf1, 0x13}, fwd, 25_000_000, 225_000)
	e.bal[fwd] = 0
	e.chain.MineN(5)
	e.step()
	if j := e.job(id); j.Outcome != "settled_elsewhere" || j.Details["extra_fixed_fee"] != "" || e.alerts.Has(flushfee.AlertKind) {
		t.Fatalf("job %+v alerts %v", j, e.alerts.Kinds())
	}
}
