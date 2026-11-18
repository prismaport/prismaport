// Package solwatch is the boundary between the worker and the Solana RPC providers, the
// counterpart of chainwatch for EVM. A Quorum holds the independent providers of one Solana
// cluster and only returns chain data (slots, block hashes, transactions, account state) that
// two of them report identically. Failing providers are cooled down and replaced by the next
// one; two healthy providers that disagree halt credit on the chain instead of picking a side.
package solwatch

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"

	"github.com/prismaport/prismaport/internal/chainwatch"
	qcore "github.com/prismaport/prismaport/internal/quorum"
	"github.com/prismaport/prismaport/internal/solrpc"
)

// Provider is one RPC endpoint of one cluster. *solrpc.Client implements it.
type Provider interface {
	Name() string
	GenesisHash(ctx context.Context) (string, error)
	Slot(ctx context.Context, cm solrpc.Commitment) (uint64, error)
	BlockHeight(ctx context.Context, cm solrpc.Commitment) (uint64, error)
	LatestBlockhash(ctx context.Context, cm solrpc.Commitment) (solrpc.Blockhash, error)
	Block(ctx context.Context, slot uint64, cm solrpc.Commitment) (solrpc.Block, error)
	Signatures(ctx context.Context, addr string, o solrpc.SignaturesOpts) ([]solrpc.SignatureInfo, error)
	Transaction(ctx context.Context, sig string, cm solrpc.Commitment) (*solrpc.Tx, error)
	Account(ctx context.Context, addr string, cm solrpc.Commitment, minContextSlot uint64) (solrpc.Account, error)
	MultipleAccounts(ctx context.Context, addrs []string, cm solrpc.Commitment, minContextSlot uint64) ([]solrpc.Account, error)
	Balance(ctx context.Context, addr string, cm solrpc.Commitment) (uint64, error)
	MinimumBalanceForRentExemption(ctx context.Context, size int) (uint64, error)
	SendTransaction(ctx context.Context, raw []byte, skipPreflight bool) (string, error)
	Simulate(ctx context.Context, raw []byte, sigVerify bool) (solrpc.Simulation, error)
	SignatureStatuses(ctx context.Context, sigs []string) ([]*solrpc.SignatureStatus, error)
	IsBlockhashValid(ctx context.Context, hash string) (bool, error)
}

// Errors returned by quorum reads.
var (
	ErrNoQuorum     = chainwatch.ErrNoQuorum
	ErrDisagreement = chainwatch.ErrDisagreement
	// ErrLag means the providers have not both caught up with the question yet; ask again later.
	ErrLag = errors.New("solwatch: providers have not caught up")
)

// Quorum asks several providers the same question and returns an answer only when enough of them agree.
type Quorum struct {
	c *qcore.Core[Provider]
}

// classify: a transport failure is the provider's fault; so is, for agreed reads and the
// Rotor, any error that is not an answer about the chain. A provider behind the
// minContextSlot asked for (solrpc.ErrBehind) is passed over by the Rotor without a cooldown.
var classify = qcore.Classify{
	Transient: transient,
	Failed:    func(err error) bool { return !answer(err) },
	Behind:    func(err error) bool { return errors.Is(err, solrpc.ErrBehind) },
}

// NewQuorum takes the providers in priority order.
func NewQuorum(chain string, providers []Provider, o chainwatch.Options) *Quorum {
	return &Quorum{c: qcore.New(chain, providers, classify, o)}
}

// Chain returns the chain this quorum reads.
func (q *Quorum) Chain() string { return q.c.Chain() }

// Status returns the current health of the quorum.
func (q *Quorum) Status() chainwatch.Status { return q.c.Status() }

func (q *Quorum) setStatus(s chainwatch.State, reason string) { q.c.SetStatus(s, reason) }

func transient(err error) bool {
	return errors.Is(err, solrpc.ErrTransport) || errors.Is(err, context.DeadlineExceeded)
}

// answer reports whether err is an answer about the chain (compared between providers) rather
// than a provider failure.
func answer(err error) bool {
	return err == nil || errors.Is(err, solrpc.ErrNotFound) || errors.Is(err, solrpc.ErrSkipped) || errors.Is(err, ErrLag)
}

