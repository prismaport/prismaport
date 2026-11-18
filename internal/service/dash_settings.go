package service

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/destination"
	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/store"
)

// AcceptedAsset is one chain and asset the merchant accepts.
type AcceptedAsset struct {
	Chain   string `json:"chain"`
	Asset   string `json:"asset"`
	Enabled bool   `json:"enabled"`
}

// FeeView is the fee a chain charges: basis points of the amount plus a fixed fee.
type FeeView struct {
	FeeBps   uint16 `json:"fee_bps"`
	FixedFee string `json:"fixed_fee"`
}

// Settings are the account settings of a mode. Wallets have their own endpoints, and where the
// money goes is set on each product. ProductNetworks lists every network each active product of
// the mode takes and how (D60), so Settings can count the products a stablecoin serves and warn
// before one is turned off.
type Settings struct {
	AcceptedAssets  []AcceptedAsset          `json:"accepted_assets"`
	Fee             FeeView                  `json:"fee"`
	ProductNetworks []SettingsProductNetwork `json:"product_networks"`
}

// SettingsProductNetwork is one network an active product takes: accept all (every stablecoin
// on in Settings for that chain, turned on later included) or some (only Assets, of which the
// ones off in Settings are not offered).
type SettingsProductNetwork struct {
	ProductID   string   `json:"product_id"`
	ProductName string   `json:"product_name"`
	Chain       string   `json:"chain"`
	Accept      string   `json:"accept"`
	Assets      []string `json:"assets,omitempty"`
}

// pilotAllowed reports whether the pilot policy allows accepting asset on c: the registry is
// narrowed to that one combination and checked by the same rule that guards the registry.
func pilotAllowed(c chain.Chain, a chain.Asset) bool {
	one := c
	one.Assets = []chain.Asset{a}
	reg, err := chain.NewRegistry([]chain.Chain{one}, nil)
	return err == nil && chain.CheckPilotPolicy(reg) == nil
}

// OfferedAssets lists every (chain, asset) of the mode a merchant may accept.
func (s *Service) OfferedAssets(livemode bool) []AcceptedAsset {
	var out []AcceptedAsset
	for _, c := range s.cfg.Registry.ForMode(livemode) {
		for _, a := range c.Assets {
			if pilotAllowed(c, a) {
				out = append(out, AcceptedAsset{Chain: c.ID, Asset: a.Symbol})
			}
		}
	}
	return out
}

func acceptedView(offered []AcceptedAsset, current []store.ChainAsset) []AcceptedAsset {
	on := map[store.ChainAsset]bool{}
	for _, x := range current {
		on[x] = true
	}
	out := make([]AcceptedAsset, 0, len(offered))
	for _, o := range offered {
		o.Enabled = on[store.ChainAsset{Chain: o.Chain, Asset: o.Asset}]
		out = append(out, o)
	}
	return out
}

// DashSettings returns the merchant's settings in the auth's mode: accepted assets, fee and the
// networks its active products take.
func (s *Service) DashSettings(ctx context.Context, a Auth) (Settings, error) {
	var out Settings
	current, err := s.store.AcceptedAssets(ctx, a.MerchantID, a.Livemode)
	if err != nil {
		return Settings{}, err
	}
	out.AcceptedAssets = acceptedView(s.OfferedAssets(a.Livemode), current)
	chains := s.cfg.Registry.ForMode(a.Livemode)
	var fixedMicros uint64
	for _, c := range chains {
		out.Fee.FeeBps = max(out.Fee.FeeBps, c.Fee.FeeBps)
		fixedMicros = max(fixedMicros, c.Fee.FixedFeeMicros)
	}
	out.Fee.FixedFee = money.FormatMinPlaces(money.FromUint64(fixedMicros), 6, 2)
	networks, err := s.store.ActiveProductNetworks(ctx, a.MerchantID, a.Livemode)
	if err != nil {
		return Settings{}, err
	}
	out.ProductNetworks = []SettingsProductNetwork{}
	for _, n := range networks {
		v := SettingsProductNetwork{ProductID: n.ProductID, ProductName: n.ProductName, Chain: n.Chain, Accept: string(n.Choice.Accept)}
		if n.Choice.Accept == store.AcceptSome {
			v.Assets = n.Choice.Assets
		}
		out.ProductNetworks = append(out.ProductNetworks, v)
	}
	return out, nil
}

