package solsender

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/prismaport/prismaport/internal/alert"
	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/payout"
	"github.com/prismaport/prismaport/internal/solana"
	"github.com/prismaport/prismaport/internal/solrpc"
	"github.com/prismaport/prismaport/internal/store"
)

func (s *Sender) send(ctx context.Context, j store.TxJob) error {
	switch j.Kind {
	case store.JobFlush:
		return s.sendFlush(ctx, j)
	case store.JobSettle:
		return s.sendWallet(ctx, j)
	}
	return s.jobs.Fail(ctx, j, "unknown_kind", j.Kind)
}

func (s *Sender) sendFlush(ctx context.Context, j store.TxJob) error {
	f, mint, ata, err := jobForwarder(j)
	if err != nil {
		return s.jobs.BadJob(ctx, j, err)
	}
	if wait, err := s.jobs.WaitForSibling(ctx, j); err != nil || wait {
		return err
	}
	bal, err := s.tokenBalance(ctx, ata)
	if err != nil {
		return err
	}
	if bal.IsZero() {
		// Flush is permissionless: record what a flush by anyone (or an earlier attempt of ours)
		// moved out of this account.
		sw, err := s.recordFlushes(ctx, j, f, mint, ata)
		if err != nil {
			return err
		}
		return s.sweptBefore(ctx, j, sw)
	}
	asset := s.assets[j.Asset]
	split, err := payout.Compute(bal, asset.Decimals, f.Params.Fee, forwarderBps(f))
	if err != nil {
		return s.jobs.Fail(ctx, j, "fee_rule", err.Error())
	}
	if split.Dust {
		s.jobs.Alert(ctx, alert.Info, "flush_dust", "balance too small to settle after the fee stays in the forwarder",
			map[string]string{"session": j.SessionID, "address": j.Address, "balance": bal.String()})
		return s.jobs.Done(ctx, j, "dust")
	}
	missing, err := s.missingRecipients(ctx, f, mint, split.Shares)
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		return s.block(ctx, j, missing)
	}
	var ixs []solana.Instruction
	if !split.Fee.IsZero() {
		// The treasury's token account is ours: create it once, paid by the gas wallet, so the
		// program never needs to (it would charge the flush's caller, which is us too).
		treATA, err := solana.AssociatedTokenAddress(f.Params.Treasury, mint)
		if err != nil {
			return s.jobs.BadJob(ctx, j, err)
		}
		have, err := s.tokenAccountReady(ctx, treATA, mint)
		if err != nil {
			return err
		}
		if !have {
			ixs = append(ixs, solana.CreateATAIdempotent(s.Wallet(), treATA, f.Params.Treasury, mint, solana.TokenProgramID))
		}
	}
	// The program never creates a recipient's token account, so the gas wallet can be the caller:
	// the only account flush could fund is the treasury's, ours, created just above.
	// The program is stateless (D41): no initialize, and close_source closes the deposit token
	// account once it is settled, rent to the rent receiver.
	flush, err := f.Flush(s.Wallet(), mint, ata, true)
	if err != nil {
		return s.jobs.BadJob(ctx, j, err)
	}
	ixs = append(ixs, flush)
	return s.submit(ctx, j, ixs)
}

// sweep is what reading a deposit account's flushes did for a flush job.
type sweep struct {
	// recorded counts the flushes recorded on (or already recorded for) the job's session.
	recorded int
	// coveredTx and coveredBy name a flush that belongs to another session on the same customer
	// address (its own job records it, or did): it moved this session's funds with that session's.
	coveredTx, coveredBy string
}

