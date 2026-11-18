package sender

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/prismaport/prismaport/internal/alert"
	"github.com/prismaport/prismaport/internal/chainwatch"
	"github.com/prismaport/prismaport/internal/evm"
	"github.com/prismaport/prismaport/internal/evmrpc"
	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/store"
)

// follow checks every hash a submitted job was sent under, one receipt request each from one
// provider in turn (no head reads). A mined one moves the job on; a stuck one is replaced with
// bumped fees at the same nonce.
func (s *Sender) follow(ctx context.Context, j store.TxJob) (bool, error) {
	for _, h := range j.TxHashes {
		w, err := evm.ParseWord(h)
		if err != nil {
			return false, err
		}
		r, err := s.rot.Receipt(ctx, w)
		if errors.Is(err, evmrpc.ErrNotFound) {
			continue
		}
		if err != nil {
			return false, err
		}
		return true, s.onReceipt(ctx, j, r)
	}
	if j.SubmittedAt != nil && s.st.Now().Sub(*j.SubmittedAt) >= s.cfg.ReplaceAfter {
		return false, s.replace(ctx, j)
	}
	if accepted, errs := s.q.Broadcast(ctx, j.RawTx); accepted == 0 {
		for _, e := range errs {
			if chainwatch.NonceTooLow(e) && j.SubmittedAt != nil && s.st.Now().Sub(*j.SubmittedAt) >= s.cfg.ReplaceAfter/2 {
				// The nonce was consumed: by one of our hashes a provider has not shown yet, or by
				// something else.
				return true, s.nonceUsed(ctx, j, "nonce consumed by another transaction")
			}
		}
	}
	return false, nil
}

