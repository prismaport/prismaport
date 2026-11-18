package detector

import (
	"context"

	"github.com/prismaport/prismaport/internal/alert"
	"github.com/prismaport/prismaport/internal/chainsettle"
	"github.com/prismaport/prismaport/internal/contracts"
	"github.com/prismaport/prismaport/internal/derive"
	"github.com/prismaport/prismaport/internal/evm"
	"github.com/prismaport/prismaport/internal/evmrpc"
	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/payout"
	"github.com/prismaport/prismaport/internal/store"
)

// recipients parses an entry's payout snapshot into the addresses and shares it commits to.
func recipients(e store.WatchEntry) ([]evm.Address, []uint16, bool) {
	addrs, bps, err := derive.EVMRecipients(e.Recipients)
	return addrs, bps, err == nil
}

// gatewayNonce is the signed-path nonce a session's pay_to entry commits to.
func gatewayNonce(chainID uint64, gateway evm.Address, e store.WatchEntry) (evm.Word, bool) {
	tre, err := evm.ParseAddressLoose(e.Treasury)
	addrs, bps, ok := recipients(e)
	if err != nil || !ok {
		return evm.Word{}, false
	}
	return evm.GatewayNonce(chainID, gateway, e.Salt, tre, e.Fee.FeeBps, e.Fee.FixedFeeMicros, addrs, bps), true
}

type split struct {
	logIndex uint32
	fee, net money.Units
	payouts  []money.Units
}

// onGateway credits a signed-path payment. Everything is checked before anything is recorded:
// the event comes from the configured Gateway and names a watched session's nonce, the fee and
// every recipient's Payout follow the fee and split rule of that session (gatewayEvent,
// gatewaySplit), and the token's own Transfer into the Gateway in the same transaction carries
// the gross (gatewayTransfer).
func (d *Detector) onGateway(ctx context.Context, ws watchSet, l evmrpc.Log, gatewayLogs, transfers []evmrpc.Log) (bool, error) {
	if d.cfg.Gateway == nil || l.Address != *d.cfg.Gateway || len(l.Topics) == 0 || l.Topics[0] != PaymentReceivedTopic {
		return false, nil
	}
	if len(l.Topics) != 4 {
		return false, nil
	}
	e, ok := ws.byNonce[l.Topics[1]]
	if !ok {
		return false, nil // someone else's settlement
	}
	pr, ok := d.gatewayEvent(ctx, e, l)
	if !ok {
		return false, nil
	}
	shares, ok := d.gatewaySplit(ctx, e, l, pr, gatewayLogs)
	if !ok {
		return false, nil
	}
	// Credit comes from the token's own Transfer into the Gateway in the same transaction, not
	// from the Gateway's event alone.
	credit := d.gatewayTransfer(l, pr, transfers)
	if credit == nil {
		d.reject(ctx, e, l, "PaymentReceived without the matching token Transfer")
		return false, nil
	}
	return true, d.record(ctx, e, *credit, credit.LogIndex, pr.Gross, store.KindGateway,
		&split{logIndex: l.LogIndex, fee: pr.Fee, net: pr.Net, payouts: shares})
}

// gatewayEvent parses a PaymentReceived event of the session's nonce and checks its token and
// gross; a failed check is rejected (or, for our own broken record, reported) and ok is false.
func (d *Detector) gatewayEvent(ctx context.Context, e store.WatchEntry, l evmrpc.Log) (contracts.PaymentReceived, bool) {
	pr, ok := contracts.ParsePaymentReceived(l)
	if !ok {
		d.reject(ctx, e, l, "malformed PaymentReceived")
		return pr, false
	}
	wantToken, err := evm.ParseAddressLoose(e.Token)
	if err != nil {
		// Our own record is broken, not the event: say so instead of "token not accepted".
		d.unparseable(ctx, e, "token", err)
		return pr, false
	}
	if pr.Token != wantToken {
		d.reject(ctx, e, l, "PaymentReceived for a token the session does not accept")
		return pr, false
	}
	if pr.Gross.IsZero() {
		d.reject(ctx, e, l, "PaymentReceived amounts out of range")
		return pr, false
	}
	return pr, true
}

