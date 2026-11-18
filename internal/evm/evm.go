// Package evm holds the EVM address primitives the backend must reproduce exactly:
// EIP-55 checksums and CREATE2 address prediction.
package evm

import (
	"encoding/hex"
	"errors"
	"strings"

	"golang.org/x/crypto/sha3"
)

// Address is a 20-byte EVM address.
type Address [20]byte

// Errors returned when parsing an address.
var (
	ErrFormat   = errors.New("evm: address must be 0x followed by 40 hex characters")
	ErrChecksum = errors.New("evm: mixed-case address fails the EIP-55 checksum")
)

// Keccak256 returns the Keccak-256 hash of the concatenated parts.
func Keccak256(parts ...[]byte) []byte {
	h := sha3.NewLegacyKeccak256()
	for _, p := range parts {
		h.Write(p)
	}
	return h.Sum(nil)
}

// ParseAddress accepts all-lowercase, all-uppercase or correctly checksummed mixed-case
// addresses. A mixed-case address with a wrong checksum is rejected: it is a typo signal.
func ParseAddress(s string) (Address, error) {
	var a Address
	body, ok := strings.CutPrefix(s, "0x")
	if !ok || len(body) != 40 {
		return a, ErrFormat
	}
	raw, err := hex.DecodeString(body)
	if err != nil {
		return a, ErrFormat
	}
	copy(a[:], raw)
	if body != strings.ToLower(body) && body != strings.ToUpper(body) && a.Hex() != s {
		return a, ErrChecksum
	}
	return a, nil
}

// Hex returns the EIP-55 checksummed form.
func (a Address) Hex() string {
	lower := hex.EncodeToString(a[:])
	hash := Keccak256([]byte(lower))
	out := []byte(lower)
	for i := range out {
		if out[i] >= 'a' && hash[i/2]>>(4*(1-uint(i%2)))&0xf >= 8 {
			out[i] -= 'a' - 'A'
		}
	}
	return "0x" + string(out)
}

// IsZero reports whether the address is the zero address.
func (a Address) IsZero() bool { return a == Address{} }

// Create2Address is keccak256(0xff ++ deployer ++ salt ++ initCodeHash)[12:].
func Create2Address(deployer Address, salt [32]byte, initCodeHash [32]byte) Address {
	h := Keccak256([]byte{0xff}, deployer[:], salt[:], initCodeHash[:])
	var a Address
	copy(a[:], h[12:])
	return a
}
