package sender

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/prismaport/prismaport/internal/alert"
	"github.com/prismaport/prismaport/internal/chainsettle"
	"github.com/prismaport/prismaport/internal/chainwatch"
	"github.com/prismaport/prismaport/internal/contracts"
	"github.com/prismaport/prismaport/internal/evm"
	"github.com/prismaport/prismaport/internal/evmrpc"
	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/store"
)

// recordForeignSettlements records the Settled events of the job's forwarder up to the finalized
// head (one header from one provider; the logs themselves through both).
func (s *Sender) recordForeignSettlements(ctx context.Context, j store.TxJob) (sweep, error) {
	var sw sweep
	addr, token, err := flushTarget(j)
	if err != nil {
		return sw, err
	}
	from, err := strconv.ParseUint(j.Details["from_block"], 10, 64)
	if err != nil {
		// Scanning from block 0 instead would read the whole chain in MaxLogRange-block steps.
		return sw, fmt.Errorf("%w: flush job %s: from_block %q", errBadJob, j.ID, j.Details["from_block"])
	}
	others, err := s.st.OtherSessionFlushes(ctx, j)
	if err != nil {
		return sw, err
	}
	fin, err := s.rot.Header(ctx, evmrpc.Finalized)
	if errors.Is(err, evmrpc.ErrNotFound) {
		return sw, nil // a young chain has nothing final yet
	}
	if err != nil {
		return sw, err
	}
	for start, end := range chainwatch.LogRanges(from, fin.Number, s.cfg.MaxLogRange) {
		logs, err := s.q.Logs(ctx, evmrpc.Filter{FromBlock: start, ToBlock: end, Addresses: []evm.Address{addr},
			Topics: [][]evm.Word{{contracts.SettledTopic, contracts.PayoutTopic}, {evm.AddressWord(token)}}})
		if err != nil {
			return sw, err
		}
		got, err := s.recordSettledLogs(ctx, j, logs, others)
		sw.recorded += got.recorded
		sw.own += got.own
		if g, err := sw.gross.Add(got.gross); err == nil {
			sw.gross = g
		}
		sw.block = max(sw.block, got.block)
		if got.coveredTx != "" {
			sw.coveredTx, sw.coveredBy = got.coveredTx, got.coveredBy
		}
		if err != nil {
			return sw, err
		}
	}
	return sw, nil
}

// recordSettledLogs stores every Settled event of the job's forwarder among logs as a
// settlement, with the Payout events the forwarder logged for it just before. A Settled event of a
// transaction in others (another session's flush job) is left to that job and reported as covering.
func (s *Sender) recordSettledLogs(ctx context.Context, j store.TxJob, logs []evmrpc.Log, others map[string]string) (sweep, error) {
	logs = uniqueLogs(logs) // the Settled events, their bounds and the Payout events, each once
	var sw sweep
	addr, token, err := flushTarget(j)
	if err != nil {
		return sw, err
	}
	settledAt := map[evm.Word][]uint32{}
	for _, l := range logs {
		if ev, ok := contracts.ParseSettled(l); ok && ev.Forwarder == addr && ev.Token == token {
			settledAt[l.TxHash] = append(settledAt[l.TxHash], l.LogIndex)
		}
	}
	for _, l := range logs {
		if sid, ok := others[l.TxHash.Hex()]; ok {
			if ev, ok := contracts.ParseSettled(l); ok && ev.Forwarder == addr && ev.Token == token {
				sw.coveredTx, sw.coveredBy = l.TxHash.Hex(), sid
			}
			continue
		}
		fresh, ours, by, err := s.recordSettled(ctx, j, l, logs, settledAt[l.TxHash])
		if err != nil {
			return sw, err
		}
		if ours {
			if fresh {
				sw.recorded++
			} else {
				sw.own++
			}
			ev, _ := contracts.ParseSettled(l)
			if g, err := sw.gross.Add(ev.Gross); err == nil {
				sw.gross = g
			}
			sw.block = max(sw.block, l.BlockNumber)
		}
		if by != "" {
			sw.coveredTx, sw.coveredBy = l.TxHash.Hex(), by
		}
	}
	return sw, nil
}

