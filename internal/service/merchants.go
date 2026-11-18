package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/prismaport/prismaport/internal/apikey"
	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/destination"
	"github.com/prismaport/prismaport/internal/store"
)

// SeedInput describes a merchant to create in one step: its name, mode, payout destinations and who
// asked for it.
type SeedInput struct {
	Name              string
	Livemode          bool
	EVMDestination    string
	SolanaDestination string
	AllowUnverified   bool
	Actor             string
}

// SeedResult is what SeedMerchant created, including the API key's plaintext, which is shown only
// once.
type SeedResult struct {
	Merchant  store.Merchant
	Wallets   []store.Wallet
	Assets    []store.ChainAsset
	APIKey    store.APIKey
	Plaintext string
}

// SeedMerchant creates a pilot merchant with one wallet per given destination (each the 100%
// default split of its family), every supported asset of those families, and one API key with
// all scopes.
func (s *Service) SeedMerchant(ctx context.Context, in SeedInput) (SeedResult, error) {
	if in.EVMDestination == "" && in.SolanaDestination == "" {
		return SeedResult{}, fmt.Errorf("at least one destination is required")
	}
	// Validate before creating anything so a bad address leaves no half-seeded merchant.
	for fam, addr := range map[chain.Family]string{chain.EVM: in.EVMDestination, chain.Solana: in.SolanaDestination} {
		if addr == "" {
			continue
		}
		if !s.familyServed(fam) {
			return SeedResult{}, fmt.Errorf("%s destination: %s is switched off here (PRISMAPORT_CHAINS_OFF)", fam, familyName(fam))
		}
		opt := destination.Options{Livemode: in.Livemode, AllowUnverified: in.AllowUnverified}
		canonical, err := s.cfg.Validator.Validate(ctx, fam, addr, opt)
		if err != nil {
			return SeedResult{}, fmt.Errorf("%s destination: %w", fam, err)
		}
		if err := s.cfg.Validator.CheckBlacklist(ctx, in.Livemode, fam, canonical); err != nil &&
			!(in.AllowUnverified && errors.Is(err, destination.ErrUnverifiable)) {
			return SeedResult{}, fmt.Errorf("%s destination: %w", fam, err)
		}
	}
	m, err := s.store.CreateMerchant(ctx, in.Name, in.Actor)
	if err != nil {
		return SeedResult{}, err
	}
	res := SeedResult{Merchant: m}
	families := map[chain.Family]bool{}
	for _, d := range []struct {
		fam  chain.Family
		addr string
	}{{chain.EVM, in.EVMDestination}, {chain.Solana, in.SolanaDestination}} {
		if d.addr == "" {
			continue
		}
		w, err := s.SetDestination(ctx, m.ID, in.Livemode, d.fam, d.addr, in.AllowUnverified, in.Actor)
		if err != nil {
			return res, err
		}
		families[d.fam] = true
		res.Wallets = append(res.Wallets, w)
	}
	for _, c := range s.cfg.Registry.ForMode(in.Livemode) {
		if !families[c.Family] {
			continue
		}
		for _, a := range c.Assets {
			res.Assets = append(res.Assets, store.ChainAsset{Chain: c.ID, Asset: a.Symbol})
		}
	}
	if err := s.store.SetAcceptedAssets(ctx, m.ID, in.Livemode, res.Assets, in.Actor); err != nil {
		return res, err
	}
	key, plaintext, err := s.CreateAPIKey(ctx, m.ID, in.Livemode, nil, in.Actor)
	if err != nil {
		return res, err
	}
	res.APIKey, res.Plaintext = key, plaintext
	return res, nil
}

// CreateAPIKey issues a key; nil scopes means all scopes. The plaintext is returned once.
func (s *Service) CreateAPIKey(ctx context.Context, merchantID string, livemode bool, scopes []string, actor string) (store.APIKey, string, error) {
	if scopes == nil {
		for _, sc := range apikey.AllScopes {
			scopes = append(scopes, string(sc))
		}
	}
	if len(scopes) == 0 {
		return store.APIKey{}, "", fmt.Errorf("at least one scope is required")
	}
	for _, sc := range scopes {
		if !apikey.ValidScope(sc) {
			return store.APIKey{}, "", fmt.Errorf("unknown scope %q", sc)
		}
	}
	g, err := apikey.Generate(livemode)
	if err != nil {
		return store.APIKey{}, "", err
	}
	k, err := s.store.InsertAPIKey(ctx, merchantID, livemode, g.Hash, g.Prefix, scopes, actor)
	if err != nil {
		return store.APIKey{}, "", err
	}
	return k, g.Plaintext, nil
}
