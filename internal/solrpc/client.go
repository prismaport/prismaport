// Package solrpc is a small Solana JSON-RPC client covering what the detector, flusher and the
// connected-wallet checkout need. It speaks plain HTTP JSON-RPC 2.0; slots and lamports are
// decoded as integers and token amounts as decimal strings (never floats).
package solrpc

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/prismaport/prismaport/internal/rpcstat"
)

const maxResponseBytes = 64 << 20

var (
	// ErrTransport means the provider could not answer (network, HTTP status, rate limit,
	// node behind, malformed response). The caller should fail over.
	ErrTransport = errors.New("solrpc: provider unavailable")
	// ErrNotFound means the provider has no such transaction, block or account yet.
	ErrNotFound = errors.New("solrpc: not found")
	// ErrSkipped means the slot was skipped: no block was produced there on this fork.
	ErrSkipped = errors.New("solrpc: slot skipped")
	// ErrBehind means the provider has not reached the minContextSlot asked for ("minimum
	// context slot has not been reached", -32016). It is also ErrNotFound (the data at that slot
	// is not there yet), so a two-provider comparison treats it as lag; a single-provider reader
	// asks another provider instead of returning it.
	ErrBehind = fmt.Errorf("solrpc: provider behind the requested slot: %w", ErrNotFound)
)

// RPCError is a JSON-RPC error object returned by a healthy provider.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("solrpc: rpc error %d: %s", e.Code, e.Message) }

// JSON-RPC error codes of the Solana RPC server.
const (
	codeBlockCleanedUp            = -32001
	codeSendTxPreflightFailure    = -32002
	codeBlockNotAvailable         = -32004
	codeNodeUnhealthy             = -32005
	codeSlotSkipped               = -32007
	codeLongTermStorageSlotSkip   = -32009
	codeMinContextSlotNotReached  = -32016
	codeTransactionHistoryMissing = -32011
)

// Commitment is the Solana commitment level a query is answered at.
type Commitment string

// The commitment levels, from least to most final.
const (
	Processed Commitment = "processed"
	Confirmed Commitment = "confirmed"
	Finalized Commitment = "finalized"
)

// Client is a JSON-RPC client for one Solana provider endpoint.
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

// Name returns the provider name used in logs and status.
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

func redactURL(err error, url string) string {
	return strings.ReplaceAll(err.Error(), url, "<provider url>")
}

// Raw calls any method; tests and the validator harness use it for methods without a typed
// wrapper.
func (c *Client) Raw(ctx context.Context, out any, method string, params ...any) error {
	return c.call(ctx, out, method, params...)
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
		switch r.Error.Code {
		case codeNodeUnhealthy, 429, -32603:
			return fmt.Errorf("%w: %s %s: %v", ErrTransport, c.name, method, r.Error)
		case codeSlotSkipped, codeLongTermStorageSlotSkip:
			return fmt.Errorf("%w: %s", ErrSkipped, r.Error.Message)
		case codeMinContextSlotNotReached:
			return fmt.Errorf("%w: %s %s: %s", ErrBehind, c.name, method, r.Error.Message)
		case codeBlockNotAvailable, codeBlockCleanedUp, codeTransactionHistoryMissing:
			return fmt.Errorf("%w: %s", ErrNotFound, r.Error.Message)
		}
		return r.Error
	}
	if out == nil {
		return nil
	}
	if string(r.Result) == "null" {
		return ErrNotFound
	}
	if err := json.Unmarshal(r.Result, out); err != nil {
		return fmt.Errorf("%w: %s %s: decode result: %v", ErrTransport, c.name, method, err)
	}
	return nil
}

type commitmentOpt struct {
	Commitment Commitment `json:"commitment,omitempty"`
}

// GenesisHash returns the cluster genesis hash, which identifies the network.
func (c *Client) GenesisHash(ctx context.Context) (string, error) {
	var h string
	err := c.call(ctx, &h, "getGenesisHash")
	return h, err
}

