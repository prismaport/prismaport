// Package derive computes per-session deposit addresses: CREATE2 clone addresses on EVM and
// PDAs on Solana. Chains with a configured Factory (EVM) or program id and rent receiver
// (Solana) get real forwarder addresses; live chains without one fail closed and test chains
// get stubs.
package derive

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"

	"github.com/prismaport/prismaport/internal/base58"
	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/evm"
	"github.com/prismaport/prismaport/internal/fee"
	"github.com/prismaport/prismaport/internal/payout"
	"github.com/prismaport/prismaport/internal/solanaderive"
)

// MaxTokens is how many tokens a forwarder binds at most: TokenList.MAX_TOKENS on EVM and
// MAX_MINTS on Solana (D41). The registry refuses a chain with more assets than that
// (chain.MaxAssetsPerChain), so every asset of a chain can always be bound.
const MaxTokens = chain.MaxAssetsPerChain

// Errors returned when an address cannot be derived or its parameters are not valid.
var (
	ErrUnavailable = errors.New("derive: address derivation unavailable for this chain")
	ErrZeroAddress = errors.New("derive: the treasury must be non-zero")

	// Bound token list errors (D41), named after the Factory's TokenList errors. Solana
	// derivation returns the program's equivalents from solanaderive (ErrMintCount,
	// ErrZeroAddress, ErrMintsNotAscending).
	ErrTokenCount         = errors.New("derive: a forwarder binds 1 to 4 tokens")
	ErrZeroToken          = errors.New("derive: a bound token must be non-zero")
	ErrTokensNotAscending = errors.New("derive: bound tokens must be sorted strictly ascending by their bytes, without duplicates")
	// ErrTokenNotRegistered: Go binds only the chain's registered assets; the contracts and the
	// program cannot tell what a token is worth.
	ErrTokenNotRegistered = errors.New("derive: not a registered asset of the chain")
)

// Params are the immutable forwarder arguments a deposit address commits to.
type Params struct {
	Treasury string
	Fee      fee.Params
	// Recipients receive the net in fixed shares, in committed order.
	Recipients []payout.Recipient
	// RentReceiver is the platform's per-chain account Solana forwarders return rent to. EVM
	// forwarders have no rent and ignore it.
	RentReceiver string
	// Tokens are the bound token contracts (EVM, hex) or mints (Solana, base58): the only assets
	// the forwarder will ever flush (D41). 1 to MaxTokens entries in canonical order, ascending by
	// their bytes; derivation refuses any other list, as the Factory and the program do. Build
	// them with BoundTokens, which also limits them to the chain's registered assets.
	Tokens []string
}

// BoundTokens is the canonical token list for a set of asset symbols offered on c: every symbol
// must be one of c's registered assets (ErrTokenNotRegistered otherwise); repeated symbols count
// once; the result is sorted by the tokens' bytes and checked like the Factory or program would.
func BoundTokens(c chain.Chain, symbols []string) ([]string, error) {
	var tokens []string
	seen := map[string]bool{}
	for _, sym := range symbols {
		if seen[sym] {
			continue
		}
		seen[sym] = true
		a, ok := c.Asset(sym)
		if !ok {
			return nil, fmt.Errorf("%w: %s on %s", ErrTokenNotRegistered, sym, c.ID)
		}
		tokens = append(tokens, a.Token)
	}
	sorted, err := SortTokens(c.Family, tokens)
	if err != nil {
		return nil, err
	}
	if err := CheckTokens(c.Family, sorted); err != nil {
		return nil, err
	}
	return sorted, nil
}

// SortTokens returns a copy of tokens in canonical order for the family: ascending by address
// bytes on EVM and by the 32 key bytes on Solana (not by the text). Duplicates are kept, so
// CheckTokens still refuses them.
func SortTokens(f chain.Family, tokens []string) ([]string, error) {
	keys := make([][]byte, len(tokens))
	for i, t := range tokens {
		b, err := tokenBytes(f, t)
		if err != nil {
			return nil, fmt.Errorf("derive: token %d: %w", i, err)
		}
		keys[i] = b
	}
	idx := make([]int, len(tokens))
	for i := range idx {
		idx[i] = i
	}
	slices.SortStableFunc(idx, func(a, b int) int { return bytes.Compare(keys[a], keys[b]) })
	out := make([]string, len(tokens))
	for i, j := range idx {
		out[i] = tokens[j]
	}
	return out, nil
}

func tokenBytes(f chain.Family, t string) ([]byte, error) {
	switch f {
	case chain.EVM:
		a, err := evm.ParseAddress(t)
		return a[:], err
	case chain.Solana:
		k, err := solanaderive.Parse(t)
		return k[:], err
	}
	return nil, fmt.Errorf("%w: no token rules for %s", ErrUnavailable, f)
}

