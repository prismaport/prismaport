package sender

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"github.com/prismaport/prismaport/internal/alert"
	"github.com/prismaport/prismaport/internal/contracts"
	"github.com/prismaport/prismaport/internal/derive"
	"github.com/prismaport/prismaport/internal/evm"
	"github.com/prismaport/prismaport/internal/evmrpc"
	"github.com/prismaport/prismaport/internal/fee"
	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/payout"
	"github.com/prismaport/prismaport/internal/store"
	"github.com/prismaport/prismaport/internal/txjob"
)

// jobAddr parses an address a job stores. A malformed one is errBadJob: it never becomes the zero
// address a transaction could be simulated, estimated or broadcast to.
func jobAddr(j store.TxJob, field, v string) (evm.Address, error) {
	a, err := evm.ParseAddressLoose(v)
	if err != nil {
		return evm.Address{}, fmt.Errorf("%w: job %s: %s %q: %v", errBadJob, j.ID, field, v, err)
	}
	return a, nil
}

// flushTarget is the forwarder and the token a flush job moves.
func flushTarget(j store.TxJob) (addr, token evm.Address, err error) {
	if addr, err = jobAddr(j, "address", j.Address); err != nil {
		return addr, token, err
	}
	token, err = jobAddr(j, "token", j.Details["token"])
	return addr, token, err
}

// precheck decides whether a pending job should be sent now. It returns send=false when the
// job was completed or failed without a transaction.
func (s *Sender) precheck(ctx context.Context, j store.TxJob) (send bool, err error) {
	switch j.Kind {
	case store.JobFlush:
		return s.precheckFlush(ctx, j)
	case store.JobSettle:
		if vb, ok := new(big.Int).SetString(j.Details["valid_before"], 10); ok {
			if big.NewInt(s.st.Now().Add(txjob.SettleMinedMargin).Unix()).Cmp(vb) >= 0 {
				return false, s.expiredSettle(ctx, j)
			}
		}
		to, err := jobAddr(j, "to", j.To)
		if err != nil {
			return false, s.jobs.BadJob(ctx, j, err)
		}
		if _, err := s.q.FirstCall(ctx, evmrpc.CallMsg{From: &s.wallet, To: to, Data: j.Calldata}, evmrpc.Latest); err != nil {
			if evmrpc.IsRevert(err) {
				return false, s.rejectSettle(ctx, j, revertText(err))
			}
			return false, err
		}
		return true, nil
	}
	return false, s.jobs.Fail(ctx, j, "unknown_kind", j.Kind)
}

// expiredSettle ends a settle job whose authorization expires before a transaction sent now
// could be mined. A retried job may have been settled already (our earlier transaction mined
// unseen, or someone submitted the authorization first): a settlement found up to the agreed
// head ends it settled, as rejectSettle does; otherwise it fails expired.
func (s *Sender) expiredSettle(ctx context.Context, j store.TxJob) error {
	if s.cfg.Recorder != nil {
		latest, err := s.q.AgreedLatest(ctx)
		if err != nil {
			return err
		}
		found, _, err := s.settledByOther(ctx, j, latest)
		if err != nil {
			return err
		}
		if found {
			s.log.Warn("expired settle authorization already settled", "chain", s.chainID(), "job", j.ID, "session", j.SessionID)
			return s.jobs.Done(ctx, j, "settled")
		}
	}
	return s.jobs.Fail(ctx, j, "expired", "authorization expires before it could be mined")
}

// rejectSettle ends a settle job whose simulation reverted. The authorization may be used
// already: our own earlier transaction mined without a receipt we saw, or someone submitted the
// signed authorization first (Gateway.settle is permissionless). A settlement found up to the
// agreed head ends the job settled, as the payer's payment went through. None found fails it
// rejected, after one more attempt on a job's first try when the authorization is used: the
// provider that ran the simulation may be ahead of the agreed head.
func (s *Sender) rejectSettle(ctx context.Context, j store.TxJob, why string) error {
	if s.cfg.Recorder == nil {
		return s.jobs.Fail(ctx, j, "rejected", why)
	}
	latest, err := s.q.AgreedLatest(ctx)
	if err != nil {
		return err
	}
	found, used, err := s.settledByOther(ctx, j, latest)
	if err != nil {
		return err
	}
	if found {
		s.log.Warn("settle authorization already settled by another transaction", "chain", s.chainID(), "job", j.ID,
			"session", j.SessionID, "simulation", why)
		return s.jobs.Done(ctx, j, "settled")
	}
	if used && j.Attempts == 0 {
		return s.jobs.Retry(ctx, j, "settle simulation reverted: "+why)
	}
	return s.jobs.Fail(ctx, j, "rejected", why)
}