// Slot returns the current slot at the given commitment.
func (c *Client) Slot(ctx context.Context, cm Commitment) (uint64, error) {
	var n uint64
	err := c.call(ctx, &n, "getSlot", commitmentOpt{cm})
	return n, err
}

// BlockHeight returns the current block height at the given commitment.
func (c *Client) BlockHeight(ctx context.Context, cm Commitment) (uint64, error) {
	var n uint64
	err := c.call(ctx, &n, "getBlockHeight", commitmentOpt{cm})
	return n, err
}

// Blockhash is the decoded result of getLatestBlockhash.
type Blockhash struct {
	Hash                 string
	LastValidBlockHeight uint64
	Slot                 uint64
}

// LatestBlockhash returns a recent blockhash, the last block height it is valid for and the slot of the answer.
func (c *Client) LatestBlockhash(ctx context.Context, cm Commitment) (Blockhash, error) {
	var r struct {
		Context struct {
			Slot uint64 `json:"slot"`
		} `json:"context"`
		Value struct {
			Blockhash            string `json:"blockhash"`
			LastValidBlockHeight uint64 `json:"lastValidBlockHeight"`
		} `json:"value"`
	}
	if err := c.call(ctx, &r, "getLatestBlockhash", commitmentOpt{cm}); err != nil {
		return Blockhash{}, err
	}
	return Blockhash{Hash: r.Value.Blockhash, LastValidBlockHeight: r.Value.LastValidBlockHeight, Slot: r.Context.Slot}, nil
}

// Block is the decoded result of getBlock.
type Block struct {
	Slot        uint64
	Blockhash   string `json:"blockhash"`
	ParentSlot  uint64 `json:"parentSlot"`
	BlockHeight uint64 `json:"blockHeight"`
}

// Block returns the block header fields at slot (ErrSkipped when no block was produced there).
func (c *Client) Block(ctx context.Context, slot uint64, cm Commitment) (Block, error) {
	var b Block
	err := c.call(ctx, &b, "getBlock", slot, map[string]any{"encoding": "json", "transactionDetails": "none", "rewards": false,
		"commitment": cm, "maxSupportedTransactionVersion": 0})
	b.Slot = slot
	return b, err
}

// SignatureInfo is one entry of the getSignaturesForAddress result.
type SignatureInfo struct {
	Signature          string          `json:"signature"`
	Slot               uint64          `json:"slot"`
	Err                json.RawMessage `json:"err"`
	ConfirmationStatus string          `json:"confirmationStatus"`
}

// Failed reports whether the transaction failed on chain (it moved nothing but its fee).
func (s SignatureInfo) Failed() bool { return len(s.Err) > 0 && string(s.Err) != "null" }

// SignaturesOpts selects the page of signatures to list for an address.
type SignaturesOpts struct {
	Before, Until string
	Limit         int
	Commitment    Commitment
	// MinContextSlot makes a provider that has not reached the slot answer ErrBehind instead
	// of a list that may miss transactions up to it (0: no bound).
	MinContextSlot uint64
}

// Signatures lists transactions that reference addr, newest first.
func (c *Client) Signatures(ctx context.Context, addr string, o SignaturesOpts) ([]SignatureInfo, error) {
	cfg := map[string]any{"commitment": o.Commitment}
	if o.Limit > 0 {
		cfg["limit"] = o.Limit
	}
	if o.Before != "" {
		cfg["before"] = o.Before
	}
	if o.Until != "" {
		cfg["until"] = o.Until
	}
	if o.MinContextSlot > 0 {
		cfg["minContextSlot"] = o.MinContextSlot
	}
	var out []SignatureInfo
	err := c.call(ctx, &out, "getSignaturesForAddress", addr, cfg)
	if errors.Is(err, ErrNotFound) && !errors.Is(err, ErrBehind) {
		return nil, nil
	}
	return out, err
}

// Transaction fetches a transaction in jsonParsed encoding.
func (c *Client) Transaction(ctx context.Context, sig string, cm Commitment) (*Tx, error) {
	var raw json.RawMessage
	if err := c.call(ctx, &raw, "getTransaction", sig, map[string]any{"encoding": "jsonParsed", "commitment": cm,
		"maxSupportedTransactionVersion": 0}); err != nil {
		return nil, err
	}
	tx, err := parseTx(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %s getTransaction: %v", ErrTransport, c.name, err)
	}
	return tx, nil
}

