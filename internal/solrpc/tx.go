package solrpc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
)

// TokenBalance is a pre- or post-transaction token balance of one account of the message.
type TokenBalance struct {
	AccountIndex int
	Mint         string
	Owner        string
	ProgramID    string
	Amount       string
	Decimals     uint8
}

// Instruction is a top-level or inner instruction. Instructions of programs the node can parse
// (System, SPL Token, ATA) carry Program, Type and the canonical JSON of their info; others
// carry their raw accounts and base58 data.
type Instruction struct {
	ProgramID string
	Program   string
	Type      string
	Info      string
	Accounts  []string
	Data      string
}

// Tx is the part of a jsonParsed transaction the detector and flusher use. Two providers agree
// on a transaction when their Tx values are equal (Equal).
type Tx struct {
	Signatures      []string
	Slot            uint64
	Err             string // canonical JSON of meta.err; empty on success
	Fee             uint64
	ComputeUnits    uint64
	AccountKeys     []string
	RecentBlockhash string
	Instructions    []Instruction
	Inner           map[int][]Instruction
	PreToken        []TokenBalance
	PostToken       []TokenBalance
	// Logs are excluded from Equal: providers may truncate them differently, and nothing is
	// credited from them.
	Logs []string
}

// Failed reports whether the transaction ended with an error.
func (t *Tx) Failed() bool { return t.Err != "" }

// Equal compares everything except logs.
func (t *Tx) Equal(o *Tx) bool {
	if t == nil || o == nil {
		return t == o
	}
	a, b := *t, *o
	a.Logs, b.Logs = nil, nil
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return bytes.Equal(ja, jb)
}

// canonical re-encodes JSON with sorted keys and exact numbers.
func canonical(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return "", err
	}
	out, err := json.Marshal(v)
	return string(out), err
}

type wireIx struct {
	ProgramID string          `json:"programId"`
	Program   string          `json:"program"`
	Parsed    json.RawMessage `json:"parsed"`
	Accounts  []string        `json:"accounts"`
	Data      string          `json:"data"`
}

func (w wireIx) decode() (Instruction, error) {
	ix := Instruction{ProgramID: w.ProgramID, Program: w.Program, Accounts: w.Accounts, Data: w.Data}
	if len(w.Parsed) == 0 {
		return ix, nil
	}
	var p struct {
		Type string          `json:"type"`
		Info json.RawMessage `json:"info"`
	}
	if err := json.Unmarshal(w.Parsed, &p); err != nil {
		// Some programs (Memo) render parsed as a plain string.
		s, err := canonical(w.Parsed)
		ix.Info = s
		return ix, err
	}
	info, err := canonical(p.Info)
	ix.Type, ix.Info = p.Type, info
	return ix, err
}

type wireTokenBalance struct {
	AccountIndex  int    `json:"accountIndex"`
	Mint          string `json:"mint"`
	Owner         string `json:"owner"`
	ProgramID     string `json:"programId"`
	UITokenAmount struct {
		Amount   string `json:"amount"`
		Decimals uint8  `json:"decimals"`
	} `json:"uiTokenAmount"`
}

type wireTx struct {
	Slot uint64 `json:"slot"`
	Meta *struct {
		Err               json.RawMessage    `json:"err"`
		Fee               uint64             `json:"fee"`
		ComputeUnits      *uint64            `json:"computeUnitsConsumed"`
		PreTokenBalances  []wireTokenBalance `json:"preTokenBalances"`
		PostTokenBalances []wireTokenBalance `json:"postTokenBalances"`
		InnerInstructions []struct {
			Index        int      `json:"index"`
			Instructions []wireIx `json:"instructions"`
		} `json:"innerInstructions"`
		LogMessages []string `json:"logMessages"`
	} `json:"meta"`
	Transaction struct {
		Signatures []string `json:"signatures"`
		Message    struct {
			AccountKeys []struct {
				Pubkey string `json:"pubkey"`
			} `json:"accountKeys"`
			RecentBlockhash string   `json:"recentBlockhash"`
			Instructions    []wireIx `json:"instructions"`
		} `json:"message"`
	} `json:"transaction"`
}

func balances(in []wireTokenBalance) []TokenBalance {
	out := make([]TokenBalance, 0, len(in))
	for _, b := range in {
		out = append(out, TokenBalance{AccountIndex: b.AccountIndex, Mint: b.Mint, Owner: b.Owner, ProgramID: b.ProgramID,
			Amount: b.UITokenAmount.Amount, Decimals: b.UITokenAmount.Decimals})
	}
	slices.SortFunc(out, func(a, b TokenBalance) int { return a.AccountIndex - b.AccountIndex })
	return out
}

