// Package solanaderive derives Solana forwarder addresses without an RPC call: the forwarder PDA
// of the prismaport_forwarder program and the associated token accounts it receives into.
// It must agree byte for byte with the program (vectors in solana/vectors/solana-pda.json).
package solanaderive

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"slices"

	"github.com/prismaport/prismaport/internal/base58"
	"github.com/prismaport/prismaport/internal/fee"
)

// Seed prefix and size limits of a forwarder account.
const (
	ForwarderSeed = "fwd"
	MaxRecipients = 5
	// MaxMints is how many mints a forwarder can bind (D41).
	MaxMints = 4
	// BpsTotal is what the recipient shares add up to.
	BpsTotal = 10_000
	// ParamsFixedLen is treasury ++ rentReceiver ++ u16 feeBps ++ u64 fixedFeeMicros ++ u32
	// recipient count ++ u32 mint count; each recipient adds ParamsRecipientLen and each mint
	// ParamsMintLen.
	ParamsFixedLen      = 32 + 32 + 2 + 8 + 4 + 4
	ParamsRecipientLen  = 32 + 2
	ParamsMintLen       = 32
	ParamsMaxEncodedLen = ParamsFixedLen + MaxRecipients*ParamsRecipientLen + MaxMints*ParamsMintLen
	maxSeeds            = 16
	maxSeedLen          = 32
	pdaMarker           = "ProgramDerivedAddress"
	publicKeyLen        = 32
)

// Token program addresses and the errors returned by derivation and parameter checks.
var (
	TokenProgramID           = MustParse("TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA")
	AssociatedTokenProgramID = MustParse("ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL")

	ErrInvalidKey  = errors.New("solanaderive: invalid public key")
	ErrOnCurve     = errors.New("solanaderive: seeds produce a point on the curve")
	ErrSeeds       = errors.New("solanaderive: too many seeds or seed too long")
	ErrNoBump      = errors.New("solanaderive: no viable bump seed")
	ErrZeroAddress = errors.New("solanaderive: treasury, rent receiver, every recipient and every mint must be non-zero")

	// ErrRecipientCount and the three errors after it are the recipient list errors, named after the
	// program's ForwarderError variants.
	ErrRecipientCount     = errors.New("solanaderive: a forwarder pays 1 to 5 recipients")
	ErrDuplicateRecipient = errors.New("solanaderive: recipients must be distinct")
	ErrZeroShare          = errors.New("solanaderive: every recipient share must be above 0 bps")
	ErrSharesNotWhole     = errors.New("solanaderive: recipient shares must add up to 10000 bps")

	// ErrMintCount and the two errors after it are the mint list errors (D41), named after the
	// program's ForwarderError variants. A zero mint is ErrZeroAddress, as on-chain.
	ErrMintCount         = errors.New("solanaderive: a forwarder binds 1 to 4 mints")
	ErrMintsNotAscending = errors.New("solanaderive: mints must be sorted strictly ascending by their bytes, without duplicates")
	ErrMintNotAllowed    = errors.New("solanaderive: mint is not bound by this forwarder")
)

// PublicKey is a 32-byte Solana account address.
type PublicKey [32]byte

// Parse decodes a base58 address and returns ErrInvalidKey if it is not 32 bytes.
func Parse(s string) (PublicKey, error) {
	b, err := base58.Decode(s)
	if err != nil || len(b) != publicKeyLen {
		return PublicKey{}, fmt.Errorf("%w: %q", ErrInvalidKey, s)
	}
	return PublicKey(b), nil
}

// MustParse is Parse that panics on bad input, for constants.
func MustParse(s string) PublicKey {
	k, err := Parse(s)
	if err != nil {
		panic(err)
	}
	return k
}

func (k PublicKey) String() string { return base58.Encode(k[:]) }

// IsZero reports whether the key is all zero bytes.
func (k PublicKey) IsZero() bool { return k == PublicKey{} }

// Recipient is one payout destination: the wallet that owns the receiving token accounts and
// its share of the net amount in basis points.
type Recipient struct {
	Wallet PublicKey
	Bps    uint16
}

// Params are the values a forwarder commits to; the PDA is derived from their hash. Nothing is
// stored on-chain: every instruction carries them and the seeds prove them. Recipient order is
// committed too (the first recipient receives the rounding remainder), and so are the bound
// mints, sorted strictly ascending by bytes (SortMints).
type Params struct {
	Treasury     PublicKey
	RentReceiver PublicKey
	Fee          fee.Params
	Recipients   []Recipient
	Mints        []PublicKey
}

