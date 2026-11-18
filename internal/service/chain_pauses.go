package service

import (
	"context"
	"log/slog"
	"slices"
	"time"

	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/chainpause"
	"github.com/prismaport/prismaport/internal/store"
	"github.com/prismaport/prismaport/internal/view"
)

// The runtime pause of a chain or of one asset on it (D64). Two layers decide what a payer is
// offered: the registry (configured: PRISMAPORT_CHAINS_OFF, RPC, contracts, D62) and the pauses
// (enabled: chain_pauses, read again about every 30 s). A paused chain or asset is gone from
// everything new (new sessions, payment_methods, new customer addresses, first reveals, wallet
// payments) while the worker keeps watching, crediting and flushing it: a session already open
// keeps its deposit address and says the network is paused, and so does a customer address.

// CodeChainUnavailable refuses something new on a paused chain or asset.
const CodeChainUnavailable = "chain_unavailable"

func chainUnavailable(format string, args ...any) *Error {
	return newErr(KindConflict, CodeChainUnavailable, format, args...)
}

// noUnpausedMethods refuses a session or customer address whose every payment method is paused.
func noUnpausedMethods() *Error {
	return chainUnavailable("payments on every network this product takes are paused right now; try again later")
}

// ChainPauses are the pauses in force, as this process last read them (at most the refresh
// interval ago).
func (s *Service) ChainPauses(ctx context.Context) (chainpause.Set, error) {
	return s.pauses.Set(ctx)
}

// unpaused narrows methods to the (chain, asset) pairs no pause takes out.
func unpaused(methods []store.ChainAsset, pauses chainpause.Set) []store.ChainAsset {
	if pauses.Empty() {
		return methods
	}
	var out []store.ChainAsset
	for _, x := range methods {
		if !pauses.Paused(x.Chain, x.Asset) {
			out = append(out, x)
		}
	}
	return out
}

// checkRevealable refuses the first reveal of a network whose every entry in the session is
// paused (D64): the payer picks another network. A network already revealed stays revealable,
// so a repeat answers the same details.
func (s *Service) checkRevealable(ctx context.Context, ses *store.Session, chainID string) error {
	if ses.Revealed(chainID) != nil {
		return nil
	}
	pauses, err := s.ChainPauses(ctx)
	if err != nil || pauses.Empty() {
		return err
	}
	for _, p := range ses.PayTo {
		if p.Chain == chainID && !pauses.Paused(p.Chain, p.Asset) {
			return nil
		}
	}
	if !offers(ses, chainID) {
		return nil
	}
	return chainUnavailable("payments on %s are paused right now; pay on another network", view.ChainName(chainID))
}

// CheckPayable refuses a wallet payment (EIP-3009 or a Solana connected wallet) of asset on a
// paused chain or asset (D64): the deposit address stays shown and watched, the wallet path does not.
func (s *Service) CheckPayable(ctx context.Context, chainID, asset string) error {
	pauses, err := s.ChainPauses(ctx)
	if err != nil {
		return err
	}
	if pauses.Paused(chainID, asset) {
		return chainUnavailable("payments of %s on %s are paused right now; pay on another network", asset, view.ChainName(chainID))
	}
	return nil
}

// checkPausable resolves a pause's chain and asset in the registry this process serves: a chain
// switched off (D62) or not known is refused, so neither a pause nor a resume can name it.
func (s *Service) checkPausable(chainID, asset string) error {
	c, ok := s.cfg.Registry.Get(chainID)
	if !ok {
		if _, known := chain.Pilot().Get(chainID); known {
			return invalid("%s is not served in this environment (switched off or not configured); a resume cannot turn it on", chainID)
		}
		return invalid("%s is not a chain: use an id such as base, base-sepolia, solana, solana-devnet", chainID)
	}
	if asset == "" {
		return nil
	}
	if _, ok := c.Asset(asset); !ok {
		var have []string
		for _, a := range c.Assets {
			have = append(have, a.Symbol)
		}
		return invalid("%s has no asset %s (it has %v)", chainID, asset, have)
	}
	return nil
}

// PauseChain pauses a chain (asset empty) or one asset on it from now on, recording actor. The
// api and the worker take it within their refresh interval. It reports false when that pause
// was already in force.
func (s *Service) PauseChain(ctx context.Context, chainID, asset, actor string) (bool, error) {
	if err := s.checkPausable(chainID, asset); err != nil {
		return false, err
	}
	return s.store.PauseChain(ctx, chainID, asset, actor)
}

