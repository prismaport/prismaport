// Package sender is the gas wallet of one EVM chain. It plans forwarder flushes for closed
// sessions, and sends flush and signed-path settle transactions one at a time (so nonces never
// collide), with EIP-1559 fees under a cap, fee-bumped replacement of stuck transactions,
// retries with backoff and alerts. A flush's Settled event is recorded as the
// session's settlement once its block is final, after checking it against the fee rule.
//
// Lean reads (D49): a submitted transaction's receipt is polled from one provider at a time; a
// mined flush waits for finality on a schedule (FinalityDelay after it was mined, then one
// FinalityRetry per check), each check one finalized header from one provider, and only then the
// agreed block hash and receipt. A mined settle hands its block to the detector, which records
// the Gateway payment from that block's agreed logs.
package sender

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"time"

	"github.com/prismaport/prismaport/internal/alert"
	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/chainwatch"
	"github.com/prismaport/prismaport/internal/contracts"
	"github.com/prismaport/prismaport/internal/derive"
	"github.com/prismaport/prismaport/internal/evm"
	"github.com/prismaport/prismaport/internal/store"
	"github.com/prismaport/prismaport/internal/txjob"
)

// gasMarginPct is added on top of eth_estimateGas.
const gasMarginPct = 25

// Config holds one EVM chain's sender settings; zero values get defaults in New.
type Config struct {
	Chain   chain.Chain
	Factory evm.Address
	// MaxFeePerGas caps the fee per gas; a quote above it waits.
	MaxFeePerGas *big.Int
	// MinGasBalance raises a warning when the wallet holds less.
	MinGasBalance *big.Int
	// ReplaceAfter is how long a transaction may stay unmined before it is re-sent with
	// higher fees under the same nonce.
	ReplaceAfter time.Duration
	BaseBackoff  time.Duration
	MaxBackoff   time.Duration
	// AlertAfter raises a critical alert once a job failed this many times.
	AlertAfter int
	// MaxAttempts gives up on a job (status failed) after this many failures.
	MaxAttempts int
	PlanBatch   int
	// MaxLogRange bounds one eth_getLogs block range (default chainwatch.DefaultMaxLogRange).
	MaxLogRange uint64
	// FinalityDelay is how long after a flush was mined its first finality check is due (default
	// 15 minutes); FinalityRetry the wait after a check that found the block not final yet
	// (default 1 minute, config passes the warm interval).
	FinalityDelay time.Duration
	FinalityRetry time.Duration
	// Recorder is told about mined settle jobs and flushes that moved more than was credited;
	// nil reports nothing (the detector implements it).
	Recorder Recorder
}

// Recorder is the detector's side of the sender's transactions.
type Recorder interface {
	// GatewaySettled records the signed-path payment of a settle job mined (or reverted) in block.
	GatewaySettled(ctx context.Context, j store.TxJob, block uint64, reverted bool) (int, error)
	// RescanSession rescans a session's deposit address up to block.
	RescanSession(ctx context.Context, sessionID string, block uint64) (int, error)
	// BaselineReach is how many blocks before a rescan's start a stored balance still explains
	// a reading: a flush's zero moves the address's checked_block back at most that far.
	BaselineReach() uint64
}

// reach is the detector's baseline reach (0 without a detector: a flush's zero then leaves
// checked_block where it is).
func (s *Sender) reach() uint64 {
	if s.cfg.Recorder == nil {
		return 0
	}
	return s.cfg.Recorder.BaselineReach()
}

func (c *Config) defaults() {
	if c.MaxFeePerGas == nil {
		c.MaxFeePerGas = big.NewInt(5_000_000_000)
	}
	if c.MinGasBalance == nil {
		c.MinGasBalance = big.NewInt(1_000_000_000_000_000)
	}
	if c.MaxLogRange == 0 {
		c.MaxLogRange = chainwatch.DefaultMaxLogRange
	}
	if c.ReplaceAfter == 0 {
		c.ReplaceAfter = 2 * time.Minute
	}
	if c.BaseBackoff == 0 {
		c.BaseBackoff = txjob.DefaultBaseBackoff
	}
	if c.MaxBackoff == 0 {
		c.MaxBackoff = time.Hour
	}
	if c.AlertAfter == 0 {
		c.AlertAfter = 5
	}
	if c.MaxAttempts == 0 {
		c.MaxAttempts = 30
	}
	if c.PlanBatch == 0 {
		c.PlanBatch = 50
	}
	if c.FinalityDelay == 0 {
		c.FinalityDelay = 15 * time.Minute
	}
	if c.FinalityRetry == 0 {
		c.FinalityRetry = time.Minute
	}
}

