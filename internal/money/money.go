// Package money holds integer amounts in base units and their exact decimal-string forms.
// Nothing here ever touches a float.
package money

import (
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// USDDecimals is the scale of USD amounts (cents).
const USDDecimals = 2

// MaxDecimals bounds token decimals so rescaling stays far inside uint256.
const MaxDecimals = 36

// Errors returned when parsing or computing amounts.
var (
	ErrSyntax    = errors.New("money: invalid decimal amount")
	ErrPrecision = errors.New("money: more fractional digits than the asset supports")
	ErrRange     = errors.New("money: amount out of range")
	ErrNegative  = errors.New("money: negative result")
	ErrInexact   = errors.New("money: conversion would lose precision")
	ErrDecimals  = errors.New("money: unsupported decimals")
)

var maxUnits = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))

// Units is a non-negative integer amount, at most 2^256-1, in some asset's base units.
// The zero value is zero. Units is immutable: every operation returns a new value.
type Units struct {
	v *big.Int
}

// Zero is the zero amount.
var Zero = Units{}

// MaxUnits returns 2^256-1, the largest representable amount.
func MaxUnits() Units { return Units{v: new(big.Int).Set(maxUnits)} }

func (u Units) big() *big.Int {
	if u.v == nil {
		return new(big.Int)
	}
	return u.v
}

// FromUint64 builds an amount from an integer number of base units.
func FromUint64(n uint64) Units { return Units{v: new(big.Int).SetUint64(n)} }

// FromBig builds an amount from a big integer, rejecting negatives and values above 2^256-1.
func FromBig(b *big.Int) (Units, error) {
	if b == nil {
		return Zero, nil
	}
	if b.Sign() < 0 {
		return Zero, ErrNegative
	}
	if b.Cmp(maxUnits) > 0 {
		return Zero, ErrRange
	}
	return Units{v: new(big.Int).Set(b)}, nil
}

// ParseInteger parses a plain base-unit integer string ("25000000"), as stored in the database.
func ParseInteger(s string) (Units, error) {
	if s == "" || !allDigits(s) || (len(s) > 1 && s[0] == '0') {
		return Zero, fmt.Errorf("%w: %q", ErrSyntax, s)
	}
	b, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return Zero, fmt.Errorf("%w: %q", ErrSyntax, s)
	}
	return FromBig(b)
}

// MustParseInteger is ParseInteger for constants in tests and tables; it panics on bad input.
func MustParseInteger(s string) Units {
	u, err := ParseInteger(s)
	if err != nil {
		panic(err)
	}
	return u
}

// Big returns a copy of the amount as a big integer.
func (u Units) Big() *big.Int { return new(big.Int).Set(u.big()) }

// String returns the base-unit integer string.
func (u Units) String() string { return u.big().String() }

// IsZero reports whether the amount is zero.
func (u Units) IsZero() bool { return u.big().Sign() == 0 }

// Cmp compares two amounts and returns -1, 0 or +1.
func (u Units) Cmp(o Units) int { return u.big().Cmp(o.big()) }

// Equal reports whether two amounts are equal.
func (u Units) Equal(o Units) bool { return u.Cmp(o) == 0 }

// Add returns u+o, failing when the sum leaves the uint256 range.
func (u Units) Add(o Units) (Units, error) {
	return FromBig(new(big.Int).Add(u.big(), o.big()))
}

// Sub returns u-o, failing when the result would be negative.
func (u Units) Sub(o Units) (Units, error) {
	return FromBig(new(big.Int).Sub(u.big(), o.big()))
}

