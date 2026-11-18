// Package chainwatch is the boundary between the worker and the EVM RPC providers. A Quorum
// holds the independent providers of one chain and only returns chain data (heads, block
// hashes, logs, call results, receipts) that two of them report identically. Providers that
// fail are put in cooldown and replaced by the next one (failover); two healthy providers that
// disagree halt credit on the chain instead of picking a side.
package chainwatch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"iter"
	"math/big"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/prismaport/prismaport/internal/destination"
	"github.com/prismaport/prismaport/internal/evm"
	"github.com/prismaport/prismaport/internal/evmrpc"
	"github.com/prismaport/prismaport/internal/quorum"
)

// Provider is one RPC endpoint of one chain. *evmrpc.Client implements it.
type Provider interface {
	Name() string
	ChainID(ctx context.Context) (uint64, error)
	HeaderByTag(ctx context.Context, tag evmrpc.BlockTag) (evmrpc.Header, error)
	Logs(ctx context.Context, f evmrpc.Filter) ([]evmrpc.Log, error)
	Call(ctx context.Context, m evmrpc.CallMsg, tag evmrpc.BlockTag) ([]byte, error)
	EstimateGas(ctx context.Context, m evmrpc.CallMsg) (uint64, error)
	Nonce(ctx context.Context, a evm.Address, tag evmrpc.BlockTag) (uint64, error)
	Balance(ctx context.Context, a evm.Address, tag evmrpc.BlockTag) (*big.Int, error)
	Code(ctx context.Context, a evm.Address, tag evmrpc.BlockTag) ([]byte, error)
	MaxPriorityFee(ctx context.Context) (*big.Int, error)
	SendRawTransaction(ctx context.Context, raw []byte) (evm.Word, error)
	Receipt(ctx context.Context, hash evm.Word) (evmrpc.Receipt, error)
}

var (
	// ErrNoQuorum means fewer than two providers are healthy: nothing may be credited.
	ErrNoQuorum = quorum.ErrNoQuorum
	// ErrDisagreement means two healthy providers returned different chain data.
	ErrDisagreement = quorum.ErrDisagreement
)

// Head is the agreed view of the chain tip: for each tag, the lower of the two providers.
type Head struct {
	Latest    uint64
	Safe      uint64
	Finalized uint64
}

// DefaultConfirmations is the depth at which an EVM payment counts unless a chain configures
// another. On Base (OP Stack, one sequencer, a block every 2 s) the unsafe head only reorgs on a
// sequencer fault or when an L1 reorg drops a batch; 10 blocks (~20 s) lets such a hiccup show
// before anything is credited, while the safe head takes minutes and finality ~15-20 minutes.
// A payment that is still reorged out later is reversed by the finality re-check.
const DefaultConfirmations = 10

// CreditPolicy decides when a detected transfer counts as confirmed on a chain.
type CreditPolicy struct {
	Confirmations uint64
	UseSafeHead   bool
}

// Confirmed reports whether a log in block n is deep enough under the policy.
func (p CreditPolicy) Confirmed(n uint64, h Head) bool {
	if p.UseSafeHead {
		return h.Safe >= n
	}
	conf := p.Confirmations
	if conf == 0 {
		conf = 1
	}
	return h.Latest >= n && h.Latest-n+1 >= conf
}

// The status, options and cooldown defaults live in package quorum, shared with solwatch.
type (
	// State is a chain's health as the quorum sees it.
	State = quorum.State
	// Status is the chain's health as the quorum last saw it.
	Status = quorum.Status
	// Options tune how the quorum backs off failing providers.
	Options = quorum.Options
)

// The chain health states, from healthy to stopped.
const (
	OK       = quorum.OK
	Degraded = quorum.Degraded
	Halted   = quorum.Halted
)

// Provider cooldown defaults (Options.Cooldown, Options.MaxCooldown), shared with solwatch.
const (
	DefaultCooldown    = quorum.DefaultCooldown
	DefaultMaxCooldown = quorum.DefaultMaxCooldown
	MaxCooldownShift   = quorum.MaxCooldownShift
)

// Quorum reads one EVM chain through its independent RPC providers and returns only data two of
// them report identically.
type Quorum struct {
	c *quorum.Core[Provider]
}