// Account is an account's state as of ContextSlot. Exists is false for an empty address.
type Account struct {
	ContextSlot uint64
	Exists      bool
	Owner       string
	Lamports    uint64
	Executable  bool
	Data        []byte
}

type accountValue struct {
	Data       []string `json:"data"`
	Executable bool     `json:"executable"`
	Lamports   uint64   `json:"lamports"`
	Owner      string   `json:"owner"`
}

func (v *accountValue) decode(slot uint64) (Account, error) {
	a := Account{ContextSlot: slot}
	if v == nil {
		return a, nil
	}
	if len(v.Data) != 2 || v.Data[1] != "base64" {
		return a, fmt.Errorf("unexpected account data encoding")
	}
	data, err := base64.StdEncoding.DecodeString(v.Data[0])
	if err != nil {
		return a, err
	}
	a.Exists, a.Owner, a.Lamports, a.Executable, a.Data = true, v.Owner, v.Lamports, v.Executable, data
	return a, nil
}

func accountCfg(cm Commitment, minContextSlot uint64) map[string]any {
	cfg := map[string]any{"encoding": "base64", "commitment": cm}
	if minContextSlot > 0 {
		cfg["minContextSlot"] = minContextSlot
	}
	return cfg
}

// Account returns one account at the given commitment, answered no earlier than minContextSlot.
func (c *Client) Account(ctx context.Context, addr string, cm Commitment, minContextSlot uint64) (Account, error) {
	var r struct {
		Context struct {
			Slot uint64 `json:"slot"`
		} `json:"context"`
		Value *accountValue `json:"value"`
	}
	if err := c.call(ctx, &r, "getAccountInfo", addr, accountCfg(cm, minContextSlot)); err != nil {
		return Account{}, err
	}
	a, err := r.Value.decode(r.Context.Slot)
	if err != nil {
		return a, fmt.Errorf("%w: %s getAccountInfo: %v", ErrTransport, c.name, err)
	}
	return a, nil
}

// MultipleAccounts returns the given accounts in order at the given commitment, answered no earlier than minContextSlot.
func (c *Client) MultipleAccounts(ctx context.Context, addrs []string, cm Commitment, minContextSlot uint64) ([]Account, error) {
	var r struct {
		Context struct {
			Slot uint64 `json:"slot"`
		} `json:"context"`
		Value []*accountValue `json:"value"`
	}
	if err := c.call(ctx, &r, "getMultipleAccounts", addrs, accountCfg(cm, minContextSlot)); err != nil {
		return nil, err
	}
	if len(r.Value) != len(addrs) {
		return nil, fmt.Errorf("%w: %s getMultipleAccounts: %d values for %d keys", ErrTransport, c.name, len(r.Value), len(addrs))
	}
	out := make([]Account, len(addrs))
	for i, v := range r.Value {
		a, err := v.decode(r.Context.Slot)
		if err != nil {
			return nil, fmt.Errorf("%w: %s getMultipleAccounts: %v", ErrTransport, c.name, err)
		}
		out[i] = a
	}
	return out, nil
}

// Balance returns the lamport balance of addr.
func (c *Client) Balance(ctx context.Context, addr string, cm Commitment) (uint64, error) {
	var r struct {
		Value uint64 `json:"value"`
	}
	err := c.call(ctx, &r, "getBalance", addr, commitmentOpt{cm})
	return r.Value, err
}

// MinimumBalanceForRentExemption returns the lamports an account of size bytes needs to be rent exempt.
func (c *Client) MinimumBalanceForRentExemption(ctx context.Context, size int) (uint64, error) {
	var n uint64
	err := c.call(ctx, &n, "getMinimumBalanceForRentExemption", size)
	return n, err
}

// SendError is a rejected sendTransaction: the preflight simulation failed or the transaction
// was already processed.
type SendError struct {
	Message string
	Err     json.RawMessage
	Logs    []string
}