// Validate refuses what the program's check_params refuses, in the same order: fee, treasury,
// rent receiver, recipients, mints.
func (p Params) Validate() error {
	if err := p.Fee.Validate(); err != nil {
		return err
	}
	if p.Treasury.IsZero() || p.RentReceiver.IsZero() {
		return ErrZeroAddress
	}
	if err := ValidateRecipients(p.Recipients); err != nil {
		return err
	}
	return ValidateMints(p.Mints)
}

// ValidateMints mirrors the program's check_mints: 1 to MaxMints entries, each non-zero and
// strictly above the previous one byte-wise (which also rules out duplicates). Checked in that
// order per entry, so the same list gets the same error.
func ValidateMints(ms []PublicKey) error {
	if len(ms) == 0 || len(ms) > MaxMints {
		return fmt.Errorf("%w (got %d)", ErrMintCount, len(ms))
	}
	for i, m := range ms {
		if m.IsZero() {
			return fmt.Errorf("%w: mint %d", ErrZeroAddress, i)
		}
		if i > 0 && bytes.Compare(ms[i-1][:], m[:]) >= 0 {
			return fmt.Errorf("%w: mint %d", ErrMintsNotAscending, i)
		}
	}
	return nil
}

// SortMints returns a copy of ms in the canonical order: ascending by the 32 bytes (not by the
// base58 text). It does not drop duplicates; ValidateMints refuses them.
func SortMints(ms []PublicKey) []PublicKey {
	out := slices.Clone(ms)
	slices.SortFunc(out, func(a, b PublicKey) int { return bytes.Compare(a[:], b[:]) })
	return out
}

// Allows reports whether mint is one of the bound mints: the program refuses any other with
// MintNotAllowed before reading the mint's data.
func (p Params) Allows(mint PublicKey) bool { return slices.Contains(p.Mints, mint) }

// ValidateRecipients checks a split: 1 to MaxRecipients entries, non-zero distinct wallets, every
// share above zero, shares adding up to exactly BpsTotal.
func ValidateRecipients(rs []Recipient) error {
	if len(rs) == 0 || len(rs) > MaxRecipients {
		return ErrRecipientCount
	}
	for i, r := range rs {
		if r.Wallet.IsZero() {
			return ErrZeroAddress
		}
		for _, o := range rs[:i] {
			if o.Wallet == r.Wallet {
				return ErrDuplicateRecipient
			}
		}
	}
	sum := 0
	for _, r := range rs {
		if r.Bps == 0 {
			return ErrZeroShare
		}
		sum += int(r.Bps)
	}
	if sum != BpsTotal {
		return ErrSharesNotWhole
	}
	return nil
}

// Equal compares every committed value, recipient and mint order included.
func (p Params) Equal(o Params) bool {
	return p.Treasury == o.Treasury && p.RentReceiver == o.RentReceiver && p.Fee == o.Fee &&
		slices.Equal(p.Recipients, o.Recipients) && slices.Equal(p.Mints, o.Mints)
}

// Encode is the Borsh layout: treasury ++ rentReceiver ++ u16 LE feeBps ++ u64 LE fixedFeeMicros
// ++ u32 LE nR ++ nR x (wallet ++ u16 LE bps) ++ u32 LE nM ++ nM x mint, 82 + 34nR + 32nM bytes.
func (p Params) Encode() []byte {
	out := make([]byte, 0, ParamsFixedLen+len(p.Recipients)*ParamsRecipientLen+len(p.Mints)*ParamsMintLen)
	out = append(out, p.Treasury[:]...)
	out = append(out, p.RentReceiver[:]...)
	out = binary.LittleEndian.AppendUint16(out, p.Fee.FeeBps)
	out = binary.LittleEndian.AppendUint64(out, p.Fee.FixedFeeMicros)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(p.Recipients)))
	for _, r := range p.Recipients {
		out = append(out, r.Wallet[:]...)
		out = binary.LittleEndian.AppendUint16(out, r.Bps)
	}
	out = binary.LittleEndian.AppendUint32(out, uint32(len(p.Mints)))
	for _, m := range p.Mints {
		out = append(out, m[:]...)
	}
	return out
}