// classify: a transport failure is the provider's fault (fail over); so is any error that is
// not an answer about the chain (a revert, a not-found), which agree and rotate fail over on
// too: a lagging backend or a rejected range must not halt credit. Answers are compared like
// any other result.
var classify = quorum.Classify{
	Transient: transient,
	Failed: func(err error) bool {
		return err != nil && (transient(err) || !(evmrpc.IsRevert(err) || errors.Is(err, evmrpc.ErrNotFound)))
	},
}

// NewQuorum takes the providers in priority order.
func NewQuorum(chain string, providers []Provider, o Options) *Quorum {
	return &Quorum{c: quorum.New(chain, providers, classify, o)}
}

// Chain returns the ID of the chain the quorum reads.
func (q *Quorum) Chain() string { return q.c.Chain() }

// Status returns the chain's health as last seen.
func (q *Quorum) Status() Status { return q.c.Status() }

func (q *Quorum) setStatus(s State, reason string) { q.c.SetStatus(s, reason) }

// transient errors are the provider's fault: fail over. Anything else (a revert, a not-found)
// is an answer about the chain and is compared like any other result.
func transient(err error) bool {
	return errors.Is(err, evmrpc.ErrTransport) || errors.Is(err, context.DeadlineExceeded)
}

// agree asks two healthy providers the same question and returns the answer only when both
// give the same one. A provider with a transport failure is cooled down and replaced by the
// next healthy one.
func agree[T any](ctx context.Context, q *Quorum, what string, ask func(Provider) (T, error), equal func(a, b T) bool) (T, error) {
	var zero T
	got, err := quorum.Ask(ctx, q.c, what, ask)
	if err != nil {
		return zero, err
	}
	a, b := got[0], got[1]
	switch {
	case a.Err == nil && b.Err == nil && equal(a.V, b.V):
		q.c.AfterSuccess()
		return a.V, nil
	case a.Err != nil && b.Err != nil && errors.Is(a.Err, evmrpc.ErrNotFound) == errors.Is(b.Err, evmrpc.ErrNotFound) &&
		evmrpc.IsRevert(a.Err) == evmrpc.IsRevert(b.Err):
		q.c.AfterSuccess()
		return zero, a.Err
	case errors.Is(a.Err, evmrpc.ErrNotFound) || errors.Is(b.Err, evmrpc.ErrNotFound):
		// One provider has not seen the block yet: lagging, not conflicting.
		q.c.AfterSuccess()
		return zero, fmt.Errorf("%s on %s: %w (one provider lags)", what, q.c.Chain(), evmrpc.ErrNotFound)
	}
	return zero, q.c.Disagree(what, a.M, b.M)
}

// first runs ask on the first healthy provider, failing over on transport errors. It is for
// requests that are not chain data (fee quotes, gas estimates) and are re-checked anyway.
func first[T any](ctx context.Context, q *Quorum, ask func(Provider) (T, error)) (T, error) {
	return quorum.First(ctx, q.c, ask)
}

// Rotor sends single-provider reads (D49) to the first two healthy providers in turn: each
// call starts at the other one than the call before, and fails over to the next healthy
// provider when that one errs. It is for reads whose result is not credited on its own: balance
// change detection, heads that only schedule an agreed re-check, receipts that are followed by
// agreed reads. Each caller keeps its own Rotor so its calls alternate tick by tick.
type Rotor struct {
	r *quorum.Rotor[Provider]
}

// Rotor returns a new round-robin reader over the quorum's providers.
func (q *Quorum) Rotor() *Rotor { return &Rotor{r: quorum.NewRotor(q.c)} }

// rotate runs ask on one provider: the next of the first two healthy ones, failing over on an
// error that is not an answer about the chain (as agree does). It returns that provider's name.
func rotate[T any](ctx context.Context, r *Rotor, ask func(Provider) (T, error)) (T, string, error) {
	return quorum.Rotate(ctx, r.r, ask)
}

// Call runs eth_call on one provider at tag.
func (r *Rotor) Call(ctx context.Context, m evmrpc.CallMsg, tag evmrpc.BlockTag) ([]byte, string, error) {
	return rotate(ctx, r, func(p Provider) ([]byte, error) { return p.Call(ctx, m, tag) })
}