func (e *SendError) Error() string { return "solrpc: transaction rejected: " + e.Message }

// AlreadyProcessed reports the node's "already processed" answer for a resent transaction.
func (e *SendError) AlreadyProcessed() bool {
	return strings.Contains(e.Message, "already been processed") || strings.Contains(string(e.Err), "AlreadyProcessed")
}

// SendTransaction submits a signed transaction and returns its signature.
func (c *Client) SendTransaction(ctx context.Context, raw []byte, skipPreflight bool) (string, error) {
	var sig string
	err := c.call(ctx, &sig, "sendTransaction", base64.StdEncoding.EncodeToString(raw), map[string]any{"encoding": "base64",
		"skipPreflight": skipPreflight, "preflightCommitment": Confirmed, "maxRetries": 0})
	var re *RPCError
	if errors.As(err, &re) && re.Code == codeSendTxPreflightFailure {
		se := &SendError{Message: re.Message}
		var d struct {
			Err  json.RawMessage `json:"err"`
			Logs []string        `json:"logs"`
		}
		if json.Unmarshal(re.Data, &d) == nil {
			se.Err, se.Logs = d.Err, d.Logs
		}
		return "", se
	}
	if errors.As(err, &re) && strings.Contains(re.Message, "already been processed") {
		return "", &SendError{Message: re.Message}
	}
	return sig, err
}

// Simulation is the decoded result of simulateTransaction.
type Simulation struct {
	Err           json.RawMessage
	Logs          []string
	UnitsConsumed uint64
}

// Failed reports whether the simulated transaction returned an error.
func (s Simulation) Failed() bool { return len(s.Err) > 0 && string(s.Err) != "null" }

// Simulate runs the transaction against the provider's bank. sigVerify=false lets a partially
// signed transaction be checked before its last signer signs.
func (c *Client) Simulate(ctx context.Context, raw []byte, sigVerify bool) (Simulation, error) {
	var r struct {
		Value struct {
			Err           json.RawMessage `json:"err"`
			Logs          []string        `json:"logs"`
			UnitsConsumed uint64          `json:"unitsConsumed"`
		} `json:"value"`
	}
	err := c.call(ctx, &r, "simulateTransaction", base64.StdEncoding.EncodeToString(raw), map[string]any{"encoding": "base64",
		"sigVerify": sigVerify, "commitment": Confirmed, "replaceRecentBlockhash": false})
	return Simulation{Err: r.Value.Err, Logs: r.Value.Logs, UnitsConsumed: r.Value.UnitsConsumed}, err
}

// SignatureStatus is one entry of the getSignatureStatuses result.
type SignatureStatus struct {
	Slot               uint64          `json:"slot"`
	Err                json.RawMessage `json:"err"`
	ConfirmationStatus string          `json:"confirmationStatus"`
}

// Failed reports whether the transaction ended with an error.
func (s SignatureStatus) Failed() bool { return len(s.Err) > 0 && string(s.Err) != "null" }

// SignatureStatuses returns one status per signature, nil where the node does not know it.
func (c *Client) SignatureStatuses(ctx context.Context, sigs []string) ([]*SignatureStatus, error) {
	var r struct {
		Value []*SignatureStatus `json:"value"`
	}
	if err := c.call(ctx, &r, "getSignatureStatuses", sigs, map[string]any{"searchTransactionHistory": true}); err != nil {
		return nil, err
	}
	if len(r.Value) != len(sigs) {
		return nil, fmt.Errorf("%w: %s getSignatureStatuses: %d values for %d signatures", ErrTransport, c.name, len(r.Value), len(sigs))
	}
	return r.Value, nil
}

// IsBlockhashValid reports whether a transaction with this blockhash can still land.
func (c *Client) IsBlockhashValid(ctx context.Context, hash string) (bool, error) {
	var r struct {
		Value bool `json:"value"`
	}
	err := c.call(ctx, &r, "isBlockhashValid", hash, commitmentOpt{Confirmed})
	return r.Value, err
}
