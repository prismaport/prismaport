// Package base58 implements the Bitcoin-alphabet base58 used by Solana public keys.
package base58

import (
	"errors"
	"math/big"
)

const alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// ErrInvalid is returned when the input has a character outside the base58 alphabet or is empty.
var ErrInvalid = errors.New("base58: invalid character")

var index = func() [256]int {
	var t [256]int
	for i := range t {
		t[i] = -1
	}
	for i := 0; i < len(alphabet); i++ {
		t[alphabet[i]] = i
	}
	return t
}()

var radix = big.NewInt(58)

// Encode returns b as a base58 string, keeping leading zero bytes as leading 1s.
func Encode(b []byte) string {
	zeros := 0
	for zeros < len(b) && b[zeros] == 0 {
		zeros++
	}
	n := new(big.Int).SetBytes(b)
	var out []byte
	mod := new(big.Int)
	for n.Sign() > 0 {
		n.QuoRem(n, radix, mod)
		out = append(out, alphabet[mod.Int64()])
	}
	for i := 0; i < zeros; i++ {
		out = append(out, alphabet[0])
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return string(out)
}

// Decode returns the bytes a base58 string stands for, or ErrInvalid.
func Decode(s string) ([]byte, error) {
	if s == "" {
		return nil, ErrInvalid
	}
	zeros := 0
	for zeros < len(s) && s[zeros] == alphabet[0] {
		zeros++
	}
	n := new(big.Int)
	for i := 0; i < len(s); i++ {
		d := index[s[i]]
		if d < 0 {
			return nil, ErrInvalid
		}
		n.Mul(n, radix)
		n.Add(n, big.NewInt(int64(d)))
	}
	body := n.Bytes()
	out := make([]byte, zeros+len(body))
	copy(out[zeros:], body)
	return out, nil
}