// Header reads one header (latest, safe, finalized or a height) from one provider.
func (r *Rotor) Header(ctx context.Context, tag evmrpc.BlockTag) (evmrpc.Header, error) {
	h, _, err := rotate(ctx, r, func(p Provider) (evmrpc.Header, error) { return p.HeaderByTag(ctx, tag) })
	return h, err
}

// Receipt reads a receipt from one provider (evmrpc.ErrNotFound while not mined).
func (r *Rotor) Receipt(ctx context.Context, hash evm.Word) (evmrpc.Receipt, error) {
	rc, _, err := rotate(ctx, r, func(p Provider) (evmrpc.Receipt, error) { return p.Receipt(ctx, hash) })
	return rc, err
}

// Logs reads logs from one provider. An answer from one provider is never credited: it only
// tells the caller where an agreed read is due (a flush the balance reads cannot see, H-4).
func (r *Rotor) Logs(ctx context.Context, f evmrpc.Filter) ([]evmrpc.Log, error) {
	ls, _, err := rotate(ctx, r, func(p Provider) ([]evmrpc.Log, error) { return p.Logs(ctx, f) })
	return ls, err
}

// VerifyChainID checks every provider serves the expected chain. A misconfigured URL would
// otherwise feed another network's logs into credit.
func (q *Quorum) VerifyChainID(ctx context.Context, want uint64) error {
	for _, m := range q.c.Members() {
		id, err := m.P.ChainID(ctx)
		if err != nil {
			return fmt.Errorf("%s provider %s: chain id: %w", q.c.Chain(), m.P.Name(), err)
		}
		if id != want {
			return fmt.Errorf("%s provider %s serves chain id %d, want %d", q.c.Chain(), m.P.Name(), id, want)
		}
	}
	return nil
}

// Head returns the lower of the two providers' latest, safe and finalized heights. Heads move,
// so they are not compared for equality; taking the minimum never runs ahead of either.
func (q *Quorum) Head(ctx context.Context) (Head, error) {
	return q.headOf(ctx, evmrpc.Latest, evmrpc.Safe, evmrpc.Finalized)
}

// AgreedLatest returns the lower of the two providers' latest heights, reading no safe or
// finalized header: one header request per provider.
func (q *Quorum) AgreedLatest(ctx context.Context) (uint64, error) {
	h, err := q.headOf(ctx, evmrpc.Latest)
	return h.Latest, err
}

// headOf is Head reading only the given tags (the others stay zero). One header request per tag
// and provider.
func (q *Quorum) headOf(ctx context.Context, tags ...evmrpc.BlockTag) (Head, error) {
	var heads []Head
	_, err := agree(ctx, q, "head", func(p Provider) (Head, error) {
		var h Head
		dst := map[evmrpc.BlockTag]*uint64{evmrpc.Latest: &h.Latest, evmrpc.Safe: &h.Safe, evmrpc.Finalized: &h.Finalized}
		for _, tag := range tags {
			hd, err := p.HeaderByTag(ctx, tag)
			if errors.Is(err, evmrpc.ErrNotFound) && tag != evmrpc.Latest {
				continue // a young chain may have no safe/finalized block yet
			}
			if err != nil {
				return h, err
			}
			*dst[tag] = hd.Number
		}
		return h, nil
	}, func(a, b Head) bool { heads = []Head{a, b}; return true })
	if err != nil {
		return Head{}, err
	}
	a, b := heads[0], heads[1]
	return Head{Latest: min(a.Latest, b.Latest), Safe: min(a.Safe, b.Safe), Finalized: min(a.Finalized, b.Finalized)}, nil
}

// Header returns the agreed header at a height.
func (q *Quorum) Header(ctx context.Context, n uint64) (evmrpc.Header, error) {
	return agree(ctx, q, fmt.Sprintf("block %d", n), func(p Provider) (evmrpc.Header, error) {
		return p.HeaderByTag(ctx, evmrpc.Number(n))
	}, func(a, b evmrpc.Header) bool { return a.Hash == b.Hash && a.Number == b.Number })
}

// BlockHash returns the agreed canonical hash at a height.
func (q *Quorum) BlockHash(ctx context.Context, n uint64) (evm.Word, error) {
	h, err := q.Header(ctx, n)
	return h.Hash, err
}

func sortLogs(ls []evmrpc.Log) []evmrpc.Log {
	out := slices.Clone(ls)
	sort.Slice(out, func(i, j int) bool {
		if out[i].BlockNumber != out[j].BlockNumber {
			return out[i].BlockNumber < out[j].BlockNumber
		}
		return out[i].LogIndex < out[j].LogIndex
	})
	return out
}

