// Package evmfake is an in-memory EVM chain for unit tests of the chain layer: blocks with
// hashes, logs, reorgs, forks (two providers on diverging chains) and per-provider faults.
package evmfake

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"sync"

	"github.com/prismaport/prismaport/internal/contracts"
	"github.com/prismaport/prismaport/internal/evm"
	"github.com/prismaport/prismaport/internal/evmrpc"
)

// Block is one block of the fake chain with the logs it holds.
type Block struct {
	Number uint64
	Hash   evm.Word
	Parent evm.Word
	Time   uint64
	Logs   []evmrpc.Log
}

// Chain is a canonical chain. All methods are safe for concurrent use.
type Chain struct {
	mu         sync.Mutex
	id         uint64
	blocks     []*Block
	salt       uint64
	SafeDepth  uint64
	FinalDepth uint64
	// CallHandler answers eth_call at a block height. Without one, balanceOf(address) is answered
	// from the Transfer logs of the canonical chain up to that height. A call to Multicall is a
	// Multicall3 aggregate3: each inner call is answered the same way (Multicall's own
	// getBlockNumber() with the height), a failing one with AllowFailure gives a failed Result,
	// any other failing one reverts the batch (one request on the provider's counters).
	CallHandler func(m evmrpc.CallMsg, block uint64) ([]byte, error)
	Multicall   evm.Address
	// SendHandler receives raw transactions.
	SendHandler func(raw []byte) (evm.Word, error)
	// EstimateHandler answers eth_estimateGas; nil means 150,000.
	EstimateHandler func(m evmrpc.CallMsg) (uint64, error)
	// BaseFee is the base fee every header reports; nil means 1,000,000 wei.
	BaseFee  *big.Int
	receipts map[evm.Word]evmrpc.Receipt
	nonces   map[evm.Address]uint64
	Balances map[evm.Address]*big.Int
	// deployed is the height from which an address has code (a forwarder deployed by a flush);
	// legacy marks a forwarder of an implementation without tokens() (before D41).
	deployed map[evm.Address]uint64
	legacy   map[evm.Address]bool
	// finalCap, when set, holds the safe and finalized heads at or below it (StallFinality).
	finalCap *uint64
}

// New returns an empty fake chain with the given chain ID.
func New(chainID uint64) *Chain {
	c := &Chain{id: chainID, SafeDepth: 2, FinalDepth: 4, Multicall: contracts.Multicall3Address, receipts: map[evm.Word]evmrpc.Receipt{},
		nonces: map[evm.Address]uint64{}, Balances: map[evm.Address]*big.Int{}}
	c.blocks = []*Block{{Number: 0, Hash: c.hash(0, evm.Word{}), Time: 1_800_000_000}}
	return c
}

func (c *Chain) hash(n uint64, parent evm.Word) evm.Word {
	c.salt++
	var b [16]byte
	binary.BigEndian.PutUint64(b[:8], n)
	binary.BigEndian.PutUint64(b[8:], c.salt)
	var w evm.Word
	copy(w[:], evm.Keccak256(parent[:], b[:]))
	return w
}

// Fork returns an independent copy that shares history up to now; blocks mined afterwards on
// either side differ.
func (c *Chain) Fork() *Chain {
	c.mu.Lock()
	defer c.mu.Unlock()
	f := New(c.id)
	f.salt = c.salt + 1_000_000
	f.blocks = nil
	for _, b := range c.blocks {
		cp := *b
		cp.Logs = append([]evmrpc.Log(nil), b.Logs...)
		f.blocks = append(f.blocks, &cp)
	}
	f.SafeDepth, f.FinalDepth, f.finalCap = c.SafeDepth, c.FinalDepth, c.finalCap
	return f
}

// LogSpec is a log to include in the next mined block. TxHash zero means "derive one".
type LogSpec struct {
	Address evm.Address
	Topics  []evm.Word
	Data    []byte
	TxHash  evm.Word
}

// Mine appends a block with the given logs and returns it.
func (c *Chain) Mine(logs ...LogSpec) *Block {
	c.mu.Lock()
	defer c.mu.Unlock()
	tip := c.blocks[len(c.blocks)-1]
	b := &Block{Number: tip.Number + 1, Parent: tip.Hash, Time: tip.Time + 2}
	b.Hash = c.hash(b.Number, tip.Hash)
	for i, l := range logs {
		tx := l.TxHash
		if tx == (evm.Word{}) {
			copy(tx[:], evm.Keccak256(b.Hash[:], []byte{byte(i)}))
		}
		b.Logs = append(b.Logs, evmrpc.Log{Address: l.Address, Topics: l.Topics, Data: l.Data, BlockNumber: b.Number,
			BlockHash: b.Hash, TxHash: tx, LogIndex: uint32(i)})
	}
	c.blocks = append(c.blocks, b)
	return b
}

