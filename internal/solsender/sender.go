// Package solsender is the fee payer of one Solana cluster. It plans forwarder flushes for
// closed sessions and sends one transaction per deposit token account (two flushes would not fit
// in 1232 bytes): a flush with close_source, which settles the account and closes it with its
// rent to our rent receiver, signed by the gas wallet, with a configurable priority fee and a
// compute-unit limit sized from a simulation. The program is stateless (D41): there is no
// forwarder account to initialize or close, and no instruction moves SOL. It never pays
// rent for a merchant's token account: the program never creates a recipient's account, and a
// flush whose recipient token accounts are not all there is blocked (the funds stay in the
// forwarder) until the merchants create them. The only account a flush can create is our own
// treasury's, which the flusher creates itself first. It also relays connected-wallet payments
// the checkout queued. A flush's settlement is read from the finalized transaction's SPL Token
// movements and recorded after checking it against the fee and split rule; a transaction whose
// blockhash expired before landing is rebuilt and sent again.
package solsender

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/prismaport/prismaport/internal/alert"
	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/solana"
	"github.com/prismaport/prismaport/internal/solwatch"
	"github.com/prismaport/prismaport/internal/store"
	"github.com/prismaport/prismaport/internal/txjob"
)

// DefaultFinalityDelay is how long after a transaction landed its first finalized-slot read is due
// when the configuration sets none.
const DefaultFinalityDelay = 20 * time.Second

const (
	// maxComputeUnits caps the compute unit limit derived from a simulation.
	maxComputeUnits uint32 = 400_000
	// blockedAlertEvery is how often a blocked flush's alert repeats while it stays blocked.
	blockedAlertEvery = 24 * time.Hour
	// expiryCheckAfter is how long after sending a transaction that has not landed its blockhash
	// expiry is checked (the agreed block height, every poll from then on). A blockhash lives
	// about 150 blocks, a minute, so nothing can expire sooner.
	expiryCheckAfter = 50 * time.Second
)

// Config holds the settings of a Sender; zero values fall back to defaults.
type Config struct {
	Chain chain.Chain
	// PriorityFee is the compute-unit price in micro-lamports.
	PriorityFee uint64
	// MinGasBalance raises a warning when the fee payer holds fewer lamports.
	MinGasBalance uint64
	BaseBackoff   time.Duration
	MaxBackoff    time.Duration
	AlertAfter    int
	MaxAttempts   int
	PlanBatch     int
	// BlockedRecheck is how often a flush blocked on a missing recipient token account is
	// checked again.
	BlockedRecheck time.Duration
	// FinalityDelay is how long after a transaction landed its first finalized-slot read is due
	// (default 20s); FinalityRetry the wait after a read that did not cover it (default 1m, the
	// warm interval in the worker). D49: no slot reads while a job waits.
	FinalityDelay time.Duration
	FinalityRetry time.Duration
	// Recorder is told about flushes that moved more than was credited; nil reports nothing (the
	// chain's detector implements it).
	Recorder Recorder
}

// Recorder is the detector's side of the sender's flushes.
type Recorder interface {
	// RescanAccount lists a deposit account's signatures and records what they hold.
	RescanAccount(ctx context.Context, account string) (int, error)
}

