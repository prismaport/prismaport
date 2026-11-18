package contracts

import (
	"fmt"
	"math/big"

	"github.com/prismaport/prismaport/internal/evm"
)

// Multicall3Address is where Multicall3 is deployed on Base, Base Sepolia and most EVM chains
// (the same CREATE address everywhere). The registry and PRISMAPORT_CHAIN_<ID>_MULTICALL3 can
// name another one.
var Multicall3Address = evm.Address{0xca, 0x11, 0xbd, 0xe0, 0x59, 0x77, 0xb3, 0x63, 0x11, 0x67, 0x02, 0x88, 0x62, 0xbe, 0x2a, 0x17,
	0x39, 0x76, 0xca, 0x11}

var (
	aggregate3     = evm.Selector("aggregate3((address,bool,bytes)[])")
	getBlockNumber = evm.Selector("getBlockNumber()")
)

// Call is one call of a Multicall3 aggregate3. A call with AllowFailure may revert without
// reverting the batch (its Result says so); any other call that reverts reverts the whole batch.
type Call struct {
	Target       evm.Address
	AllowFailure bool
	Data         []byte
}

// Result is one call's outcome in aggregate3's answer.
type Result struct {
	Success bool
	Data    []byte
}

// padded returns b followed by zeros up to a whole number of words.
func padded(b []byte) []byte {
	out := make([]byte, (len(b)+31)/32*32)
	copy(out, b)
	return out
}

func wordBytes(n uint64) []byte { w := evm.Uint64Word(n); return w[:] }

func boolWord(b bool) []byte {
	if b {
		return wordBytes(1)
	}
	return wordBytes(0)
}

// encodeBytesArray is the tail of a dynamic array of dynamic elements: the length, one offset
// per element (relative to the word after the length), then the elements.
func encodeBytesArray(elems [][]byte) []byte {
	out := wordBytes(uint64(len(elems)))
	off := uint64(32 * len(elems))
	for _, e := range elems {
		out = append(out, wordBytes(off)...)
		off += uint64(len(e))
	}
	for _, e := range elems {
		out = append(out, e...)
	}
	return out
}

// Aggregate3 encodes Multicall3.aggregate3(calls). It returns (bool success, bytes returnData)[]
// and reverts only when a call without AllowFailure fails.
func Aggregate3(calls []Call) []byte {
	elems := make([][]byte, len(calls))
	for i, c := range calls {
		a := evm.AddressWord(c.Target)
		e := append(append(append(a[:], boolWord(c.AllowFailure)...), wordBytes(96)...), wordBytes(uint64(len(c.Data)))...)
		elems[i] = append(e, padded(c.Data)...)
	}
	out := append(aggregate3[:], wordBytes(32)...)
	return append(out, encodeBytesArray(elems)...)
}

// EncodeAggregate3Result encodes aggregate3's return value (used by test fakes).
func EncodeAggregate3Result(results []Result) []byte {
	elems := make([][]byte, len(results))
	for i, r := range results {
		e := append(append(boolWord(r.Success), wordBytes(64)...), wordBytes(uint64(len(r.Data)))...)
		elems[i] = append(e, padded(r.Data)...)
	}
	return append(wordBytes(32), encodeBytesArray(elems)...)
}

// GetBlockNumber is the calldata of Multicall3.getBlockNumber().
func GetBlockNumber() []byte { return getBlockNumber[:] }

// IsGetBlockNumber reports whether data calls getBlockNumber() (used by test fakes).
func IsGetBlockNumber(data []byte) bool { return len(data) == 4 && [4]byte(data) == getBlockNumber }

// BlockAndAggregate3 encodes one aggregate3 to the Multicall3 at multicall that first reads the
// block it runs at (its own getBlockNumber(), never allowed to fail), then makes calls.
func BlockAndAggregate3(multicall evm.Address, calls []Call) []byte {
	return Aggregate3(append([]Call{{Target: multicall, Data: GetBlockNumber()}}, calls...))
}