// MineN appends n empty blocks.
func (c *Chain) MineN(n int) {
	for range n {
		c.Mine()
	}
}

// Reorg drops the last depth blocks; the next mined blocks get new hashes.
func (c *Chain) Reorg(depth int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.blocks = c.blocks[:len(c.blocks)-depth]
}

// Tip returns the newest block of the chain.
func (c *Chain) Tip() *Block {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.blocks[len(c.blocks)-1]
}

// Block returns the block at height n, if the chain has one.
func (c *Chain) Block(n uint64) (*Block, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n >= uint64(len(c.blocks)) {
		return nil, false
	}
	return c.blocks[n], true
}

// StallFinality holds the safe and finalized heads at or below n while blocks keep coming (a
// stalled finality); ResumeFinality lets them follow SafeDepth and FinalDepth again.
func (c *Chain) StallFinality(n uint64) {
	c.mu.Lock()
	c.finalCap = &n
	c.mu.Unlock()
}

// ResumeFinality lifts StallFinality, so the safe and finalized heads follow the tip again.
func (c *Chain) ResumeFinality() {
	c.mu.Lock()
	c.finalCap = nil
	c.mu.Unlock()
}

// SetReceipt registers a mined transaction's receipt.
func (c *Chain) SetReceipt(r evmrpc.Receipt) {
	c.mu.Lock()
	c.receipts[r.TxHash] = r
	c.mu.Unlock()
}

// SetNonce sets the next nonce the chain reports for the address.
func (c *Chain) SetNonce(a evm.Address, n uint64) {
	c.mu.Lock()
	c.nonces[a] = n
	c.mu.Unlock()
}

// Provider is one view of a chain with fault injection.
type Provider struct {
	name string
	mu   sync.Mutex
	c    *Chain
	// FailNext makes the next n calls fail with a transport error.
	failNext int
	down     bool
	lag      uint64
	calls    map[string]int
	// maxLogRange rejects an eth_getLogs range wider than it (0: no limit), as hosted
	// providers do; logRanges records every range asked for.
	maxLogRange uint64
	logRanges   [][2]uint64
	// hideReceipts answers the next n receipt requests "not found" (an index that has not caught
	// up with a mined transaction yet).
	hideReceipts int
	// logLag is how far the provider's log index trails its head (SetLogLag); finalityLag holds
	// its safe and finalized tags that much further behind (SetFinalityLag).
	logLag      uint64
	finalityLag uint64
}

// Provider returns a new healthy provider named name that reads this chain.
func (c *Chain) Provider(name string) *Provider {
	return &Provider{name: name, c: c, calls: map[string]int{}}
}

// Point switches the provider to another chain (simulating a provider on a fork).
func (p *Provider) Point(c *Chain) {
	p.mu.Lock()
	p.c = c
	p.mu.Unlock()
}

// FailNext makes the provider's next n calls fail as if it were unreachable.
func (p *Provider) FailNext(n int) { p.mu.Lock(); p.failNext = n; p.mu.Unlock() }

// SetDown makes every call of the provider fail, as if it were unreachable, while d is true.
func (p *Provider) SetDown(d bool) { p.mu.Lock(); p.down = d; p.mu.Unlock() }

// SetLag makes the provider report a tip n blocks behind the chain's.
func (p *Provider) SetLag(n uint64) { p.mu.Lock(); p.lag = n; p.mu.Unlock() }

// SetMaxLogRange makes the provider reject log queries spanning more than n blocks; zero means no
// limit.
func (p *Provider) SetMaxLogRange(n uint64) { p.mu.Lock(); p.maxLogRange = n; p.mu.Unlock() }

// HideReceipts answers the provider's next n eth_getTransactionReceipt requests "not found".
func (p *Provider) HideReceipts(n int) { p.mu.Lock(); p.hideReceipts = n; p.mu.Unlock() }

// SetLogLag makes the provider's log index trail its head by n blocks: eth_getLogs still accepts
// a range up to the head but leaves out the logs of the last n blocks (a partial answer), while
// headers and eth_call (balances) already show them. Hosted providers index logs after the block.
func (p *Provider) SetLogLag(n uint64) { p.mu.Lock(); p.logLag = n; p.mu.Unlock() }