// recordSettled stores one Settled log of the job's forwarder as a settlement, with the payout
// of each recipient read from the forwarder's own Payout events. The store checks the fee and
// every payout (chainsettle.Verify) against the rules the address was created with. A
// settlement already recorded on another session (sessions on one customer address share the forwarder) is reported in by.
// ours is set for a settlement of the job's session, fresh when this call stored it (not when an
// earlier flush of the session, or an earlier pass of this job, did).
func (s *Sender) recordSettled(ctx context.Context, j store.TxJob, l evmrpc.Log, logs []evmrpc.Log, bounds []uint32) (fresh, ours bool, by string, err error) {
	ev, ok := contracts.ParseSettled(l)
	addr, token, err := flushTarget(j)
	if err != nil {
		return false, false, "", err
	}
	if !ok || ev.Forwarder != addr || ev.Token != token {
		return false, false, "", nil
	}
	rcpts, _, err := jobRecipients(j)
	if err != nil {
		return false, false, "", err
	}
	shares, ok := contracts.MatchPayouts(rcpts, contracts.PayoutsBefore(logs, addr, token, l.TxHash, l.LogIndex, bounds))
	if !ok {
		s.jobs.Alert(ctx, alert.Critical, "settlement_mismatch", "Payout events name a recipient outside the forwarder's split",
			map[string]string{"session": j.SessionID, "tx": l.TxHash.Hex()})
		return false, false, "", nil
	}
	ses, err := s.st.RecordSettlement(ctx, j.SessionID, store.Settlement{Chain: s.chainID(), Asset: j.Asset, Gross: ev.Gross,
		Fee: ev.Fee, Net: ev.Net, Shares: shares, TxHash: l.TxHash.Hex(), LogIndex: l.LogIndex, SettledAt: s.st.Now(), Block: l.BlockNumber,
		Reach: s.reach()}, chainsettle.Verify)
	switch {
	case errors.Is(err, store.ErrConflict):
		owner, err := s.st.SettlementSession(ctx, s.chainID(), l.TxHash.Hex(), l.LogIndex)
		switch {
		case err != nil:
			return false, false, "", err
		case owner == j.SessionID:
			return false, true, "", nil
		}
		return false, false, owner, nil
	case errors.Is(err, store.ErrSettlementMismatch):
		s.jobs.Alert(ctx, alert.Critical, "settlement_mismatch", err.Error(), map[string]string{"session": j.SessionID, "tx": l.TxHash.Hex()})
		return false, false, "", nil
	case err != nil:
		return false, false, "", err
	}
	s.jobs.ReconcileFlushFees(ctx, j, ses)
	return true, true, "", nil
}

// uniqueLogs drops repeats of a log (same transaction and index), keeping the first: a provider
// repeating one must not add its gross twice.
func uniqueLogs(logs []evmrpc.Log) []evmrpc.Log {
	type logID struct {
		tx    evm.Word
		index uint32
	}
	seen := make(map[logID]bool, len(logs))
	out := make([]evmrpc.Log, 0, len(logs))
	for _, l := range logs {
		id := logID{l.TxHash, l.LogIndex}
		if !seen[id] {
			seen[id] = true
			out = append(out, l)
		}
	}
	return out
}

// settledGross sums the gross of the Settled events of forwarder addr for token among logs.
func settledGross(addr, token evm.Address, logs []evmrpc.Log) money.Units {
	sum := money.Zero
	for _, l := range uniqueLogs(logs) {
		if ev, ok := contracts.ParseSettled(l); ok && ev.Forwarder == addr && ev.Token == token {
			if v, err := sum.Add(ev.Gross); err == nil {
				sum = v
			}
		}
	}
	return sum
}