// Sender signs and broadcasts the queued transaction jobs of one EVM chain and follows them until
// they are final.
type Sender struct {
	cfg       Config
	q         *chainwatch.Quorum
	rot       *chainwatch.Rotor
	st        jobStore
	key       *evm.PrivateKey
	wallet    evm.Address
	log       *slog.Logger
	tokens    map[string]chain.Asset
	predictor derive.EVMCreate2
	lowSince  time.Time
	jobs      *txjob.Lifecycle
}

// jobStore is what the sender reads and writes: its transaction jobs, the flush candidates and
// the settlements they record. *store.Store satisfies it.
type jobStore interface {
	txjob.JobStore
	FlushCandidates(ctx context.Context, chainID string, limit int) ([]store.FlushCandidate, error)
	FlushMined(ctx context.Context, chainID, address, token string, block, reach uint64) error
	InsertTxJob(ctx context.Context, j store.TxJob) (store.TxJob, bool, error)
	MaxSenderNonce(ctx context.Context, chainID, sender string) (uint64, bool, error)
	NextDueTxJob(ctx context.Context, chainID string) (store.TxJob, bool, error)
	OtherSessionFlushes(ctx context.Context, j store.TxJob) (map[string]string, error)
	RecordSettlement(ctx context.Context, sessionID string, s store.Settlement, rule store.SettlementRule) (*store.Session, error)
	SettlementSession(ctx context.Context, chainID, txHash string, logIndex uint32) (string, error)
	TxJobs(ctx context.Context, chainID string, statuses ...string) ([]store.TxJob, error)
}

// New returns a sender for the configured chain, filling unset config fields with their defaults.
func New(cfg Config, q *chainwatch.Quorum, st jobStore, key *evm.PrivateKey, alerts alert.Sink, log *slog.Logger) *Sender {
	cfg.defaults()
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if alerts == nil {
		alerts = alert.Log{Logger: log}
	}
	s := &Sender{cfg: cfg, q: q, rot: q.Rotor(), st: st, key: key, wallet: key.Address(), log: log,
		tokens: map[string]chain.Asset{}, predictor: derive.Forwarders(cfg.Factory)}
	for _, a := range cfg.Chain.Assets {
		s.tokens[a.Symbol] = a
	}
	s.jobs = &txjob.Lifecycle{Chain: cfg.Chain.ID, Store: st, Alerts: alerts, Log: log,
		BaseBackoff: cfg.BaseBackoff, MaxBackoff: cfg.MaxBackoff, AlertAfter: cfg.AlertAfter, MaxAttempts: cfg.MaxAttempts,
		ResetForRetry: func(j *store.TxJob) { j.Nonce, j.RawTx, j.SubmittedAt, j.TxHashes = nil, nil, nil, nil },
		ClearOnFail:   func(j *store.TxJob) { j.Nonce, j.RawTx = nil, nil },
		Sibling:       "forwarder"}
	return s
}

func (s *Sender) chainID() string { return s.cfg.Chain.ID }