// ResumeChain ends a pause PauseChain set, recording actor. Only a chain this environment
// serves can be resumed: a resume never brings back a chain switched off or not configured. It
// reports false when no such pause was in force.
func (s *Service) ResumeChain(ctx context.Context, chainID, asset, actor string) (bool, error) {
	if err := s.checkPausable(chainID, asset); err != nil {
		return false, err
	}
	return s.store.ResumeChain(ctx, chainID, asset, actor)
}

// ChainPauseView is a pause as the dashboard shows it: the network, the asset (null: the whole
// network) and since when. Who paused it is never shown.
type ChainPauseView struct {
	Chain     string  `json:"chain"`
	ChainName string  `json:"chain_name"`
	Asset     *string `json:"asset"`
	Livemode  bool    `json:"livemode"`
	PausedAt  string  `json:"paused_at"`
}

// DashChainPauses lists the pauses in force that affect the merchant, in both modes: a paused
// asset it accepts, or a paused chain where it accepts an asset or has an active wallet of the
// chain's family. A merchant no pause affects gets an empty list, so its dashboard shows no strip.
func (s *Service) DashChainPauses(ctx context.Context, merchantID string) ([]ChainPauseView, error) {
	out := []ChainPauseView{}
	pauses, err := s.ChainPauses(ctx)
	if err != nil || pauses.Empty() {
		return out, err
	}
	accepted := map[bool][]store.ChainAsset{}
	wallets := map[bool][]store.Wallet{}
	for _, live := range []bool{true, false} {
		if accepted[live], err = s.store.AcceptedAssets(ctx, merchantID, live); err != nil {
			return nil, err
		}
		if wallets[live], err = s.store.ListWallets(ctx, merchantID, live, store.WalletFilter{}); err != nil {
			return nil, err
		}
	}
	for _, p := range pauses.List() {
		c, ok := s.cfg.Registry.Get(p.Chain)
		if !ok {
			continue
		}
		hit := slices.ContainsFunc(accepted[c.Livemode], func(x store.ChainAsset) bool {
			return x.Chain == p.Chain && (p.Asset == "" || x.Asset == p.Asset)
		})
		if !hit && p.Asset == "" {
			hit = slices.ContainsFunc(wallets[c.Livemode], func(w store.Wallet) bool { return w.Family == c.Family })
		}
		if !hit {
			continue
		}
		v := ChainPauseView{Chain: p.Chain, ChainName: view.ChainName(p.Chain), Livemode: c.Livemode, PausedAt: rfc3339(p.PausedAt)}
		if p.Asset != "" {
			asset := p.Asset
			v.Asset = &asset
		}
		out = append(out, v)
	}
	return out, nil
}

// ChainStatus is one (chain, asset) of the registry this process serves and its pause.
type ChainStatus struct {
	Chain    string
	Livemode bool
	Asset    string
	// Pause is the pause that takes the asset out (the chain's own first), nil when enabled.
	Pause *chainpause.Pause
}

// ChainStatuses lists every (chain, asset) this environment serves with the pause in force on it,
// read from the database now (not through the refresh interval). Pauses of chains it does not
// serve are returned apart, as stale.
func (s *Service) ChainStatuses(ctx context.Context) ([]ChainStatus, []chainpause.Pause, error) {
	list, err := s.store.ChainPauses(ctx)
	if err != nil {
		return nil, nil, err
	}
	var out []ChainStatus
	for _, live := range []bool{true, false} {
		for _, c := range s.cfg.Registry.ForMode(live) {
			for _, a := range c.Assets {
				st := ChainStatus{Chain: c.ID, Livemode: live, Asset: a.Symbol}
				for i := range list {
					if p := list[i]; p.Chain == c.ID && (p.Asset == "" || p.Asset == a.Symbol) && (st.Pause == nil || p.Asset == "") {
						st.Pause = &list[i]
					}
				}
				out = append(out, st)
			}
		}
	}
	var stale []chainpause.Pause
	for _, p := range list {
		if _, ok := s.cfg.Registry.Get(p.Chain); !ok {
			stale = append(stale, p)
		}
	}
	return out, stale, nil
}

// pauseReader builds the service's reader over st's chain_pauses.
func pauseReader(st *store.Store, refresh *time.Duration, log func() *slog.Logger) *chainpause.Reader {
	d := chainpause.DefaultRefresh
	if refresh != nil {
		d = *refresh
	}
	r := chainpause.NewReader(st.ChainPauses, d, st.Now)
	r.Stale = func(err error) {
		log().Warn("chain pauses not read again; serving the last copy", "err", err)
	}
	return r
}
