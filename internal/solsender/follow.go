package solsender

import (
	"context"
	"errors"
	"fmt"

	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/solattr"
	"github.com/prismaport/prismaport/internal/solrpc"
	"github.com/prismaport/prismaport/internal/solwatch"
	"github.com/prismaport/prismaport/internal/store"
)

// follow checks every signature a submitted job was sent under (one provider in turn, D49). A
// transaction that has not landed is broadcast again; once it may have expired, the agreed block
// height (both providers past its last valid height) decides that it never can.
func (s *Sender) follow(ctx context.Context, j store.TxJob) (bool, error) {
	sts, err := s.rot.SignatureStatuses(ctx, j.TxHashes)
	if err != nil {
		return false, err
	}
	for i, st := range sts {
		if st == nil || (st.ConfirmationStatus != "confirmed" && st.ConfirmationStatus != "finalized") {
			continue
		}
		if st.Failed() {
			why := fmt.Sprintf("transaction %s failed on chain: %s", j.TxHashes[i], st.Err)
			if j.Kind == store.JobSettle {
				return true, s.jobs.Fail(ctx, j, "reverted", why)
			}
			if blocked, err := s.blockIfRecipientGone(ctx, j); blocked || err != nil {
				return blocked, err
			}
			return true, s.jobs.Retry(ctx, j, why)
		}
		slot := st.Slot
		if j.Kind == store.JobFlush {
			// The flush emptied the deposit account at slot: the detector's stored balance is 0 from
			// there on, so a deposit right after it (even one equal to or below the amount flushed)
			// reads as a rise and is scanned. A reading from before slot is ignored; if the slot
			// does not survive, readings past it show the funds again.
			if err := s.st.SolanaFlushLanded(ctx, s.chainID(), j.Address, slot); err != nil {
				return false, err
			}
		}
		fin := s.st.Now().Add(s.cfg.FinalityDelay)
		j.Status, j.MinedTxHash, j.MinedBlock, j.FinalityCheckAt = store.JobMined, j.TxHashes[i], &slot, &fin
		s.log.Info("tx landed", "chain", s.chainID(), "job", j.ID, "tx", j.TxHashes[i], "slot", slot)
		return true, s.st.UpdateTxJob(ctx, &j)
	}
	expired := false
	if j.LastValidHeight != nil && j.SubmittedAt != nil && s.st.Now().Sub(*j.SubmittedAt) >= expiryCheckAfter {
		height, err := s.q.BlockHeight(ctx)
		if err != nil {
			return false, err
		}
		expired = height > *j.LastValidHeight
	}
	if expired {
		if j.Kind == store.JobSettle {
			return true, s.jobs.Fail(ctx, j, "expired", "the transaction's blockhash expired before it landed")
		}
		// The blockhash expired: the transaction can never land. Flushing is idempotent, so a
		// new one with a fresh blockhash is safe.
		j.Status, j.RawTx, j.SubmittedAt, j.LastValidHeight = store.JobPending, nil, nil, nil
		j.NextAttemptAt = s.st.Now()
		s.log.Warn("flush expired before landing; rebuilding", "chain", s.chainID(), "job", j.ID)
		return true, s.st.UpdateTxJob(ctx, &j)
	}
	s.broadcast(ctx, j, j.RawTx)
	return false, nil
}

// finality runs the due finality checks of landed jobs (FinalityDelay after landing, then one
// FinalityRetry per check): one finalized slot from one provider; a job whose slot it covers is
// finished through agreed reads, any other is checked again later.
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
	fin, err := s.rot.Slot(ctx, solrpc.Finalized)
	if err != nil {
		return 0, err
	}
	changed := 0
	for _, j := range due {
		done := false
		if j.MinedBlock != nil && fin >= *j.MinedBlock {
			if done, err = s.finish(ctx, j); err != nil {
				return changed, err
			}
		}
		if done {
			changed++
			continue
		}
		at := now.Add(s.cfg.FinalityRetry)
		j.FinalityCheckAt = &at
		if err := s.st.UpdateTxJob(ctx, &j); err != nil {
			return changed, err
		}
	}
	return changed, nil
}

// finish records a landed flush's settlement once its transaction is finalized (the caller saw
// a finalized slot covering it). false leaves the job for the next check.
func (s *Sender) finish(ctx context.Context, j store.TxJob) (bool, error) {
	tx, err := s.q.Transaction(ctx, j.MinedTxHash, solrpc.Finalized)
	if errors.Is(err, solwatch.ErrLag) {
		return false, nil
	}
	if errors.Is(err, solrpc.ErrNotFound) {
		// Landed on a fork that did not survive.
		s.log.Warn("transaction not on the finalized fork", "chain", s.chainID(), "job", j.ID, "tx", j.MinedTxHash)
		if j.Kind == store.JobSettle {
			return true, s.jobs.Fail(ctx, j, "expired", "the transaction did not reach finality")
		}
		j.Status, j.RawTx, j.SubmittedAt, j.LastValidHeight, j.MinedTxHash, j.MinedBlock = store.JobPending, nil, nil, nil, "", nil
		j.FinalityCheckAt = nil
		j.NextAttemptAt = s.st.Now()
		return true, s.st.UpdateTxJob(ctx, &j)
	}
	if err != nil {
		return false, err
	}
	feePaid := money.FromUint64(tx.Fee)
	j.GasUsed, j.MaxFee = tx.ComputeUnits, &feePaid
	j.MinedBlockHash, err = s.q.BlockHash(ctx, tx.Slot, solrpc.Finalized)
	if errors.Is(err, solwatch.ErrLag) || errors.Is(err, solrpc.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if j.Kind == store.JobSettle {
		// The detector credits the payment through the session reference key.
		return true, s.jobs.Done(ctx, j, "settled")
	}
	f, mint, ata, err := jobForwarder(j)
	if err != nil {
		return true, s.jobs.BadJob(ctx, j, err)
	}
	ok, _, err := s.recordFlush(ctx, j, f, mint, ata, tx)
	if err != nil {
		return false, err
	}
	if s.cfg.Recorder != nil && j.PlannedTotal != nil {
		// The flush moved the account's whole balance: more than was credited when it was planned
		// means a deposit arrived between two balance reads and left with it. The agreed finalized
		// transaction's movement out of the account says how much moved; the account's history is
		// read again so the deposit is recorded (for the current session on a customer address).
		if sp, moved, err := solattr.FlushSettlement(tx, f, ata, mint, s.assets[j.Asset].Decimals); err == nil && moved &&
			sp.Gross.Cmp(*j.PlannedTotal) > 0 {
			s.log.Warn("flush moved more than was credited; rescanning the deposit account", "chain", s.chainID(), "job", j.ID,
				"session", j.SessionID, "planned", j.PlannedTotal.String(), "settled", sp.Gross.String())
			if _, err := s.cfg.Recorder.RescanAccount(ctx, ata.String()); err != nil {
				return false, err
			}
		}
	}
	s.log.Info("flush final", "chain", s.chainID(), "job", j.ID, "tx", j.MinedTxHash, "compute_units", tx.ComputeUnits, "fee_lamports", tx.Fee)
	return true, s.jobs.Done(ctx, j, map[bool]string{true: "settled", false: "no_settlement"}[ok])
}