// ParseBlockAndAggregate3 decodes the answer to BlockAndAggregate3 with n calls: the block and
// one Result per call.
func ParseBlockAndAggregate3(out []byte, n int) (uint64, []Result, error) {
	res, err := ParseAggregate3Result(out)
	if err != nil {
		return 0, nil, err
	}
	if len(res) != n+1 {
		return 0, nil, fmt.Errorf("%w: %d results for %d calls and the block", evm.ErrABI, len(res), n)
	}
	b := res[0]
	if !b.Success || len(b.Data) != 32 {
		return 0, nil, fmt.Errorf("%w: getBlockNumber answered %d bytes (success %v)", evm.ErrABI, len(b.Data), b.Success)
	}
	block := new(big.Int).SetBytes(b.Data)
	if !block.IsUint64() {
		return 0, nil, fmt.Errorf("%w: block number out of range", evm.ErrABI)
	}
	return block.Uint64(), res[1:], nil
}

// abiReader reads offsets, lengths and byte strings of ABI data with bounds checks.
type abiReader []byte

func (r abiReader) uint(at uint64) (uint64, error) {
	if at+32 > uint64(len(r)) || at+32 < at {
		return 0, fmt.Errorf("%w: word at %d past %d bytes", evm.ErrABI, at, len(r))
	}
	v := new(big.Int).SetBytes(r[at : at+32])
	if !v.IsUint64() || v.Uint64() > uint64(len(r)) {
		return 0, fmt.Errorf("%w: value at %d out of range", evm.ErrABI, at)
	}
	return v.Uint64(), nil
}

func (r abiReader) bytes(at uint64) ([]byte, error) {
	n, err := r.uint(at)
	if err != nil {
		return nil, err
	}
	if at+32+n > uint64(len(r)) {
		return nil, fmt.Errorf("%w: bytes at %d past the end", evm.ErrABI, at)
	}
	return r[at+32 : at+32+n], nil
}

// array returns the start of each element of the dynamic array whose length word is at at.
func (r abiReader) array(at uint64) ([]uint64, error) {
	n, err := r.uint(at)
	if err != nil {
		return nil, err
	}
	base := at + 32
	out := make([]uint64, n)
	for i := range out {
		off, err := r.uint(base + 32*uint64(i))
		if err != nil {
			return nil, err
		}
		out[i] = base + off
	}
	return out, nil
}

// boolAt reads an ABI bool.
func (r abiReader) boolAt(at uint64) (bool, error) {
	if at+32 > uint64(len(r)) || at+32 < at {
		return false, fmt.Errorf("%w: bool at %d past %d bytes", evm.ErrABI, at, len(r))
	}
	w := evm.Word(r[at : at+32])
	switch {
	case w == evm.Word{}:
		return false, nil
	case w == evm.Uint64Word(1):
		return true, nil
	}
	return false, fmt.Errorf("%w: bool at %d is not 0 or 1", evm.ErrABI, at)
}

// ParseAggregate3Result decodes aggregate3's return value.
func ParseAggregate3Result(out []byte) ([]Result, error) {
	r := abiReader(out)
	off, err := r.uint(0)
	if err != nil {
		return nil, err
	}
	starts, err := r.array(off)
	if err != nil {
		return nil, err
	}
	res := make([]Result, len(starts))
	for i, s := range starts {
		if res[i].Success, err = r.boolAt(s); err != nil {
			return nil, err
		}
		doff, err := r.uint(s + 32)
		if err != nil {
			return nil, err
		}
		if res[i].Data, err = r.bytes(s + doff); err != nil {
			return nil, err
		}
	}
	return res, nil
}

// ParseAggregate3 decodes aggregate3 calldata (used by test fakes); ok is false for any other call.
func ParseAggregate3(data []byte) ([]Call, bool, error) {
	if len(data) < 4 || [4]byte(data[:4]) != aggregate3 {
		return nil, false, nil
	}
	r := abiReader(data[4:])
	off, err := r.uint(0)
	if err != nil {
		return nil, true, err
	}
	starts, err := r.array(off)
	if err != nil {
		return nil, true, err
	}
	calls := make([]Call, len(starts))
	for i, s := range starts {
		if s+96 > uint64(len(r)) {
			return nil, true, fmt.Errorf("%w: call %d past the end", evm.ErrABI, i)
		}
		a, ok := evm.Word(r[s : s+32]).Address()
		if !ok {
			return nil, true, fmt.Errorf("%w: call %d target", evm.ErrABI, i)
		}
		allow, err := r.boolAt(s + 32)
		if err != nil {
			return nil, true, err
		}
		doff, err := r.uint(s + 64)
		if err != nil {
			return nil, true, err
		}
		d, err := r.bytes(s + doff)
		if err != nil {
			return nil, true, err
		}
		calls[i] = Call{Target: a, AllowFailure: allow, Data: d}
	}
	return calls, true, nil
}
