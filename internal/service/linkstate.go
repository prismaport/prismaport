package service

import (
	"fmt"
	"sort"
	"time"

	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/store"
)

// Why a link is not live. The order is the precedence when several apply.
const (
	// ReasonProductInactive: the product the link sells is deactivated; reactivating it brings
	// the link back.
	ReasonProductInactive = "product_inactive"
	ReasonInactive        = "inactive"
	ReasonExpired         = "expired"
	ReasonExhausted       = "exhausted"
	// ReasonReserved: paid and still-open sessions hold every max_payments slot. The link is
	// live again as soon as one of the open sessions expires unpaid.
	ReasonReserved = "reserved"
)

// CodeLinkUnavailable is returned when a payment link may not open a session now.
const CodeLinkUnavailable = "link_unavailable"

// Liveness reports whether a link may open a session now and, when it may not, why. A link is
// live when it and its product are active, it is not expired and it has a free max_payments slot
// (counted over the link's own sessions).
func Liveness(l store.PaymentLink, productActive bool, c store.LinkCounts, now time.Time) (bool, string) {
	switch {
	case !productActive:
		return false, ReasonProductInactive
	case !l.Active:
		return false, ReasonInactive
	case l.ExpiresAt != nil && !now.Before(*l.ExpiresAt):
		return false, ReasonExpired
	case l.MaxPayments != nil && c.Paid >= int64(*l.MaxPayments):
		return false, ReasonExhausted
	case l.MaxPayments != nil && c.Held >= int64(*l.MaxPayments):
		return false, ReasonReserved
	}
	return true, ""
}

func linkUnavailable(reason string) *Error {
	msg := "This link is no longer available."
	switch reason {
	case ReasonExpired:
		msg = "This link has expired."
	case ReasonExhausted:
		msg = "This link has reached its payment limit."
	case ReasonReserved:
		msg = "Every remaining payment of this link is in progress. Try again later."
	}
	return newErr(KindGone, CodeLinkUnavailable, "%s", msg)
}

// Conversion is paid / opened as a decimal fraction with four places, rounded down, computed
// in integers. It is nil when nothing was opened.
func Conversion(paid, opened int64) *string {
	if opened <= 0 {
		return nil
	}
	if paid < 0 {
		paid = 0
	}
	bp := paid * 10000 / opened
	s := fmt.Sprintf("%d.%04d", bp/10000, bp%10000)
	return &s
}

// AssetTotal is an exact amount of one asset.
type AssetTotal struct {
	Asset  string `json:"asset"`
	Amount string `json:"amount"`
}

// ReceivedByAsset merges per-decimals sums into one exact amount per asset symbol. An asset seen
// with one number of decimals keeps them ("25.000000"); mixed decimals are summed in the
// canonical scale and keep every non-zero digit.
func ReceivedByAsset(list []store.AssetAmount) ([]AssetTotal, error) {
	type acc struct {
		sum      money.Units
		decimals map[uint8]bool
		maxDec   uint8
	}
	by := map[string]*acc{}
	for _, a := range list {
		c, err := chain.ToCanonical(a.Amount, a.Decimals)
		if err != nil {
			return nil, err
		}
		x := by[a.Asset]
		if x == nil {
			x = &acc{decimals: map[uint8]bool{}}
			by[a.Asset] = x
		}
		if x.sum, err = x.sum.Add(c); err != nil {
			return nil, err
		}
		x.decimals[a.Decimals] = true
		x.maxDec = max(x.maxDec, a.Decimals)
	}
	out := []AssetTotal{}
	for asset, x := range by {
		var amount string
		if len(x.decimals) == 1 {
			u, err := money.Rescale(x.sum, chain.CanonicalDecimals, x.maxDec)
			if err != nil {
				return nil, err
			}
			amount = money.FormatDecimal(u, x.maxDec)
		} else {
			amount = money.FormatMinPlaces(x.sum, chain.CanonicalDecimals, x.maxDec)
		}
		out = append(out, AssetTotal{Asset: asset, Amount: amount})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Asset < out[j].Asset })
	return out, nil
}
