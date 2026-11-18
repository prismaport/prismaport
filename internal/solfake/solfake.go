// Package solfake is an in-memory Solana cluster for tests of the Solana worker loops: slots
// with block hashes at the processed, confirmed and finalized commitments, account state,
// per-address signature histories, jsonParsed transactions and signature statuses, slots that
// are skipped (Orphan) or rolled back with their effects (Rollback), plus per-provider rewrites
// that make two providers disagree and per-method error injection (FailNext). It is the Solana
// counterpart of evmfake.
package solfake

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"

	"github.com/prismaport/prismaport/internal/base58"
	"github.com/prismaport/prismaport/internal/solana"
	"github.com/prismaport/prismaport/internal/solrpc"
	"github.com/prismaport/prismaport/internal/solwatch"
)

// Genesis is the genesis hash every fake provider reports.
const Genesis = "solfake-genesis"

// Cluster is one canonical fork. All methods are safe for concurrent use.
type Cluster struct {
	mu sync.Mutex
	// FinalDepth is how many slots finality trails the confirmed tip.
	FinalDepth uint64
	// ConfirmDepth is how many slots the confirmed commitment trails the processed tip, the
	// newest mined slot (0, the default: a slot is confirmed as soon as it is mined). Reads at
	// confirmed or finalized do not see the processed-only slots: their transactions, signatures
	// and account changes.
	ConfirmDepth uint64
	// SimulateHandler answers simulateTransaction; nil succeeds with 50,000 compute units.
	SimulateHandler func(raw []byte) solrpc.Simulation
	tip             uint64
	salt            uint64
	blocks          map[uint64]string
	skipped         map[uint64]bool
	accounts        map[string]solrpc.Account
	balances        map[string]uint64
	txs             map[string]*solrpc.Tx
	history         map[string][]string // address -> signatures, oldest first
	sent            [][]byte
	// undo holds, per slot, the state each account had before that slot's transactions changed
	// it: what a read at a lower commitment sees, and what Rollback restores.
	undo map[uint64]map[string]solrpc.Account
}

// New returns an empty cluster at slot zero.
func New() *Cluster {
	c := &Cluster{FinalDepth: 3, blocks: map[uint64]string{}, skipped: map[uint64]bool{}, accounts: map[string]solrpc.Account{},
		balances: map[string]uint64{}, txs: map[string]*solrpc.Tx{}, history: map[string][]string{},
		undo: map[uint64]map[string]solrpc.Account{}}
	c.blocks[0] = c.hash(0)
	return c
}

func (c *Cluster) hash(slot uint64) string {
	c.salt++
	var b [16]byte
	binary.BigEndian.PutUint64(b[:8], slot)
	binary.BigEndian.PutUint64(b[8:], c.salt)
	h := sha256.Sum256(b[:])
	return base58.Encode(h[:])
}

func (c *Cluster) confirmed() uint64 { return c.tip - min(c.tip, c.ConfirmDepth) }

func (c *Cluster) finalized() uint64 {
	conf := c.confirmed()
	return conf - min(conf, c.FinalDepth)
}

// Tip is the confirmed slot (ConfirmDepth below Processed); Finalized trails it by FinalDepth.
func (c *Cluster) Tip() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.confirmed()
}

// Processed is the newest mined slot, the processed commitment.
func (c *Cluster) Processed() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tip
}

// Finalized returns the highest slot that counts as finalized.
func (c *Cluster) Finalized() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.finalized()
}

// Mine produces the next slot holding txs (their Slot is set) and returns the slot. Every
// account key of a transaction gets its signature in its history, and the post token balances
// of a successful one become the state of those token accounts (getMultipleAccounts sees the
// transfer, as on a real cluster).
func (c *Cluster) Mine(txs ...*solrpc.Tx) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tip++
	c.blocks[c.tip] = c.hash(c.tip)
	for _, tx := range txs {
		cp := clone(tx)
		cp.Slot = c.tip
		tx.Slot = c.tip
		sig := cp.Signatures[0]
		c.txs[sig] = cp
		seen := map[string]bool{}
		for _, k := range cp.AccountKeys {
			if !seen[k] {
				seen[k] = true
				c.history[k] = append(c.history[k], sig)
			}
		}
		if cp.Err == "" {
			c.applyTokenBalances(c.tip, cp)
		}
	}
	return c.tip
}