// ask gets answers from two healthy providers, failing over on provider errors.
func ask[T any](ctx context.Context, q *Quorum, what string, f func(Provider) (T, error)) ([2]qcore.Reply[Provider, T], error) {
	return qcore.Ask(ctx, q.c, what, f)
}

// agree returns an answer only when two providers give the same one. Both not having the data
// yet is returned as is; only one of them having it is lag, not conflict.
func agree[T any](ctx context.Context, q *Quorum, what string, f func(Provider) (T, error), equal func(a, b T) bool) (T, error) {
	var zero T
	got, err := ask(ctx, q, what, f)
	if err != nil {
		return zero, err
	}
	a, b := got[0], got[1]
	same := func(e error) bool { return errors.Is(a.Err, e) && errors.Is(b.Err, e) }
	switch {
	case a.Err == nil && b.Err == nil && equal(a.V, b.V):
		q.c.AfterSuccess()
		return a.V, nil
	case same(solrpc.ErrSkipped):
		q.c.AfterSuccess()
		return zero, a.Err
	case same(solrpc.ErrNotFound):
		q.c.AfterSuccess()
		return zero, a.Err
	case errors.Is(a.Err, solrpc.ErrNotFound) || errors.Is(b.Err, solrpc.ErrNotFound) || errors.Is(a.Err, ErrLag) || errors.Is(b.Err, ErrLag):
		q.c.AfterSuccess()
		return zero, fmt.Errorf("%s on %s: %w (one provider lags)", what, q.c.Chain(), ErrLag)
	}
	return zero, q.c.Disagree(what, a.M, b.M)
}

// first runs f on the first healthy provider, failing over on provider errors. It is for
// requests whose answer is not credited (fee quotes, simulations, submissions).
func first[T any](ctx context.Context, q *Quorum, f func(Provider) (T, error)) (T, error) {
	return qcore.First(ctx, q.c, f)
}

// VerifyGenesis checks every provider serves the expected cluster. A misconfigured URL would
// otherwise feed another cluster's transactions into credit.
func (q *Quorum) VerifyGenesis(ctx context.Context, want string) error {
	for _, m := range q.c.Members() {
		h, err := m.P.GenesisHash(ctx)
		if err != nil {
			return fmt.Errorf("%s provider %s: genesis hash: %w", q.c.Chain(), m.P.Name(), err)
		}
		if h != want {
			return fmt.Errorf("%s provider %s serves genesis %s, want %s", q.c.Chain(), m.P.Name(), h, want)
		}
	}
	return nil
}

// Slots is the agreed view of the cluster tip: for each level, the lower of the two providers.
type Slots struct {
	Confirmed   uint64
	Finalized   uint64
	BlockHeight uint64 // at the confirmed commitment
}

// Slots returns the slots at each commitment that the providers agree on.
func (q *Quorum) Slots(ctx context.Context) (Slots, error) {
	got, err := ask(ctx, q, "slots", func(p Provider) (Slots, error) {
		var s Slots
		var err error
		if s.Confirmed, err = p.Slot(ctx, solrpc.Confirmed); err != nil {
			return s, err
		}
		if s.Finalized, err = p.Slot(ctx, solrpc.Finalized); err != nil {
			return s, err
		}
		s.BlockHeight, err = p.BlockHeight(ctx, solrpc.Confirmed)
		return s, err
	})
	if err != nil {
		return Slots{}, err
	}
	a, b := got[0], got[1]
	if a.Err != nil || b.Err != nil {
		return Slots{}, fmt.Errorf("slots on %s: %w", q.c.Chain(), ErrLag)
	}
	q.c.AfterSuccess()
	return Slots{Confirmed: min(a.V.Confirmed, b.V.Confirmed), Finalized: min(a.V.Finalized, b.V.Finalized),
		BlockHeight: min(a.V.BlockHeight, b.V.BlockHeight)}, nil
}