// settledByOther asks the detector to record a settlement of the job's authorization by another
// transaction up to block, looking back to the first block the session's address was watched
// from. A token that reports the authorization unused (a plain rejection: balance, window) needs
// no look back; used is false then. found is whether the detector holds a Gateway payment of the
// session.
func (s *Sender) settledByOther(ctx context.Context, j store.TxJob, block uint64) (found, used bool, err error) {
	used, known, err := s.authorizationUsed(ctx, j)
	if err != nil {
		return false, false, err
	}
	if known && !used {
		return false, false, nil
	}
	n, err := s.cfg.Recorder.GatewaySettled(ctx, j, block, true)
	return n > 0, true, err
}

// authorizationUsed reads the token's EIP-3009 authorizationState for the job's payer and
// session nonce (the job ref is "payer|nonce"). known is false when the token does not answer it.
func (s *Sender) authorizationUsed(ctx context.Context, j store.TxJob) (used, known bool, err error) {
	token, from, ok := contracts.SettleAuthorizer(j.Calldata)
	_, ref, cut := strings.Cut(j.Ref, "|")
	nonce, perr := evm.ParseWord(ref)
	if !ok || !cut || perr != nil {
		return false, false, nil
	}
	out, err := s.q.FirstCall(ctx, evmrpc.CallMsg{To: token, Data: contracts.AuthorizationState(from, nonce)}, evmrpc.Latest)
	if evmrpc.IsRevert(err) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	used, err = contracts.Bool(out)
	return used, err == nil, nil
}

func revertText(err error) string {
	var re *evmrpc.RPCError
	if errors.As(err, &re) {
		if data, ok := re.RevertData(); ok && len(data) > 0 {
			return contracts.RevertReason(data)
		}
		return re.Message
	}
	return err.Error()
}

func (s *Sender) precheckFlush(ctx context.Context, j store.TxJob) (bool, error) {
	addr, token, err := flushTarget(j)
	if err != nil {
		return false, s.jobs.BadJob(ctx, j, err)
	}
	if wait, err := s.jobs.WaitForSibling(ctx, j); err != nil || wait {
		return false, err
	}
	// latest only (D49): the balance is read through both providers at the lower of their heads.
	latest, err := s.q.AgreedLatest(ctx)
	if err != nil {
		return false, err
	}
	out, err := s.q.Call(ctx, evmrpc.CallMsg{To: token, Data: contracts.BalanceOf(addr)}, latest)
	if err != nil {
		return false, err
	}
	bal, err := contracts.Uint(out)
	if err != nil {
		return false, err
	}
	asset := s.tokens[j.Asset]
	if bal.IsZero() {
		// Someone else may have flushed (flush is permissionless): record what reached the
		// merchant from the forwarder's own finalized Settled events.
		sw, err := s.recordForeignSettlements(ctx, j)
		if errors.Is(err, errBadJob) {
			s.jobs.Alert(ctx, alert.Critical, j.Kind+"_bad_job", "flush job cannot be processed; settlements elsewhere are not recorded",
				map[string]string{"job": j.ID, "session": j.SessionID, "address": j.Address, "error": err.Error()})
			return false, s.jobs.Fail(ctx, j, "bad_job", err.Error())
		}
		if err != nil {
			return false, err
		}
		if sw.short(j) && sw.coveredTx == "" && s.st.Now().Before(j.CreatedAt.Add(2*s.cfg.FinalityDelay+s.cfg.FinalityRetry)) {
			// The credited deposits left the address, through a flush that is not final yet
			// (someone else's deployAndFlush right after the payment, D49): look again once it
			// can be, instead of ending without its settlement. The settlements found so far
			// (an earlier flush of the session's among them) do not reach the planned total.
			j.NextAttemptAt = s.st.Now().Add(s.cfg.FinalityRetry)
			s.log.Info("flush found an empty address; waiting for a settlement to become final", "chain", s.chainID(), "job", j.ID)
			return false, s.st.UpdateTxJob(ctx, &j)
		}
		return false, s.sweptBefore(ctx, j, sw)
	}
	split, err := fee.Compute(bal, asset.Decimals, s.feeOf(j))
	if err != nil {
		return false, s.jobs.Fail(ctx, j, "fee_rule", err.Error())
	}
	if split.Dust {
		s.jobs.Alert(ctx, alert.Info, "flush_dust", "balance too small to settle after the fee stays in the forwarder",
			map[string]string{"session": j.SessionID, "address": j.Address, "balance": bal.String()})
		return false, s.jobs.Done(ctx, j, "dust")
	}
	return true, nil
}