// CheckTokens refuses a token list the family's contracts refuse: TokenList.check on EVM,
// check_mints on Solana.
func CheckTokens(f chain.Family, tokens []string) error {
	switch f {
	case chain.EVM:
		_, err := EVMTokens(tokens)
		return err
	case chain.Solana:
		_, err := solanaMints(tokens)
		return err
	}
	return fmt.Errorf("%w: no token rules for %s", ErrUnavailable, f)
}

// TokenListError is a token list the Factory refuses. Err is ErrTokenCount, ErrZeroToken or
// ErrTokensNotAscending; Index is the argument of the matching Solidity error: the list length
// for InvalidTokenCount, the offending position otherwise.
type TokenListError struct {
	Err   error
	Index int
}

func (e *TokenListError) solidity() string {
	switch e.Err {
	case ErrTokenCount:
		return "InvalidTokenCount"
	case ErrZeroToken:
		return "ZeroToken"
	}
	return "TokensNotAscending"
}

func (e *TokenListError) Error() string {
	return fmt.Sprintf("%v: %s(%d)", e.Err, e.solidity(), e.Index)
}

func (e *TokenListError) Unwrap() error { return e.Err }

// RevertData is what Factory.encodeArgs, predict and deploy revert with for the same list: the
// custom error's selector and its uint256 argument.
func (e *TokenListError) RevertData() []byte {
	sel := evm.Selector(e.solidity() + "(uint256)")
	w := evm.Uint64Word(uint64(e.Index))
	return append(sel[:], w[:]...)
}

// CheckEVMTokens mirrors TokenList.check: 1 to MaxTokens entries, each non-zero and strictly
// above the previous one (which rules out duplicates), checked in that order per entry so the
// same list gets the same error.
func CheckEVMTokens(ts []evm.Address) error {
	if len(ts) == 0 || len(ts) > MaxTokens {
		return &TokenListError{ErrTokenCount, len(ts)}
	}
	for i, t := range ts {
		if t.IsZero() {
			return &TokenListError{ErrZeroToken, i}
		}
		if i > 0 && bytes.Compare(ts[i-1][:], t[:]) >= 0 {
			return &TokenListError{ErrTokensNotAscending, i}
		}
	}
	return nil
}

// EVMTokens parses and checks a bound EVM token list (CheckEVMTokens).
func EVMTokens(tokens []string) ([]evm.Address, error) {
	if len(tokens) == 0 || len(tokens) > MaxTokens {
		return nil, &TokenListError{ErrTokenCount, len(tokens)}
	}
	out := make([]evm.Address, len(tokens))
	for i, t := range tokens {
		a, err := evm.ParseAddress(t)
		if err != nil {
			return nil, fmt.Errorf("derive: token %d: %w", i, err)
		}
		out[i] = a
	}
	return out, CheckEVMTokens(out)
}

func solanaMints(tokens []string) ([]solanaderive.PublicKey, error) {
	out := make([]solanaderive.PublicKey, len(tokens))
	for i, t := range tokens {
		k, err := solanaderive.Parse(t)
		if err != nil {
			return nil, fmt.Errorf("derive: mint %d: %w", i, err)
		}
		out[i] = k
	}
	return out, solanaderive.ValidateMints(out)
}

// AddressDeriver returns the deposit address for a session salt. It must be a pure function of
// its inputs: the same salt and params always give the same address.
type AddressDeriver interface {
	Address(c chain.Chain, salt [32]byte, p Params) (string, error)
}

// ByFamily dispatches to one deriver per chain family.
type ByFamily map[chain.Family]AddressDeriver

// Address derives the address with the deriver of the chain's family, or fails with ErrUnavailable.
func (b ByFamily) Address(c chain.Chain, salt [32]byte, p Params) (string, error) {
	d, ok := b[c.Family]
	if !ok {
		return "", fmt.Errorf("%w: no deriver for %s", ErrUnavailable, c.Family)
	}
	return d.Address(c, salt, p)
}

