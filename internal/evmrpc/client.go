// Package evmrpc is a small Ethereum JSON-RPC client covering what the detector, flusher and
// relayer need. It speaks plain HTTP JSON-RPC 2.0; every quantity is parsed exactly (no floats).
package evmrpc

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/prismaport/prismaport/internal/evm"
	"github.com/prismaport/prismaport/internal/rpcstat"
)

const maxResponseBytes = 64 << 20

var (
	// ErrTransport means the provider could not answer (network, HTTP status, rate limit,
	// malformed response). The caller should fail over.
	ErrTransport = errors.New("evmrpc: provider unavailable")
	// ErrNotFound is returned when the provider has no such block, transaction or receipt.
	ErrNotFound = errors.New("evmrpc: not found")
)

// RPCError is a JSON-RPC error object returned by a healthy provider.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("evmrpc: rpc error %d: %s", e.Code, e.Message) }

// RevertData returns the ABI-encoded revert payload when the error is an execution revert.
func (e *RPCError) RevertData() ([]byte, bool) {
	if len(e.Data) == 0 {
		return nil, strings.Contains(strings.ToLower(e.Message), "revert")
	}
	var s string
	if err := json.Unmarshal(e.Data, &s); err == nil {
		b, err := evm.ParseHexBytes(s)
		return b, err == nil
	}
	return nil, strings.Contains(strings.ToLower(e.Message), "revert")
}

// IsRevert reports whether err is an execution revert (a property of the call, not of the
// provider's health).
func IsRevert(err error) bool {
	var re *RPCError
	if !errors.As(err, &re) {
		return false
	}
	_, ok := re.RevertData()
	return ok || re.Code == 3
}

// Client calls one EVM JSON-RPC provider over HTTP.
type Client struct {
	name string
	url  string
	http *http.Client
	id   atomic.Uint64
}

// New returns a client for one provider URL. name is used in logs and must not contain secrets
// (provider URLs often embed API keys, so the URL itself is never logged).
func New(name, url string, timeout time.Duration) *Client {
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	return &Client{name: name, url: url, http: &http.Client{Timeout: timeout}}
}

// Name returns the provider's name, used in logs and alerts.
func (c *Client) Name() string { return c.name }