func parseTx(raw json.RawMessage) (*Tx, error) {
	var w wireTx
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, err
	}
	if w.Meta == nil {
		return nil, fmt.Errorf("transaction without meta")
	}
	t := &Tx{Signatures: w.Transaction.Signatures, Slot: w.Slot, Fee: w.Meta.Fee, RecentBlockhash: w.Transaction.Message.RecentBlockhash,
		Inner: map[int][]Instruction{}, PreToken: balances(w.Meta.PreTokenBalances), PostToken: balances(w.Meta.PostTokenBalances),
		Logs: w.Meta.LogMessages}
	if w.Meta.ComputeUnits != nil {
		t.ComputeUnits = *w.Meta.ComputeUnits
	}
	var err error
	if t.Err, err = canonical(w.Meta.Err); err != nil {
		return nil, err
	}
	for _, k := range w.Transaction.Message.AccountKeys {
		t.AccountKeys = append(t.AccountKeys, k.Pubkey)
	}
	for _, wi := range w.Transaction.Message.Instructions {
		ix, err := wi.decode()
		if err != nil {
			return nil, err
		}
		t.Instructions = append(t.Instructions, ix)
	}
	for _, g := range w.Meta.InnerInstructions {
		for _, wi := range g.Instructions {
			ix, err := wi.decode()
			if err != nil {
				return nil, err
			}
			t.Inner[g.Index] = append(t.Inner[g.Index], ix)
		}
	}
	return t, nil
}

// AccountIndex returns the message index of key, or -1.
func (t *Tx) AccountIndex(key string) int {
	return slices.Index(t.AccountKeys, key)
}

// TokenBalanceOf returns the pre and post balance entries of the account at index i.
func (t *Tx) TokenBalanceOf(i int) (pre, post *TokenBalance) {
	for k := range t.PreToken {
		if t.PreToken[k].AccountIndex == i {
			pre = &t.PreToken[k]
		}
	}
	for k := range t.PostToken {
		if t.PostToken[k].AccountIndex == i {
			post = &t.PostToken[k]
		}
	}
	return pre, post
}

// Movement is one token balance change an SPL Token instruction made: a transfer (From -> To),
// a mint (To only) or a burn (From only). Amount is a base-unit integer string.
type Movement struct {
	// Instruction is the index of the top-level instruction it belongs to (directly or as an
	// inner instruction).
	Instruction int
	ProgramID   string
	Kind        string
	From, To    string
	Mint        string
	Authority   string
	Amount      string
}

type tokenInfo struct {
	Source        string `json:"source"`
	Destination   string `json:"destination"`
	Account       string `json:"account"`
	Mint          string `json:"mint"`
	Authority     string `json:"authority"`
	MultisigAuth  string `json:"multisigAuthority"`
	MintAuthority string `json:"mintAuthority"`
	Amount        string `json:"amount"`
	TokenAmount   *struct {
		Amount string `json:"amount"`
	} `json:"tokenAmount"`
}

// Movements lists every SPL Token (classic or 2022) balance-changing instruction, top-level and
// inner, in execution order.
func (t *Tx) Movements() []Movement {
	var out []Movement
	add := func(i int, ix Instruction) {
		if ix.Program != "spl-token" && ix.Program != "spl-token-2022" {
			return
		}
		var info tokenInfo
		if json.Unmarshal([]byte(ix.Info), &info) != nil {
			return
		}
		amt := info.Amount
		if info.TokenAmount != nil {
			amt = info.TokenAmount.Amount
		}
		auth := info.Authority
		if auth == "" {
			auth = info.MultisigAuth
		}
		m := Movement{Instruction: i, ProgramID: ix.ProgramID, Kind: ix.Type, Mint: info.Mint, Authority: auth, Amount: amt}
		switch ix.Type {
		case "transfer", "transferChecked":
			m.From, m.To = info.Source, info.Destination
		case "mintTo", "mintToChecked":
			m.To, m.Authority = info.Account, info.MintAuthority
		case "burn", "burnChecked":
			m.From = info.Account
		default:
			return
		}
		out = append(out, m)
	}
	for i, ix := range t.Instructions {
		add(i, ix)
		for _, in := range t.Inner[i] {
			add(i, in)
		}
	}
	return out
}

// ParseTransactionJSON decodes a getTransaction result in jsonParsed encoding (for example a
// recorded answer).
func ParseTransactionJSON(raw json.RawMessage) (*Tx, error) { return parseTx(raw) }
