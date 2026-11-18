package detector

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"slices"

	"github.com/prismaport/prismaport/internal/alert"
	"github.com/prismaport/prismaport/internal/chainwatch"
	"github.com/prismaport/prismaport/internal/contracts"
	"github.com/prismaport/prismaport/internal/evm"
	"github.com/prismaport/prismaport/internal/evmrpc"
	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/service"
	"github.com/prismaport/prismaport/internal/session"
	"github.com/prismaport/prismaport/internal/store"
)

// watchSet indexes entries by (address, token) for the deposit path and by Gateway nonce for
// the signed path.
type watchSet struct {
	byAddr  map[[2]evm.Address]store.WatchEntry
	byNonce map[evm.Word]store.WatchEntry
	addrs   []evm.Address
	// settledFrom are deployed forwarders whose Settled events a rescan reads too, and flows
	// collects what the rescan saw move (nil: not collected); see reconcile.go.
	settledFrom []evm.Address
	flows       flows
}

func (d *Detector) buildWatch(entries []store.WatchEntry) watchSet {
	ws := watchSet{byAddr: map[[2]evm.Address]store.WatchEntry{}, byNonce: map[evm.Word]store.WatchEntry{}}
	seen := map[evm.Address]bool{}
	for _, e := range entries {
		addr, err1 := evm.ParseAddressLoose(e.Address)
		token, err2 := evm.ParseAddressLoose(e.Token)
		if err1 != nil || err2 != nil {
			d.log.Error("unparseable pay_to entry", "session", e.SessionID, "chain", e.Chain)
			continue
		}
		asset, ok := d.tokens[token]
		if !ok || asset.Symbol != e.Asset {
			continue // not a configured token of this chain
		}
		ws.byAddr[[2]evm.Address{addr, token}] = e
		if !seen[addr] {
			seen[addr] = true
			ws.addrs = append(ws.addrs, addr)
		}
		if asset.EIP3009 && d.cfg.Gateway != nil {
			if n, ok := gatewayNonce(d.cfg.Chain.EVMID, *d.cfg.Gateway, e); ok {
				ws.byNonce[n] = e
			}
		}
	}
	return ws
}

func (d *Detector) tokenList(eip3009Only bool) []evm.Address {
	var out []evm.Address
	for t, a := range d.tokens {
		if !eip3009Only || a.EIP3009 {
			out = append(out, t)
		}
	}
	return out
}

type fetched struct {
	transfers   []evmrpc.Log
	gateway     []evmrpc.Log
	gwTransfers []evmrpc.Log
	settled     []evmrpc.Log
}

func (d *Detector) fetch(ctx context.Context, ws watchSet, from, to uint64) (fetched, error) {
	var f fetched
	tokens := d.tokenList(false)
	for batch := range slices.Chunk(ws.addrs, addressBatch) {
		topics := make([]evm.Word, len(batch))
		for j, a := range batch {
			topics[j] = evm.AddressWord(a)
		}
		ls, err := d.q.Logs(ctx, evmrpc.Filter{FromBlock: from, ToBlock: to, Addresses: tokens,
			Topics: [][]evm.Word{{TransferTopic}, nil, topics}})
		if err != nil {
			return f, err
		}
		f.transfers = append(f.transfers, ls...)
	}
	for batch := range slices.Chunk(ws.settledFrom, addressBatch) {
		ls, err := d.q.Logs(ctx, evmrpc.Filter{FromBlock: from, ToBlock: to, Addresses: batch, Topics: [][]evm.Word{{contracts.SettledTopic}}})
		if err != nil {
			return f, err
		}
		f.settled = append(f.settled, ls...)
	}
	if d.cfg.Gateway != nil && len(ws.byNonce) > 0 {
		gw := *d.cfg.Gateway
		// Payout events come along: they carry the per-recipient split of each payment.
		ls, err := d.q.Logs(ctx, evmrpc.Filter{FromBlock: from, ToBlock: to, Addresses: []evm.Address{gw},
			Topics: [][]evm.Word{{PaymentReceivedTopic, PayoutTopic}}})
		if err != nil {
			return f, err
		}
		f.gateway = ls
		if len(ls) > 0 {
			if f.gwTransfers, err = d.q.Logs(ctx, evmrpc.Filter{FromBlock: from, ToBlock: to, Addresses: d.tokenList(true),
				Topics: [][]evm.Word{{TransferTopic}, nil, {evm.AddressWord(gw)}}}); err != nil {
				return f, err
			}
		}
	}
	return f, nil
}

// process records the payments in f and returns how many logs it attributed to a session.
func (d *Detector) process(ctx context.Context, ws watchSet, f fetched) (int, error) {
	n := 0
	for _, l := range f.transfers {
		ok, err := d.onTransfer(ctx, ws, l)
		if err != nil {
			return n, err
		}
		if ok {
			n++
		}
	}
	for _, l := range f.gateway {
		ok, err := d.onGateway(ctx, ws, l, f.gateway, f.gwTransfers)
		if err != nil {
			return n, err
		}
		if ok {
			n++
		}
	}
	return n, nil
}

func amountWord(data []byte) (money.Units, bool) {
	if len(data) != 32 {
		return money.Zero, false
	}
	u, err := money.FromBig(new(big.Int).SetBytes(data))
	return u, err == nil
}

func (d *Detector) onTransfer(ctx context.Context, ws watchSet, l evmrpc.Log) (bool, error) {
	if len(l.Topics) != 3 || l.Topics[0] != TransferTopic {
		return false, nil // ERC-721 transfers index a fourth topic
	}
	if _, ok := d.tokens[l.Address]; !ok {
		return false, nil
	}
	to, ok := l.Topics[2].Address()
	if !ok {
		return false, nil
	}
	e, ok := ws.byAddr[[2]evm.Address{to, l.Address}]
	if !ok {
		return false, nil
	}
	amount, ok := amountWord(l.Data)
	if !ok || amount.IsZero() {
		return false, nil // zero-value transfers are address poisoning, never a payment
	}
	return true, d.record(ctx, e, l, l.LogIndex, amount, store.KindTransfer, nil)
}