// nonceUsed handles a job whose nonce is used already. Every hash the job was sent under is looked
// up on every provider first: one that mined (a rotated provider had not shown its receipt yet)
// moves the job on with its receipt. Only when every provider says none of them mined is the
// nonce someone else's and the job retried, which forgets those hashes.
func (s *Sender) nonceUsed(ctx context.Context, j store.TxJob, why string) error {
	for _, h := range j.TxHashes {
		w, err := evm.ParseWord(h)
		if err != nil {
			return err
		}
		r, err := s.q.AnyReceipt(ctx, w)
		if errors.Is(err, evmrpc.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		return s.onReceipt(ctx, j, r)
	}
	return s.jobs.Retry(ctx, j, why)
}

func bump(old *money.Units, floor *big.Int) *big.Int {
	b := new(big.Int)
	if old != nil {
		b = old.Big()
	}
	// Nodes require at least +10% on both fields to accept a replacement.
	b.Add(b, new(big.Int).Add(new(big.Int).Div(b, big.NewInt(8)), big.NewInt(1)))
	if floor != nil && floor.Cmp(b) > 0 {
		b.Set(floor)
	}
	return b
}

func (s *Sender) replace(ctx context.Context, j store.TxJob) error {
	tip, feeCap, _, err := s.quote(ctx)
	if err != nil {
		return err
	}
	newTip, newCap := bump(j.Tip, tip), bump(j.MaxFee, feeCap)
	if newCap.Cmp(s.cfg.MaxFeePerGas) > 0 {
		s.jobs.Alert(ctx, alert.Warning, "replacement_above_cap", "stuck transaction needs fees above the cap",
			map[string]string{"job": j.ID, "tx": j.TxHashes[len(j.TxHashes)-1]})
		now := s.st.Now()
		j.SubmittedAt = &now
		return s.st.UpdateTxJob(ctx, &j)
	}
	if newTip.Cmp(newCap) > 0 {
		newTip = new(big.Int).Set(newCap)
	}
	s.log.Warn("replacing stuck transaction", "chain", s.chainID(), "job", j.ID, "nonce", *j.Nonce)
	return s.sign(ctx, j, newTip, newCap)
}

func (s *Sender) onReceipt(ctx context.Context, j store.TxJob, r evmrpc.Receipt) error {
	n := r.BlockNumber
	j.MinedTxHash, j.MinedBlock, j.MinedBlockHash, j.GasUsed = r.TxHash.Hex(), &n, r.BlockHash.Hex(), r.GasUsed
	s.log.Info("tx mined", "chain", s.chainID(), "job", j.ID, "tx", r.TxHash.Hex(), "status", r.Status, "gas_used", r.GasUsed, "block", n)
	if r.Status != 1 {
		if j.Kind == store.JobSettle {
			// Someone may have settled the same authorization first: the detector looks for the
			// session's Gateway event back to when its address was first watched.
			if s.cfg.Recorder != nil {
				found, _, err := s.settledByOther(ctx, j, n)
				if err != nil {
					return err
				}
				if found {
					// The payment is the other transaction (in the session's payments); ours, which
					// reverted, is not shown as it.
					j.MinedTxHash, j.MinedBlock, j.MinedBlockHash = "", nil, ""
					return s.jobs.Done(ctx, j, "settled")
				}
			}
			return s.jobs.Fail(ctx, j, "reverted", "settle transaction reverted")
		}
		return s.jobs.Retry(ctx, j, "flush transaction reverted")
	}
	if j.Kind == store.JobSettle {
		// The detector records the payment from the agreed Gateway logs of the receipt's block.
		if s.cfg.Recorder != nil {
			found, err := s.cfg.Recorder.GatewaySettled(ctx, j, n, false)
			if err != nil {
				return err
			}
			if found == 0 {
				return s.unmatchedSettle(ctx, j, r)
			}
		}
		return s.jobs.Done(ctx, j, "settled")
	}
	// The flush moved the token's whole balance at block n: the detector's stored balance is 0
	// from there on, so a deposit right after it (even one equal to or below the amount just
	// flushed) reads as a rise and is rescanned, instead of waiting for the settlement at
	// finality, by which time the session may have left the watch (D47). A reading from before n
	// (a provider behind) is ignored; if n is reorged out, readings past it show the funds again.
	if err := s.st.FlushMined(ctx, s.chainID(), j.Address, j.Details["token"], n, s.reach()); err != nil {
		return err
	}
	j.Status = store.JobMined
	at := s.st.Now().Add(s.cfg.FinalityDelay)
	j.FinalityCheckAt = &at
	return s.st.UpdateTxJob(ctx, &j)
}

// unmatchedSettle handles a settle whose receipt (from one provider) names a block whose agreed
// Gateway logs hold no payment of the session. Only a receipt both providers agree on ends the
// job (with an alert: the event was rejected); otherwise the job stays submitted and is followed
// again.
func (s *Sender) unmatchedSettle(ctx context.Context, j store.TxJob, r evmrpc.Receipt) error {
	agreed, err := s.q.Receipt(ctx, r.TxHash)
	if errors.Is(err, evmrpc.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if agreed.BlockHash != r.BlockHash || agreed.Status != r.Status {
		return nil
	}
	s.jobs.Alert(ctx, alert.Critical, "settle_unmatched", "settle transaction mined without a Gateway payment of its session",
		map[string]string{"job": j.ID, "session": j.SessionID, "tx": r.TxHash.Hex()})
	return s.jobs.Done(ctx, j, "settled_unmatched")
}

// finality runs the finality checks of mined jobs that are due: one finalized header from one
// provider for all of them; a job whose block it covers is finished, any other is checked again
// FinalityRetry later. Nothing is read while no check is due.
func (s *Sender) finality(ctx context.Context) (int, error) {
	mined, err := s.st.TxJobs(ctx, s.chainID(), store.JobMined)
	if err != nil {
		return 0, err
	}
	now := s.st.Now()
	var due []store.TxJob
	for _, j := range mined {
		if j.FinalityCheckAt == nil || !j.FinalityCheckAt.After(now) {
			due = append(due, j)
		}
	}
	if len(due) == 0 {
		return 0, nil
	}
	fin, ferr := s.rot.Header(ctx, evmrpc.Finalized)
	if ferr != nil && !errors.Is(ferr, evmrpc.ErrNotFound) {
		return 0, ferr
	}
	changed := 0
	for _, j := range due {
		if ferr != nil || j.MinedBlock == nil || fin.Number < *j.MinedBlock {
			at := now.Add(s.cfg.FinalityRetry)
			j.FinalityCheckAt = &at
			if err := s.st.UpdateTxJob(ctx, &j); err != nil {
				return changed, err
			}
			continue
		}
		done, err := s.finish(ctx, j)
		if err != nil {
			return changed, err
		}
		if done {
			changed++
		}
	}
	return changed, nil
}

// finish records a mined flush's Settled event once its block is final (the caller checked) and
// still canonical by the agreed hash. A flush that moved more than was credited when it was
// planned makes the detector rescan the address: something reached it between two balance reads.
// So does one that moved less (or nothing): someone else flushed the address between our
// precheck and our transaction, and that flush may have carried a deposit no balance read saw.
// The zero our receipt stored at its block (FlushMined) starts the detector's later windows
// after it; it also moved checked_block back to the balance it replaced, so this rescan reads
// every block since that balance, a deposit whose log lagged then included (ADR-0008 H-1).
func (s *Sender) finish(ctx context.Context, j store.TxJob) (bool, error) {
	hash, err := s.q.BlockHash(ctx, *j.MinedBlock)
	if err != nil {
		return false, err
	}
	if hash.Hex() != j.MinedBlockHash {
		// Reorged out before finality: the same signed transaction is re-broadcast.
		s.log.Warn("flush reorged out", "chain", s.chainID(), "job", j.ID)
		j.Status, j.MinedBlock, j.MinedBlockHash, j.MinedTxHash, j.FinalityCheckAt = store.JobSubmitted, nil, "", "", nil
		now := s.st.Now()
		j.SubmittedAt = &now
		return false, s.st.UpdateTxJob(ctx, &j)
	}
	w, err := evm.ParseWord(j.MinedTxHash)
	if err != nil {
		return true, s.jobs.BadJob(ctx, j, fmt.Errorf("%w: job %s: mined tx hash %q", errBadJob, j.ID, j.MinedTxHash))
	}
	addr, token, err := flushTarget(j)
	if err != nil {
		return true, s.jobs.BadJob(ctx, j, err)
	}
	r, err := s.q.Receipt(ctx, w)
	if err != nil {
		return false, err
	}
	sw, err := s.recordSettledLogs(ctx, j, r.Logs, nil)
	if err != nil {
		return false, err
	}
	if s.cfg.Recorder != nil && j.PlannedTotal != nil {
		if gross := settledGross(addr, token, r.Logs); !gross.Equal(*j.PlannedTotal) {
			// On a customer address the rescan reads the address for its current session (the
			// forwarder is shared; another session's funds moving with this flush are expected).
			s.log.Warn("flush moved other than was credited; rescanning the address", "chain", s.chainID(), "job", j.ID,
				"session", j.SessionID, "planned", j.PlannedTotal.String(), "settled", gross.String())
			if _, err := s.cfg.Recorder.RescanSession(ctx, j.SessionID, *j.MinedBlock); err != nil {
				return false, err
			}
		}
	}
	return true, s.jobs.Done(ctx, j, map[bool]string{true: "settled", false: "no_settlement"}[sw.recorded+sw.own > 0])
}