// applyTokenBalances writes a transaction's post token balances into the token accounts,
// keeping the state before slot in its undo log.
func (c *Cluster) applyTokenBalances(slot uint64, tx *solrpc.Tx) {
	for _, b := range tx.PostToken {
		if b.AccountIndex < 0 || b.AccountIndex >= len(tx.AccountKeys) {
			continue
		}
		mint, err1 := solana.ParsePublicKey(b.Mint)
		owner, err2 := solana.ParsePublicKey(b.Owner)
		amount, err3 := strconv.ParseUint(b.Amount, 10, 64)
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		addr := tx.AccountKeys[b.AccountIndex]
		acc := c.accounts[addr]
		if c.undo[slot] == nil {
			c.undo[slot] = map[string]solrpc.Account{}
		}
		if _, ok := c.undo[slot][addr]; !ok {
			c.undo[slot][addr] = acc
		}
		if !acc.Exists {
			acc = solrpc.Account{Exists: true, Owner: solana.TokenProgramID.String(), Lamports: 2_039_280}
		}
		acc.Data = TokenAccountData(mint, owner, amount)
		c.accounts[addr] = acc
	}
}

// MineN produces n empty slots.
func (c *Cluster) MineN(n int) {
	for range n {
		c.Mine()
	}
}

// Confirm mines empty slots until slot is confirmed.
func (c *Cluster) Confirm(slot uint64) {
	for c.Tip() < slot {
		c.Mine()
	}
}

// Finalize mines empty slots until slot is finalized.
func (c *Cluster) Finalize(slot uint64) {
	for c.Finalized() < slot {
		c.Mine()
	}
}

// Orphan drops slot from the fork, as a fork that did not survive: the slot reads as skipped
// and its transactions vanish from every view. The account state they wrote stays (set it
// explicitly, or use Rollback, which also undoes it).
func (c *Cluster) Orphan(slot uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.drop(slot)
}

// Rollback abandons the fork from slot to the processed tip, as when the cluster switches to
// another fork: every one of those slots reads as skipped, their transactions and signatures
// vanish and the account changes they made are undone. Later Mine calls build the surviving
// fork on top. A finalized slot never rolls back: Rollback panics for one.
func (c *Cluster) Rollback(slot uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if slot <= c.finalized() {
		panic(fmt.Sprintf("solfake: slot %d is finalized (finalized slot %d) and cannot roll back", slot, c.finalized()))
	}
	for s := c.tip; s >= slot; s-- {
		c.drop(s)
		for addr, acc := range c.undo[s] {
			if acc.Exists {
				c.accounts[addr] = acc
			} else {
				delete(c.accounts, addr)
			}
		}
		delete(c.undo, s)
	}
}

// drop marks slot skipped and removes its transactions and their signatures. c.mu is held.
func (c *Cluster) drop(slot uint64) {
	c.skipped[slot] = true
	gone := map[string]bool{}
	for sig, tx := range c.txs {
		if tx.Slot == slot {
			gone[sig] = true
			delete(c.txs, sig)
		}
	}
	for addr, sigs := range c.history {
		var keep []string
		for _, s := range sigs {
			if !gone[s] {
				keep = append(keep, s)
			}
		}
		c.history[addr] = keep
	}
}

// SetAccount replaces an account's state; Exists false removes it.
func (c *Cluster) SetAccount(addr string, a solrpc.Account) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// A direct write holds at every commitment: no read rewinds past it.
	for _, u := range c.undo {
		delete(u, addr)
	}
	if !a.Exists {
		delete(c.accounts, addr)
		return
	}
	c.accounts[addr] = a
}

// accountAt is addr's state as of slot: the current state with the changes of every later slot
// undone. c.mu is held.
func (c *Cluster) accountAt(addr string, slot uint64) solrpc.Account {
	acc := c.accounts[addr]
	for s := c.tip; s > slot; s-- {
		if prev, ok := c.undo[s][addr]; ok {
			acc = prev
		}
	}
	return acc
}