// unparseable reports a watch entry whose stored address or token does not parse: it is skipped
// (never read as the zero address), loudly, since no payment to it can be seen.
func (d *Detector) unparseable(ctx context.Context, e store.WatchEntry, field string, err error) {
	d.log.Error("unparseable pay_to entry", "session", e.SessionID, "chain", e.Chain, "field", field, "err", err)
	d.alerts.Alert(ctx, alert.Alert{Severity: alert.Critical, Chain: d.chainID(), Kind: "watch_entry_unparseable",
		Message: "a session's pay_to entry holds a malformed " + field + "; it is not watched",
		Fields:  map[string]string{"session": e.SessionID, "address": e.Address, "token": e.Token, "error": err.Error()}})
}

// record registers a payment and its on-chain position. It is idempotent: a rescan of the same
// log returns the existing payment. A log that reappears in a different block (re-mined after
// a reorg we have not processed yet) replaces the stale payment.
func (d *Detector) record(ctx context.Context, e store.WatchEntry, l evmrpc.Log, logIndex uint32, amount money.Units, kind string, sp *split) error {
	obs := service.Observation{SessionID: e.SessionID, Chain: e.Chain, Asset: e.Asset, Address: e.Address,
		TxHash: l.TxHash.Hex(), LogIndex: logIndex, Amount: amount}
	pid, _, err := d.svc.RecordPayment(ctx, obs)
	if err != nil {
		if errors.Is(err, session.ErrAmountOutOfRange) || errors.Is(err, session.ErrUnknownPayTo) {
			d.alerts.Alert(ctx, alert.Alert{Severity: alert.Warning, Chain: d.chainID(), Kind: "payment_not_recordable",
				Message: err.Error(), Fields: map[string]string{"session": e.SessionID, "tx": obs.TxHash}})
			return nil
		}
		if errors.Is(err, service.ErrPaymentOnOtherSession) {
			// Another session on the same customer address recorded it first.
			d.log.Info("payment already credited to another session", "chain", d.chainID(), "session", e.SessionID, "tx", obs.TxHash)
			return nil
		}
		if errors.Is(err, service.ErrSessionSuperseded) {
			// A newer session opened on the customer address meanwhile: it reads the address again
			// from the cursor, which this rescan does not move.
			d.log.Info("transfer left to the customer address's newer session", "chain", d.chainID(), "session", e.SessionID, "tx", obs.TxHash)
			return nil
		}
		return fmt.Errorf("record payment %s/%d: %w", obs.TxHash, logIndex, err)
	}
	existing, err := d.st.ChainPaymentsOfSession(ctx, e.SessionID)
	if err != nil {
		return err
	}
	for _, p := range existing {
		if p.PaymentID == pid && p.BlockHash != l.BlockHash.Hex() {
			if _, err := d.svc.ReversePayment(ctx, e.SessionID, pid); err != nil {
				return err
			}
			if pid, _, err = d.svc.RecordPayment(ctx, obs); err != nil {
				return err
			}
		}
	}
	fin := d.st.Now().Add(d.cfg.FinalityDelay)
	cp := store.ChainPayment{PaymentID: pid, SessionID: e.SessionID, Chain: e.Chain, Asset: e.Asset, Kind: kind,
		BlockNumber: l.BlockNumber, BlockHash: l.BlockHash.Hex(), FinalityCheckAt: &fin}
	if sp != nil {
		idx := sp.logIndex
		f, n := sp.fee, sp.net
		cp.SplitLogIndex, cp.SplitFee, cp.SplitNet, cp.SplitPayouts = &idx, &f, &n, sp.payouts
	}
	d.log.Info("payment detected", "chain", e.Chain, "session", e.SessionID, "payment", pid, "kind", kind, "block", l.BlockNumber)
	return d.st.UpsertChainPayment(ctx, cp)
}

func (d *Detector) scanRange(ctx context.Context, ws watchSet, from, to uint64, hashes map[uint64]evm.Word) (int, error) {
	found := 0
	for lo, hi := range chainwatch.LogRanges(from, to, d.cfg.MaxRange) {
		logs, err := d.fetch(ctx, ws, lo, hi)
		if err != nil {
			return found, err
		}
		if err := d.checkBlocks(ctx, logs, hashes); err != nil {
			return found, err
		}
		if ws.flows != nil {
			ws.flows.add(d, logs)
		}
		n, err := d.process(ctx, ws, logs)
		found += n
		if err != nil {
			return found, err
		}
	}
	return found, nil
}

// checkBlocks requires every log to sit in the block both providers call canonical: the logs and
// the block hashes are separate requests, and a reorg between them must not slip a stale log
// through. hashes caches the agreed hashes already read.
func (d *Detector) checkBlocks(ctx context.Context, f fetched, hashes map[uint64]evm.Word) error {
	for _, group := range [][]evmrpc.Log{f.transfers, f.gateway, f.gwTransfers, f.settled} {
		for _, l := range group {
			if l.Removed {
				return errInconsistent
			}
			h, ok := hashes[l.BlockNumber]
			if !ok {
				var err error
				if h, err = d.q.BlockHash(ctx, l.BlockNumber); err != nil {
					return err
				}
				hashes[l.BlockNumber] = h
			}
			if h != l.BlockHash {
				return errInconsistent
			}
		}
	}
	return nil
}
