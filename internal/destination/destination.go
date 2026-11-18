// Package destination validates merchant payout addresses. A destination is baked immutably into
// every forwarder created for it, so a mistake here is a permanent loss: validation is strict and
// fails closed when a check cannot be performed.
package destination

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/prismaport/prismaport/internal/base58"
	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/evm"
)

// Errors returned when a payout destination fails validation.
var (
	ErrFormat       = errors.New("destination: invalid address format")
	ErrChecksum     = errors.New("destination: EIP-55 checksum mismatch")
	ErrZeroAddress  = errors.New("destination: zero address")
	ErrDenylisted   = errors.New("destination: address is a known token, mint or protocol contract")
	ErrTokenAccount = errors.New("destination: address is a token account, not a wallet")
	ErrMint         = errors.New("destination: address is a token mint")
	ErrProgram      = errors.New("destination: address is an executable program")
	ErrCodeMismatch = errors.New("destination: contract on some enabled EVMs but not on others")
	// ErrTokenAccountMissing means the Solana wallet has no token account for an enabled mint.
	// Flushes never pay rent to create one for a merchant, so the wallet must hold it first.
	ErrTokenAccountMissing = errors.New("destination: Solana wallet has no token account for an accepted asset")
	// ErrTokenAccountFrozen means the issuer froze the wallet's token account for an enabled mint:
	// a flush paying it fails, and one frozen recipient blocks a whole split (ADR 0008 I-2). It is
	// a definite refusal in both modes.
	ErrTokenAccountFrozen = errors.New("destination: the token issuer froze this wallet's token account")
	// ErrUnverifiable means an on-chain check could not run. Callers may only proceed when they
	// explicitly allow unverified destinations (Options.AllowUnverified).
	ErrUnverifiable = errors.New("destination: on-chain checks unavailable")
	// ErrBlacklisted means a token issuer froze the address (Circle's USDC blacklist): a transfer
	// to it reverts, and one blacklisted recipient blocks a whole split.
	ErrBlacklisted = errors.New("destination: the token issuer has blacklisted this address")
	// ErrNoProviders is what a BlacklistChecker answers for a chain without RPC providers.
	ErrNoProviders = errors.New("destination: no providers configured for the chain")
)

// SolanaAccountKind classifies an account by its owner program and data.
type SolanaAccountKind int

// The kinds of Solana account a destination address may be.
const (
	SolanaUnknown SolanaAccountKind = iota
	// SolanaWallet is a system-owned account or one that does not exist yet.
	SolanaWallet
	SolanaTokenAccount
	SolanaMint
	SolanaProgram
	SolanaOther
)

// MissingTokenAccount is an enabled Solana mint whose associated token account is not usable for
// a wallet: it does not exist (or is not a classic token account of the mint), or Frozen, the
// issuer froze it.
type MissingTokenAccount struct {
	Chain, Asset, Mint, Account string
	Frozen                      bool
}

// SolanaInspector looks accounts up over RPC.
type SolanaInspector interface {
	Inspect(ctx context.Context, livemode bool, pubkey string) (SolanaAccountKind, error)
	// MissingTokenAccounts lists, over every Solana chain of the mode, the enabled mints for
	// which the wallet's associated token account is not an existing classic token account.
	MissingTokenAccounts(ctx context.Context, livemode bool, wallet string) ([]MissingTokenAccount, error)
}

// EVMCodeChecker reports whether an address has contract code on a chain.
type EVMCodeChecker interface {
	HasCode(ctx context.Context, chainID string, address string) (bool, error)
}

// BlacklistChecker asks a token whether its issuer blacklisted an address (FiatToken's
// isBlacklisted). It answers ErrNoProviders for a chain it has no providers for.
type BlacklistChecker interface {
	IsBlacklisted(ctx context.Context, chainID, token, address string) (bool, error)
}

// Unavailable is the stub used until the RPC layer exists: every lookup fails, so validation
// fails closed with ErrUnverifiable.
type Unavailable struct{}

// Inspect always fails with ErrUnverifiable: no Solana RPC is configured.
func (Unavailable) Inspect(context.Context, bool, string) (SolanaAccountKind, error) {
	return SolanaUnknown, ErrUnverifiable
}

// MissingTokenAccounts always fails with ErrUnverifiable: no Solana RPC is configured.
func (Unavailable) MissingTokenAccounts(context.Context, bool, string) ([]MissingTokenAccount, error) {
	return nil, ErrUnverifiable
}

// HasCode always fails with ErrUnverifiable: no EVM RPC is configured.
func (Unavailable) HasCode(context.Context, string, string) (bool, error) {
	return false, ErrUnverifiable
}

// IsBlacklisted always fails with ErrNoProviders: no RPC is configured to read the token's
// blacklist.
func (Unavailable) IsBlacklisted(context.Context, string, string, string) (bool, error) {
	return false, ErrNoProviders
}