// BlockHeight is the agreed block height at the confirmed commitment: one getBlockHeight per
// provider, the lower of the two. It bounds how long a transaction can still land, so one
// provider running ahead can never make a transaction look expired.
func (q *Quorum) BlockHeight(ctx context.Context) (uint64, error) {
	return q.lowest(ctx, "block height", func(p Provider) (uint64, error) { return p.BlockHeight(ctx, solrpc.Confirmed) })
}

// Tip is the idle heartbeat: one getSlot (confirmed) per provider, the lower of the two.
func (q *Quorum) Tip(ctx context.Context) (uint64, error) {
	return q.lowest(ctx, "slot", func(p Provider) (uint64, error) { return p.Slot(ctx, solrpc.Confirmed) })
}

// lowest asks two providers for a height and returns the lower one (ErrLag when either has none).
func (q *Quorum) lowest(ctx context.Context, what string, f func(Provider) (uint64, error)) (uint64, error) {
	got, err := ask(ctx, q, what, f)
	if err != nil {
		return 0, err
	}
	a, b := got[0], got[1]
	if a.Err != nil || b.Err != nil {
		return 0, fmt.Errorf("%s on %s: %w", what, q.c.Chain(), ErrLag)
	}
	q.c.AfterSuccess()
	return min(a.V, b.V), nil
}

// BlockHash returns the agreed blockhash at slot (solrpc.ErrSkipped when both say the slot
// was skipped).
func (q *Quorum) BlockHash(ctx context.Context, slot uint64, cm solrpc.Commitment) (string, error) {
	b, err := agree(ctx, q, fmt.Sprintf("block %d (%s)", slot, cm), func(p Provider) (solrpc.Block, error) {
		return p.Block(ctx, slot, cm)
	}, func(a, b solrpc.Block) bool { return a.Blockhash == b.Blockhash && a.ParentSlot == b.ParentSlot })
	return b.Blockhash, err
}

// Transaction returns a transaction both providers report identically at the commitment.
func (q *Quorum) Transaction(ctx context.Context, sig string, cm solrpc.Commitment) (*solrpc.Tx, error) {
	return agree(ctx, q, "transaction "+sig, func(p Provider) (*solrpc.Tx, error) {
		return p.Transaction(ctx, sig, cm)
	}, func(a, b *solrpc.Tx) bool { return a.Equal(b) })
}

// Signatures lists the signatures of addr newer than until from two providers, merged and
// sorted newest first. Signature lists only drive discovery: nothing is credited unless the
// transaction itself is agreed, so the union never trusts either side alone.
func (q *Quorum) Signatures(ctx context.Context, addr, until string, untilSlot uint64, page int) ([]solrpc.SignatureInfo, error) {
	sigs, _, err := q.SignaturesAt(ctx, addr, until, untilSlot, page, 0)
	return sigs, err
}

// SignaturesAt is Signatures asking each provider for a list at least at minSlot (getSignaturesForAddress
// minContextSlot). reached is false when neither provider had reached minSlot: the list may
// then miss transactions up to it (a provider behind answers nothing and is not cooled down).
func (q *Quorum) SignaturesAt(ctx context.Context, addr, until string, untilSlot uint64, page int, minSlot uint64) (sigs []solrpc.SignatureInfo, reached bool, err error) {
	if page <= 0 {
		page = 1000
	}
	got, err := ask(ctx, q, "signatures of "+addr, func(p Provider) ([]solrpc.SignatureInfo, error) {
		return listSignatures(ctx, p, addr, until, untilSlot, page, minSlot)
	})
	if err != nil {
		return nil, false, err
	}
	seen := map[string]int{}
	var out []solrpc.SignatureInfo
	for _, r := range got {
		reached = reached || r.Err == nil
		for _, s := range r.V {
			if i, ok := seen[s.Signature]; ok {
				// Keep the lower slot claim and the failure flag of either side: a tx is only
				// skipped as failed after the transaction itself is checked.
				if s.Slot < out[i].Slot {
					out[i].Slot = s.Slot
				}
				continue
			}
			seen[s.Signature] = len(out)
			out = append(out, s)
		}
	}
	q.c.AfterSuccess()
	sort.SliceStable(out, func(i, j int) bool { return out[i].Slot > out[j].Slot })
	return out, reached, nil
}

