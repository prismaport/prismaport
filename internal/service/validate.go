package service

import (
	"errors"
	"net/url"
	"regexp"
	"unicode/utf8"

	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/fee"
	"github.com/prismaport/prismaport/internal/money"
)

const (
	maxMetadataKeys     = 50
	maxMetadataKeyLen   = 40
	maxMetadataValueLen = 500
	maxURLLen           = 2048
	maxNameLen          = 200
	maxSuggested        = 3
)

// maxAmountCents caps every USD amount the API accepts (a price, a range bound, a suggested or
// session amount, a link amount): 10^15 cents, 10 trillion USD. Far above any real payment, and
// far inside what every token scale and the fee arithmetic can hold, so an absurd amount is a 400
// here instead of an out-of-range error deep in the session math.
var maxAmountCents = money.Pow10(15)

func validateMetadata(m map[string]string) error {
	if len(m) > maxMetadataKeys {
		return invalid("metadata has more than %d keys", maxMetadataKeys)
	}
	for k, v := range m {
		if k == "" || utf8.RuneCountInString(k) > maxMetadataKeyLen {
			return invalid("metadata keys must have 1 to %d characters", maxMetadataKeyLen)
		}
		if utf8.RuneCountInString(v) > maxMetadataValueLen {
			return invalid("metadata value for %q exceeds %d characters", k, maxMetadataValueLen)
		}
	}
	return nil
}

// Who set a session's client_reference_id.
const (
	// ReferenceMerchant: the merchant's server, through the API. Trusted like metadata.
	ReferenceMerchant = "merchant"
	// ReferencePayer: a payment link URL (?ref=). Anyone can edit a URL: correlation only.
	ReferencePayer = "payer"
)

// ClientReferencePattern is what a client_reference_id may hold, from either source.
var ClientReferencePattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

func validateClientReference(v string) error {
	if !ClientReferencePattern.MatchString(v) {
		return invalid("client_reference_id must have 1 to 128 characters, each a letter, a digit, '.', '_', ':' or '-'")
	}
	return nil
}

// validateURL accepts absolute https URLs; plain http only when allowHTTP.
func validateURL(field, raw string, allowHTTP bool) error {
	if len(raw) > maxURLLen {
		return invalid("%s exceeds %d characters", field, maxURLLen)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return invalid("%s must be an absolute URL without credentials", field)
	}
	if u.Scheme != "https" && !(allowHTTP && u.Scheme == "http") {
		return invalid("%s must use https", field)
	}
	return nil
}

// parseAmount parses a positive USD amount with at most two decimals, up to maxAmountCents.
func parseAmount(field, s string) (money.Units, error) {
	u, err := money.ParseUSD(s)
	if err != nil {
		if errors.Is(err, money.ErrPrecision) {
			return money.Zero, invalid("%s must have at most 2 decimal places", field)
		}
		if errors.Is(err, money.ErrRange) {
			return money.Zero, invalid("%s must be at most %s", field, money.FormatUSD(maxAmountCents))
		}
		return money.Zero, invalid("%s must be a decimal string such as \"25.00\"", field)
	}
	if u.IsZero() {
		return money.Zero, invalid("%s must be greater than zero", field)
	}
	if u.Cmp(maxAmountCents) > 0 {
		return money.Zero, invalid("%s must be at most %s", field, money.FormatUSD(maxAmountCents))
	}
	return u, nil
}

// minPrice is the smallest USD amount in cents that settles on every given chain: a payment of
// exactly that amount, or more, pays the fee and leaves a net instead of being dust.
func minPrice(chains []chain.Chain) (money.Units, error) {
	least := money.FromUint64(1)
	for _, c := range chains {
		m, err := fee.MinSettlingCents(c.Fee)
		if err != nil {
			return money.Zero, err
		}
		least = money.Max(least, m)
	}
	return least, nil
}

func checkFloor(field string, cents money.Units, chains []chain.Chain) error {
	least, err := minPrice(chains)
	if err != nil {
		return err
	}
	if cents.Cmp(least) < 0 {
		return newErr(KindInvalid, CodeAmountBelowMinimum, "%s must be at least %s USD, the smallest amount that settles after the Prismaport fee",
			field, money.FormatUSD(least))
	}
	return nil
}