func sameLog(a, b evmrpc.Log) bool {
	return a.Address == b.Address && a.BlockNumber == b.BlockNumber && a.BlockHash == b.BlockHash &&
		a.TxHash == b.TxHash && a.LogIndex == b.LogIndex && a.Removed == b.Removed &&
		bytes.Equal(a.Data, b.Data) && slices.Equal(a.Topics, b.Topics)
}

// LogRangeEnd is the last block of the eth_getLogs range that starts at from, ends at most at to
// and spans at most limit blocks. A limit of 0 is read as 1, never as unbounded.
func LogRangeEnd(from, to, limit uint64) uint64 {
	limit = max(limit, 1)
	if to-from >= limit {
		return from + limit - 1
	}
	return to
}

// DefaultMaxLogRange is the eth_getLogs block range the detector and the sender use when the
// chain configures none: one default for both, the one strict providers accept.
const DefaultMaxLogRange = 500

// LogRanges splits the inclusive range [from, to] into consecutive eth_getLogs ranges of at most
// limit blocks each, in order. It yields nothing when from > to.
func LogRanges(from, to, limit uint64) iter.Seq2[uint64, uint64] {
	return func(yield func(uint64, uint64) bool) {
		if from > to {
			return
		}
		for {
			end := LogRangeEnd(from, to, limit)
			if !yield(from, end) || end == to {
				return
			}
			from = end + 1
		}
	}
}

// Logs returns the logs both providers report for the filter, sorted by block and index.
func (q *Quorum) Logs(ctx context.Context, f evmrpc.Filter) ([]evmrpc.Log, error) {
	return agree(ctx, q, fmt.Sprintf("logs [%d,%d]", f.FromBlock, f.ToBlock), func(p Provider) ([]evmrpc.Log, error) {
		ls, err := p.Logs(ctx, f)
		return sortLogs(ls), err
	}, func(a, b []evmrpc.Log) bool { return slices.EqualFunc(a, b, sameLog) })
}

// CallLatest returns an eth_call result both providers agree on at their latest block. It is
// for values that rarely change (a blacklist flag); a value that changed between the two
// providers' heads shows as a disagreement.
func (q *Quorum) CallLatest(ctx context.Context, m evmrpc.CallMsg) ([]byte, error) {
	return agree(ctx, q, "call", func(p Provider) ([]byte, error) {
		return p.Call(ctx, m, evmrpc.Latest)
	}, bytes.Equal)
}

// Call returns an eth_call result both providers agree on at a fixed block.
func (q *Quorum) Call(ctx context.Context, m evmrpc.CallMsg, n uint64) ([]byte, error) {
	return agree(ctx, q, "call", func(p Provider) ([]byte, error) {
		return p.Call(ctx, m, evmrpc.Number(n))
	}, bytes.Equal)
}

// Code returns contract code both providers agree on at the latest block.
func (q *Quorum) Code(ctx context.Context, a evm.Address) ([]byte, error) {
	return agree(ctx, q, "code", func(p Provider) ([]byte, error) {
		return p.Code(ctx, a, evmrpc.Latest)
	}, bytes.Equal)
}

func sameReceipt(a, b evmrpc.Receipt) bool {
	return a.TxHash == b.TxHash && a.Status == b.Status && a.BlockNumber == b.BlockNumber &&
		a.BlockHash == b.BlockHash && slices.EqualFunc(sortLogs(a.Logs), sortLogs(b.Logs), sameLog)
}

// Receipt returns a receipt both providers agree on (evmrpc.ErrNotFound while not mined).
func (q *Quorum) Receipt(ctx context.Context, hash evm.Word) (evmrpc.Receipt, error) {
	return agree(ctx, q, "receipt", func(p Provider) (evmrpc.Receipt, error) {
		return p.Receipt(ctx, hash)
	}, sameReceipt)
}