// SetTokenAccount makes addr an initialized classic token account of mint owned by owner.
func (c *Cluster) SetTokenAccount(addr, mint, owner solana.PublicKey, amount uint64) {
	c.SetAccount(addr.String(), solrpc.Account{Exists: true, Owner: solana.TokenProgramID.String(), Lamports: 2_039_280,
		Data: TokenAccountData(mint, owner, amount)})
}

// CloseAccount removes addr.
func (c *Cluster) CloseAccount(addr solana.PublicKey) { c.SetAccount(addr.String(), solrpc.Account{}) }

// SetBalance sets an account's lamports as getBalance reports them (unset is 10 SOL).
func (c *Cluster) SetBalance(addr string, lamports uint64) {
	c.mu.Lock()
	c.balances[addr] = lamports
	c.mu.Unlock()
}

// Sent lists every raw transaction any provider accepted, in order.
func (c *Cluster) Sent() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]byte(nil), c.sent...)
}

// TokenAccountData is the 165-byte state of an initialized classic token account.
func TokenAccountData(mint, owner solana.PublicKey, amount uint64) []byte {
	d := make([]byte, solana.TokenAccountSize)
	copy(d[0:32], mint[:])
	copy(d[32:64], owner[:])
	binary.LittleEndian.PutUint64(d[64:72], amount)
	d[108] = 1
	return d
}

func clone(tx *solrpc.Tx) *solrpc.Tx {
	raw, err := json.Marshal(tx)
	if err != nil {
		panic(err)
	}
	var out solrpc.Tx
	if err := json.Unmarshal(raw, &out); err != nil {
		panic(err)
	}
	return &out
}

// Signature is a distinct, well-formed transaction signature for hand-built transactions.
func Signature(seed string) string {
	h := sha256.Sum256([]byte(seed))
	g := sha256.Sum256(h[:])
	return base58.Encode(append(h[:], g[:]...))
}

// Key is a distinct public key named by seed.
func Key(seed string) solana.PublicKey {
	return solana.PublicKey(sha256.Sum256([]byte("solfake:" + seed)))
}

// Transfer is a parsed SPL Token transfer, top-level or inner.
func Transfer(from, to, authority solana.PublicKey, amount uint64) solrpc.Instruction {
	info, _ := json.Marshal(map[string]string{"source": from.String(), "destination": to.String(),
		"authority": authority.String(), "amount": strconv.FormatUint(amount, 10)})
	return solrpc.Instruction{ProgramID: solana.TokenProgramID.String(), Program: "spl-token", Type: "transfer", Info: string(info)}
}

// Raw is an instruction of a program the node does not parse, as jsonParsed renders it.
func Raw(ix solana.Instruction) solrpc.Instruction {
	out := solrpc.Instruction{ProgramID: ix.Program.String(), Data: base58.Encode(ix.Data)}
	for _, a := range ix.Accounts {
		out.Accounts = append(out.Accounts, a.Key.String())
	}
	return out
}

// TokenBalance is one pre or post balance entry of a classic token account.
type TokenBalance struct {
	Account, Mint, Owner solana.PublicKey
	Amount               uint64
}

// Tx assembles a successful transaction: its account keys are every account the instructions
// and balances name (the fee payer first), and pre/post balances are indexed against them.
func Tx(sig string, payer solana.PublicKey, ixs []solrpc.Instruction, inner map[int][]solrpc.Instruction, pre, post []TokenBalance) *solrpc.Tx {
	tx := &solrpc.Tx{Signatures: []string{sig}, Fee: 5_000, ComputeUnits: 30_000, RecentBlockhash: base58.Encode(make([]byte, 32)),
		Instructions: ixs, Inner: map[int][]solrpc.Instruction{}}
	idx := map[string]int{}
	add := func(k string) int {
		if i, ok := idx[k]; ok {
			return i
		}
		idx[k] = len(tx.AccountKeys)
		tx.AccountKeys = append(tx.AccountKeys, k)
		return idx[k]
	}
	add(payer.String())
	accountsOf := func(ix solrpc.Instruction) {
		add(ix.ProgramID)
		for _, a := range ix.Accounts {
			add(a)
		}
		var info map[string]any
		if json.Unmarshal([]byte(ix.Info), &info) == nil {
			for _, f := range []string{"source", "destination", "authority"} {
				if s, ok := info[f].(string); ok {
					add(s)
				}
			}
		}
	}
	for i, ix := range ixs {
		accountsOf(ix)
		for _, in := range inner[i] {
			accountsOf(in)
		}
		if len(inner[i]) > 0 {
			tx.Inner[i] = inner[i]
		}
	}
	bal := func(bs []TokenBalance) []solrpc.TokenBalance {
		out := []solrpc.TokenBalance{}
		for _, b := range bs {
			out = append(out, solrpc.TokenBalance{AccountIndex: add(b.Account.String()), Mint: b.Mint.String(), Owner: b.Owner.String(),
				ProgramID: solana.TokenProgramID.String(), Amount: strconv.FormatUint(b.Amount, 10), Decimals: 6})
		}
		return out
	}
	tx.PreToken, tx.PostToken = bal(pre), bal(post)
	return tx
}