// EVMRecipients parses and checks an EVM recipient list the way SplitLib.check does: 1 to 5
// non-zero, distinct addresses with shares above zero adding up to 10_000 bps.
func EVMRecipients(rs []payout.Recipient) ([]evm.Address, []uint16, error) {
	if len(rs) == 0 || len(rs) > payout.MaxRecipients {
		return nil, nil, payout.ErrCount
	}
	addrs := make([]evm.Address, len(rs))
	for i, r := range rs {
		a, err := evm.ParseAddress(r.Address)
		if err != nil {
			return nil, nil, fmt.Errorf("derive: recipient %d: %w", i+1, err)
		}
		if a.IsZero() {
			return nil, nil, payout.ErrZeroRecipient
		}
		for _, o := range addrs[:i] {
			if o == a {
				return nil, nil, payout.ErrDuplicateRecipient
			}
		}
		addrs[i] = a
	}
	bps := payout.Bps(rs)
	if err := payout.CheckBps(bps); err != nil {
		return nil, nil, err
	}
	return addrs, bps, nil
}

// PackEVMArgs is the ABI-packed immutable args of a forwarder clone, in protocol order:
// treasury(20) ++ uint16 feeBps ++ uint64 fixedFeeMicros ++ uint8 nR ++ uint8 nT ++ nR x
// (recipient(20) ++ uint16 bps) ++ nT x token(20), 32 + 22nR + 20nT bytes. It refuses what
// Factory.encodeArgs refuses, in the same order (treasury, fee, recipients, tokens), so a derived
// address can always be deployed.
func PackEVMArgs(p Params) ([]byte, error) {
	tre, err := evm.ParseAddress(p.Treasury)
	if err != nil {
		return nil, fmt.Errorf("derive: treasury: %w", err)
	}
	if tre.IsZero() {
		return nil, ErrZeroAddress
	}
	if err := p.Fee.Validate(); err != nil {
		return nil, err
	}
	addrs, bps, err := EVMRecipients(p.Recipients)
	if err != nil {
		return nil, err
	}
	tokens, err := EVMTokens(p.Tokens)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, 32+22*len(addrs)+20*len(tokens))
	out = append(out, tre[:]...)
	out = binary.BigEndian.AppendUint16(out, p.Fee.FeeBps)
	out = binary.BigEndian.AppendUint64(out, p.Fee.FixedFeeMicros)
	out = append(out, byte(len(addrs)), byte(len(tokens)))
	for i, a := range addrs {
		out = append(out, a[:]...)
		out = binary.BigEndian.AppendUint16(out, bps[i])
	}
	for _, t := range tokens {
		out = append(out, t[:]...)
	}
	return out, nil
}

// EVMCreate2 predicts Factory.deploy addresses. InitCode builds the clone creation code for
// the packed args; it must match the deployed Factory byte for byte (shared test vectors).
type EVMCreate2 struct {
	Factory  evm.Address
	InitCode func(packedArgs []byte) ([]byte, error)
}

// Address predicts the forwarder address the Factory deploys for the salt and parameters.
func (d EVMCreate2) Address(c chain.Chain, salt [32]byte, p Params) (string, error) {
	if c.Family != chain.EVM || d.InitCode == nil {
		return "", ErrUnavailable
	}
	args, err := PackEVMArgs(p)
	if err != nil {
		return "", err
	}
	code, err := d.InitCode(args)
	if err != nil {
		return "", err
	}
	var h [32]byte
	copy(h[:], evm.Keccak256(code))
	return evm.Create2Address(d.Factory, salt, h).Hex(), nil
}

// CloneImplementation is the ForwarderImpl address: the Factory constructor deploys it first,
// so it sits at CREATE(factory, nonce 1).
func CloneImplementation(factory evm.Address) evm.Address { return evm.CreateAddress(factory, 1) }

var (
	clonePrefix = []byte{0x3d, 0x81, 0x60, 0x0a, 0x3d, 0x39, 0xf3, 0x36, 0x3d, 0x3d, 0x37, 0x3d, 0x3d, 0x3d, 0x36, 0x3d, 0x73}
	cloneSuffix = []byte{0x5a, 0xf4, 0x3d, 0x82, 0x80, 0x3e, 0x90, 0x3d, 0x91, 0x60, 0x2b, 0x57, 0xfd, 0x5b, 0xf3}
)

// CloneInitCode is Solady LibClone's creation code for an ERC-1167 clone with immutable args
// appended to the runtime: PUSH2 runtime length, copy and return the runtime, which is the
// 45-byte proxy followed by args.
func CloneInitCode(implementation evm.Address, args []byte) []byte {
	n := 0x2d + len(args)
	out := make([]byte, 0, 10+45+len(args))
	out = append(out, 0x61, byte(n>>8), byte(n))
	out = append(out, clonePrefix...)
	out = append(out, implementation[:]...)
	out = append(out, cloneSuffix...)
	return append(out, args...)
}

// Forwarders predicts the deposit addresses of a deployed Factory.
func Forwarders(factory evm.Address) EVMCreate2 {
	impl := CloneImplementation(factory)
	return EVMCreate2{Factory: factory, InitCode: func(args []byte) ([]byte, error) {
		return CloneInitCode(impl, args), nil
	}}
}

