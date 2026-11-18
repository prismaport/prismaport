package solrpc

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fake answers each method with a fixed JSON-RPC body (the id is rewritten to match).
func fake(t *testing.T, answers map[string]string, status int) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID     uint64 `json:"id"`
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &req)
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		a, ok := answers[req.Method]
		if !ok {
			t.Errorf("unexpected method %s", req.Method)
		}
		var m map[string]json.RawMessage
		_ = json.Unmarshal([]byte(a), &m)
		m["id"], _ = json.Marshal(req.ID)
		m["jsonrpc"] = json.RawMessage(`"2.0"`)
		_ = json.NewEncoder(w).Encode(m)
	}))
	t.Cleanup(srv.Close)
	return New("fake", srv.URL+"/secret-key", 0)
}

func TestErrorMapping(t *testing.T) {
	ctx := t.Context()
	c := fake(t, map[string]string{
		"getBlock":                `{"error":{"code":-32007,"message":"Slot 5 was skipped, or missing due to ledger jump to recent snapshot"}}`,
		"getTransaction":          `{"result":null}`,
		"getSlot":                 `{"error":{"code":-32005,"message":"Node is behind by 100 slots"}}`,
		"getAccountInfo":          `{"error":{"code":-32016,"message":"Minimum context slot has not been reached"}}`,
		"getSignaturesForAddress": `{"result":[]}`,
		"sendTransaction":         `{"error":{"code":-32002,"message":"Transaction simulation failed: Error processing Instruction 1: custom program error: 0x1","data":{"err":{"InstructionError":[1,{"Custom":1}]},"logs":["Program log: boom"]}}}`,
		"getGenesisHash":          `{"error":{"code":-32601,"message":"Method not found"}}`,
	}, 0)
	if _, err := c.Block(ctx, 5, Finalized); !errors.Is(err, ErrSkipped) {
		t.Fatalf("skipped slot: %v", err)
	}
	if _, err := c.Transaction(ctx, "x", Confirmed); !errors.Is(err, ErrNotFound) {
		t.Fatalf("null transaction: %v", err)
	}
	if _, err := c.Slot(ctx, Confirmed); !errors.Is(err, ErrTransport) {
		t.Fatalf("node behind: %v", err)
	}
	if _, err := c.Account(ctx, "x", Finalized, 10); !errors.Is(err, ErrBehind) || !errors.Is(err, ErrNotFound) {
		t.Fatalf("min context slot: %v", err)
	}
	if s, err := c.Signatures(ctx, "x", SignaturesOpts{}); err != nil || len(s) != 0 {
		t.Fatalf("empty signatures: %v %v", s, err)
	}
	_, err := c.SendTransaction(ctx, []byte{1}, false)
	var se *SendError
	if !errors.As(err, &se) || len(se.Logs) != 1 || se.AlreadyProcessed() || !strings.Contains(string(se.Err), "InstructionError") {
		t.Fatalf("preflight failure: %#v", err)
	}
	var re *RPCError
	if _, err := c.GenesisHash(ctx); !errors.As(err, &re) || re.Code != -32601 {
		t.Fatalf("rpc error: %v", err)
	}
	down := fake(t, nil, http.StatusTooManyRequests)
	_, err = down.Slot(ctx, Confirmed)
	if !errors.Is(err, ErrTransport) || strings.Contains(err.Error(), "secret-key") {
		t.Fatalf("429: %v", err)
	}
}

func TestAccountDecoding(t *testing.T) {
	c := fake(t, map[string]string{
		"getAccountInfo":      `{"result":{"context":{"slot":42},"value":{"data":["AQID","base64"],"executable":false,"lamports":2039280,"owner":"TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA","rentEpoch":18446744073709551615,"space":3}}}`,
		"getMultipleAccounts": `{"result":{"context":{"slot":43},"value":[null,{"data":["","base64"],"executable":false,"lamports":1,"owner":"11111111111111111111111111111111","rentEpoch":0,"space":0}]}}`,
	}, 0)
	a, err := c.Account(t.Context(), "x", Confirmed, 0)
	if err != nil || !a.Exists || a.ContextSlot != 42 || a.Lamports != 2039280 || string(a.Data) != "\x01\x02\x03" {
		t.Fatalf("account %+v %v", a, err)
	}
	as, err := c.MultipleAccounts(t.Context(), []string{"a", "b"}, Confirmed, 0)
	if err != nil || len(as) != 2 || as[0].Exists || !as[1].Exists || as[1].ContextSlot != 43 {
		t.Fatalf("accounts %+v %v", as, err)
	}
}

func TestParseRecordedTransaction(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "solattr", "testdata", "flush.json"))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := ParseTransactionJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	if tx.Failed() || len(tx.AccountKeys) != 13 || len(tx.Instructions) != 6 || tx.ComputeUnits == 0 || tx.Fee == 0 {
		t.Fatalf("parsed %+v", tx)
	}
	ms := tx.Movements()
	var transfers int
	for _, m := range ms {
		if m.Kind == "transferChecked" && m.Instruction == 3 && m.From == tx.AccountKeys[2] {
			transfers++
		}
	}
	if transfers != 2 {
		t.Fatalf("movements %+v", ms)
	}
	again, _ := ParseTransactionJSON(raw)
	if !tx.Equal(again) {
		t.Fatal("same answer not equal")
	}
	again.Logs = nil
	if !tx.Equal(again) {
		t.Fatal("logs must not matter")
	}
	tampered, _ := ParseTransactionJSON([]byte(strings.Replace(string(raw), `"amount": "24750000"`, `"amount": "24750001"`, 1)))
	if tx.Equal(tampered) {
		t.Fatal("different token balances compare equal")
	}
	reordered := strings.Replace(string(raw), `"decimals": 6, "uiAmount"`, `"uiAmount"`, 1)
	if _, err := ParseTransactionJSON([]byte(reordered)); err != nil {
		t.Fatal(err)
	}
}

// A signature list asked at a slot the node has not reached is ErrBehind, not an empty list.
func TestSignaturesBehindTheMinContextSlot(t *testing.T) {
	c := fake(t, map[string]string{
		"getSignaturesForAddress": `{"error":{"code":-32016,"message":"Minimum context slot has not been reached"}}`,
	}, 0)
	if s, err := c.Signatures(t.Context(), "x", SignaturesOpts{MinContextSlot: 9}); !errors.Is(err, ErrBehind) || s != nil {
		t.Fatalf("behind: %v %v", s, err)
	}
}
