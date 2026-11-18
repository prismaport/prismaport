package app

import (
	"context"
	"fmt"
	"time"

	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/chainwatch"
	"github.com/prismaport/prismaport/internal/checkout"
	"github.com/prismaport/prismaport/internal/destination"
	"github.com/prismaport/prismaport/internal/evmrpc"
	"github.com/prismaport/prismaport/internal/service"
	"github.com/prismaport/prismaport/internal/solana"
	"github.com/prismaport/prismaport/internal/solrpc"
	"github.com/prismaport/prismaport/internal/solwatch"
	"github.com/prismaport/prismaport/internal/store"
)

// rpcTimeout bounds one request to a chain's provider.
const rpcTimeout = 10 * time.Second

// providers opens one client per RPC URL of a chain, named "<chain>#<n>": URLs often embed API
// keys and are never logged.
func providers[P any](id string, urls []string, open func(name, url string) P) []P {
	var ps []P
	for i, u := range urls {
		ps = append(ps, open(fmt.Sprintf("%s#%d", id, i+1), u))
	}
	return ps
}

// Quorums builds one provider quorum per chain with at least two RPC URLs. Provider names are
// "<chain>#<n>": URLs often embed API keys and are never logged.
func (e Root) Quorums(onStatus func(chainwatch.Status)) map[string]*chainwatch.Quorum {
	out := map[string]*chainwatch.Quorum{}
	for id, c := range e.Chains {
		if !c.HasRPC() {
			continue
		}
		ps := providers(id, c.RPCURLs, func(name, url string) chainwatch.Provider { return evmrpc.New(name, url, rpcTimeout) })
		out[id] = chainwatch.NewQuorum(id, ps, chainwatch.Options{OnStatus: onStatus})
	}
	return out
}

// SolanaQuorums builds one provider quorum per Solana cluster with at least two RPC URLs.
// Provider names are "<chain>#<n>": URLs often embed API keys and are never logged.
func (e Root) SolanaQuorums(onStatus func(chainwatch.Status)) map[string]*solwatch.Quorum {
	out := map[string]*solwatch.Quorum{}
	for id, c := range e.Solana {
		if !c.HasRPC() {
			continue
		}
		ps := providers(id, c.RPCURLs, func(name, url string) solwatch.Provider { return solrpc.New(name, url, rpcTimeout) })
		out[id] = solwatch.NewQuorum(id, ps, chainwatch.Options{OnStatus: onStatus})
	}
	return out
}

// Checkout builds the public checkout service over svc and st with the signed path on every
// chain that has a Gateway and providers.
func (e Root) Checkout(svc *service.Service, st *store.Store) *checkout.Checkout {
	quorums := e.Quorums(nil)
	chains := map[string]checkout.Chain{}
	for id, c := range e.Chains {
		reg, ok := svc.Registry().Get(id)
		if !ok {
			continue
		}
		// The detector's count (Runtime builds the same policy); the safe head has none to show.
		conf := c.Confirmations
		if c.UseSafeHead {
			conf = 0
		} else if conf == 0 {
			conf = chainwatch.DefaultConfirmations
		}
		chains[id] = checkout.Chain{Chain: reg, Gateway: c.Gateway, Quorum: quorums[id], Confirmations: conf}
	}
	return e.withSolana(checkout.New(svc, st, chains), svc)
}

// withSolana enables connected-wallet payments on every cluster with providers and real
// addresses, when the fee payer key is configured (otherwise those endpoints answer 503).
func (e Root) withSolana(co *checkout.Checkout, svc *service.Service) *checkout.Checkout {
	quorums := e.SolanaQuorums(nil)
	if len(quorums) == 0 {
		return co
	}
	key, err := e.SolanaGasKey()
	if err != nil {
		return co
	}
	chains := map[string]checkout.SolanaChain{}
	for id, q := range quorums {
		c := e.Solana[id]
		reg, ok := svc.Registry().Get(id)
		if !ok || !c.Addresses() {
			continue
		}
		chains[id] = checkout.SolanaChain{Chain: reg, Quorum: q, FeePayer: key, PriorityFee: c.PriorityFee}
	}
	return co.WithSolana(chains)
}

// solanaInspector classifies destination accounts over the clusters' providers.
type solanaInspector struct {
	reg     *chain.Registry
	quorums map[string]*solwatch.Quorum
}

func (s solanaInspector) Inspect(ctx context.Context, livemode bool, pubkey string) (destination.SolanaAccountKind, error) {
	for _, c := range s.reg.ForMode(livemode) {
		q, ok := s.quorums[c.ID]
		if c.Family != chain.Solana || !ok {
			continue
		}
		acc, err := q.Account(ctx, pubkey, solrpc.Confirmed, 0)
		if err != nil {
			return destination.SolanaUnknown, err
		}
		switch {
		case !acc.Exists || acc.Owner == solana.SystemProgramID.String():
			return destination.SolanaWallet, nil
		case acc.Executable:
			return destination.SolanaProgram, nil
		case acc.Owner == solana.TokenProgramID.String() || acc.Owner == solana.Token2022ProgramID.String():
			if len(acc.Data) == solana.MintSize {
				return destination.SolanaMint, nil
			}
			return destination.SolanaTokenAccount, nil
		}
		return destination.SolanaOther, nil
	}
	return destination.SolanaUnknown, destination.ErrUnverifiable
}

// MissingTokenAccounts checks the wallet's associated token account for every mint of every
// Solana chain of the mode, agreed by two providers; one the issuer froze is reported Frozen. A
// chain without providers makes the whole check unverifiable (fail closed).
func (s solanaInspector) MissingTokenAccounts(ctx context.Context, livemode bool, wallet string) ([]destination.MissingTokenAccount, error) {
	owner, err := solana.ParsePublicKey(wallet)
	if err != nil {
		return nil, err
	}
	var missing []destination.MissingTokenAccount
	for _, c := range s.reg.ForMode(livemode) {
		if c.Family != chain.Solana {
			continue
		}
		q, ok := s.quorums[c.ID]
		if !ok {
			return nil, fmt.Errorf("%s: %w", c.ID, destination.ErrUnverifiable)
		}
		mints := make([]solana.PublicKey, len(c.Assets))
		atas := make([]string, len(c.Assets))
		for i, a := range c.Assets {
			if mints[i], err = solana.ParsePublicKey(a.Token); err != nil {
				return nil, err
			}
			ata, err := solana.AssociatedTokenAddress(owner, mints[i])
			if err != nil {
				return nil, err
			}
			atas[i] = ata.String()
		}
		if len(atas) == 0 {
			continue
		}
		accs, err := q.Accounts(ctx, atas, solrpc.Confirmed, 0)
		if err != nil {
			return nil, err
		}
		for i, acc := range accs {
			if !acc.Exists || !solana.TokenAccountUsable(acc.Owner, acc.Data, mints[i]) {
				missing = append(missing, destination.MissingTokenAccount{Chain: c.ID, Asset: c.Assets[i].Symbol, Mint: c.Assets[i].Token, Account: atas[i],
					Frozen: acc.Exists && solana.TokenAccountFrozen(acc.Owner, acc.Data, mints[i])})
			}
		}
	}
	return missing, nil
}

func (e Root) solanaValidator(reg *chain.Registry) destination.SolanaInspector {
	q := e.SolanaQuorums(nil)
	if len(q) == 0 {
		return destination.Unavailable{}
	}
	return solanaInspector{reg: reg, quorums: q}
}
