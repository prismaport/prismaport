package sender

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/prismaport/prismaport/internal/alert"
	"github.com/prismaport/prismaport/internal/chainwatch"
	"github.com/prismaport/prismaport/internal/evm"
	"github.com/prismaport/prismaport/internal/evmrpc"
	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/store"
)

// quote returns EIP-1559 fees: tip from the node, fee cap 2x base fee + tip, bounded by the
// configured cap. ok is false while the base fee alone exceeds the cap.
func (s *Sender) quote(ctx context.Context) (tip, feeCap *big.Int, ok bool, err error) {
	h, err := s.q.LatestHeader(ctx)
	if err != nil {
		return nil, nil, false, err
	}
	base := h.BaseFee
	if base == nil {
		base = new(big.Int)
	}
	tip, err = s.q.MaxPriorityFee(ctx)
	if err != nil {
		if !errors.Is(err, chainwatch.ErrNoQuorum) {
			tip = big.NewInt(1_000_000) // 0.001 gwei when a node does not implement the method
		} else {
			return nil, nil, false, err
		}
	}
	feeCap = new(big.Int).Add(new(big.Int).Mul(base, big.NewInt(2)), tip)
	if feeCap.Cmp(s.cfg.MaxFeePerGas) > 0 {
		feeCap = new(big.Int).Set(s.cfg.MaxFeePerGas)
	}
	if tip.Cmp(feeCap) > 0 {
		tip = new(big.Int).Set(feeCap)
	}
	return tip, feeCap, base.Cmp(feeCap) < 0, nil
}

func (s *Sender) nextNonce(ctx context.Context) (uint64, error) {
	n, err := s.q.PendingNonce(ctx, s.wallet)
	if err != nil {
		return 0, err
	}
	used, ok, err := s.st.MaxSenderNonce(ctx, s.chainID(), s.wallet.Hex())
	if err != nil {
		return 0, err
	}
	if ok && used+1 > n {
		n = used + 1
	}
	return n, nil
}

func (s *Sender) send(ctx context.Context, j store.TxJob) error {
	to, err := jobAddr(j, "to", j.To)
	if err != nil {
		return s.jobs.BadJob(ctx, j, err)
	}
	ok, err := s.precheck(ctx, j)
	if err != nil || !ok {
		return err
	}
	gas, err := s.q.EstimateGas(ctx, evmrpc.CallMsg{From: &s.wallet, To: to, Data: j.Calldata})
	if err != nil {
		if evmrpc.IsRevert(err) {
			if j.Kind == store.JobSettle {
				return s.rejectSettle(ctx, j, revertText(err))
			}
			// A paused or frozen token reverts the flush; the funds stay and we retry later.
			return s.jobs.Retry(ctx, j, "estimate reverted: "+revertText(err))
		}
		return err
	}
	gas += gas * gasMarginPct / 100
	tip, feeCap, ok, err := s.quote(ctx)
	if err != nil {
		return err
	}
	if !ok {
		s.jobs.Alert(ctx, alert.Warning, "gas_price_above_cap", "base fee above the configured cap; waiting",
			map[string]string{"cap_wei": s.cfg.MaxFeePerGas.String()})
		j.NextAttemptAt = s.st.Now().Add(s.cfg.BaseBackoff)
		return s.st.UpdateTxJob(ctx, &j)
	}
	need := new(big.Int).Mul(new(big.Int).SetUint64(gas), feeCap)
	bal, err := s.q.Balance(ctx, s.wallet)
	if err != nil {
		return err
	}
	if bal.Cmp(s.cfg.MinGasBalance) < 0 && s.st.Now().Sub(s.lowSince) > time.Hour {
		s.lowSince = s.st.Now()
		s.jobs.Alert(ctx, alert.Warning, "gas_wallet_low", "gas wallet below the configured minimum",
			map[string]string{"wallet": s.wallet.Hex(), "balance_wei": bal.String()})
	}
	if bal.Cmp(need) < 0 {
		s.jobs.Alert(ctx, alert.Critical, "gas_wallet_empty", "gas wallet cannot pay for the next transaction",
			map[string]string{"wallet": s.wallet.Hex(), "balance_wei": bal.String(), "need_wei": need.String()})
		j.NextAttemptAt = s.st.Now().Add(s.cfg.BaseBackoff)
		return s.st.UpdateTxJob(ctx, &j)
	}
	nonce, err := s.nextNonce(ctx)
	if err != nil {
		return err
	}
	j.Sender = s.wallet.Hex()
	j.Nonce, j.GasLimit = &nonce, gas
	return s.sign(ctx, j, tip, feeCap)
}

func units(b *big.Int) *money.Units {
	u, err := money.FromBig(b)
	if err != nil {
		return nil
	}
	return &u
}

// sign signs the job's transaction at its nonce, persists it before broadcasting (so a crash
// can never lose track of a sent transaction), then broadcasts to every provider.
func (s *Sender) sign(ctx context.Context, j store.TxJob, tip, feeCap *big.Int) error {
	to, err := jobAddr(j, "to", j.To)
	if err != nil {
		return s.jobs.BadJob(ctx, j, err) // never signed to the zero address
	}
	tx := evm.DynamicFeeTx{ChainID: s.cfg.Chain.EVMID, Nonce: *j.Nonce, GasTipCap: tip, GasFeeCap: feeCap, Gas: j.GasLimit,
		To: &to, Value: new(big.Int), Data: j.Calldata}
	raw, hash := tx.Sign(s.key)
	now := s.st.Now()
	j.Status, j.RawTx, j.MaxFee, j.Tip = store.JobSubmitted, raw, units(feeCap), units(tip)
	j.TxHashes = append(j.TxHashes, hash.Hex())
	j.SubmittedAt = &now
	if err := s.st.UpdateTxJob(ctx, &j); err != nil {
		return err
	}
	accepted, errs := s.q.Broadcast(ctx, raw)
	if accepted == 0 {
		for _, e := range errs {
			if chainwatch.NonceTooLow(e) {
				return s.nonceUsed(ctx, j, "nonce already used: "+e.Error())
			}
		}
		s.log.Warn("broadcast not accepted yet", "chain", s.chainID(), "job", j.ID, "errs", fmt.Sprint(errs))
	}
	s.log.Info("tx sent", "chain", s.chainID(), "job", j.ID, "kind", j.Kind, "tx", hash.Hex(), "nonce", *j.Nonce,
		"gas", j.GasLimit, "max_fee", feeCap.String(), "tip", tip.String())
	return nil
}