// FromRaw is the jsonParsed view of a serialized transaction: its top-level instructions as
// raw program calls. inner and logs are what the runtime would add when it executes it.
func FromRaw(raw []byte, inner map[int][]solrpc.Instruction, logs []string) (*solrpc.Tx, error) {
	t, err := solana.ParseTransaction(raw)
	if err != nil {
		return nil, err
	}
	m := t.Message
	tx := &solrpc.Tx{Signatures: []string{t.ID()}, Fee: 5_000, ComputeUnits: 60_000, RecentBlockhash: base58.Encode(m.RecentBlockhash[:]),
		Inner: map[int][]solrpc.Instruction{}, PreToken: []solrpc.TokenBalance{}, PostToken: []solrpc.TokenBalance{}, Logs: logs}
	for _, k := range m.AccountKeys {
		tx.AccountKeys = append(tx.AccountKeys, k.String())
	}
	for _, ix := range m.Instructions {
		out := solrpc.Instruction{ProgramID: tx.AccountKeys[ix.ProgramIndex], Data: base58.Encode(ix.Data)}
		for _, a := range ix.Accounts {
			out.Accounts = append(out.Accounts, tx.AccountKeys[a])
		}
		tx.Instructions = append(tx.Instructions, out)
	}
	for i, ins := range inner {
		if i < 0 || i >= len(tx.Instructions) {
			return nil, fmt.Errorf("solfake: inner instructions for missing instruction %d", i)
		}
		tx.Inner[i] = ins
	}
	return tx, nil
}

// InstructionIndex is the index of the first top-level instruction of program whose data starts
// with disc, or -1.
func InstructionIndex(tx *solrpc.Tx, program solana.PublicKey, disc [8]byte) int {
	for i, ix := range tx.Instructions {
		if ix.ProgramID != program.String() {
			continue
		}
		if d, err := base58.Decode(ix.Data); err == nil && len(d) >= 8 && [8]byte(d[:8]) == disc {
			return i
		}
	}
	return -1
}

func programData(parts ...[]byte) string {
	var b []byte
	for _, p := range parts {
		b = append(b, p...)
	}
	return "Program data: " + base64.StdEncoding.EncodeToString(b)
}

func u64(v uint64) []byte { return binary.LittleEndian.AppendUint64(nil, v) }

// SettledLog is the log line the forwarder's Settled event prints.
func SettledLog(e solana.SettledEvent) string {
	return programData(solana.SettledEventDiscriminator[:], e.Forwarder[:], e.Mint[:], e.Source[:], u64(e.Gross), u64(e.Fee), u64(e.Net), e.Treasury[:])
}

// PayoutLog is the log line one Payout event prints.
func PayoutLog(e solana.PayoutEvent) string {
	return programData(solana.PayoutEventDiscriminator[:], e.Forwarder[:], e.Mint[:], e.Recipient[:], u64(e.Amount))
}

// Provider is one RPC endpoint on the cluster. It implements solwatch.Provider.
type Provider struct {
	name string
	c    *Cluster
	mu   sync.Mutex
	down bool
	// rewrite changes the transactions this provider serves (a provider disagreeing).
	rewrite func(*solrpc.Tx) *solrpc.Tx
	calls   map[string]int
	// behind and sigBehind are how many slots this provider's account reads and its signature
	// index trail the cluster (SetBehind, SetSignaturesBehind).
	behind, sigBehind uint64
	// faults are the errors armed by FailNext, by method ("" for any method).
	faults map[string]*fault
}

