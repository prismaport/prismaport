package contracts_test

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/prismaport/prismaport/internal/contracts"
	"github.com/prismaport/prismaport/internal/evm"
)

func TestMulticall3Address(t *testing.T) {
	if got := contracts.Multicall3Address.Hex(); got != "0xcA11bde05977b3631167028862bE2a173976CA11" {
		t.Fatalf("address %s", got)
	}
}

// The calldata of aggregate3([(token, true, balanceOf(holder))]) as solc/ethers encode it.
func TestAggregate3Encoding(t *testing.T) {
	token, holder := evm.Address{0xaa}, evm.Address{0xbb}
	got := hex.EncodeToString(contracts.Aggregate3([]contracts.Call{{Target: token, AllowFailure: true, Data: contracts.BalanceOf(holder)}}))
	want := "82ad56cb" +
		"0000000000000000000000000000000000000000000000000000000000000020" + // offset of calls
		"0000000000000000000000000000000000000000000000000000000000000001" + // length
		"0000000000000000000000000000000000000000000000000000000000000020" + // offset of calls[0]
		"000000000000000000000000aa00000000000000000000000000000000000000" + // target
		"0000000000000000000000000000000000000000000000000000000000000001" + // allowFailure
		"0000000000000000000000000000000000000000000000000000000000000060" + // offset of callData
		"0000000000000000000000000000000000000000000000000000000000000024" + // 36 bytes
		"70a08231000000000000000000000000bb000000000000000000000000000000" +
		"0000000000000000000000000000000000000000000000000000000000000000"
	if got != want {
		t.Fatalf("calldata\n got %s\nwant %s", got, want)
	}
	if got := hex.EncodeToString(contracts.GetBlockNumber()); got != "42cbb15c" {
		t.Fatalf("getBlockNumber selector %s", got)
	}
}

// aggregate3's return value: ((true, uint256 25), (false, "")) as solc encodes it.
func TestAggregate3ResultDecoding(t *testing.T) {
	raw, _ := hex.DecodeString("" +
		"0000000000000000000000000000000000000000000000000000000000000020" + // offset of the array
		"0000000000000000000000000000000000000000000000000000000000000002" + // length
		"0000000000000000000000000000000000000000000000000000000000000040" + // offset of [0]
		"00000000000000000000000000000000000000000000000000000000000000c0" + // offset of [1]
		"0000000000000000000000000000000000000000000000000000000000000001" + // [0].success
		"0000000000000000000000000000000000000000000000000000000000000040" + // [0] offset of returnData
		"0000000000000000000000000000000000000000000000000000000000000020" + // 32 bytes
		"0000000000000000000000000000000000000000000000000000000000000019" + // 25
		"0000000000000000000000000000000000000000000000000000000000000000" + // [1].success
		"0000000000000000000000000000000000000000000000000000000000000040" + // [1] offset of returnData
		"0000000000000000000000000000000000000000000000000000000000000000") // 0 bytes
	res, err := contracts.ParseAggregate3Result(raw)
	w := evm.Uint64Word(25)
	if err != nil || len(res) != 2 || !res[0].Success || !bytes.Equal(res[0].Data, w[:]) || res[1].Success || len(res[1].Data) != 0 {
		t.Fatalf("results %+v %v", res, err)
	}
	if !bytes.Equal(contracts.EncodeAggregate3Result(res), raw) {
		t.Fatal("encoding differs from solc's")
	}
}

func TestAggregate3RoundTrip(t *testing.T) {
	calls := []contracts.Call{{Target: evm.Address{1}, Data: contracts.BalanceOf(evm.Address{2})}, {Target: evm.Address{3}, AllowFailure: true},
		{Target: evm.Address{4}, Data: bytes.Repeat([]byte{7}, 70)}}
	back, ok, err := contracts.ParseAggregate3(contracts.Aggregate3(calls))
	if err != nil || !ok || len(back) != 3 {
		t.Fatalf("parse: %v %v %d", ok, err, len(back))
	}
	for i := range calls {
		if back[i].Target != calls[i].Target || back[i].AllowFailure != calls[i].AllowFailure || !bytes.Equal(back[i].Data, calls[i].Data) {
			t.Fatalf("call %d: %+v", i, back[i])
		}
	}
	if _, ok, _ := contracts.ParseAggregate3(contracts.BalanceOf(evm.Address{2})); ok {
		t.Fatal("balanceOf parsed as aggregate3")
	}
	w := evm.Uint64Word(25)
	res := []contracts.Result{{Success: true, Data: w[:]}, {}, {Success: true, Data: bytes.Repeat([]byte{9}, 33)}}
	out, err := contracts.ParseAggregate3Result(contracts.EncodeAggregate3Result(res))
	if err != nil || len(out) != 3 || !bytes.Equal(out[0].Data, w[:]) || out[1].Success || len(out[1].Data) != 0 ||
		!bytes.Equal(out[2].Data, res[2].Data) {
		t.Fatalf("result: %v %+v", err, out)
	}
	if _, err := contracts.ParseAggregate3Result(contracts.EncodeAggregate3Result(res)[:100]); err == nil {
		t.Fatal("truncated result parsed")
	}
}

// BlockAndAggregate3 reads the block first (Multicall3's own getBlockNumber, never allowed to
// fail); the answer gives the block and one result per call.
func TestBlockAndAggregate3(t *testing.T) {
	mc := contracts.Multicall3Address
	calls := []contracts.Call{{Target: evm.Address{1}, Data: contracts.BalanceOf(evm.Address{2})}}
	back, _, err := contracts.ParseAggregate3(contracts.BlockAndAggregate3(mc, calls))
	if err != nil || len(back) != 2 || back[0].Target != mc || back[0].AllowFailure || !contracts.IsGetBlockNumber(back[0].Data) {
		t.Fatalf("calls %+v %v", back, err)
	}
	b, w := evm.Uint64Word(42), evm.Uint64Word(25)
	block, res, err := contracts.ParseBlockAndAggregate3(contracts.EncodeAggregate3Result([]contracts.Result{{Success: true, Data: b[:]},
		{Success: true, Data: w[:]}}), 1)
	if err != nil || block != 42 || len(res) != 1 || !bytes.Equal(res[0].Data, w[:]) {
		t.Fatalf("block %d results %+v %v", block, res, err)
	}
	if _, _, err := contracts.ParseBlockAndAggregate3(contracts.EncodeAggregate3Result([]contracts.Result{{Success: true, Data: b[:]}}), 1); err == nil {
		t.Fatal("a missing result parsed")
	}
	if _, _, err := contracts.ParseBlockAndAggregate3(contracts.EncodeAggregate3Result([]contracts.Result{{Data: b[:]}, {}}), 1); err == nil {
		t.Fatal("a failed block read parsed")
	}
}

// The forwarder probe is ForwarderImpl.tokens(): no data from an address without code, an ABI
// array from a deployed clone, a revert from a clone of an implementation without tokens().
func TestForwarderProbe(t *testing.T) {
	if got, want := contracts.ForwarderProbe(), evm.Selector("tokens()"); !bytes.Equal(got, want[:]) {
		t.Fatalf("probe %x, want %x", got, want)
	}
	if contracts.ForwarderDeployed(contracts.Result{Success: true}) || !contracts.ForwarderDeployed(contracts.Result{Success: true, Data: make([]byte, 64)}) {
		t.Fatal("deployment read wrong")
	}
	// A reverting probe means code without tokens(): a forwarder of an older implementation.
	if !contracts.ForwarderDeployed(contracts.Result{}) {
		t.Fatal("a reverting probe read as no forwarder")
	}
}