// listSignatures pages through one provider's signatures of addr newer than until (newest
// first), at the confirmed commitment and at least at minSlot (0: no bound).
func listSignatures(ctx context.Context, p Provider, addr, until string, untilSlot uint64, page int, minSlot uint64) ([]solrpc.SignatureInfo, error) {
	if page <= 0 {
		page = 1000
	}
	var all []solrpc.SignatureInfo
	before := ""
	for {
		sigs, err := p.Signatures(ctx, addr, solrpc.SignaturesOpts{Before: before, Until: until, Limit: page, Commitment: solrpc.Confirmed,
			MinContextSlot: minSlot})
		if err != nil {
			return nil, err
		}
		stop := len(sigs) < page
		for _, s := range sigs {
			// A cursor signature that vanished from the provider's history (or a provider
			// ignoring until) must not make us walk the whole history again.
			if until != "" && s.Slot < untilSlot {
				stop = true
				break
			}
			all = append(all, s)
		}
		if stop || len(sigs) == 0 {
			return all, nil
		}
		before = sigs[len(sigs)-1].Signature
	}
}

func sameAccount(a, b solrpc.Account) bool {
	return a.Exists == b.Exists && a.Owner == b.Owner && a.Lamports == b.Lamports && a.Executable == b.Executable &&
		slices.Equal(a.Data, b.Data)
}

// Account returns account state both providers agree on, read at commitment cm and at least at
// minContextSlot. Providers a slot apart may legitimately differ; that is lag, retried later.
// Only different state at the same slot halts.
func (q *Quorum) Account(ctx context.Context, addr string, cm solrpc.Commitment, minContextSlot uint64) (solrpc.Account, error) {
	accs, err := q.Accounts(ctx, []string{addr}, cm, minContextSlot)
	if err != nil {
		return solrpc.Account{}, err
	}
	return accs[0], nil
}

// Accounts returns the given accounts as agreed by the providers, answered no earlier than minContextSlot.
func (q *Quorum) Accounts(ctx context.Context, addrs []string, cm solrpc.Commitment, minContextSlot uint64) ([]solrpc.Account, error) {
	what := fmt.Sprintf("accounts %v", addrs)
	got, err := ask(ctx, q, what, func(p Provider) ([]solrpc.Account, error) {
		return p.MultipleAccounts(ctx, addrs, cm, minContextSlot)
	})
	if err != nil {
		return nil, err
	}
	a, b := got[0], got[1]
	if a.Err != nil || b.Err != nil {
		q.c.AfterSuccess()
		return nil, fmt.Errorf("%s on %s: %w", what, q.c.Chain(), ErrLag)
	}
	if slices.EqualFunc(a.V, b.V, sameAccount) {
		q.c.AfterSuccess()
		return a.V, nil
	}
	if len(a.V) > 0 && len(b.V) > 0 && a.V[0].ContextSlot != b.V[0].ContextSlot {
		q.c.AfterSuccess()
		return nil, fmt.Errorf("%s on %s: %w (read at slots %d and %d)", what, q.c.Chain(), ErrLag, a.V[0].ContextSlot, b.V[0].ContextSlot)
	}
	return nil, q.c.Disagree(what, a.M, b.M)
}

// LatestBlockhash returns a recent blockhash from the first provider that answers.
func (q *Quorum) LatestBlockhash(ctx context.Context) (solrpc.Blockhash, error) {
	return first(ctx, q, func(p Provider) (solrpc.Blockhash, error) { return p.LatestBlockhash(ctx, solrpc.Confirmed) })
}

// Simulate runs the transaction on the first provider that answers.
func (q *Quorum) Simulate(ctx context.Context, raw []byte, sigVerify bool) (solrpc.Simulation, error) {
	return first(ctx, q, func(p Provider) (solrpc.Simulation, error) { return p.Simulate(ctx, raw, sigVerify) })
}