type fault struct {
	n   int
	err error
}

var _ solwatch.Provider = (*Provider)(nil)

// Provider returns a named view of the cluster that behaves like one RPC provider.
func (c *Cluster) Provider(name string) *Provider { return &Provider{name: name, c: c} }

// Name returns the provider name.
func (p *Provider) Name() string { return p.name }

// SetDown makes every call fail with a transport error.
func (p *Provider) SetDown(d bool) { p.mu.Lock(); p.down = d; p.mu.Unlock() }

// FailNext makes the next n calls of method (the Provider method name, as Calls counts it; ""
// for any method) fail with err. A nil err is a transport error (solrpc.ErrTransport), as from
// an unreachable node; pass solrpc.ErrNotFound, solrpc.ErrSkipped, solrpc.ErrBehind or an
// *solrpc.RPCError to fake a node's answer instead. A method-specific fault fires before an
// any-method one; n <= 0 disarms the method's fault.
func (p *Provider) FailNext(method string, n int, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.faults == nil {
		p.faults = map[string]*fault{}
	}
	if n <= 0 {
		delete(p.faults, method)
		return
	}
	p.faults[method] = &fault{n: n, err: err}
}

// SetBehind makes the provider's account reads trail the cluster by n slots: getMultipleAccounts
// answers a context slot n below the tip and refuses a minContextSlot above it with
// solrpc.ErrBehind, as a real node does. (The fake keeps no account history: the values read are
// the current ones.)
func (p *Provider) SetBehind(n uint64) { p.mu.Lock(); p.behind = n; p.mu.Unlock() }

// SetSignaturesBehind makes the provider's signature index trail the cluster by n slots: a
// signature list leaves out the transactions of the last n slots and refuses a minContextSlot
// above the slot it reached with solrpc.ErrBehind.
func (p *Provider) SetSignaturesBehind(n uint64) { p.mu.Lock(); p.sigBehind = n; p.mu.Unlock() }

func (p *Provider) lags() (accounts, signatures uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.behind, p.sigBehind
}

// Rewrite makes the provider serve f(tx) instead of each transaction (nil restores it).
func (p *Provider) Rewrite(f func(*solrpc.Tx) *solrpc.Tx) { p.mu.Lock(); p.rewrite = f; p.mu.Unlock() }

// Calls is the number of requests of a method (the Provider method name, e.g. "Signatures",
// "MultipleAccounts", "Slot") this provider received.
func (p *Provider) Calls(method string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[method]
}

// Total is the number of requests of every method this provider received.
func (p *Provider) Total() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, c := range p.calls {
		n += c
	}
	return n
}

func (p *Provider) enter(method string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.calls == nil {
		p.calls = map[string]int{}
	}
	p.calls[method]++
	if p.down {
		return fmt.Errorf("%w: %s down", solrpc.ErrTransport, p.name)
	}
	for _, key := range []string{method, ""} {
		f := p.faults[key]
		if f == nil {
			continue
		}
		if f.n--; f.n <= 0 {
			delete(p.faults, key)
		}
		if f.err == nil {
			return fmt.Errorf("%w: %s %s (injected)", solrpc.ErrTransport, p.name, method)
		}
		return f.err
	}
	return nil
}

// level is the highest slot visible at the commitment: the processed tip, the confirmed slot
// (also for an empty commitment, the node default) or the finalized one.
func (c *Cluster) level(cm solrpc.Commitment) uint64 {
	switch cm {
	case solrpc.Finalized:
		return c.finalized()
	case solrpc.Processed:
		return c.tip
	default:
		return c.confirmed()
	}
}

// GenesisHash returns Genesis.
func (p *Provider) GenesisHash(context.Context) (string, error) {
	return Genesis, p.enter("GenesisHash")
}

// Slot returns the current slot at the given commitment.
func (p *Provider) Slot(_ context.Context, cm solrpc.Commitment) (uint64, error) {
	if err := p.enter("Slot"); err != nil {
		return 0, err
	}
	p.c.mu.Lock()
	defer p.c.mu.Unlock()
	return p.c.level(cm), nil
}