// Validator checks a payout destination's format, denylist and on-chain kind before it is saved.
type Validator struct {
	Registry *chain.Registry
	// ProtocolAddresses are our own factory, gateway and treasury addresses per family.
	ProtocolAddresses map[chain.Family][]string
	Solana            SolanaInspector
	EVMCode           EVMCodeChecker
	// Blacklist checks EVM payout addresses against the issuer blacklist of every EIP-3009
	// (FiatToken) asset of the mode.
	Blacklist BlacklistChecker
}

// Options tune one destination check: the mode and which on-chain checks to run.
type Options struct {
	Livemode bool
	// AllowUnverified skips on-chain checks that cannot run. Format, checksum and denylist
	// checks always apply.
	AllowUnverified bool
	// AllowMissingTokenAccounts accepts a Solana wallet without a token account for an enabled
	// mint. Test mode only (ignored in live mode): test tokens have no value, and a flush to
	// such a wallet waits (blocked, re-checked) until the account exists.
	AllowMissingTokenAccounts bool
}

// Validate returns the canonical form of addr (EIP-55 on EVM) or an error wrapping one of the
// sentinel errors above.
func (v Validator) Validate(ctx context.Context, family chain.Family, addr string, opt Options) (string, error) {
	var canonical string
	switch family {
	case chain.EVM:
		a, err := ValidateEVMFormat(addr)
		if err != nil {
			return "", err
		}
		canonical = a.Hex()
	case chain.Solana:
		if err := ValidateSolanaFormat(addr); err != nil {
			return "", err
		}
		canonical = addr
	default:
		return "", fmt.Errorf("%w: unknown family %q", ErrFormat, family)
	}
	if v.Registry != nil && v.Registry.IsKnownToken(family, canonical) {
		return "", ErrDenylisted
	}
	for _, p := range v.ProtocolAddresses[family] {
		if sameAddress(family, p, canonical) {
			return "", ErrDenylisted
		}
	}
	if err := v.onChainChecks(ctx, family, canonical, opt); err != nil {
		if errors.Is(err, ErrUnverifiable) && opt.AllowUnverified {
			return canonical, nil
		}
		return "", err
	}
	return canonical, nil
}

func (v Validator) onChainChecks(ctx context.Context, family chain.Family, addr string, opt Options) error {
	switch family {
	case chain.Solana:
		inspector := v.Solana
		if inspector == nil {
			inspector = Unavailable{}
		}
		kind, err := inspector.Inspect(ctx, opt.Livemode, addr)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrUnverifiable, err)
		}
		switch kind {
		case SolanaWallet:
			err := tokenAccountsReady(ctx, inspector, opt.Livemode, addr)
			if errors.Is(err, ErrTokenAccountMissing) && opt.AllowMissingTokenAccounts && !opt.Livemode {
				return nil
			}
			return err
		case SolanaTokenAccount:
			return ErrTokenAccount
		case SolanaMint:
			return ErrMint
		case SolanaProgram:
			return ErrProgram
		default:
			return fmt.Errorf("%w: account kind %d", ErrUnverifiable, kind)
		}
	case chain.EVM:
		checker := v.EVMCode
		if checker == nil {
			checker = Unavailable{}
		}
		var chains []chain.Chain
		if v.Registry != nil {
			for _, c := range v.Registry.ForMode(opt.Livemode) {
				if c.Family == chain.EVM {
					chains = append(chains, c)
				}
			}
		}
		withCode := 0
		for _, c := range chains {
			has, err := checker.HasCode(ctx, c.ID, addr)
			if err != nil {
				return fmt.Errorf("%w: %v", ErrUnverifiable, err)
			}
			if has {
				withCode++
			}
		}
		if withCode != 0 && withCode != len(chains) {
			return ErrCodeMismatch
		}
	}
	return nil
}

// CheckBlacklist refuses an EVM payout address that the issuer of an accepted FiatToken asset
// (USDC) blacklisted: a transfer to it reverts, so a split paying it could never settle and its
// funds would wait in the forwarder. It runs when a wallet or a split is saved and when a session
// opens. A live chain whose providers are missing or failing fails closed (ErrUnverifiable); a
// test chain without providers is skipped, so test mode works without RPC. Other families have
// no such list (Solana freezes token accounts, which the token-account checks see).
func (v Validator) CheckBlacklist(ctx context.Context, livemode bool, family chain.Family, addr string) error {
	if family != chain.EVM || v.Registry == nil {
		return nil
	}
	checker := v.Blacklist
	if checker == nil {
		checker = Unavailable{}
	}
	for _, c := range v.Registry.ForMode(livemode) {
		if c.Family != chain.EVM {
			continue
		}
		for _, a := range c.Assets {
			if !a.EIP3009 {
				continue
			}
			listed, err := checker.IsBlacklisted(ctx, c.ID, a.Token, addr)
			switch {
			case errors.Is(err, ErrNoProviders) && !livemode:
				continue
			case err != nil:
				return fmt.Errorf("%w: %s blacklist on %s: %v", ErrUnverifiable, a.Symbol, c.ID, err)
			case listed:
				return fmt.Errorf("%w: %s on %s blacklists %s", ErrBlacklisted, a.Symbol, c.ID, addr)
			}
		}
	}
	return nil
}

