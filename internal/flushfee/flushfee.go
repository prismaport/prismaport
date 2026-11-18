// Package flushfee reconciles the fees a session's deposit-path flushes charged against what one
// flush of the same funds would have charged (ADR 0008 I-1). The deployed forwarders charge the
// whole fixed fee on every flush, and anyone may call flush: a third party flushing after each part
// payment makes the merchant pay the fixed fee once per flush. The contracts are immutable, so the
// backend detects it after the fact and raises an alert.
package flushfee

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/prismaport/prismaport/internal/alert"
	"github.com/prismaport/prismaport/internal/fee"
	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/store"
)

// AlertKind is the alert raised when a session paid the fixed fee more than once on flushes.
const AlertKind = "flush_fixed_fee_excess"

// Excess is what a session's flushes of one (chain, asset) charged above a single flush.
type Excess struct {
	Chain, Asset string
	Decimals     uint8
	// Flushes counts the session's flush settlements of the asset; FixedFee is one fixed fee in
	// the token's base units.
	Flushes  int
	FixedFee money.Units
	// Gross and Charged are the flushes' total gross and total fee; SingleFlush is the fee one
	// flush of that gross would have charged. Extra is Charged - SingleFlush: (Flushes - 1) fixed
	// fees, less any rounding the percentage part gained by being applied per flush.
	Gross, Charged, SingleFlush, Extra money.Units
}

// Reconcile compares the fees of the session's flush settlements of (chain, asset) with one
// flush of their total. ok is false when there is at most one flush, or the flushes charged
// nothing above a single one. A flush settlement is one recorded from a transaction that is not
// one of the session's payments: signed-path and connected-wallet settlements happen in the
// payment's own transaction, the payer signed them, and they cannot be repeated by a third party.
// Simulated settlements moved nothing and are ignored.
func Reconcile(ses *store.Session, chainID, asset string) (Excess, bool, error) {
	var pt *store.PayToRecord
	for i := range ses.PayTo {
		if ses.PayTo[i].Chain == chainID && ses.PayTo[i].Asset == asset {
			pt = &ses.PayTo[i]
		}
	}
	if pt == nil {
		return Excess{}, false, nil
	}
	paymentTx := map[string]bool{}
	for _, p := range ses.State.Payments {
		if p.Chain == chainID {
			paymentTx[strings.ToLower(p.TxHash)] = true
		}
	}
	ex := Excess{Chain: chainID, Asset: asset, Decimals: pt.Decimals}
	for _, st := range ses.Settlements {
		if st.Chain != chainID || st.Asset != asset || st.Simulated || paymentTx[strings.ToLower(st.TxHash)] {
			continue
		}
		var err error
		if ex.Gross, err = ex.Gross.Add(st.Gross); err != nil {
			return Excess{}, false, err
		}
		if ex.Charged, err = ex.Charged.Add(st.Fee); err != nil {
			return Excess{}, false, err
		}
		ex.Flushes++
	}
	if ex.Flushes < 2 {
		return Excess{}, false, nil
	}
	fixed, err := fee.FixedFee(pt.Fee, pt.Decimals)
	if err != nil {
		return Excess{}, false, err
	}
	ex.FixedFee = fixed
	one, err := fee.Compute(ex.Gross, pt.Decimals, pt.Fee)
	if err != nil {
		return Excess{}, false, err
	}
	// Every flush settled (its fee was below its gross), so their sum settles too; Dust cannot
	// happen here, but a zero single fee would only overstate the excess.
	ex.SingleFlush = one.Fee
	if ex.Charged.Cmp(ex.SingleFlush) <= 0 {
		return Excess{}, false, nil
	}
	if ex.Extra, err = ex.Charged.Sub(ex.SingleFlush); err != nil {
		return Excess{}, false, err
	}
	return ex, true, nil
}

// Details are the keys recorded on the flush job that found the excess, amounts in base units.
func (ex Excess) Details() map[string]string {
	return map[string]string{
		"flush_settlements": strconv.Itoa(ex.Flushes),
		"flush_fee_charged": ex.Charged.String(),
		"single_flush_fee":  ex.SingleFlush.String(),
		"extra_fixed_fee":   ex.Extra.String(),
	}
}

// Alert is the alert for the excess on ses. Amounts are in token units (6 places for
// USDC), so they read as US dollars.
func (ex Excess) Alert(ses *store.Session) alert.Alert {
	f := func(u money.Units) string { return money.FormatMinPlaces(u, ex.Decimals, 2) }
	return alert.Alert{Severity: alert.Warning, Chain: ex.Chain, Kind: AlertKind,
		Message: fmt.Sprintf("session paid the fixed fee on %d flushes of %s: %s charged, %s for one flush, %s extra",
			ex.Flushes, ex.Asset, f(ex.Charged), f(ex.SingleFlush), f(ex.Extra)),
		Fields: map[string]string{"merchant": ses.MerchantID, "session": ses.ID, "livemode": strconv.FormatBool(ses.Livemode),
			"asset": ex.Asset, "flushes": strconv.Itoa(ex.Flushes), "fixed_fee": f(ex.FixedFee), "fee_charged": f(ex.Charged),
			"single_flush_fee": f(ex.SingleFlush), "extra_fee": f(ex.Extra)}}
}
