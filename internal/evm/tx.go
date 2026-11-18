package evm

import (
	"errors"
	"fmt"
	"math/big"
	"os"
	"strings"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	decdsa "github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
)

// rlpBytes encodes a byte string.
func rlpBytes(b []byte) []byte {
	if len(b) == 1 && b[0] < 0x80 {
		return []byte{b[0]}
	}
	return append(rlpHeader(0x80, len(b)), b...)
}

func rlpHeader(base byte, n int) []byte {
	if n < 56 {
		return []byte{base + byte(n)}
	}
	var l []byte
	for x := n; x > 0; x >>= 8 {
		l = append([]byte{byte(x)}, l...)
	}
	return append([]byte{base + 55 + byte(len(l))}, l...)
}

func rlpList(items ...[]byte) []byte {
	var body []byte
	for _, it := range items {
		body = append(body, it...)
	}
	return append(rlpHeader(0xc0, len(body)), body...)
}

// rlpUint encodes an unsigned integer as its minimal big-endian bytes (zero is the empty string).
func rlpUint(n *big.Int) []byte {
	if n == nil || n.Sign() == 0 {
		return rlpBytes(nil)
	}
	return rlpBytes(n.Bytes())
}

// CreateAddress is the address CREATE gives for (deployer, nonce).
func CreateAddress(deployer Address, nonce uint64) Address {
	h := Keccak256(rlpList(rlpBytes(deployer[:]), rlpUint(new(big.Int).SetUint64(nonce))))
	var a Address
	copy(a[:], h[12:])
	return a
}

// DynamicFeeTx is an EIP-1559 (type 2) transaction without an access list.
type DynamicFeeTx struct {
	ChainID   uint64
	Nonce     uint64
	GasTipCap *big.Int
	GasFeeCap *big.Int
	Gas       uint64
	To        *Address
	Value     *big.Int
	Data      []byte
}

func (tx DynamicFeeTx) fields() [][]byte {
	to := rlpBytes(nil)
	if tx.To != nil {
		to = rlpBytes(tx.To[:])
	}
	return [][]byte{
		rlpUint(new(big.Int).SetUint64(tx.ChainID)),
		rlpUint(new(big.Int).SetUint64(tx.Nonce)),
		rlpUint(tx.GasTipCap),
		rlpUint(tx.GasFeeCap),
		rlpUint(new(big.Int).SetUint64(tx.Gas)),
		to,
		rlpUint(tx.Value),
		rlpBytes(tx.Data),
		rlpList(), // access list
	}
}

// SigningHash is keccak256(0x02 ++ rlp([chainId, nonce, tip, feeCap, gas, to, value, data, accessList])).
func (tx DynamicFeeTx) SigningHash() Word {
	var w Word
	copy(w[:], Keccak256([]byte{0x02}, rlpList(tx.fields()...)))
	return w
}

// Sign returns the raw signed transaction and its hash.
func (tx DynamicFeeTx) Sign(k *PrivateKey) (raw []byte, hash Word) {
	h := tx.SigningHash()
	v, r, s := k.SignHash(h)
	f := append(tx.fields(), rlpUint(new(big.Int).SetUint64(uint64(v))), rlpUint(r.Big()), rlpUint(s.Big()))
	raw = append([]byte{0x02}, rlpList(f...)...)
	copy(hash[:], Keccak256(raw))
	return raw, hash
}

// PrivateKey is a secp256k1 signing key. Its String and GoString never reveal the key, so it
// cannot leak through logging or %v formatting.
type PrivateKey struct {
	k *secp256k1.PrivateKey
}

// ErrKey is returned when a private key is malformed or out of range.
var ErrKey = errors.New("evm: invalid private key")

func (*PrivateKey) String() string { return "evm.PrivateKey([redacted])" }

// GoString hides the key, so %#v never prints it.
func (*PrivateKey) GoString() string { return "evm.PrivateKey([redacted])" }

// ParsePrivateKey reads a 32-byte hex key, with or without 0x, surrounding whitespace ignored.
func ParsePrivateKey(s string) (*PrivateKey, error) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "0x")
	if len(s) != 64 {
		return nil, ErrKey
	}
	b, err := ParseHexBytes("0x" + s)
	if err != nil {
		return nil, ErrKey
	}
	var n secp256k1.ModNScalar
	if overflow := n.SetByteSlice(b); overflow || n.IsZero() {
		return nil, ErrKey
	}
	return &PrivateKey{k: secp256k1.NewPrivateKey(&n)}, nil
}

// LoadPrivateKey reads a key from a file (preferred) or, when file is empty, from the value of
// an environment variable. Errors never include the key material.
func LoadPrivateKey(file, envValue string) (*PrivateKey, error) {
	raw := envValue
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("evm: read key file: %w", err)
		}
		raw = string(b)
	}
	if raw == "" {
		return nil, fmt.Errorf("%w: none configured", ErrKey)
	}
	return ParsePrivateKey(raw)
}

// Address returns the address of the key's public key.
func (k *PrivateKey) Address() Address {
	return PubkeyAddress(k.k.PubKey())
}

// PubkeyAddress returns the address of a secp256k1 public key.
func PubkeyAddress(p *secp256k1.PublicKey) Address {
	u := p.SerializeUncompressed()
	var a Address
	copy(a[:], Keccak256(u[1:])[12:])
	return a
}

// SignHash returns the recovery id (0 or 1) and r, s. s is always in the lower half order.
func (k *PrivateKey) SignHash(h Word) (v byte, r, s Word) {
	sig := decdsa.SignCompact(k.k, h[:], false)
	v = sig[0] - 27
	copy(r[:], sig[1:33])
	copy(s[:], sig[33:65])
	return v, r, s
}

// halfN is secp256k1's order divided by two; signatures with s above it are malleable and
// rejected by EIP-2 and by FiatToken's ECRecover.
var halfN = new(big.Int).Rsh(secp256k1.Params().N, 1)

// ErrSignature is returned when a signature is malformed or does not recover a key.
var ErrSignature = errors.New("evm: invalid signature")

// RecoverAddress returns the signer of a 32-byte digest for a (v, r, s) signature where v is
// 27/28 (or 0/1). High-s signatures are rejected.
func RecoverAddress(digest Word, v byte, r, s Word) (Address, error) {
	if v >= 27 {
		v -= 27
	}
	if v > 1 {
		return Address{}, ErrSignature
	}
	sb := s.Big()
	if sb.Sign() == 0 || sb.Cmp(halfN) > 0 || r.Big().Sign() == 0 {
		return Address{}, ErrSignature
	}
	compact := make([]byte, 65)
	compact[0] = 27 + v
	copy(compact[1:], r[:])
	copy(compact[33:], s[:])
	pub, _, err := decdsa.RecoverCompact(compact, digest[:])
	if err != nil {
		return Address{}, ErrSignature
	}
	return PubkeyAddress(pub), nil
}

// SplitSignature parses a 65-byte r ++ s ++ v signature as produced by eth_signTypedData_v4.
func SplitSignature(sig []byte) (v byte, r, s Word, err error) {
	if len(sig) != 65 {
		return 0, r, s, ErrSignature
	}
	copy(r[:], sig[:32])
	copy(s[:], sig[32:64])
	v = sig[64]
	if v < 27 {
		v += 27
	}
	if v != 27 && v != 28 {
		return 0, r, s, ErrSignature
	}
	return v, r, s, nil
}