// SignatureStatuses returns the statuses of sigs from the first provider that answers.
func (q *Quorum) SignatureStatuses(ctx context.Context, sigs []string) ([]*solrpc.SignatureStatus, error) {
	return first(ctx, q, func(p Provider) ([]*solrpc.SignatureStatus, error) { return p.SignatureStatuses(ctx, sigs) })
}

// Landed asks two providers whether sig is on chain without an error (at any commitment, with
// history): true when either says so. Any provider that cannot answer is an error, so a caller
// that must never miss a landing fails closed.
func (q *Quorum) Landed(ctx context.Context, sig string) (bool, error) {
	got, err := ask(ctx, q, "signature "+sig, func(p Provider) ([]*solrpc.SignatureStatus, error) {
		return p.SignatureStatuses(ctx, []string{sig})
	})
	if err != nil {
		return false, err
	}
	landed := false
	for _, r := range got {
		if r.Err != nil || len(r.V) != 1 {
			return false, fmt.Errorf("signature %s on %s: %w", sig, q.c.Chain(), ErrLag)
		}
		landed = landed || (r.V[0] != nil && !r.V[0].Failed())
	}
	q.c.AfterSuccess()
	return landed, nil
}

// IsBlockhashValid reports whether hash is still valid, from the first provider that answers.
func (q *Quorum) IsBlockhashValid(ctx context.Context, hash string) (bool, error) {
	return first(ctx, q, func(p Provider) (bool, error) { return p.IsBlockhashValid(ctx, hash) })
}

// Balance returns the confirmed lamport balance of addr from the first provider that answers.
func (q *Quorum) Balance(ctx context.Context, addr string) (uint64, error) {
	return first(ctx, q, func(p Provider) (uint64, error) { return p.Balance(ctx, addr, solrpc.Confirmed) })
}

// MinimumBalanceForRentExemption returns the rent-exempt minimum for size bytes from the first provider that answers.
func (q *Quorum) MinimumBalanceForRentExemption(ctx context.Context, size int) (uint64, error) {
	return first(ctx, q, func(p Provider) (uint64, error) { return p.MinimumBalanceForRentExemption(ctx, size) })
}

// FirstAccounts reads accounts from one provider (sizing decisions that are re-checked on chain).
func (q *Quorum) FirstAccounts(ctx context.Context, addrs []string) ([]solrpc.Account, error) {
	return first(ctx, q, func(p Provider) ([]solrpc.Account, error) {
		return p.MultipleAccounts(ctx, addrs, solrpc.Confirmed, 0)
	})
}

// Broadcast sends a signed transaction to every healthy provider. It succeeds when at least one
// accepted it (or had already processed it); the others' errors are returned for logging.
func (q *Quorum) Broadcast(ctx context.Context, raw []byte) (accepted int, errs []error) {
	return q.c.Broadcast(func(p Provider) error {
		_, err := p.SendTransaction(ctx, raw, true)
		return err
	}, func(err error) bool {
		var se *solrpc.SendError
		return err == nil || (errors.As(err, &se) && se.AlreadyProcessed())
	})
}

// Rotor sends single-provider reads (D49) to the first two healthy providers in turn: each call
// starts at the other one than the call before, and fails over to the next healthy provider when
// that one errs (the Solana counterpart of chainwatch.Rotor). It is for reads whose result is
// never credited on its own: balance change detection, signature lists that only lead to agreed
// transactions, the finalized slot that schedules an agreed re-check, signature statuses that
// are followed by agreed reads. Each caller keeps its own Rotor so its calls alternate.
type Rotor struct {
	r *qcore.Rotor[Provider]
}

// Rotor returns a new round-robin reader over the quorum's providers.
func (q *Quorum) Rotor() *Rotor { return &Rotor{r: qcore.NewRotor(q.c)} }

