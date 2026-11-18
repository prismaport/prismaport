// Package fee implements the protocol fee rule shared with the on-chain programs:
// fee = fixedFee + gross*feeBps/10_000 rounded down; no settlement when fee >= gross.
package fee

import (
	"fmt"

	"github.com/prismaport/prismaport/internal/money"
)

// Fee bounds every chain's parameters must stay within.
const (
	MaxFeeBps         = 300
	MaxFixedFeeMicros = 5_000_000
	MinTokenDecimals  = 6
	bpsDenominator    = 10_000
	microsDecimals    = 6
)

// Errors returned by Params.Validate.
var (
	ErrFeeBpsCap    = fmt.Errorf("fee: feeBps above %d", MaxFeeBps)
	ErrFixedFeeCap  = fmt.Errorf("fee: fixedFeeMicros above %d", MaxFixedFeeMicros)
	ErrTokenDecimal = fmt.Errorf("fee: token decimals below %d", MinTokenDecimals)
)

// Params are the fee parameters fixed per forwarder or bound in a signed nonce.
type Params struct {
	FeeBps         uint16
	FixedFeeMicros uint64
}

// Validate returns an error when the fee parameters exceed a bound.
func (p Params) Validate() error {
	if p.FeeBps > MaxFeeBps {
		return ErrFeeBpsCap
	}
	if p.FixedFeeMicros > MaxFixedFeeMicros {
		return ErrFixedFeeCap
	}
	return nil
}

// Split is the outcome of applying the fee rule to a gross amount. When Dust is true nothing
// settles: Fee and Net are zero and the funds stay where they are.
type Split struct {
	Gross money.Units
	Fee   money.Units
	Net   money.Units
	Dust  bool
}

// FixedFee is fixedFeeMicros scaled to the token's base units.
func FixedFee(p Params, decimals uint8) (money.Units, error) {
	if decimals < MinTokenDecimals {
		return money.Zero, ErrTokenDecimal
	}
	return money.Rescale(money.FromUint64(p.FixedFeeMicros), microsDecimals, decimals)
}

// Compute applies the fee rule to gross base units of a token with the given decimals.
func Compute(gross money.Units, decimals uint8, p Params) (Split, error) {
	if err := p.Validate(); err != nil {
		return Split{}, err
	}
	fixedFee, err := FixedFee(p, decimals)
	if err != nil {
		return Split{}, err
	}
	pct, err := money.MulDivDown(gross, uint64(p.FeeBps), bpsDenominator)
	if err != nil {
		return Split{}, err
	}
	f, err := fixedFee.Add(pct)
	if err != nil {
		return Split{}, err
	}
	if f.Cmp(gross) >= 0 {
		return Split{Gross: gross, Dust: true}, nil
	}
	net, err := gross.Sub(f)
	if err != nil {
		return Split{}, err
	}
	return Split{Gross: gross, Fee: f, Net: net}, nil
}

// Cap is the most the contracts can ever take from gross, whatever the parameters.
func Cap(gross money.Units, decimals uint8) (money.Units, error) {
	pct, err := money.MulDivDown(gross, MaxFeeBps, bpsDenominator)
	if err != nil {
		return money.Zero, err
	}
	fixedCap, err := FixedFee(Params{FixedFeeMicros: MaxFixedFeeMicros}, decimals)
	if err != nil {
		return money.Zero, err
	}
	return pct.Add(fixedCap)
}

// SettlesUSD reports whether a payment of exactly a USD amount in cents settles instead of being
// dust. A whole-cent amount scales to every token with an exact percentage, so the answer is the
// same for every token decimals.
func SettlesUSD(cents money.Units, p Params) (bool, error) {
	micros, err := money.Rescale(cents, money.USDDecimals, microsDecimals)
	if err != nil {
		return false, err
	}
	s, err := Compute(micros, microsDecimals, p)
	if err != nil {
		return false, err
	}
	return !s.Dust, nil
}

// MinSettlingCents is the smallest USD amount in cents that settles: the least c with
// fixedFeeMicros + c*feeBps < c*10_000 (c cents are c*10_000 micros), i.e.
// c*(10_000 - feeBps) > fixedFeeMicros.
func MinSettlingCents(p Params) (money.Units, error) {
	if err := p.Validate(); err != nil {
		return money.Zero, err
	}
	return money.FromUint64(p.FixedFeeMicros/(bpsDenominator-uint64(p.FeeBps)) + 1), nil
}
