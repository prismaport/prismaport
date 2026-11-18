package evmrpc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/evm"
)

type rpcReq struct {
	ID     uint64            `json:"id"`
	Method string            `json:"method"`
	Params []json.RawMessage `json:"params"`
}

// fake answers each method with a canned result (or error object, or HTTP status).
type fake struct {
	results map[string]string
	errors  map[string]string
	status  int
	last    rpcReq
	badID   bool
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if f.status != 0 {
		w.WriteHeader(f.status)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var req rpcReq
	_ = json.Unmarshal(body, &req)
	f.last = req
	id := req.ID
	if f.badID {
		id++
	}
	idJSON, _ := json.Marshal(id)
	if e, ok := f.errors[req.Method]; ok {
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + string(idJSON) + `,"error":` + e + `}`))
		return
	}
	res, ok := f.results[req.Method]
	if !ok {
		res = "null"
	}
	_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + string(idJSON) + `,"result":` + res + `}`))
}

func setup(t *testing.T, f *fake) *Client {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return New("p1", srv.URL+"/secret-key-123", time.Second)
}

const (
	hash1 = "0x1111111111111111111111111111111111111111111111111111111111111111"
	hash2 = "0x2222222222222222222222222222222222222222222222222222222222222222"
)

func TestHeaderAndQuantities(t *testing.T) {
	f := &fake{results: map[string]string{
		"eth_chainId":          `"0x2105"`,
		"eth_getBlockByNumber": `{"number":"0x10","hash":"` + hash1 + `","parentHash":"` + hash2 + `","timestamp":"0x6b49d200","baseFeePerGas":"0x3b9aca00"}`,
	}}
	c := setup(t, f)
	ctx := t.Context()
	id, err := c.ChainID(ctx)
	if err != nil || id != 8453 {
		t.Fatalf("chain id %d %v", id, err)
	}
	h, err := c.HeaderByTag(ctx, Finalized)
	if err != nil || h.Number != 16 || h.Hash.Hex() != hash1 || h.ParentHash.Hex() != hash2 || h.BaseFee.Int64() != 1e9 || h.Timestamp != 0x6b49d200 {
		t.Fatalf("header %+v %v", h, err)
	}
	if string(f.last.Params[0]) != `"finalized"` || string(f.last.Params[1]) != "false" {
		t.Fatalf("params %s", f.last.Params)
	}
	if _, err := c.HeaderByTag(ctx, Number(255)); err != nil || string(f.last.Params[0]) != `"0xff"` {
		t.Fatalf("number tag %s %v", f.last.Params[0], err)
	}
	f.results["eth_getBlockByNumber"] = "null"
	if _, err := c.HeaderByTag(ctx, Latest); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing block: %v", err)
	}
	f.results["eth_chainId"] = `"0x"`
	if _, err := c.ChainID(ctx); !errors.Is(err, ErrTransport) {
		t.Fatalf("empty quantity: %v", err)
	}
	f.results["eth_chainId"] = `"0x10000000000000000"`
	if _, err := c.ChainID(ctx); err == nil {
		t.Fatal("overflowing quantity accepted")
	}
}