// SetFinalityLag holds the provider's safe and finalized tags n more blocks behind its own head
// than the chain's SafeDepth and FinalDepth (a provider slow to follow finality).
func (p *Provider) SetFinalityLag(n uint64) { p.mu.Lock(); p.finalityLag = n; p.mu.Unlock() }

// tagLag reads the provider's finality and log index lags.
func (p *Provider) tagLag() (finality, logs uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.finalityLag, p.logLag
}

// LogRanges returns the [from, to] of every eth_getLogs call that reached the provider.
func (p *Provider) LogRanges() [][2]uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.logRanges)
}

// Calls returns how many times the provider was called with the given JSON-RPC method.
func (p *Provider) Calls(method string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[method]
}

// Total is the number of requests of any method that reached the provider.
func (p *Provider) Total() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, v := range p.calls {
		n += v
	}
	return n
}

// Name returns the provider's name.
func (p *Provider) Name() string { return p.name }

var errDown = fmt.Errorf("%w: fake provider down", evmrpc.ErrTransport)

func (p *Provider) enter(method string) (*Chain, uint64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls[method]++
	if p.down {
		return nil, 0, errDown
	}
	if p.failNext > 0 {
		p.failNext--
		return nil, 0, errDown
	}
	return p.c, p.lag, nil
}

// visibleTip is the highest block this provider shows.
func (c *Chain) visibleTip(lag uint64) uint64 {
	tip := c.blocks[len(c.blocks)-1].Number
	if lag > tip {
		return 0
	}
	return tip - lag
}

// ChainID returns the fake chain's ID.
func (p *Provider) ChainID(context.Context) (uint64, error) {
	c, _, err := p.enter("eth_chainId")
	if err != nil {
		return 0, err
	}
	return c.id, nil
}

// HeaderByTag returns the header of the latest, safe or finalized block, or of a numbered one, as
// the provider sees the chain.
func (p *Provider) HeaderByTag(_ context.Context, tag evmrpc.BlockTag) (evmrpc.Header, error) {
	c, lag, err := p.enter("eth_getBlockByNumber")
	if err != nil {
		return evmrpc.Header{}, err
	}
	finLag, _ := p.tagLag()
	c.mu.Lock()
	defer c.mu.Unlock()
	tip := c.visibleTip(lag)
	var n uint64
	switch tag {
	case evmrpc.Latest, evmrpc.Pending:
		n = tip
	case evmrpc.Safe:
		n = c.final(tip, c.SafeDepth+finLag)
	case evmrpc.Finalized:
		n = c.final(tip, c.FinalDepth+finLag)
	default:
		var q evmrpc.Quantity
		if err := q.UnmarshalJSON([]byte(`"` + string(tag) + `"`)); err != nil {
			return evmrpc.Header{}, err
		}
		n = q.Int.Uint64()
		if n > tip {
			return evmrpc.Header{}, evmrpc.ErrNotFound
		}
	}
	b := c.blocks[n]
	base := big.NewInt(1_000_000)
	if c.BaseFee != nil {
		base = new(big.Int).Set(c.BaseFee)
	}
	return evmrpc.Header{Number: b.Number, Hash: b.Hash, ParentHash: b.Parent, Timestamp: b.Time, BaseFee: base}, nil
}

// final is a safe or finalized head depth blocks below tip, held at the stall cap if any.
func (c *Chain) final(tip, depth uint64) uint64 {
	n := tip - min(tip, depth)
	if c.finalCap != nil {
		n = min(n, *c.finalCap)
	}
	return n
}

func matches(l evmrpc.Log, f evmrpc.Filter) bool {
	if len(f.Addresses) > 0 {
		ok := false
		for _, a := range f.Addresses {
			ok = ok || a == l.Address
		}
		if !ok {
			return false
		}
	}
	for i, alts := range f.Topics {
		if len(alts) == 0 {
			continue
		}
		if i >= len(l.Topics) {
			return false
		}
		ok := false
		for _, t := range alts {
			ok = ok || t == l.Topics[i]
		}
		if !ok {
			return false
		}
	}
	return true
}

