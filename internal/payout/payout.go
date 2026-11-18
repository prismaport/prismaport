// Package payout is the split rule shared with the EVM contracts and the Solana program: after
// the fee, the net of a settlement goes to 1 to 5 recipients in fixed basis-point shares. Every
// recipient after the first gets floor(net * bps / 10_000); the first gets the rest, so the
// shares always add up to exactly the net. Vectors: contracts/test/vectors/splits.json.
package payout

import (
	"errors"
	"fmt"
	"strings"

	"github.com/prismaport/prismaport/internal/fee"
	"github.com/prismaport/prismaport/internal/money"
)

// Split limits: at most MaxRecipients recipients whose shares add up to BpsTotal basis points.
const (
	MaxRecipients = 5
	BpsTotal      = 10_000
	// shareDecimals is the precision of a share on the wire: "70.00" percent is 7000 bps.
	shareDecimals = 2
)

// Errors returned when a split fails validation.
var (
	ErrCount       = errors.New("payout: a split has 1 to 5 recipients")
	ErrZeroShare   = errors.New("payout: every share must be above 0%")
	ErrSum         = errors.New("payout: shares must add up to exactly 100%")
	ErrShareFormat = errors.New("payout: a share is a percentage above 0 and at most 100 with up to two decimals, such as \"70.00\"")
	ErrMismatch    = errors.New("payout: observed amounts do not follow the split rule")
)

// CheckBps validates shares in basis points: 1 to MaxRecipients entries, each above zero,
// adding up to exactly BpsTotal (summed as int, so values that would wrap a uint16 are caught).
func CheckBps(bps []uint16) error {
	if len(bps) == 0 || len(bps) > MaxRecipients {
		return ErrCount
	}
	sum := 0
	for _, b := range bps {
		if b == 0 {
			return ErrZeroShare
		}
		sum += int(b)
	}
	if sum != BpsTotal {
		return fmt.Errorf("%w (now %s%%)", ErrSum, FormatShareSum(sum))
	}
	return nil
}

// Shares divides net by the split rule. bps must pass CheckBps.
func Shares(net money.Units, bps []uint16) ([]money.Units, error) {
	if err := CheckBps(bps); err != nil {
		return nil, err
	}
	out := make([]money.Units, len(bps))
	rest := net
	for i := 1; i < len(bps); i++ {
		s, err := money.MulDivDown(net, uint64(bps[i]), BpsTotal)
		if err != nil {
			return nil, err
		}
		if rest, err = rest.Sub(s); err != nil {
			return nil, err
		}
		out[i] = s
	}
	out[0] = rest
	return out, nil
}

// Settlement is the fee rule followed by the split. Dust means nothing settles: the fee would
// reach the gross, so no transfer happens and Shares is empty.
type Settlement struct {
	Gross  money.Units
	Fee    money.Units
	Net    money.Units
	Shares []money.Units
	Dust   bool
}

// Compute applies the fee rule to gross and splits the net.
func Compute(gross money.Units, decimals uint8, p fee.Params, bps []uint16) (Settlement, error) {
	if err := CheckBps(bps); err != nil {
		return Settlement{}, err
	}
	f, err := fee.Compute(gross, decimals, p)
	if err != nil {
		return Settlement{}, err
	}
	if f.Dust {
		return Settlement{Gross: gross, Fee: money.Zero, Net: money.Zero, Dust: true}, nil
	}
	shares, err := Shares(f.Net, bps)
	if err != nil {
		return Settlement{}, err
	}
	return Settlement{Gross: gross, Fee: f.Fee, Net: f.Net, Shares: shares}, nil
}

// Verify checks an observed settlement against the rule: the fee and every recipient's amount
// (one entry per recipient, zero where nothing arrived) must be exactly what the rule gives, so
// fee + sum(payouts) == gross holds too.
func Verify(gross money.Units, decimals uint8, p fee.Params, bps []uint16, observedFee money.Units, observed []money.Units) error {
	want, err := Compute(gross, decimals, p, bps)
	if err != nil {
		return err
	}
	if want.Dust {
		return fmt.Errorf("%w: a gross of %s is dust under the fee rule", ErrMismatch, gross)
	}
	if !want.Fee.Equal(observedFee) {
		return fmt.Errorf("%w: fee %s, the rule gives %s", ErrMismatch, observedFee, want.Fee)
	}
	if len(observed) != len(want.Shares) {
		return fmt.Errorf("%w: %d payouts for %d recipients", ErrMismatch, len(observed), len(want.Shares))
	}
	for i := range observed {
		if !observed[i].Equal(want.Shares[i]) {
			return fmt.Errorf("%w: recipient %d received %s, the rule gives %s", ErrMismatch, i+1, observed[i], want.Shares[i])
		}
	}
	return nil
}

// ParseShare reads a percentage share ("70.00", "70", "33.33") into basis points.
func ParseShare(s string) (uint16, error) {
	u, err := money.ParseDecimal(s, shareDecimals)
	if err != nil || u.IsZero() || u.Cmp(money.FromUint64(BpsTotal)) > 0 {
		return 0, ErrShareFormat
	}
	return uint16(u.Big().Uint64()), nil
}

// FormatShare renders basis points as a percentage with two decimals: 7000 is "70.00".
func FormatShare(bps uint16) string {
	return money.FormatDecimal(money.FromUint64(uint64(bps)), shareDecimals)
}

// FormatShareSum renders a sum of basis points as a percentage for messages, without needless
// decimals: 9000 is "90", 9050 is "90.5".
func FormatShareSum(sum int) string {
	if sum < 0 {
		return "-" + FormatShareSum(-sum)
	}
	s := money.FormatDecimal(money.FromUint64(uint64(sum)), shareDecimals)
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

// Recipient is one payout destination committed by a deposit address or a signed nonce: an
// address of the chain family and its share in basis points. Order matters: the first takes
// the rounding remainder, and the order is part of the address and of the nonce.
type Recipient struct {
	Address string
	Bps     uint16
}

// Bps lists the recipients' shares in order.
func Bps(rs []Recipient) []uint16 {
	out := make([]uint16, len(rs))
	for i, r := range rs {
		out[i] = r.Bps
	}
	return out
}

// Errors returned when a split's recipient addresses fail validation.
var (
	ErrZeroRecipient      = errors.New("payout: a recipient address is zero")
	ErrDuplicateRecipient = errors.New("payout: recipients must be distinct")
)