func TestLogs(t *testing.T) {
	transfer := evm.Topic("Transfer(address,address,uint256)")
	to := "0x0000000000000000000000005aaeb6053f3e94c9b9a09f33669435e7ef1beaed"
	f := &fake{results: map[string]string{"eth_getLogs": `[{"address":"0x833589FCD6EDB6E08F4C7C32D4F71B54BDA02913","topics":["` + transfer.Hex() + `","` + to + `","` + to + `"],
		"data":"0x00000000000000000000000000000000000000000000000000000000017d7840","blockNumber":"0x5","blockHash":"` + hash1 + `",
		"transactionHash":"` + hash2 + `","logIndex":"0x3","removed":false}]`}}
	c := setup(t, f)
	token, _ := evm.ParseAddress("0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913")
	toW, _ := evm.ParseWord(to)
	logs, err := c.Logs(t.Context(), Filter{FromBlock: 5, ToBlock: 6, Addresses: []evm.Address{token},
		Topics: [][]evm.Word{{transfer}, nil, {toW}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 || logs[0].Address != token || logs[0].LogIndex != 3 || logs[0].BlockNumber != 5 || len(logs[0].Topics) != 3 {
		t.Fatalf("logs %+v", logs)
	}
	if w, _ := evm.Words(logs[0].Data); w[0].Big().Int64() != 25_000_000 {
		t.Fatal("data")
	}
	var q map[string]any
	_ = json.Unmarshal(f.last.Params[0], &q)
	if q["fromBlock"] != "0x5" || q["toBlock"] != "0x6" {
		t.Fatalf("range %v", q)
	}
	topics := q["topics"].([]any)
	if topics[1] != nil || topics[2].([]any)[0] != to {
		t.Fatalf("topics %v", topics)
	}
	if _, err := c.Logs(t.Context(), Filter{FromBlock: 6, ToBlock: 9}); !errors.Is(err, ErrTransport) {
		t.Fatalf("log outside the requested range accepted: %v", err)
	}
}

func TestReceiptCallAndSend(t *testing.T) {
	f := &fake{results: map[string]string{
		"eth_getTransactionReceipt": `{"transactionHash":"` + hash2 + `","status":"0x1","blockNumber":"0x7","blockHash":"` + hash1 + `","gasUsed":"0x5208","effectiveGasPrice":"0x10","logs":[]}`,
		"eth_call":                  `"0x0000000000000000000000000000000000000000000000000000000000000006"`,
		"eth_estimateGas":           `"0x249f0"`,
		"eth_getTransactionCount":   `"0x7"`,
		"eth_getBalance":            `"0xde0b6b3a7640000"`,
		"eth_getCode":               `"0x6000"`,
		"eth_maxPriorityFeePerGas":  `"0x3b9aca00"`,
		"eth_sendRawTransaction":    `"` + hash2 + `"`,
	}}
	c := setup(t, f)
	ctx := t.Context()
	h, _ := evm.ParseWord(hash2)
	r, err := c.Receipt(ctx, h)
	if err != nil || r.Status != 1 || r.BlockNumber != 7 || r.GasUsed != 21000 || r.EffectiveGasPrice.Int64() != 16 {
		t.Fatalf("receipt %+v %v", r, err)
	}
	delete(f.results, "eth_getTransactionReceipt")
	if _, err := c.Receipt(ctx, h); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pending receipt: %v", err)
	}
	var to evm.Address
	from := evm.Address{1}
	out, err := c.Call(ctx, CallMsg{From: &from, To: to, Data: []byte{1, 2}}, Latest)
	if err != nil || len(out) != 32 || out[31] != 6 {
		t.Fatalf("call %x %v", out, err)
	}
	var msg map[string]string
	_ = json.Unmarshal(f.last.Params[0], &msg)
	if msg["data"] != "0x0102" || msg["from"] != from.Lower() {
		t.Fatalf("call msg %v", msg)
	}
	if g, err := c.EstimateGas(ctx, CallMsg{To: to}); err != nil || g != 150000 {
		t.Fatalf("gas %d %v", g, err)
	}
	if n, err := c.Nonce(ctx, to, Pending); err != nil || n != 7 {
		t.Fatalf("nonce %d %v", n, err)
	}
	if b, err := c.Balance(ctx, to, Latest); err != nil || b.String() != "1000000000000000000" {
		t.Fatalf("balance %v %v", b, err)
	}
	if code, err := c.Code(ctx, to, Latest); err != nil || len(code) != 2 {
		t.Fatalf("code %x %v", code, err)
	}
	if p, err := c.MaxPriorityFee(ctx); err != nil || p.Int64() != 1e9 {
		t.Fatalf("tip %v %v", p, err)
	}
	if got, err := c.SendRawTransaction(ctx, []byte{2, 1}); err != nil || got != h {
		t.Fatalf("send %v %v", got, err)
	}
	var raw string
	if err := c.Raw(ctx, &raw, "eth_getTransactionCount", "0x0", "latest"); err != nil || raw != "0x7" {
		t.Fatalf("raw %q %v", raw, err)
	}
	if HexUint(255) != "0xff" || HexBytes([]byte{1}) != "0x01" {
		t.Fatal("hex helpers")
	}
}

func TestErrorClassification(t *testing.T) {
	f := &fake{errors: map[string]string{
		"eth_call":               `{"code":3,"message":"execution reverted: custom error","data":"0x3a1b2c3d"}`,
		"eth_estimateGas":        `{"code":-32005,"message":"rate limited"}`,
		"eth_sendRawTransaction": `{"code":-32000,"message":"nonce too low"}`,
	}}
	c := setup(t, f)
	ctx := t.Context()
	_, err := c.Call(ctx, CallMsg{}, Latest)
	var re *RPCError
	if !errors.As(err, &re) || !IsRevert(err) || errors.Is(err, ErrTransport) {
		t.Fatalf("revert misclassified: %v", err)
	}
	if data, ok := re.RevertData(); !ok || len(data) != 4 {
		t.Fatalf("revert data %x", data)
	}
	if _, err := c.EstimateGas(ctx, CallMsg{}); !errors.Is(err, ErrTransport) {
		t.Fatalf("rate limit must be a transport failure: %v", err)
	}
	if _, err := c.SendRawTransaction(ctx, nil); errors.Is(err, ErrTransport) || IsRevert(err) {
		t.Fatalf("nonce too low misclassified: %v", err)
	}
	f.status = http.StatusTooManyRequests
	if _, err := c.ChainID(ctx); !errors.Is(err, ErrTransport) {
		t.Fatalf("HTTP 429: %v", err)
	}
	f.status = 0
	f.badID = true
	if _, err := c.ChainID(ctx); !errors.Is(err, ErrTransport) {
		t.Fatalf("mismatched id: %v", err)
	}

	dead := New("dead", "http://127.0.0.1:1/key-in-url", 200*time.Millisecond)
	_, err = dead.ChainID(ctx)
	if !errors.Is(err, ErrTransport) || strings.Contains(err.Error(), "key-in-url") {
		t.Fatalf("dial error must be transport and hide the url: %v", err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := dead.ChainID(cctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
}