// sweptBefore ends a flush job that found the account empty. A flush recorded on the job's session
// ends it settled_elsewhere; a flush that another session's job sent (or one already recorded on
// another session) ends it covered, naming that flush and session in the job's details: one
// on-chain settlement is recorded once, on the session whose flush moved the funds
// (static-addresses.md, "Flush"). Otherwise it is empty.
func (s *Sender) sweptBefore(ctx context.Context, j store.TxJob, sw sweep) error {
	switch {
	case sw.recorded > 0:
		return s.jobs.Done(ctx, j, "settled_elsewhere")
	case sw.coveredTx != "":
		if err := s.st.AddTxJobDetails(ctx, j.ID, map[string]string{"covered_by_tx": sw.coveredTx, "covered_by_session": sw.coveredBy}); err != nil {
			return err
		}
		return s.jobs.Done(ctx, j, "covered")
	}
	return s.jobs.Done(ctx, j, "empty")
}

// build signs the instructions behind a compute budget, sized from a simulation, with the gas
// wallet as fee payer plus any extra signers the instructions need.
func (s *Sender) build(ctx context.Context, ixs []solana.Instruction, limit uint32, extra ...*solana.Keypair) (*solana.Transaction, solrpc.Blockhash, error) {
	bh, err := s.q.LatestBlockhash(ctx)
	if err != nil {
		return nil, bh, err
	}
	hash, err := solana.ParseBlockhash(bh.Hash)
	if err != nil {
		return nil, bh, err
	}
	all := []solana.Instruction{solana.SetComputeUnitLimit(limit)}
	if s.cfg.PriorityFee > 0 {
		all = append(all, solana.SetComputeUnitPrice(s.cfg.PriorityFee))
	}
	msg, err := solana.Compile(s.Wallet(), hash, append(all, ixs...))
	if err != nil {
		return nil, bh, err
	}
	tx := solana.NewTransaction(msg)
	return tx, bh, tx.Sign(append([]*solana.Keypair{s.key}, extra...)...)
}

// errUnserializable marks instructions that do not fit in a transaction: the job can never be sent.
var errUnserializable = errors.New("transaction cannot be serialized")

// simulate signs the instructions under the maximum compute-unit limit and simulates them.
func (s *Sender) simulate(ctx context.Context, ixs []solana.Instruction, extra ...*solana.Keypair) (solrpc.Simulation, error) {
	tx, _, err := s.build(ctx, ixs, maxComputeUnits, extra...)
	if err != nil {
		return solrpc.Simulation{}, err
	}
	raw, err := tx.Serialize()
	if err != nil {
		return solrpc.Simulation{}, fmt.Errorf("%w: %v", errUnserializable, err)
	}
	return s.q.Simulate(ctx, raw, true)
}

// submit simulates, re-signs with a tight compute-unit limit, persists the signed transaction
// before broadcasting it (a crash can never lose track of a sent transaction), then broadcasts.
func (s *Sender) submit(ctx context.Context, j store.TxJob, ixs []solana.Instruction, extra ...*solana.Keypair) error {
	sim, err := s.simulate(ctx, ixs, extra...)
	if errors.Is(err, errUnserializable) {
		return s.jobs.BadJob(ctx, j, err)
	}
	if err != nil {
		return err
	}
	if sim.Failed() {
		if blocked, err := s.blockIfRecipientGone(ctx, j); blocked || err != nil {
			return err
		}
		// A frozen account or a paused mint fails the flush; the funds stay and we retry later.
		return s.jobs.Retry(ctx, j, fmt.Sprintf("simulation failed: %s %v", sim.Err, lastLogs(sim.Logs)))
	}
	limit := uint32(min(uint64(maxComputeUnits), sim.UnitsConsumed+sim.UnitsConsumed/5+2_000))
	tx, bh, err := s.build(ctx, ixs, limit, extra...)
	if err != nil {
		return err
	}
	raw, err := tx.Serialize()
	if err != nil {
		return s.jobs.BadJob(ctx, j, err)
	}
	if err := s.checkGas(ctx, uint64(limit)); err != nil {
		return err
	}
	now := s.st.Now()
	lv := bh.LastValidBlockHeight
	price := money.FromUint64(s.cfg.PriorityFee)
	j.Status, j.RawTx, j.SubmittedAt, j.LastValidHeight = store.JobSubmitted, raw, &now, &lv
	j.TxHashes = append(j.TxHashes, tx.ID())
	j.Sender, j.GasLimit, j.Tip = s.Wallet().String(), uint64(limit), &price
	if err := s.st.UpdateTxJob(ctx, &j); err != nil {
		return err
	}
	s.broadcast(ctx, j, raw)
	s.log.Info("tx sent", "chain", s.chainID(), "job", j.ID, "kind", j.Kind, "tx", tx.ID(), "cu_limit", limit,
		"cu_simulated", sim.UnitsConsumed, "priority_micro_lamports", s.cfg.PriorityFee)
	return nil
}