// Hash returns the SHA-256 of the encoded params, used as a seed of the forwarder address.
func (p Params) Hash() [32]byte { return sha256.Sum256(p.Encode()) }

// ForwarderAddress is find_program_address(["fwd", salt, sha256(params)], program). It refuses
// params the program would reject, so it never hands out an address nobody can flush.
func ForwarderAddress(program PublicKey, salt [32]byte, p Params) (PublicKey, uint8, error) {
	if err := p.Validate(); err != nil {
		return PublicKey{}, 0, err
	}
	h := p.Hash()
	return FindProgramAddress([][]byte{[]byte(ForwarderSeed), salt[:], h[:]}, program)
}

// AssociatedTokenAddress is the canonical classic-SPL-Token ATA of (owner, mint). Owners may be
// off-curve (PDAs).
func AssociatedTokenAddress(owner, mint PublicKey) (PublicKey, error) {
	a, _, err := FindProgramAddress(
		[][]byte{owner[:], TokenProgramID[:], mint[:]}, AssociatedTokenProgramID)
	return a, err
}

// CreateProgramAddress derives a program address from seeds and program, failing if it falls on the ed25519 curve.
func CreateProgramAddress(seeds [][]byte, program PublicKey) (PublicKey, error) {
	if len(seeds) > maxSeeds {
		return PublicKey{}, ErrSeeds
	}
	h := sha256.New()
	for _, s := range seeds {
		if len(s) > maxSeedLen {
			return PublicKey{}, ErrSeeds
		}
		h.Write(s)
	}
	h.Write(program[:])
	h.Write([]byte(pdaMarker))
	var out PublicKey
	h.Sum(out[:0])
	if IsOnCurve(out) {
		return PublicKey{}, ErrOnCurve
	}
	return out, nil
}

// FindProgramAddress tries bumps from 255 down, appending the bump as the last seed.
func FindProgramAddress(seeds [][]byte, program PublicKey) (PublicKey, uint8, error) {
	if len(seeds) >= maxSeeds {
		return PublicKey{}, 0, ErrSeeds
	}
	withBump := append(append([][]byte{}, seeds...), nil)
	for bump := 255; bump >= 0; bump-- {
		withBump[len(seeds)] = []byte{byte(bump)}
		a, err := CreateProgramAddress(withBump, program)
		if err == nil {
			return a, uint8(bump), nil
		}
		if !errors.Is(err, ErrOnCurve) {
			return PublicKey{}, 0, err
		}
	}
	return PublicKey{}, 0, ErrNoBump
}

var (
	fieldP     = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 255), big.NewInt(19))
	edwardsD   = curveD()
	legendreEx = new(big.Int).Rsh(new(big.Int).Sub(fieldP, big.NewInt(1)), 1)
)

func curveD() *big.Int {
	inv := new(big.Int).ModInverse(big.NewInt(121666), fieldP)
	d := new(big.Int).Mul(big.NewInt(-121665), inv)
	return d.Mod(d, fieldP)
}

// IsOnCurve reports whether b decompresses to an ed25519 point, matching curve25519-dalek's
// CompressedEdwardsY::decompress as used by the Solana runtime: the sign bit is ignored, y is
// taken mod p (non-canonical encodings accepted), and the point exists iff
// (y^2 - 1) / (d*y^2 + 1) is a square mod p.
func IsOnCurve(b PublicKey) bool {
	le := b
	le[31] &= 0x7f
	be := make([]byte, 32)
	for i := range le {
		be[31-i] = le[i]
	}
	y := new(big.Int).SetBytes(be)
	y.Mod(y, fieldP)
	y2 := new(big.Int).Mul(y, y)
	y2.Mod(y2, fieldP)
	u := new(big.Int).Sub(y2, big.NewInt(1))
	u.Mod(u, fieldP)
	v := new(big.Int).Mul(edwardsD, y2)
	v.Add(v, big.NewInt(1))
	v.Mod(v, fieldP)
	// d is not a square mod p, so v is never zero.
	uv := new(big.Int).Mul(u, v)
	uv.Mod(uv, fieldP)
	if uv.Sign() == 0 {
		return true
	}
	return new(big.Int).Exp(uv, legendreEx, fieldP).Cmp(big.NewInt(1)) == 0
}