// AnyReceipt asks every healthy provider for a receipt and returns the first one found. It is
// evmrpc.ErrNotFound only when every provider asked answered that the transaction is not mined,
// and at least two did (one when only one is configured): a provider that failed, or too few
// answers, make it an error instead, so "not mined" is never concluded from one lagging index or
// from a provider that did not answer. It is for deciding that a transaction of ours was not
// mined before giving up on it; the receipt itself is followed by agreed reads like any other.
func (q *Quorum) AnyReceipt(ctx context.Context, hash evm.Word) (evmrpc.Receipt, error) {
	need := min(2, len(q.c.Members()))
	var failed error
	notFound := 0
	for _, m := range q.c.Healthy(nil) {
		r, err := m.P.Receipt(ctx, hash)
		switch {
		case err == nil:
			q.c.Succeed(m)
			return r, nil
		case errors.Is(err, evmrpc.ErrNotFound):
			q.c.Succeed(m)
			notFound++
		default:
			q.c.Fail(m, err)
			failed = err
			if ctx.Err() != nil {
				return evmrpc.Receipt{}, ctx.Err()
			}
		}
	}
	if failed != nil {
		return evmrpc.Receipt{}, fmt.Errorf("%s: receipt: %w", q.c.Chain(), failed)
	}
	if notFound < need {
		return evmrpc.Receipt{}, fmt.Errorf("%s: receipt: %w", q.c.Chain(), ErrNoQuorum)
	}
	return evmrpc.Receipt{}, evmrpc.ErrNotFound
}

// FirstCall runs eth_call on one provider (simulations whose outcome is not credited).
func (q *Quorum) FirstCall(ctx context.Context, m evmrpc.CallMsg, tag evmrpc.BlockTag) ([]byte, error) {
	return first(ctx, q, func(p Provider) ([]byte, error) { return p.Call(ctx, m, tag) })
}

// EstimateGas estimates the gas of a call through the providers.
func (q *Quorum) EstimateGas(ctx context.Context, m evmrpc.CallMsg) (uint64, error) {
	return first(ctx, q, func(p Provider) (uint64, error) { return p.EstimateGas(ctx, m) })
}

// MaxPriorityFee returns the suggested priority fee per gas through the providers.
func (q *Quorum) MaxPriorityFee(ctx context.Context) (*big.Int, error) {
	return first(ctx, q, func(p Provider) (*big.Int, error) { return p.MaxPriorityFee(ctx) })
}

// LatestHeader returns the newest block header through the providers.
func (q *Quorum) LatestHeader(ctx context.Context) (evmrpc.Header, error) {
	return first(ctx, q, func(p Provider) (evmrpc.Header, error) { return p.HeaderByTag(ctx, evmrpc.Latest) })
}

// Balance returns the native balance of an address through the providers.
func (q *Quorum) Balance(ctx context.Context, a evm.Address) (*big.Int, error) {
	return first(ctx, q, func(p Provider) (*big.Int, error) { return p.Balance(ctx, a, evmrpc.Latest) })
}

// PendingNonce is the highest pending nonce any healthy provider reports, so a transaction one
// provider already knows about is never reused.
func (q *Quorum) PendingNonce(ctx context.Context, a evm.Address) (uint64, error) {
	var best uint64
	ok := false
	for _, m := range q.c.Healthy(nil) {
		n, err := m.P.Nonce(ctx, a, evmrpc.Pending)
		if err != nil {
			if transient(err) {
				q.c.Fail(m, err)
			}
			continue
		}
		q.c.Succeed(m)
		if !ok || n > best {
			best, ok = n, true
		}
	}
	if !ok {
		return 0, fmt.Errorf("%s: pending nonce: %w", q.c.Chain(), ErrNoQuorum)
	}
	return best, nil
}

// Broadcast sends a raw transaction to every healthy provider. It succeeds when at least one
// accepted it (or already knew it); errors from the others are returned for logging.
func (q *Quorum) Broadcast(ctx context.Context, raw []byte) (accepted int, errs []error) {
	return q.c.Broadcast(func(p Provider) error {
		_, err := p.SendRawTransaction(ctx, raw)
		return err
	}, func(err error) bool { return err == nil || AlreadyKnown(err) })
}

// AlreadyKnown reports a node's "I already have this transaction" answer.
func AlreadyKnown(err error) bool {
	var re *evmrpc.RPCError
	if !errors.As(err, &re) {
		return false
	}
	return containsAny(re.Message, "already known", "known transaction", "already imported", "transaction already exists")
}