// BlockHeight is the slot at the commitment (the fake skips no slot), counted as "BlockHeight".
func (p *Provider) BlockHeight(_ context.Context, cm solrpc.Commitment) (uint64, error) {
	if err := p.enter("BlockHeight"); err != nil {
		return 0, err
	}
	p.c.mu.Lock()
	defer p.c.mu.Unlock()
	return p.c.level(cm), nil
}

// LatestBlockhash returns the blockhash of the latest block at the given commitment.
func (p *Provider) LatestBlockhash(_ context.Context, cm solrpc.Commitment) (solrpc.Blockhash, error) {
	if err := p.enter("LatestBlockhash"); err != nil {
		return solrpc.Blockhash{}, err
	}
	p.c.mu.Lock()
	defer p.c.mu.Unlock()
	slot := p.c.level(cm)
	return solrpc.Blockhash{Hash: p.c.blocks[slot], LastValidBlockHeight: slot + 150, Slot: slot}, nil
}

// Block returns the block at slot, or an error if it was skipped or is not yet at the commitment.
func (p *Provider) Block(_ context.Context, slot uint64, cm solrpc.Commitment) (solrpc.Block, error) {
	if err := p.enter("Block"); err != nil {
		return solrpc.Block{}, err
	}
	p.c.mu.Lock()
	defer p.c.mu.Unlock()
	if slot > p.c.level(cm) {
		return solrpc.Block{}, solrpc.ErrNotFound
	}
	if p.c.skipped[slot] {
		return solrpc.Block{}, solrpc.ErrSkipped
	}
	return solrpc.Block{Slot: slot, Blockhash: p.c.blocks[slot], ParentSlot: slot - min(slot, 1), BlockHeight: slot}, nil
}

// Signatures lists the signatures that touched addr, newest first, honoring the before, until and limit options.
func (p *Provider) Signatures(_ context.Context, addr string, o solrpc.SignaturesOpts) ([]solrpc.SignatureInfo, error) {
	if err := p.enter("Signatures"); err != nil {
		return nil, err
	}
	_, lag := p.lags()
	p.c.mu.Lock()
	defer p.c.mu.Unlock()
	level := p.c.level(o.Commitment)
	level -= min(level, lag)
	if o.MinContextSlot > level {
		return nil, fmt.Errorf("%w: %s at slot %d", solrpc.ErrBehind, p.name, level)
	}
	sigs := p.c.history[addr]
	started := o.Before == ""
	var out []solrpc.SignatureInfo
	for i := len(sigs) - 1; i >= 0; i-- {
		s := sigs[i]
		if !started {
			started = s == o.Before
			continue
		}
		if s == o.Until {
			break
		}
		tx := p.c.txs[s]
		if tx.Slot > level {
			continue
		}
		info := solrpc.SignatureInfo{Signature: s, Slot: tx.Slot, ConfirmationStatus: p.c.status(tx)}
		if tx.Err != "" {
			info.Err = json.RawMessage(tx.Err)
		}
		out = append(out, info)
		if o.Limit > 0 && len(out) == o.Limit {
			break
		}
	}
	return out, nil
}

func (c *Cluster) status(tx *solrpc.Tx) string {
	switch {
	case tx.Slot <= c.finalized():
		return "finalized"
	case tx.Slot <= c.confirmed():
		return "confirmed"
	default:
		return "processed"
	}
}

// Transaction returns the transaction with the given signature at the commitment, or nil if unknown.
func (p *Provider) Transaction(_ context.Context, sig string, cm solrpc.Commitment) (*solrpc.Tx, error) {
	if err := p.enter("Transaction"); err != nil {
		return nil, err
	}
	p.c.mu.Lock()
	tx, ok := p.c.txs[sig]
	if ok && tx.Slot > p.c.level(cm) {
		ok = false
	}
	var out *solrpc.Tx
	if ok {
		out = clone(tx)
	}
	p.c.mu.Unlock()
	if !ok {
		return nil, solrpc.ErrNotFound
	}
	p.mu.Lock()
	f := p.rewrite
	p.mu.Unlock()
	if f != nil {
		out = f(out)
	}
	return out, nil
}

// Account returns one account, or the empty account if it does not exist.
func (p *Provider) Account(ctx context.Context, addr string, cm solrpc.Commitment, minContextSlot uint64) (solrpc.Account, error) {
	accs, err := p.MultipleAccounts(ctx, []string{addr}, cm, minContextSlot)
	if err != nil {
		return solrpc.Account{}, err
	}
	return accs[0], nil
}