type request struct {
	JSONRPC string `json:"jsonrpc"`
	ID      uint64 `json:"id"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
}

type response struct {
	ID     uint64          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *RPCError       `json:"error"`
}

func (c *Client) call(ctx context.Context, out any, method string, params ...any) error {
	rpcstat.Count(ctx)
	if params == nil {
		params = []any{}
	}
	id := c.id.Add(1)
	body, err := json.Marshal(request{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%w: %s: build request", ErrTransport, c.name)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: %s %s: %v", ErrTransport, c.name, method, redactURL(err, c.url))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("%w: %s %s: read: %v", ErrTransport, c.name, method, redactURL(err, c.url))
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: %s %s: HTTP %d", ErrTransport, c.name, method, resp.StatusCode)
	}
	var r response
	if err := json.Unmarshal(raw, &r); err != nil {
		return fmt.Errorf("%w: %s %s: malformed response", ErrTransport, c.name, method)
	}
	if r.ID != id {
		return fmt.Errorf("%w: %s %s: response id %d for request %d", ErrTransport, c.name, method, r.ID, id)
	}
	if r.Error != nil {
		// Rate limits and server-side failures are the provider's health, not the call's.
		if r.Error.Code == -32005 || r.Error.Code == 429 || (r.Error.Code == -32603 && !IsRevert(r.Error)) {
			return fmt.Errorf("%w: %s %s: %v", ErrTransport, c.name, method, r.Error)
		}
		return r.Error
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(r.Result, out); err != nil {
		return fmt.Errorf("%w: %s %s: decode result: %v", ErrTransport, c.name, method, err)
	}
	return nil
}

func redactURL(err error, url string) string {
	return strings.ReplaceAll(err.Error(), url, "<provider url>")
}

// Quantity is a hex-encoded JSON-RPC integer.
type Quantity struct{ big.Int }

// UnmarshalJSON decodes a 0x-prefixed hex quantity of at most 256 bits.
func (q *Quantity) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	body, ok := strings.CutPrefix(s, "0x")
	if !ok || body == "" || len(body) > 64 {
		return fmt.Errorf("evmrpc: bad quantity %q", s)
	}
	if _, ok := q.SetString(body, 16); !ok {
		return fmt.Errorf("evmrpc: bad quantity %q", s)
	}
	return nil
}

// Uint64 returns the quantity as a uint64, or an error when it does not fit.
func (q Quantity) Uint64() (uint64, error) {
	if !q.IsUint64() {
		return 0, fmt.Errorf("evmrpc: quantity %s exceeds uint64", q.String())
	}
	return q.Int.Uint64(), nil
}

func hexUint(n uint64) string { return "0x" + strconv.FormatUint(n, 16) }

func hexBytes(b []byte) string { return "0x" + hex.EncodeToString(b) }

// BlockTag is "latest", "safe", "finalized", "pending" or a block number.
type BlockTag string

// The named block tags.
const (
	Latest    BlockTag = "latest"
	Safe      BlockTag = "safe"
	Finalized BlockTag = "finalized"
	Pending   BlockTag = "pending"
)

// Number returns the block tag of block n.
func Number(n uint64) BlockTag { return BlockTag(hexUint(n)) }

// ChainID returns the chain ID the provider serves (eth_chainId).
func (c *Client) ChainID(ctx context.Context) (uint64, error) {
	var q Quantity
	if err := c.call(ctx, &q, "eth_chainId"); err != nil {
		return 0, err
	}
	return q.Uint64()
}

// Header is the part of a block header the chain layer reads.
type Header struct {
	Number     uint64
	Hash       evm.Word
	ParentHash evm.Word
	Timestamp  uint64
	BaseFee    *big.Int
}

type rawHeader struct {
	Number     Quantity  `json:"number"`
	Hash       string    `json:"hash"`
	ParentHash string    `json:"parentHash"`
	Timestamp  Quantity  `json:"timestamp"`
	BaseFee    *Quantity `json:"baseFeePerGas"`
}

// HeaderByTag returns the block header at a tag or number; ErrNotFound when the block does not
// exist (yet).
func (c *Client) HeaderByTag(ctx context.Context, tag BlockTag) (Header, error) {
	var raw *rawHeader
	if err := c.call(ctx, &raw, "eth_getBlockByNumber", string(tag), false); err != nil {
		return Header{}, err
	}
	if raw == nil {
		return Header{}, ErrNotFound
	}
	var h Header
	var err error
	if h.Number, err = raw.Number.Uint64(); err != nil {
		return h, fmt.Errorf("%w: %s: %v", ErrTransport, c.name, err)
	}
	if h.Timestamp, err = raw.Timestamp.Uint64(); err != nil {
		return h, fmt.Errorf("%w: %s: %v", ErrTransport, c.name, err)
	}
	if h.Hash, err = evm.ParseWord(raw.Hash); err != nil {
		return h, fmt.Errorf("%w: %s: block hash: %v", ErrTransport, c.name, err)
	}
	if h.ParentHash, err = evm.ParseWord(raw.ParentHash); err != nil {
		return h, fmt.Errorf("%w: %s: parent hash: %v", ErrTransport, c.name, err)
	}
	if raw.BaseFee != nil {
		h.BaseFee = new(big.Int).Set(&raw.BaseFee.Int)
	}
	return h, nil
}

// Filter is an eth_getLogs query over an inclusive block range. Topics[i] is an OR list for
// position i; an empty list matches anything.
type Filter struct {
	FromBlock uint64
	ToBlock   uint64
	Addresses []evm.Address
	Topics    [][]evm.Word
}

// Log is one event log as eth_getLogs returns it.
type Log struct {
	Address     evm.Address
	Topics      []evm.Word
	Data        []byte
	BlockNumber uint64
	BlockHash   evm.Word
	TxHash      evm.Word
	LogIndex    uint32
	Removed     bool
}

type rawLog struct {
	Address     string   `json:"address"`
	Topics      []string `json:"topics"`
	Data        string   `json:"data"`
	BlockNumber Quantity `json:"blockNumber"`
	BlockHash   string   `json:"blockHash"`
	TxHash      string   `json:"transactionHash"`
	LogIndex    Quantity `json:"logIndex"`
	Removed     bool     `json:"removed"`
}

func (r rawLog) decode() (Log, error) {
	var l Log
	var err error
	if l.Address, err = evm.ParseAddressLoose(r.Address); err != nil {
		return l, err
	}
	for _, t := range r.Topics {
		w, err := evm.ParseWord(t)
		if err != nil {
			return l, err
		}
		l.Topics = append(l.Topics, w)
	}
	if l.Data, err = evm.ParseHexBytes(r.Data); err != nil {
		return l, err
	}
	if l.BlockNumber, err = r.BlockNumber.Uint64(); err != nil {
		return l, err
	}
	if l.BlockHash, err = evm.ParseWord(r.BlockHash); err != nil {
		return l, err
	}
	if l.TxHash, err = evm.ParseWord(r.TxHash); err != nil {
		return l, err
	}
	idx, err := r.LogIndex.Uint64()
	if err != nil || idx > 1<<31-1 {
		return l, fmt.Errorf("evmrpc: log index out of range")
	}
	l.LogIndex, l.Removed = uint32(idx), r.Removed
	return l, nil
}

// Logs returns the logs matching the filter (eth_getLogs).
func (c *Client) Logs(ctx context.Context, f Filter) ([]Log, error) {
	q := map[string]any{"fromBlock": hexUint(f.FromBlock), "toBlock": hexUint(f.ToBlock)}
	if len(f.Addresses) > 0 {
		as := make([]string, len(f.Addresses))
		for i, a := range f.Addresses {
			as[i] = a.Lower()
		}
		q["address"] = as
	}
	if len(f.Topics) > 0 {
		ts := make([]any, len(f.Topics))
		for i, pos := range f.Topics {
			if len(pos) == 0 {
				ts[i] = nil
				continue
			}
			alts := make([]string, len(pos))
			for j, w := range pos {
				alts[j] = w.Hex()
			}
			ts[i] = alts
		}
		q["topics"] = ts
	}
	var raws []rawLog
	if err := c.call(ctx, &raws, "eth_getLogs", q); err != nil {
		return nil, err
	}
	out := make([]Log, 0, len(raws))
	for _, r := range raws {
		l, err := r.decode()
		if err != nil {
			return nil, fmt.Errorf("%w: %s: log: %v", ErrTransport, c.name, err)
		}
		if l.BlockNumber < f.FromBlock || l.BlockNumber > f.ToBlock {
			return nil, fmt.Errorf("%w: %s: log at block %d outside [%d,%d]", ErrTransport, c.name, l.BlockNumber, f.FromBlock, f.ToBlock)
		}
		out = append(out, l)
	}
	return out, nil
}

// CallMsg is an eth_call / eth_estimateGas message.
type CallMsg struct {
	From *evm.Address
	To   evm.Address
	Data []byte
}

func (m CallMsg) json() map[string]any {
	q := map[string]any{"to": m.To.Lower(), "data": hexBytes(m.Data)}
	if m.From != nil {
		q["from"] = m.From.Lower()
	}
	return q
}

// Call runs a read-only contract call at the given block (eth_call) and returns its output.
func (c *Client) Call(ctx context.Context, m CallMsg, tag BlockTag) ([]byte, error) {
	var s string
	if err := c.call(ctx, &s, "eth_call", m.json(), string(tag)); err != nil {
		return nil, err
	}
	b, err := evm.ParseHexBytes(s)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: call result: %v", ErrTransport, c.name, err)
	}
	return b, nil
}

// EstimateGas estimates the gas the call needs (eth_estimateGas).
func (c *Client) EstimateGas(ctx context.Context, m CallMsg) (uint64, error) {
	var q Quantity
	if err := c.call(ctx, &q, "eth_estimateGas", m.json()); err != nil {
		return 0, err
	}
	return q.Uint64()
}

// Nonce returns the address's transaction count at the given block (eth_getTransactionCount).
func (c *Client) Nonce(ctx context.Context, a evm.Address, tag BlockTag) (uint64, error) {
	var q Quantity
	if err := c.call(ctx, &q, "eth_getTransactionCount", a.Lower(), string(tag)); err != nil {
		return 0, err
	}
	return q.Uint64()
}

// Balance returns the address's native balance at the given block (eth_getBalance).
func (c *Client) Balance(ctx context.Context, a evm.Address, tag BlockTag) (*big.Int, error) {
	var q Quantity
	if err := c.call(ctx, &q, "eth_getBalance", a.Lower(), string(tag)); err != nil {
		return nil, err
	}
	return new(big.Int).Set(&q.Int), nil
}

// Code returns the contract code at the address at the given block (eth_getCode).
func (c *Client) Code(ctx context.Context, a evm.Address, tag BlockTag) ([]byte, error) {
	var s string
	if err := c.call(ctx, &s, "eth_getCode", a.Lower(), string(tag)); err != nil {
		return nil, err
	}
	return evm.ParseHexBytes(s)
}

// MaxPriorityFee returns the provider's suggested priority fee per gas (eth_maxPriorityFeePerGas).
func (c *Client) MaxPriorityFee(ctx context.Context) (*big.Int, error) {
	var q Quantity
	if err := c.call(ctx, &q, "eth_maxPriorityFeePerGas"); err != nil {
		return nil, err
	}
	return new(big.Int).Set(&q.Int), nil
}

// SendRawTransaction broadcasts a signed transaction and returns the hash the node reports.
func (c *Client) SendRawTransaction(ctx context.Context, raw []byte) (evm.Word, error) {
	var s string
	if err := c.call(ctx, &s, "eth_sendRawTransaction", hexBytes(raw)); err != nil {
		return evm.Word{}, err
	}
	return evm.ParseWord(s)
}

// Receipt is the part of a transaction receipt the chain layer reads.
type Receipt struct {
	TxHash            evm.Word
	Status            uint64
	BlockNumber       uint64
	BlockHash         evm.Word
	GasUsed           uint64
	EffectiveGasPrice *big.Int
	Logs              []Log
}

type rawReceipt struct {
	TxHash            string    `json:"transactionHash"`
	Status            Quantity  `json:"status"`
	BlockNumber       Quantity  `json:"blockNumber"`
	BlockHash         string    `json:"blockHash"`
	GasUsed           Quantity  `json:"gasUsed"`
	EffectiveGasPrice *Quantity `json:"effectiveGasPrice"`
	Logs              []rawLog  `json:"logs"`
}

// Receipt returns ErrNotFound while the transaction is not mined.
func (c *Client) Receipt(ctx context.Context, hash evm.Word) (Receipt, error) {
	var raw *rawReceipt
	if err := c.call(ctx, &raw, "eth_getTransactionReceipt", hash.Hex()); err != nil {
		return Receipt{}, err
	}
	if raw == nil {
		return Receipt{}, ErrNotFound
	}
	var r Receipt
	var err error
	bad := func(what string, err error) (Receipt, error) {
		return Receipt{}, fmt.Errorf("%w: %s: receipt %s: %v", ErrTransport, c.name, what, err)
	}
	if r.TxHash, err = evm.ParseWord(raw.TxHash); err != nil {
		return bad("hash", err)
	}
	if r.Status, err = raw.Status.Uint64(); err != nil {
		return bad("status", err)
	}
	if r.BlockNumber, err = raw.BlockNumber.Uint64(); err != nil {
		return bad("block", err)
	}
	if r.BlockHash, err = evm.ParseWord(raw.BlockHash); err != nil {
		return bad("block hash", err)
	}
	if r.GasUsed, err = raw.GasUsed.Uint64(); err != nil {
		return bad("gas", err)
	}
	if raw.EffectiveGasPrice != nil {
		r.EffectiveGasPrice = new(big.Int).Set(&raw.EffectiveGasPrice.Int)
	}
	for _, rl := range raw.Logs {
		l, err := rl.decode()
		if err != nil {
			return bad("log", err)
		}
		r.Logs = append(r.Logs, l)
	}
	return r, nil
}

// Raw sends an arbitrary method; tests use it for anvil_* and evm_* calls.
func (c *Client) Raw(ctx context.Context, out any, method string, params ...any) error {
	return c.call(ctx, out, method, params...)
}

// HexUint and HexBytes format quantities and data for Raw calls.
func HexUint(n uint64) string { return hexUint(n) }

// HexBytes encodes b as 0x-prefixed lowercase hex.
func HexBytes(b []byte) string { return hexBytes(b) }