// gatewaySplit checks the event's fee, net and Payout events against the session's fee and
// split rule and returns each recipient's share; a mismatch is rejected and ok is false.
func (d *Detector) gatewaySplit(ctx context.Context, e store.WatchEntry, l evmrpc.Log, pr contracts.PaymentReceived,
	gatewayLogs []evmrpc.Log) ([]money.Units, bool) {
	addrs, bps, ok := recipients(e)
	if !ok {
		d.reject(ctx, e, l, "the session's payout snapshot is not a valid split")
		return nil, false
	}
	var bounds []uint32
	for _, o := range gatewayLogs {
		if o.TxHash == l.TxHash && len(o.Topics) > 0 && o.Topics[0] == PaymentReceivedTopic && o.LogIndex != l.LogIndex {
			bounds = append(bounds, o.LogIndex)
		}
	}
	events := contracts.PayoutsBefore(gatewayLogs, *d.cfg.Gateway, pr.Token, l.TxHash, l.LogIndex, bounds)
	shares, ok := contracts.MatchPayouts(addrs, events)
	if !ok {
		d.reject(ctx, e, l, "Payout events name a recipient outside the session's split or break its order")
		return nil, false
	}
	if err := payout.Verify(pr.Gross, e.Decimals, e.Fee, bps, pr.Fee, shares); err != nil {
		d.reject(ctx, e, l, "PaymentReceived split differs from the fee and split rule: "+err.Error())
		return nil, false
	}
	if net, err := pr.Gross.Sub(pr.Fee); err != nil || !net.Equal(pr.Net) {
		d.reject(ctx, e, l, "PaymentReceived net differs from gross minus fee")
		return nil, false
	}
	return shares, true
}

// gatewayTransfer is the token's Transfer from the payer into the Gateway of the event's gross,
// before the event in the same transaction (the last such one), or nil.
func (d *Detector) gatewayTransfer(l evmrpc.Log, pr contracts.PaymentReceived, transfers []evmrpc.Log) *evmrpc.Log {
	var credit *evmrpc.Log
	for i := range transfers {
		t := transfers[i]
		if t.TxHash != l.TxHash || t.Address != pr.Token || t.LogIndex >= l.LogIndex || len(t.Topics) != 3 {
			continue
		}
		tf, okF := t.Topics[1].Address()
		tt, okT := t.Topics[2].Address()
		amt, okA := amountWord(t.Data)
		if okF && okT && okA && tf == pr.From && tt == *d.cfg.Gateway && amt.Equal(pr.Gross) {
			credit = &transfers[i]
		}
	}
	return credit
}

// reject raises a critical alert for a gateway event that does not match its session; the event
// is not credited.
func (d *Detector) reject(ctx context.Context, e store.WatchEntry, l evmrpc.Log, why string) {
	d.alerts.Alert(ctx, alert.Alert{Severity: alert.Critical, Chain: d.chainID(), Kind: "gateway_event_rejected", Message: why,
		Fields: map[string]string{"session": e.SessionID, "tx": l.TxHash.Hex()}})
}

// settleGateway records the signed-path split once its block is final. It runs before the
// payment is finalized, so a crash in between retries it (a duplicate is a conflict, ignored).
func (d *Detector) settleGateway(ctx context.Context, p store.TrackedPayment) error {
	return chainsettle.RecordSplit(ctx, d.st, d.alerts, d.chainID(), "detector: gateway", p)
}

// GatewaySettled records the signed-path payment of a settle job the sender saw mined in block:
// the block's agreed hash, then its Gateway events (and the token transfers into the Gateway)
// through two-provider agreement. When the settle reverted (someone else may have settled the
// same authorization first) the range runs back to the first block the session's address was
// watched from. It returns the number of payments recorded.
func (d *Detector) GatewaySettled(ctx context.Context, j store.TxJob, block uint64, reverted bool) (int, error) {
	if d.cfg.Gateway == nil {
		return 0, nil
	}
	entries, err := d.st.SessionWatchEntries(ctx, d.chainID(), j.SessionID)
	if err != nil {
		return 0, err
	}
	ws := d.buildWatch(entries)
	if len(ws.byNonce) == 0 {
		return 0, nil
	}
	ws.byAddr, ws.addrs = map[[2]evm.Address]store.WatchEntry{}, nil
	hash, err := d.q.BlockHash(ctx, block)
	if err != nil {
		return 0, err
	}
	from := block
	if reverted {
		for _, e := range entries {
			from = min(from, d.firstBlock(block, e))
		}
	}
	found, err := d.scanRange(ctx, ws, from, block, map[uint64]evm.Word{block: hash})
	d.log.Info("settle job's gateway events read", "chain", d.chainID(), "job", j.ID, "session", j.SessionID, "from", from,
		"to", block, "found", found)
	return found, err
}