// Plan creates a flush job for every closed session holding confirmed deposits not covered by
// an earlier job. An address the Factory would not produce from the stored salt and params is
// never flushed: it is not ours.
func (s *Sender) Plan(ctx context.Context) (int, error) {
	cands, err := s.st.FlushCandidates(ctx, s.chainID(), s.cfg.PlanBatch)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, c := range cands {
		asset, ok := s.tokens[c.Asset]
		if !ok {
			continue
		}
		// The address binds every token the session offered on the chain (bound_tokens, D41).
		predicted, err := s.predictor.Address(s.cfg.Chain, c.Salt, derive.Params{Treasury: c.Treasury, Fee: c.Fee, Recipients: c.Recipients,
			Tokens: c.Tokens})
		if err != nil || !strings.EqualFold(predicted, c.Address) {
			s.jobs.Alert(ctx, alert.Critical, "flush_address_mismatch", "deposit address does not match the Factory derivation; not flushing",
				map[string]string{"session": c.SessionID, "address": c.Address})
			continue
		}
		rcpts, bps, err1 := derive.EVMRecipients(c.Recipients)
		boundTokens, err2 := derive.EVMTokens(c.Tokens)
		tre, err3 := evm.ParseAddressLoose(c.Treasury)
		token, err4 := evm.ParseAddressLoose(asset.Token)
		addr, err5 := evm.ParseAddressLoose(c.Address)
		if err := errors.Join(err1, err2, err3, err4, err5); err != nil {
			// Never planned with a zero address in its place.
			s.jobs.Alert(ctx, alert.Critical, "flush_bad_candidate", "flush candidate holds a malformed address; not flushing",
				map[string]string{"session": c.SessionID, "address": c.Address, "error": err.Error()})
			continue
		}
		total := c.Total
		from := uint64(0)
		if c.FirstBlock != nil {
			from = *c.FirstBlock
		}
		if c.CustomerAddressID != nil {
			// The forwarder is shared with the address's other sessions: only a flush from the
			// session's first counted deposit on can have moved its funds.
			from = c.FirstPaymentBlock
		}
		_, inserted, err := s.st.InsertTxJob(ctx, store.TxJob{
			Chain: s.chainID(), Kind: store.JobFlush, Ref: store.FlushRef(fmt.Sprintf("%s|%s|%s", addr.Lower(), token.Lower(), total.String()), c.WatchEntry),
			SessionID: c.SessionID, Asset: c.Asset, Address: addr.Hex(), PlannedTotal: &total, To: s.cfg.Factory.Hex(),
			Calldata: contracts.DeployAndFlush(c.Salt, contracts.ForwarderParams{Treasury: tre, Fee: c.Fee, Recipients: rcpts, Bps: bps,
				Tokens: boundTokens}, []evm.Address{token}), // only this asset: a paused or frozen token never blocks another (D34)
			Details: map[string]string{"token": token.Hex(), "from_block": fmt.Sprint(from), "recipients": txjob.EncodeRecipients(c.Recipients),
				"fee_bps": fmt.Sprint(c.Fee.FeeBps), "fixed_fee_micros": fmt.Sprint(c.Fee.FixedFeeMicros)},
		})
		if err != nil {
			return n, err
		}
		if inserted {
			n++
			s.log.Info("flush planned", "chain", s.chainID(), "session", c.SessionID, "asset", c.Asset, "total", total.String())
		}
	}
	return n, nil
}

// Step finishes mined jobs whose finality check is due, follows the in-flight transaction, and
// sends the next pending job once nothing is in flight. It returns the number of jobs that
// changed state.
func (s *Sender) Step(ctx context.Context) (int, error) {
	changed, err := s.finality(ctx)
	if err != nil {
		return changed, fmt.Errorf("sender: finality: %w", err)
	}
	inflight, err := s.st.TxJobs(ctx, s.chainID(), store.JobSubmitted)
	if err != nil {
		return changed, err
	}
	for _, j := range inflight {
		moved, err := s.follow(ctx, j)
		if err != nil {
			return changed, fmt.Errorf("sender: follow job %s: %w", j.ID, err)
		}
		if moved {
			changed++
		}
	}
	if len(inflight) > 0 {
		if still, err := s.st.TxJobs(ctx, s.chainID(), store.JobSubmitted); err != nil || len(still) > 0 {
			return changed, err
		}
	}
	j, ok, err := s.st.NextDueTxJob(ctx, s.chainID())
	if err != nil || !ok {
		return changed, err
	}
	if err := s.send(ctx, j); err != nil {
		return changed, fmt.Errorf("sender: send job %s: %w", j.ID, err)
	}
	return changed + 1, nil
}

// Wallet is the gas wallet address.
func (s *Sender) Wallet() evm.Address { return s.wallet }