func lastLogs(l []string) []string {
	if len(l) > 6 {
		return l[len(l)-6:]
	}
	return l
}

func (s *Sender) broadcast(ctx context.Context, j store.TxJob, raw []byte) {
	if accepted, errs := s.q.Broadcast(ctx, raw); accepted == 0 {
		s.log.Warn("broadcast not accepted yet", "chain", s.chainID(), "job", j.ID, "errs", fmt.Sprint(errs))
	}
}

// checkGas warns when the fee payer runs low. The simulation already proved the transaction
// is affordable right now.
func (s *Sender) checkGas(ctx context.Context, limit uint64) error {
	bal, err := s.q.Balance(ctx, s.Wallet().String())
	if err != nil {
		return err
	}
	if bal < s.cfg.MinGasBalance && s.st.Now().Sub(s.lowSince) > time.Hour {
		s.lowSince = s.st.Now()
		s.jobs.Alert(ctx, alert.Warning, "gas_wallet_low", "fee payer below the configured minimum",
			map[string]string{"wallet": s.Wallet().String(), "balance_lamports": fmt.Sprint(bal), "cu_limit": fmt.Sprint(limit)})
	}
	return nil
}

// sendWallet relays a connected-wallet payment the checkout verified and queued. The
// transaction is the payer's and is sent unchanged: if its blockhash expired it cannot be
// rebuilt here, so the job fails and the payer signs a new one.
func (s *Sender) sendWallet(ctx context.Context, j store.TxJob) error {
	tx, err := solana.ParseTransaction(j.Calldata)
	if err != nil {
		return s.jobs.Fail(ctx, j, "rejected", err.Error())
	}
	lv, err := strconv.ParseUint(j.Details["last_valid_height"], 10, 64)
	if err != nil {
		return s.jobs.Fail(ctx, j, "rejected", "missing last valid block height")
	}
	// The checkout queued it with a valid blockhash; only a job that waited long enough to expire
	// reads the agreed block height (both providers past the last valid height).
	if s.st.Now().Sub(j.CreatedAt) >= expiryCheckAfter {
		height, err := s.q.BlockHeight(ctx)
		if err != nil {
			return err
		}
		if height > lv {
			return s.jobs.Fail(ctx, j, "expired", "the transaction's blockhash expired before it could be sent")
		}
	}
	sim, err := s.q.Simulate(ctx, j.Calldata, true)
	if err != nil {
		return err
	}
	if sim.Failed() {
		if alreadyLanded(sim) {
			// Submitted twice or sent by the wallet itself: follow the original.
			sim.Err = nil
		} else {
			return s.jobs.Fail(ctx, j, "rejected", fmt.Sprintf("simulation failed: %s %v", sim.Err, lastLogs(sim.Logs)))
		}
	}
	now := s.st.Now()
	j.Status, j.RawTx, j.SubmittedAt, j.LastValidHeight = store.JobSubmitted, j.Calldata, &now, &lv
	j.TxHashes, j.Sender = []string{tx.ID()}, s.Wallet().String()
	if err := s.st.UpdateTxJob(ctx, &j); err != nil {
		return err
	}
	s.broadcast(ctx, j, j.Calldata)
	return nil
}

func alreadyLanded(sim solrpc.Simulation) bool {
	return string(sim.Err) == `"AlreadyProcessed"`
}