// destinationError explains a refused address. Every refusal is a 400 except when the on-chain
// checks could not run, which is ours to fix (503).
func destinationError(err error) error {
	msg := ""
	switch {
	case errors.Is(err, destination.ErrUnverifiable):
		return &Error{Kind: KindUnavailable, Code: CodeServiceUnavailable, Cause: err,
			Message: "The address could not be checked on chain right now. Try again in a few minutes."}
	case errors.Is(err, destination.ErrChecksum):
		msg = "The address checksum does not match. Copy the address again from your wallet."
	case errors.Is(err, destination.ErrZeroAddress):
		msg = "The zero address cannot receive payments."
	case errors.Is(err, destination.ErrDenylisted):
		msg = "This is a token or protocol contract, not a wallet. Paste your wallet address."
	case errors.Is(err, destination.ErrTokenAccount):
		msg = "This is a token account, not a wallet. Paste the wallet (owner) address."
	case errors.Is(err, destination.ErrMint):
		msg = "This is a token mint, not a wallet."
	case errors.Is(err, destination.ErrProgram):
		msg = "This is a program, not a wallet."
	case errors.Is(err, destination.ErrCodeMismatch):
		msg = "This contract exists on some of the accepted networks but not on all of them."
	case errors.Is(err, destination.ErrTokenAccountMissing):
		msg = "This wallet has no token account for every accepted asset. Receive any amount of each token once, then try again."
		var missing *destination.MissingTokenAccountsError
		if errors.As(err, &missing) {
			msg = "This wallet has no token account for " + missing.Assets() + ". Receive any amount of each of these tokens at this wallet once, then try again."
		}
	case errors.Is(err, destination.ErrFormat):
		msg = "This is not a valid address for this network family."
	case errors.Is(err, destination.ErrBlacklisted):
		return &Error{Kind: KindInvalid, Code: CodeWalletBlacklisted, Cause: err,
			Message: "The USDC issuer has blacklisted this address, so payments to it can never settle."}
	case errors.Is(err, destination.ErrTokenAccountFrozen):
		return frozenError(err, "this wallet")
	default:
		return err
	}
	return &Error{Kind: KindInvalid, Code: CodeInvalidRequest, Message: msg, Cause: err}
}

// DashSetAcceptedAssets turns (chain, asset) pairs on or off; pairs not listed keep their state.
// Only pairs the pilot policy allows can be listed, and at least one must stay on.
func (s *Service) DashSetAcceptedAssets(ctx context.Context, a Auth, items []AcceptedAsset) ([]AcceptedAsset, error) {
	if len(items) == 0 {
		return nil, invalid("items must list at least one asset")
	}
	offered := s.OfferedAssets(a.Livemode)
	allowed := map[store.ChainAsset]bool{}
	for _, o := range offered {
		allowed[store.ChainAsset{Chain: o.Chain, Asset: o.Asset}] = true
	}
	seen := map[store.ChainAsset]bool{}
	for _, it := range items {
		k := store.ChainAsset{Chain: it.Chain, Asset: it.Asset}
		if !allowed[k] {
			return nil, invalid("%s %s cannot be accepted in %s mode", it.Asset, it.Chain, modeName(a.Livemode))
		}
		if seen[k] {
			return nil, invalid("%s %s is listed twice", it.Asset, it.Chain)
		}
		seen[k] = true
	}
	var result []store.ChainAsset
	err := pgx.BeginFunc(ctx, s.store.Pool(), func(tx pgx.Tx) error {
		current, err := s.store.LockAcceptedAssets(ctx, tx, a.MerchantID, a.Livemode)
		if err != nil {
			return err
		}
		on := map[store.ChainAsset]bool{}
		for _, x := range current {
			if allowed[x] {
				on[x] = true
			}
		}
		for _, it := range items {
			on[store.ChainAsset{Chain: it.Chain, Asset: it.Asset}] = it.Enabled
		}
		result = nil
		for _, o := range offered {
			k := store.ChainAsset{Chain: o.Chain, Asset: o.Asset}
			if on[k] {
				result = append(result, k)
			}
		}
		if len(result) == 0 {
			return invalid("keep at least one asset enabled")
		}
		return s.store.Bind(tx).SetAcceptedAssets(ctx, a.MerchantID, a.Livemode, result, a.actor())
	})
	if err != nil {
		return nil, err
	}
	return acceptedView(offered, result), nil
}