func (c *Config) defaults() {
	if c.MinGasBalance == 0 {
		c.MinGasBalance = 50_000_000 // 0.05 SOL
	}
	if c.BaseBackoff == 0 {
		c.BaseBackoff = 30 * time.Second
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
	if c.BlockedRecheck == 0 {
		c.BlockedRecheck = 10 * time.Minute
	}
	if c.FinalityDelay == 0 {
		c.FinalityDelay = DefaultFinalityDelay
	}
	if c.FinalityRetry == 0 {
		c.FinalityRetry = time.Minute
	}
}

// Sender builds, signs and submits the Solana transactions queued as jobs, and tracks them until they settle.
type Sender struct {
	cfg      Config
	q        *solwatch.Quorum
	rot      *solwatch.Rotor
	st       jobStore
	key      *solana.Keypair
	log      *slog.Logger
	assets   map[string]chain.Asset
	lowSince time.Time
	// alerted is when each blocked job was last alerted (in memory: a restart alerts again).
	alerted map[string]time.Time
	jobs    *txjob.Lifecycle
}

// jobStore is what the fee payer reads and writes: its transaction jobs, the flush candidates
// and the settlements they record. *store.Store satisfies it.
type jobStore interface {
	txjob.JobStore
	FlushCandidates(ctx context.Context, chainID string, limit int) ([]store.FlushCandidate, error)
	InsertTxJob(ctx context.Context, j store.TxJob) (store.TxJob, bool, error)
	NextDueTxJob(ctx context.Context, chainID string) (store.TxJob, bool, error)
	OtherSessionFlushes(ctx context.Context, j store.TxJob) (map[string]string, error)
	RecordSettlement(ctx context.Context, sessionID string, s store.Settlement, rule store.SettlementRule) (*store.Session, error)
	SettlementSession(ctx context.Context, chainID, txHash string, logIndex uint32) (string, error)
	SolanaFlushLanded(ctx context.Context, chainID, address string, slot uint64) error
	TxJobs(ctx context.Context, chainID string, statuses ...string) ([]store.TxJob, error)
}

// New returns a Sender that submits through q, stores job state in st and signs with key.
func New(cfg Config, q *solwatch.Quorum, st jobStore, key *solana.Keypair, alerts alert.Sink, log *slog.Logger) *Sender {
	cfg.defaults()
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if alerts == nil {
		alerts = alert.Log{Logger: log}
	}
	s := &Sender{cfg: cfg, q: q, rot: q.Rotor(), st: st, key: key, log: log, assets: map[string]chain.Asset{}, alerted: map[string]time.Time{}}
	for _, a := range cfg.Chain.Assets {
		s.assets[a.Symbol] = a
	}
	s.jobs = &txjob.Lifecycle{Chain: cfg.Chain.ID, Store: st, Alerts: alerts, Log: log,
		BaseBackoff: cfg.BaseBackoff, MaxBackoff: cfg.MaxBackoff, AlertAfter: cfg.AlertAfter, MaxAttempts: cfg.MaxAttempts,
		ResetForRetry: func(j *store.TxJob) {
			j.RawTx, j.SubmittedAt, j.LastValidHeight = nil, nil, nil
			j.MinedTxHash, j.MinedBlock, j.MinedBlockHash = "", nil, ""
		},
		Sibling: "account"}
	return s
}

func (s *Sender) chainID() string { return s.cfg.Chain.ID }

// Wallet is the fee payer's public key.
func (s *Sender) Wallet() solana.PublicKey { return s.key.PublicKey() }

// Plan creates a flush job for every closed session holding confirmed deposits not covered by
// an earlier job. An address that does not re-derive from the stored salt and params is never
// flushed: it is not ours.
func (s *Sender) Plan(ctx context.Context) (int, error) {
	cands, err := s.st.FlushCandidates(ctx, s.chainID(), s.cfg.PlanBatch)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, c := range cands {
		asset, ok := s.assets[c.Asset]
		if !ok || c.Program == "" {
			continue
		}
		f, err := forwarderOf(c.WatchEntry)
		if err != nil || f.Address.String() != c.Address {
			s.jobs.Alert(ctx, alert.Critical, "flush_address_mismatch", "deposit address does not match the forwarder derivation; not flushing",
				map[string]string{"session": c.SessionID, "address": c.Address})
			continue
		}
		mint, err := solana.ParsePublicKey(asset.Token)
		if err != nil {
			return n, err
		}
		ata, err := solana.AssociatedTokenAddress(f.Address, mint)
		if err != nil {
			return n, err
		}
		total := c.Total
		details := map[string]string{"owner": f.Address.String(), "mint": mint.String(), "program": f.Program.String(),
			"salt": hex.EncodeToString(f.Salt[:]), "recipients": txjob.EncodeRecipients(c.Recipients), "treasury": c.Treasury,
			"rent_receiver": c.RentReceiver, "mints": strings.Join(c.Tokens, ","), "fee_bps": strconv.Itoa(int(c.Fee.FeeBps)),
			"fixed_fee_micros": strconv.FormatUint(c.Fee.FixedFeeMicros, 10)}
		if c.CustomerAddressID != nil {
			// The account is shared with the address's other sessions: only a flush from the
			// session's first counted deposit on can have moved its funds.
			details["from_slot"] = strconv.FormatUint(c.FirstPaymentBlock, 10)
		}
		_, inserted, err := s.st.InsertTxJob(ctx, store.TxJob{
			Chain: s.chainID(), Kind: store.JobFlush, Ref: store.FlushRef(fmt.Sprintf("%s|%s", ata, total.String()), c.WatchEntry),
			SessionID: c.SessionID, Asset: c.Asset, Address: ata.String(), PlannedTotal: &total, To: f.Program.String(),
			Calldata: solana.IxFlush.Discriminator[:], Details: details,
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

// Step runs the due finality checks of landed jobs, follows submitted ones, and sends the next
// due job. It returns the number of jobs that changed state.
func (s *Sender) Step(ctx context.Context) (int, error) {
	changed, err := s.finality(ctx)
	if err != nil {
		return changed, err
	}
	open, err := s.st.TxJobs(ctx, s.chainID(), store.JobSubmitted)
	if err != nil {
		return changed, err
	}
	for _, j := range open {
		moved, err := s.follow(ctx, j)
		if err != nil {
			return changed, err
		}
		if moved {
			changed++
		}
	}
	n, err := s.recheckBlocked(ctx)
	changed += n
	if err != nil {
		return changed, err
	}
	j, ok, err := s.st.NextDueTxJob(ctx, s.chainID())
	if err != nil || !ok {
		return changed, err
	}
	if err := s.send(ctx, j); err != nil {
		return changed, err
	}
	return changed + 1, nil
}