// NonceTooLow reports that the nonce was already used on chain.
func NonceTooLow(err error) bool {
	var re *evmrpc.RPCError
	if !errors.As(err, &re) {
		return false
	}
	return containsAny(re.Message, "nonce too low", "nonce has already been used")
}

func containsAny(s string, subs ...string) bool {
	ls := []byte(s)
	for i := range ls {
		if ls[i] >= 'A' && ls[i] <= 'Z' {
			ls[i] += 'a' - 'A'
		}
	}
	for _, sub := range subs {
		if bytes.Contains(ls, []byte(sub)) {
			return true
		}
	}
	return false
}

// CodeChecker implements destination.EVMCodeChecker over the chains' quorums.
type CodeChecker struct {
	Quorums map[string]*Quorum
}

// ErrNoProviders is returned when a lookup names a chain without a quorum.
var ErrNoProviders = errors.New("chainwatch: no providers configured for chain")

// HasCode reports whether a contract is deployed at the address on the given chain.
func (c CodeChecker) HasCode(ctx context.Context, chainID, address string) (bool, error) {
	q, ok := c.Quorums[chainID]
	if !ok {
		return false, fmt.Errorf("%w: %s", ErrNoProviders, chainID)
	}
	a, err := evm.ParseAddressLoose(address)
	if err != nil {
		return false, err
	}
	code, err := q.Code(ctx, a)
	if err != nil {
		return false, err
	}
	return len(code) > 0, nil
}

// isBlacklisted is FiatToken's isBlacklisted(address) selector.
var isBlacklisted = evm.Selector("isBlacklisted(address)")

// Blacklist implements destination.BlacklistChecker over the chains' quorums: it reads
// isBlacklisted(address) from the token at latest through two-provider agreement, without head
// reads (two requests). Answers "not listed" are kept for TTL (zero keeps nothing), so a busy
// merchant does not query the same recipients for every session; a listed address is never
// cached, so lifting a freeze takes effect at once.
type Blacklist struct {
	Quorums map[string]*Quorum
	TTL     time.Duration
	Now     func() time.Time

	mu    sync.Mutex
	clear map[string]time.Time
}

// IsBlacklisted reports whether the token's issuer has frozen the address on the given chain,
// serving recent "not listed" answers from the cache.
func (b *Blacklist) IsBlacklisted(ctx context.Context, chainID, token, address string) (bool, error) {
	q, ok := b.Quorums[chainID]
	if !ok {
		return false, fmt.Errorf("%w: %s", destination.ErrNoProviders, chainID)
	}
	t, err := evm.ParseAddressLoose(token)
	if err != nil {
		return false, err
	}
	a, err := evm.ParseAddressLoose(address)
	if err != nil {
		return false, err
	}
	now := time.Now
	if b.Now != nil {
		now = b.Now
	}
	key := chainID + "|" + t.Lower() + "|" + a.Lower()
	b.mu.Lock()
	if until, ok := b.clear[key]; ok && now().Before(until) {
		b.mu.Unlock()
		return false, nil
	}
	b.mu.Unlock()
	// Read at latest through both providers, without head reads (D49): two requests.
	out, err := q.CallLatest(ctx, evmrpc.CallMsg{To: t, Data: evm.Calldata(isBlacklisted, evm.AddressWord(a))})
	if err != nil {
		return false, err
	}
	if len(out) != 32 {
		return false, fmt.Errorf("chainwatch: isBlacklisted returned %d bytes", len(out))
	}
	v := new(big.Int).SetBytes(out)
	if v.Cmp(big.NewInt(1)) > 0 {
		return false, fmt.Errorf("chainwatch: isBlacklisted returned %x", out)
	}
	listed := v.Sign() != 0
	if !listed && b.TTL > 0 {
		b.mu.Lock()
		if b.clear == nil {
			b.clear = map[string]time.Time{}
		}
		b.clear[key] = now().Add(b.TTL)
		b.mu.Unlock()
	}
	return listed, nil
}

// Probe is the idle heartbeat: one latest header from one provider, alternating, failing over
// to the next one on an error. A provider that failed is cooled down and the chain's status
// shows it (Degraded; a halted chain stays halted until an agreed read). An error means every
// provider failed.
func (r *Rotor) Probe(ctx context.Context) error {
	_, err := r.Header(ctx, evmrpc.Latest)
	r.r.Core().AfterProbe()
	return err
}