// MultipleAccounts returns the given accounts in order, with the empty account for ones that do not exist.
func (p *Provider) MultipleAccounts(_ context.Context, addrs []string, cm solrpc.Commitment, minContextSlot uint64) ([]solrpc.Account, error) {
	if err := p.enter("MultipleAccounts"); err != nil {
		return nil, err
	}
	lag, _ := p.lags()
	p.c.mu.Lock()
	defer p.c.mu.Unlock()
	slot := p.c.level(cm)
	slot -= min(slot, lag)
	if minContextSlot > slot {
		return nil, fmt.Errorf("%w: %s at slot %d", solrpc.ErrBehind, p.name, slot)
	}
	out := make([]solrpc.Account, len(addrs))
	at := p.c.level(cm)
	for i, a := range addrs {
		acc := p.c.accountAt(a, at)
		acc.Data = append([]byte(nil), acc.Data...)
		acc.ContextSlot = slot
		out[i] = acc
	}
	return out, nil
}

// Balance returns the lamport balance of addr.
func (p *Provider) Balance(_ context.Context, addr string, _ solrpc.Commitment) (uint64, error) {
	if err := p.enter("Balance"); err != nil {
		return 0, err
	}
	p.c.mu.Lock()
	defer p.c.mu.Unlock()
	if b, ok := p.c.balances[addr]; ok {
		return b, nil
	}
	return 10_000_000_000, nil
}

// MinimumBalanceForRentExemption returns a fixed-rate rent-exempt minimum for an account of size bytes.
func (p *Provider) MinimumBalanceForRentExemption(_ context.Context, size int) (uint64, error) {
	return uint64(size+128) * 6_960, p.enter("MinimumBalanceForRentExemption")
}

// SendTransaction decodes the transaction, records it in the cluster and returns its signature.
func (p *Provider) SendTransaction(_ context.Context, raw []byte, _ bool) (string, error) {
	if err := p.enter("SendTransaction"); err != nil {
		return "", err
	}
	t, err := solana.ParseTransaction(raw)
	if err != nil {
		return "", &solrpc.SendError{Message: err.Error()}
	}
	p.c.mu.Lock()
	p.c.sent = append(p.c.sent, append([]byte(nil), raw...))
	p.c.mu.Unlock()
	return t.ID(), nil
}

// Simulate decodes the transaction and returns a successful simulation unless the cluster is set to fail it.
func (p *Provider) Simulate(_ context.Context, raw []byte, _ bool) (solrpc.Simulation, error) {
	if err := p.enter("Simulate"); err != nil {
		return solrpc.Simulation{}, err
	}
	p.c.mu.Lock()
	h := p.c.SimulateHandler
	p.c.mu.Unlock()
	if h != nil {
		return h(raw), nil
	}
	return solrpc.Simulation{UnitsConsumed: 50_000}, nil
}

// SignatureStatuses returns one status per signature, nil where the cluster does not know it.
func (p *Provider) SignatureStatuses(_ context.Context, sigs []string) ([]*solrpc.SignatureStatus, error) {
	if err := p.enter("SignatureStatuses"); err != nil {
		return nil, err
	}
	p.c.mu.Lock()
	defer p.c.mu.Unlock()
	out := make([]*solrpc.SignatureStatus, len(sigs))
	for i, s := range sigs {
		tx, ok := p.c.txs[s]
		if !ok {
			continue
		}
		st := &solrpc.SignatureStatus{Slot: tx.Slot, ConfirmationStatus: p.c.status(tx)}
		if tx.Err != "" {
			st.Err = json.RawMessage(tx.Err)
		}
		out[i] = st
	}
	return out, nil
}

// IsBlockhashValid reports whether hash is a blockhash the cluster still accepts.
func (p *Provider) IsBlockhashValid(_ context.Context, hash string) (bool, error) {
	if err := p.enter("IsBlockhashValid"); err != nil {
		return false, err
	}
	p.c.mu.Lock()
	defer p.c.mu.Unlock()
	for slot, h := range p.c.blocks {
		if h == hash {
			return p.c.tip <= slot+150, nil
		}
	}
	return false, nil
}