// sweep is what reading a forwarder's Settled events did for a flush job.
type sweep struct {
	// recorded counts the settlements this pass stored on the job's session; own those found
	// already stored on it (an earlier flush of the session, or an earlier pass of this job that
	// failed after recording). gross is the total of both and block the latest block they were
	// mined in.
	recorded, own int
	gross         money.Units
	block         uint64
	// coveredTx and coveredBy name a flush that belongs to another session on the same customer
	// address (its own job records it, or did): it moved this session's funds with that session's.
	coveredTx, coveredBy string
}

// sweptBefore ends a flush job that found the forwarder empty. A settlement found and recorded on
// the job's session ends it settled_elsewhere; a flush that another session's job sent (or a
// settlement already recorded on another session) ends it covered, naming that flush and session
// in the job's details: one on-chain settlement is recorded once, on the session whose flush moved
// the funds (static-addresses.md, "Flush"). Otherwise it is empty. A settlement found that moved
// more than was credited when the job was planned (a deposit arrived between two balance reads and
// left with that flush) makes the detector rescan the address up to its block, as finish does for
// our own flush, so the deposit is still recorded.
//
// Settlements already stored on the session count toward the gross, so a pass that failed after
// recording (in the rescan, a later log range, or a restart) compares and rescans again on the
// next one, whatever the job then ends as. The label is decided after the rescan: settled_elsewhere
// on a new settlement; covered when nothing new was recorded and another session's flush covered
// the funds; settled_elsewhere when stored settlements alone reach the planned total; else empty.
func (s *Sender) sweptBefore(ctx context.Context, j store.TxJob, sw sweep) error {
	// First the rescan, whatever the job ends as (a covering flush of another session found in the
	// same range must not skip it): it fails the pass, so the job retries until it succeeds.
	if s.cfg.Recorder != nil && j.PlannedTotal != nil && sw.recorded+sw.own > 0 && sw.gross.Cmp(*j.PlannedTotal) > 0 {
		s.log.Warn("a flush found moved more than was credited; rescanning the address", "chain", s.chainID(), "job", j.ID,
			"session", j.SessionID, "planned", j.PlannedTotal.String(), "settled", sw.gross.String())
		if _, err := s.cfg.Recorder.RescanSession(ctx, j.SessionID, sw.block); err != nil {
			return err
		}
	}
	switch {
	case sw.recorded > 0, sw.coveredTx == "" && sw.own > 0 && !sw.short(j):
		return s.jobs.Done(ctx, j, "settled_elsewhere")
	case sw.coveredTx != "":
		if err := s.st.AddTxJobDetails(ctx, j.ID, map[string]string{"covered_by_tx": sw.coveredTx, "covered_by_session": sw.coveredBy}); err != nil {
			return err
		}
		return s.jobs.Done(ctx, j, "covered")
	}
	return s.jobs.Done(ctx, j, "empty")
}

// short reports whether the settlements found (new and already stored) stay below the job's
// planned total. A job without one (a legacy row) is never short: it does not wait the window.
func (sw sweep) short(j store.TxJob) bool {
	return j.PlannedTotal != nil && sw.gross.Cmp(*j.PlannedTotal) < 0
}

// feeOf reads the fee params the job's forwarder commits to.
func (s *Sender) feeOf(j store.TxJob) fee.Params {
	bps, err1 := strconv.ParseUint(j.Details["fee_bps"], 10, 16)
	fixedFee, err2 := strconv.ParseUint(j.Details["fixed_fee_micros"], 10, 64)
	if err1 != nil || err2 != nil {
		return fee.Params{FeeBps: fee.MaxFeeBps + 1}
	}
	return fee.Params{FeeBps: uint16(bps), FixedFeeMicros: fixedFee}
}

// jobRecipients reads the recipient list a flush job's forwarder commits to.
func jobRecipients(j store.TxJob) ([]evm.Address, []uint16, error) {
	var rs []payout.Recipient
	for _, part := range strings.Split(j.Details["recipients"], ",") {
		addr, bps, ok := strings.Cut(part, ":")
		n, err := strconv.ParseUint(bps, 10, 16)
		if !ok || err != nil {
			return nil, nil, fmt.Errorf("flush job %s: malformed recipients", j.ID)
		}
		rs = append(rs, payout.Recipient{Address: addr, Bps: uint16(n)})
	}
	return derive.EVMRecipients(rs)
}

// errBadJob marks a job whose stored details cannot be read; retrying cannot fix it.
var errBadJob = errors.New("malformed job")