// tokenAccountsReady requires the wallet's token account for every enabled mint. A definite
// "missing" is never overridable: the flusher would otherwise pay its rent, or block the funds.
// A frozen account is refused before any missing one, as its own error: receiving tokens cannot
// fix it.
func tokenAccountsReady(ctx context.Context, inspector SolanaInspector, livemode bool, wallet string) error {
	missing, err := inspector.MissingTokenAccounts(ctx, livemode, wallet)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnverifiable, err)
	}
	if frozen := frozenOnly(missing); len(frozen) != 0 {
		return &FrozenTokenAccountsError{Frozen: frozen}
	}
	if len(missing) == 0 {
		return nil
	}
	return &MissingTokenAccountsError{Missing: missing}
}

func frozenOnly(accounts []MissingTokenAccount) []MissingTokenAccount {
	var out []MissingTokenAccount
	for _, m := range accounts {
		if m.Frozen {
			out = append(out, m)
		}
	}
	return out
}

// CheckFrozen refuses a Solana payout wallet whose token account for an enabled mint the issuer
// froze (ErrTokenAccountFrozen): bound into a split it would block every recipient's flush of that
// mint. It runs when a wallet is bound into a product's payout; the wallet passed the full
// token-account checks when it was added, so a missing account is not refused here. A lookup that
// cannot run answers ErrUnverifiable. Other families return nil (EVM has CheckBlacklist).
func (v Validator) CheckFrozen(ctx context.Context, livemode bool, family chain.Family, addr string) error {
	if family != chain.Solana {
		return nil
	}
	inspector := v.Solana
	if inspector == nil {
		inspector = Unavailable{}
	}
	accounts, err := inspector.MissingTokenAccounts(ctx, livemode, addr)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnverifiable, err)
	}
	if frozen := frozenOnly(accounts); len(frozen) != 0 {
		return &FrozenTokenAccountsError{Frozen: frozen}
	}
	return nil
}

// FrozenTokenAccountsError names the enabled mints whose token account the issuer froze.
type FrozenTokenAccountsError struct {
	Frozen []MissingTokenAccount
}

func (e *FrozenTokenAccountsError) Error() string {
	var parts []string
	for _, m := range e.Frozen {
		parts = append(parts, fmt.Sprintf("%s on %s (mint %s, token account %s)", m.Asset, m.Chain, m.Mint, m.Account))
	}
	return fmt.Sprintf("%s: %s", ErrTokenAccountFrozen, strings.Join(parts, "; "))
}

func (e *FrozenTokenAccountsError) Unwrap() error { return ErrTokenAccountFrozen }

// Assets lists the frozen ones as "USDT on solana".
func (e *FrozenTokenAccountsError) Assets() string {
	var parts []string
	for _, m := range e.Frozen {
		parts = append(parts, m.Asset+" on "+m.Chain)
	}
	return strings.Join(parts, ", ")
}

// MissingTokenAccountsError names the enabled mints a Solana wallet has no token account for.
type MissingTokenAccountsError struct {
	Missing []MissingTokenAccount
}

func (e *MissingTokenAccountsError) Error() string {
	var parts []string
	for _, m := range e.Missing {
		parts = append(parts, fmt.Sprintf("%s on %s (mint %s, token account %s)", m.Asset, m.Chain, m.Mint, m.Account))
	}
	return fmt.Sprintf("%s: %s. Receive any amount of each of these tokens at this wallet once (that creates the token account), then add the wallet again",
		ErrTokenAccountMissing, strings.Join(parts, "; "))
}

func (e *MissingTokenAccountsError) Unwrap() error { return ErrTokenAccountMissing }

// Assets lists the missing ones as "USDT on solana".
func (e *MissingTokenAccountsError) Assets() string {
	var parts []string
	for _, m := range e.Missing {
		parts = append(parts, m.Asset+" on "+m.Chain)
	}
	return strings.Join(parts, ", ")
}

func sameAddress(f chain.Family, a, b string) bool {
	if f == chain.EVM {
		pa, errA := evm.ParseAddress(a)
		pb, errB := evm.ParseAddress(b)
		return errA == nil && errB == nil && pa == pb
	}
	return a == b
}

// ValidateEVMFormat checks shape, checksum and the zero address.
func ValidateEVMFormat(addr string) (evm.Address, error) {
	a, err := evm.ParseAddress(addr)
	switch {
	case errors.Is(err, evm.ErrChecksum):
		return a, ErrChecksum
	case err != nil:
		return a, ErrFormat
	case a.IsZero():
		return a, ErrZeroAddress
	}
	return a, nil
}

// ValidateSolanaFormat checks that addr is the canonical base58 form of 32 bytes and not the
// all-zero key (the system program).
func ValidateSolanaFormat(addr string) error {
	b, err := base58.Decode(addr)
	if err != nil || len(b) != 32 || base58.Encode(b) != addr {
		return ErrFormat
	}
	for _, x := range b {
		if x != 0 {
			return nil
		}
	}
	return ErrZeroAddress
}