// ParseDecimal parses a canonical non-negative decimal string ("25", "25.5", "25.000001") into
// base units of an asset with the given decimals. It never rounds: more fractional digits than
// decimals is an error, as are signs, exponents, whitespace and leading zeros.
func ParseDecimal(s string, decimals uint8) (Units, error) {
	if decimals > MaxDecimals {
		return Zero, ErrDecimals
	}
	intPart, fracPart, hasPoint := strings.Cut(s, ".")
	if intPart == "" || !allDigits(intPart) || (len(intPart) > 1 && intPart[0] == '0') {
		return Zero, fmt.Errorf("%w: %q", ErrSyntax, s)
	}
	if hasPoint {
		if fracPart == "" || !allDigits(fracPart) {
			return Zero, fmt.Errorf("%w: %q", ErrSyntax, s)
		}
		if len(fracPart) > int(decimals) {
			return Zero, fmt.Errorf("%w: %q has %d, max %d", ErrPrecision, s, len(fracPart), decimals)
		}
	}
	digits := intPart + fracPart + strings.Repeat("0", int(decimals)-len(fracPart))
	b, ok := new(big.Int).SetString(digits, 10)
	if !ok {
		return Zero, fmt.Errorf("%w: %q", ErrSyntax, s)
	}
	return FromBig(b)
}

// FormatDecimal renders base units with exactly `decimals` fractional digits ("25.000000").
func FormatDecimal(u Units, decimals uint8) string {
	s := u.big().String()
	if decimals == 0 {
		return s
	}
	d := int(decimals)
	if len(s) <= d {
		s = strings.Repeat("0", d-len(s)+1) + s
	}
	return s[:len(s)-d] + "." + s[len(s)-d:]
}

// FormatMinPlaces renders base units with at least minPlaces fractional digits, keeping every
// non-zero digit beyond that: FormatMinPlaces(10_000001 µ, 6, 2) == "10.000001" while
// 10_000000 µ renders "10.00". The value is always exact.
func FormatMinPlaces(u Units, decimals, minPlaces uint8) string {
	full := FormatDecimal(u, decimals)
	if decimals == 0 || minPlaces >= decimals {
		return full
	}
	keep := len(full) - int(decimals-minPlaces)
	trimmed := strings.TrimRight(full[keep:], "0")
	out := full[:keep] + trimmed
	if minPlaces == 0 {
		out = strings.TrimSuffix(out, ".")
	}
	return out
}

// Rescale converts an amount between scales. Scaling up is always exact; scaling down fails
// with ErrInexact unless the dropped digits are all zero.
func Rescale(u Units, from, to uint8) (Units, error) {
	if from > MaxDecimals || to > MaxDecimals {
		return Zero, ErrDecimals
	}
	switch {
	case from == to:
		return u, nil
	case to > from:
		return FromBig(new(big.Int).Mul(u.big(), pow10(to-from)))
	default:
		q, r := new(big.Int).QuoRem(u.big(), pow10(from-to), new(big.Int))
		if r.Sign() != 0 {
			return Zero, ErrInexact
		}
		return FromBig(q)
	}
}

// USDToToken converts a USD amount in cents into the base units of a 1:1 USD stablecoin.
func USDToToken(cents Units, tokenDecimals uint8) (Units, error) {
	if tokenDecimals < USDDecimals {
		return Zero, ErrDecimals
	}
	return Rescale(cents, USDDecimals, tokenDecimals)
}

// ParseUSD parses a USD decimal string with at most two fractional digits into cents.
func ParseUSD(s string) (Units, error) { return ParseDecimal(s, USDDecimals) }

// FormatUSD renders cents with exactly two fractional digits.
func FormatUSD(cents Units) string { return FormatDecimal(cents, USDDecimals) }

// Pow10 returns 10^n as base units.
func Pow10(n uint8) Units { return Units{v: pow10(n)} }

// MulDivDown returns floor(u * num / den). den must be positive.
func MulDivDown(u Units, num, den uint64) (Units, error) {
	if den == 0 {
		return Zero, errors.New("money: division by zero")
	}
	p := new(big.Int).Mul(u.big(), new(big.Int).SetUint64(num))
	return FromBig(p.Quo(p, new(big.Int).SetUint64(den)))
}

// Mul returns u * o.
func (u Units) Mul(o Units) (Units, error) {
	return FromBig(new(big.Int).Mul(u.big(), o.big()))
}

// Max returns the larger of a and b.
func Max(a, b Units) Units {
	if a.Cmp(b) >= 0 {
		return a
	}
	return b
}

func pow10(n uint8) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