// Logs returns the chain's logs matching the filter.
func (p *Provider) Logs(_ context.Context, f evmrpc.Filter) ([]evmrpc.Log, error) {
	c, lag, err := p.enter("eth_getLogs")
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.logRanges = append(p.logRanges, [2]uint64{f.FromBlock, f.ToBlock})
	limit, logLag := p.maxLogRange, p.logLag
	p.mu.Unlock()
	if limit > 0 && f.ToBlock >= f.FromBlock && f.ToBlock-f.FromBlock >= limit {
		return nil, &evmrpc.RPCError{Code: -32600, Message: fmt.Sprintf("eth_getLogs is limited to a %d block range", limit)}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	tip := c.visibleTip(lag)
	if f.ToBlock > tip || f.FromBlock > f.ToBlock {
		return nil, &evmrpc.RPCError{Code: -32000, Message: "block range extends beyond current head"}
	}
	indexed := tip - min(tip, logLag)
	var out []evmrpc.Log
	for n := f.FromBlock; n <= min(f.ToBlock, indexed); n++ {
		for _, l := range c.blocks[n].Logs {
			if matches(l, f) {
				out = append(out, l)
			}
		}
	}
	return out, nil
}

// Call answers eth_call at the given block as the provider sees the chain: Multicall3 batches are
// split and answered one by one, each call as the chain answers it (see call).
func (p *Provider) Call(_ context.Context, m evmrpc.CallMsg, tag evmrpc.BlockTag) ([]byte, error) {
	c, lag, err := p.enter("eth_call")
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	h := c.CallHandler
	n := c.visibleTip(lag)
	c.mu.Unlock()
	if tag != evmrpc.Latest && tag != evmrpc.Pending {
		var q evmrpc.Quantity
		if err := q.UnmarshalJSON([]byte(`"` + string(tag) + `"`)); err == nil {
			n = q.Int.Uint64()
		}
	}
	if mc := c.multicall(); m.To == mc {
		calls, ok, err := contracts.ParseAggregate3(m.Data)
		if !ok || err != nil {
			return nil, fmt.Errorf("evmfake: not an aggregate3 call: %v", err)
		}
		results := make([]contracts.Result, len(calls))
		for i, sub := range calls {
			var out []byte
			var err error
			if sub.Target == mc && contracts.IsGetBlockNumber(sub.Data) {
				w := evm.Uint64Word(n)
				out = w[:]
			} else {
				out, err = c.call(h, evmrpc.CallMsg{To: sub.Target, Data: sub.Data}, n)
			}
			if err != nil && !sub.AllowFailure {
				return nil, &evmrpc.RPCError{Code: 3, Message: "execution reverted: Multicall3: call failed"}
			}
			results[i] = contracts.Result{Success: err == nil, Data: out}
			if err != nil {
				results[i].Data = nil
			}
		}
		return contracts.EncodeAggregate3Result(results), nil
	}
	return c.call(h, m, n)
}

func (c *Chain) multicall() evm.Address {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Multicall
}

var (
	balanceOf       = evm.Selector("balanceOf(address)")
	forwarderTokens = evm.Selector("tokens()")
)

// Deploy gives addr code from height on (a forwarder deployed by deployAndFlush): tokens()
// answers an ABI array there, as ForwarderImpl does; before it, as on any address without code,
// the call succeeds with no return data.
func (c *Chain) Deploy(addr evm.Address, height uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.deployed == nil {
		c.deployed = map[evm.Address]uint64{}
	}
	c.deployed[addr] = height
}

// DeployLegacy is Deploy for a forwarder of an implementation without tokens() (before D41):
// the probe reverts there.
func (c *Chain) DeployLegacy(addr evm.Address, height uint64) {
	c.Deploy(addr, height)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.legacy == nil {
		c.legacy = map[evm.Address]bool{}
	}
	c.legacy[addr] = true
}

// call answers one eth_call: the forwarder probe tokens() from the deployments, then the handler,
// or balanceOf from the logs without one.
func (c *Chain) call(h func(evmrpc.CallMsg, uint64) ([]byte, error), m evmrpc.CallMsg, n uint64) ([]byte, error) {
	if len(m.Data) == 4 && [4]byte(m.Data) == forwarderTokens {
		c.mu.Lock()
		at, ok := c.deployed[m.To]
		legacy := c.legacy[m.To]
		c.mu.Unlock()
		if !ok || at > n {
			return nil, nil
		}
		if legacy {
			return nil, &evmrpc.RPCError{Code: 3, Message: "execution reverted"} // as a node answers a revert
		}
		var out [64]byte
		out[31] = 0x20 // offset of an empty address[]
		return out[:], nil
	}
	if h != nil {
		return h(m, n)
	}
	if len(m.Data) == 36 && [4]byte(m.Data[:4]) == balanceOf {
		holder, _ := evm.Word(m.Data[4:36]).Address()
		w := evm.BigWord(c.TokenBalance(m.To, holder, n))
		return w[:], nil
	}
	return nil, errors.New("evmfake: no call handler")
}

// TokenBalance is holder's balance of token at height n: Transfer logs of the canonical chain into
// it minus those out of it.
func (c *Chain) TokenBalance(token, holder evm.Address, n uint64) *big.Int {
	c.mu.Lock()
	defer c.mu.Unlock()
	bal := new(big.Int)
	h := evm.AddressWord(holder)
	for _, b := range c.blocks {
		if b.Number > n {
			break
		}
		for _, l := range b.Logs {
			if l.Address != token || len(l.Topics) != 3 || l.Topics[0] != transferTopic || len(l.Data) != 32 {
				continue
			}
			v := new(big.Int).SetBytes(l.Data)
			if l.Topics[2] == h {
				bal.Add(bal, v)
			}
			if l.Topics[1] == h {
				bal.Sub(bal, v)
			}
		}
	}
	return bal
}

var transferTopic = evm.Topic("Transfer(address,address,uint256)")

// EstimateGas returns the chain's EstimateHandler answer, or 150000 when it has none.
func (p *Provider) EstimateGas(_ context.Context, m evmrpc.CallMsg) (uint64, error) {
	c, _, err := p.enter("eth_estimateGas")
	if err != nil {
		return 0, err
	}
	c.mu.Lock()
	h := c.EstimateHandler
	c.mu.Unlock()
	if h != nil {
		return h(m)
	}
	return 150_000, nil
}

// Nonce returns the nonce set for the address, zero by default.
func (p *Provider) Nonce(_ context.Context, a evm.Address, _ evmrpc.BlockTag) (uint64, error) {
	c, _, err := p.enter("eth_getTransactionCount")
	if err != nil {
		return 0, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.nonces[a], nil
}

// Balance returns the native balance set in Balances for the address, or 1 ETH by default.
func (p *Provider) Balance(_ context.Context, a evm.Address, _ evmrpc.BlockTag) (*big.Int, error) {
	c, _, err := p.enter("eth_getBalance")
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if b, ok := c.Balances[a]; ok {
		return new(big.Int).Set(b), nil
	}
	return big.NewInt(1e18), nil
}

// Code reports no code at any address.
func (p *Provider) Code(context.Context, evm.Address, evmrpc.BlockTag) ([]byte, error) {
	if _, _, err := p.enter("eth_getCode"); err != nil {
		return nil, err
	}
	return nil, nil
}

// MaxPriorityFee returns a fixed priority fee of 1000 wei.
func (p *Provider) MaxPriorityFee(context.Context) (*big.Int, error) {
	if _, _, err := p.enter("eth_maxPriorityFeePerGas"); err != nil {
		return nil, err
	}
	return big.NewInt(1000), nil
}

// SendRawTransaction passes the raw transaction to the chain's SendHandler, or returns its
// Keccak-256 hash when there is none.
func (p *Provider) SendRawTransaction(_ context.Context, raw []byte) (evm.Word, error) {
	c, _, err := p.enter("eth_sendRawTransaction")
	if err != nil {
		return evm.Word{}, err
	}
	c.mu.Lock()
	h := c.SendHandler
	c.mu.Unlock()
	if h == nil {
		var w evm.Word
		copy(w[:], evm.Keccak256(raw))
		return w, nil
	}
	return h(raw)
}

// Receipt returns the receipt registered with SetReceipt, or evmrpc.ErrNotFound when there is none
// or HideReceipts hides it.
func (p *Provider) Receipt(_ context.Context, hash evm.Word) (evmrpc.Receipt, error) {
	c, _, err := p.enter("eth_getTransactionReceipt")
	if err != nil {
		return evmrpc.Receipt{}, err
	}
	p.mu.Lock()
	hidden := p.hideReceipts > 0
	if hidden {
		p.hideReceipts--
	}
	p.mu.Unlock()
	if hidden {
		return evmrpc.Receipt{}, evmrpc.ErrNotFound
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.receipts[hash]
	if !ok {
		return evmrpc.Receipt{}, evmrpc.ErrNotFound
	}
	if r.BlockNumber >= uint64(len(c.blocks)) || c.blocks[r.BlockNumber].Hash != r.BlockHash {
		return evmrpc.Receipt{}, evmrpc.ErrNotFound
	}
	return r, nil
}
