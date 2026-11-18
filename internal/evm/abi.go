package evm

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// Word is one 32-byte ABI slot.
type Word [32]byte

// ErrABI is returned when hex or ABI-encoded data is malformed.
var ErrABI = errors.New("evm: malformed ABI data")

// Selector is the first four bytes of keccak256(signature).
func Selector(signature string) [4]byte {
	var s [4]byte
	copy(s[:], Keccak256([]byte(signature)))
	return s
}

// Topic is keccak256(signature), the topic0 of an event.
func Topic(signature string) Word {
	var w Word
	copy(w[:], Keccak256([]byte(signature)))
	return w
}

// AddressWord left-pads an address into a 32-byte ABI word.
func AddressWord(a Address) Word {
	var w Word
	copy(w[12:], a[:])
	return w
}

// Uint64Word encodes n as a 32-byte big-endian ABI word.
func Uint64Word(n uint64) Word {
	return BigWord(new(big.Int).SetUint64(n))
}

// BigWord encodes a non-negative integer below 2^256. It panics otherwise: callers pass values
// that are already range checked.
func BigWord(n *big.Int) Word {
	if n.Sign() < 0 || n.BitLen() > 256 {
		panic(fmt.Sprintf("evm: %s does not fit a uint256 word", n))
	}
	var w Word
	n.FillBytes(w[:])
	return w
}

// Big returns the word as an unsigned integer.
func (w Word) Big() *big.Int { return new(big.Int).SetBytes(w[:]) }

// Address reads the low 20 bytes. ok is false when the high 12 bytes are not zero, which a
// well-formed ABI address never has.
func (w Word) Address() (Address, bool) {
	var a Address
	copy(a[:], w[12:])
	for _, b := range w[:12] {
		if b != 0 {
			return a, false
		}
	}
	return a, true
}

// Hex returns the word as 0x-prefixed lowercase hex.
func (w Word) Hex() string { return "0x" + hex.EncodeToString(w[:]) }

// ParseWord parses a 0x-prefixed 32-byte hex string.
func ParseWord(s string) (Word, error) {
	var w Word
	b, err := ParseHexBytes(s)
	if err != nil || len(b) != 32 {
		return w, fmt.Errorf("%w: %q is not a 32-byte word", ErrABI, s)
	}
	copy(w[:], b)
	return w, nil
}

// ParseHexBytes decodes 0x-prefixed hex ("0x" alone is empty).
func ParseHexBytes(s string) ([]byte, error) {
	body, ok := strings.CutPrefix(s, "0x")
	if !ok {
		return nil, fmt.Errorf("%w: %q lacks the 0x prefix", ErrABI, s)
	}
	if len(body)%2 == 1 {
		body = "0" + body
	}
	return hex.DecodeString(body)
}

// Calldata concatenates a selector and static words.
func Calldata(sel [4]byte, words ...Word) []byte {
	out := make([]byte, 0, 4+32*len(words))
	out = append(out, sel[:]...)
	for _, w := range words {
		out = append(out, w[:]...)
	}
	return out
}

// Words splits ABI data into 32-byte slots; the length must be a multiple of 32.
func Words(data []byte) ([]Word, error) {
	if len(data)%32 != 0 {
		return nil, fmt.Errorf("%w: %d bytes is not a whole number of words", ErrABI, len(data))
	}
	out := make([]Word, len(data)/32)
	for i := range out {
		copy(out[i][:], data[i*32:])
	}
	return out, nil
}

// EncodeAddressArray encodes the tail of a dynamic address[]: length then elements.
func EncodeAddressArray(as []Address) []Word {
	out := []Word{Uint64Word(uint64(len(as)))}
	for _, a := range as {
		out = append(out, AddressWord(a))
	}
	return out
}

// EncodeUint16Array encodes the tail of a dynamic uint16[]: length then elements.
func EncodeUint16Array(vs []uint16) []Word {
	out := []Word{Uint64Word(uint64(len(vs)))}
	for _, v := range vs {
		out = append(out, Uint64Word(uint64(v)))
	}
	return out
}

// ParseAddressLoose accepts any letter case; it is for addresses read from RPC responses and
// configuration that was already validated, never for user input.
func ParseAddressLoose(s string) (Address, error) {
	return ParseAddress(strings.ToLower(s))
}

// Lower is the lowercase hex form used as a lookup key.
func (a Address) Lower() string { return "0x" + hex.EncodeToString(a[:]) }