// rotate runs f on one provider: the next of the first two healthy ones, failing over on an error
// that is not an answer about the chain. A provider behind the minContextSlot asked for
// (solrpc.ErrBehind) is asked no further and the next one answers instead; it is not cooled down
// (a slot or two behind is normal and over in a moment). Only when every provider is behind is
// ErrBehind returned.
func rotate[T any](ctx context.Context, r *Rotor, f func(Provider) (T, error)) (T, error) {
	v, _, err := qcore.Rotate(ctx, r.r, f)
	return v, err
}

// Accounts reads accounts at the confirmed commitment, at least at minContextSlot, from one
// provider (getMultipleAccounts, at most 100 addresses).
func (r *Rotor) Accounts(ctx context.Context, addrs []string, minContextSlot uint64) ([]solrpc.Account, error) {
	return rotate(ctx, r, func(p Provider) ([]solrpc.Account, error) {
		accs, err := p.MultipleAccounts(ctx, addrs, solrpc.Confirmed, minContextSlot)
		if err == nil && len(accs) != len(addrs) {
			return nil, fmt.Errorf("%w: %d accounts for %d addresses", solrpc.ErrTransport, len(accs), len(addrs))
		}
		return accs, err
	})
}

// Slot reads the slot at commitment cm from one provider.
func (r *Rotor) Slot(ctx context.Context, cm solrpc.Commitment) (uint64, error) {
	return rotate(ctx, r, func(p Provider) (uint64, error) { return p.Slot(ctx, cm) })
}

// Signatures lists the signatures of addr newer than until from one provider, newest first.
func (r *Rotor) Signatures(ctx context.Context, addr, until string, untilSlot uint64, page int) ([]solrpc.SignatureInfo, error) {
	return rotate(ctx, r, func(p Provider) ([]solrpc.SignatureInfo, error) {
		return listSignatures(ctx, p, addr, until, untilSlot, page, 0)
	})
}

// SignatureStatuses reads the statuses of sigs from one provider.
func (r *Rotor) SignatureStatuses(ctx context.Context, sigs []string) ([]*solrpc.SignatureStatus, error) {
	return rotate(ctx, r, func(p Provider) ([]*solrpc.SignatureStatus, error) {
		sts, err := p.SignatureStatuses(ctx, sigs)
		if err == nil && len(sts) != len(sigs) {
			return nil, fmt.Errorf("%w: %d statuses for %d signatures", solrpc.ErrTransport, len(sts), len(sigs))
		}
		return sts, err
	})
}

// SignaturesAfter lists the signatures of addr newer than until and not below slot after (newest
// first) from one provider, paging. It bounds a history that has no cursor yet by a slot.
func (r *Rotor) SignaturesAfter(ctx context.Context, addr, until string, after uint64, page int) ([]solrpc.SignatureInfo, error) {
	if page <= 0 {
		page = 1000
	}
	return rotate(ctx, r, func(p Provider) ([]solrpc.SignatureInfo, error) {
		var all []solrpc.SignatureInfo
		before := ""
		for {
			sigs, err := p.Signatures(ctx, addr, solrpc.SignaturesOpts{Before: before, Until: until, Limit: page, Commitment: solrpc.Confirmed})
			if err != nil {
				return nil, err
			}
			for _, s := range sigs {
				if s.Signature == until || s.Slot < after {
					return all, nil
				}
				all = append(all, s)
			}
			if len(sigs) < page {
				return all, nil
			}
			before = sigs[len(sigs)-1].Signature
		}
	})
}

// Transaction reads a transaction at the confirmed commitment from one provider. It only finds
// accounts to rescan; nothing is credited from it.
func (r *Rotor) Transaction(ctx context.Context, sig string) (*solrpc.Tx, error) {
	return rotate(ctx, r, func(p Provider) (*solrpc.Tx, error) { return p.Transaction(ctx, sig, solrpc.Confirmed) })
}

// Probe is the idle heartbeat: one getSlot from one provider, alternating, failing over to the
// next one on an error. A provider that failed is cooled down and the chain's status shows it
// (Degraded; a halted chain stays halted until an agreed read). An error means every provider
// failed.
func (r *Rotor) Probe(ctx context.Context) error {
	_, err := r.Slot(ctx, solrpc.Confirmed)
	r.r.Core().AfterProbe()
	return err
}