// SolanaForwarders derives prismaport_forwarder PDAs. The rent receiver is part of what the
// address commits to; without one there is no real address (fail closed).
type SolanaForwarders struct {
	Program solanaderive.PublicKey
}

// SolanaParams converts the generic params into the program's committed values.
func SolanaParams(p Params) (solanaderive.Params, error) {
	var out solanaderive.Params
	if p.RentReceiver == "" {
		return out, fmt.Errorf("%w: no rent receiver configured", ErrUnavailable)
	}
	var err error
	if out.Treasury, err = solanaderive.Parse(p.Treasury); err != nil {
		return out, fmt.Errorf("derive: treasury: %w", err)
	}
	if out.RentReceiver, err = solanaderive.Parse(p.RentReceiver); err != nil {
		return out, fmt.Errorf("derive: rent receiver: %w", err)
	}
	for i, r := range p.Recipients {
		k, err := solanaderive.Parse(r.Address)
		if err != nil {
			return out, fmt.Errorf("derive: recipient %d: %w", i+1, err)
		}
		out.Recipients = append(out.Recipients, solanaderive.Recipient{Wallet: k, Bps: r.Bps})
	}
	for i, t := range p.Tokens {
		k, err := solanaderive.Parse(t)
		if err != nil {
			return out, fmt.Errorf("derive: mint %d: %w", i, err)
		}
		out.Mints = append(out.Mints, k)
	}
	out.Fee = p.Fee
	return out, out.Validate()
}

// Address derives the forwarder PDA for the salt and parameters on the chain's program.
func (d SolanaForwarders) Address(c chain.Chain, salt [32]byte, p Params) (string, error) {
	if c.Family != chain.Solana || d.Program.IsZero() {
		return "", ErrUnavailable
	}
	sp, err := SolanaParams(p)
	if err != nil {
		return "", err
	}
	pda, _, err := solanaderive.ForwarderAddress(d.Program, salt, sp)
	if err != nil {
		return "", err
	}
	return pda.String(), nil
}

// PerChain picks a deriver by chain id and falls back to Fallback for chains without one.
type PerChain struct {
	Chains   map[string]AddressDeriver
	Fallback AddressDeriver
}

// Address derives with the chain's own deriver, or with Fallback when it has none.
func (p PerChain) Address(c chain.Chain, salt [32]byte, params Params) (string, error) {
	if d, ok := p.Chains[c.ID]; ok {
		return d.Address(c, salt, params)
	}
	if p.Fallback == nil {
		return "", fmt.Errorf("%w: no deriver for %s", ErrUnavailable, c.ID)
	}
	return p.Fallback.Address(c, salt, params)
}

// Stub derives deterministic placeholder addresses for test-mode chains only. Nobody controls
// these addresses, so they are never produced for a live chain. It refuses the token lists the
// real derivers refuse and commits to them (v3: bound tokens, D41).
type Stub struct{}

// Address returns a deterministic placeholder address for a test chain and refuses live ones.
func (Stub) Address(c chain.Chain, salt [32]byte, p Params) (string, error) {
	if c.Livemode {
		return "", fmt.Errorf("%w: stub deriver refuses live chain %s", ErrUnavailable, c.ID)
	}
	if err := p.Fee.Validate(); err != nil {
		return "", err
	}
	if err := payout.CheckBps(payout.Bps(p.Recipients)); err != nil {
		return "", err
	}
	if err := CheckTokens(c.Family, p.Tokens); err != nil {
		return "", err
	}
	var buf []byte
	buf = append(buf, "prismaport/stub/v3/"+string(c.Family)+"\x00"...)
	buf = append(buf, salt[:]...)
	buf = append(buf, p.Treasury+"\x00"...)
	buf = binary.BigEndian.AppendUint16(buf, p.Fee.FeeBps)
	buf = binary.BigEndian.AppendUint64(buf, p.Fee.FixedFeeMicros)
	for _, r := range p.Recipients {
		buf = append(buf, r.Address+"\x00"...)
		buf = binary.BigEndian.AppendUint16(buf, r.Bps)
	}
	buf = append(buf, byte(len(p.Tokens)))
	for _, t := range p.Tokens {
		buf = append(buf, t+"\x00"...)
	}
	switch c.Family {
	case chain.EVM:
		var a evm.Address
		copy(a[:], evm.Keccak256(buf)[12:])
		return a.Hex(), nil
	case chain.Solana:
		h := sha256.Sum256(buf)
		return base58.Encode(h[:]), nil
	}
	return "", ErrUnavailable
}
