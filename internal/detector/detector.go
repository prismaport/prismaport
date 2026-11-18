// Package detector turns agreed EVM chain data into session payments (D49, lean scanning). Each
// tick of a scanning chain reads the token balance of every watched deposit entry in one
// Multicall3 aggregate from one provider (alternating providers tick by tick); the aggregate's
// block number is the head. Only a balance that rose costs more: the bounded rescan of those
// addresses (eth_getLogs of the configured tokens' Transfers to them, from the block the last
// rescan reached, with two providers agreeing on the logs and on every block hash), which
// records the transfers it finds. A detected payment at block B is confirmed once a later tick's
// head reaches B + CONFIRMATIONS - 1 and B's agreed hash is unchanged; finality is scheduled
// (FINALITY_DELAY after detection, then one warm interval per retry): one finalized-head read
// from one provider, then the agreed hash re-check. A hash that changed reverses the payment.
//
// Signed-path (EIP-3009, Gateway) payments are recorded from the sender's settle job: the
// detector reads the Gateway logs of the job's block (GatewaySettled). Expired sessions get late
// checks on a schedule (the same batched balance read and rescan) until their watch ends; a
// dashboard "Check again" request forces one rescan later. A scan.Pacer decides when each runs
// (adaptive scanner, payer selection and lean scanning).
package detector

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/prismaport/prismaport/internal/alert"
	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/chainsettle"
	"github.com/prismaport/prismaport/internal/chainwatch"
	"github.com/prismaport/prismaport/internal/contracts"
	"github.com/prismaport/prismaport/internal/evm"
	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/scan"
	"github.com/prismaport/prismaport/internal/service"
	"github.com/prismaport/prismaport/internal/session"
	"github.com/prismaport/prismaport/internal/store"
)

// Event topics the detector reads from the chain logs.
var (
	TransferTopic        = contracts.TransferTopic
	PaymentReceivedTopic = contracts.PaymentReceivedTopic
	PayoutTopic          = contracts.PayoutTopic
)

// DefaultFinalityDelay is how long after detection the first finalized-head read of a payment is
// due on a chain that configures none (Base finalizes in about 15-20 minutes).
const DefaultFinalityDelay = 15 * time.Minute

const (
	// addressBatch bounds the OR list of recipient topics (or contract addresses) per log query.
	addressBatch = 200
	// multicallBatch bounds the balanceOf calls of one Multicall3 aggregate.
	multicallBatch = 500
)

// Config holds one EVM chain's detector settings; zero values get defaults in New.
type Config struct {
	Chain   chain.Chain
	Gateway *evm.Address
	Policy  chainwatch.CreditPolicy
	// MaxRange bounds one eth_getLogs block range (many providers cap it; default chainwatch.DefaultMaxLogRange). Both
	// providers of the quorum are asked for the same ranges, so it must fit the stricter one.
	MaxRange uint64
	// Multicall is the Multicall3 contract; nil uses the registry's (Chain.Multicall3), else the
	// canonical address.
	Multicall *evm.Address
	// BlockTime (default 2s) and ReanchorMargin (blocks; zero means none, config passes 30) turn
	// the time since a network was revealed into the first block its address is rescanned from:
	// head - ceil(age / BlockTime) - ReanchorMargin.
	BlockTime      time.Duration
	ReanchorMargin uint64
	// FinalityDelay is how long after detection a payment's first finalized-head read is due
	// (default 15m); FinalityRetry is the wait after a read that did not cover it (default 1m,
	// config passes the warm interval).
	FinalityDelay time.Duration
	FinalityRetry time.Duration
	// Schedule is the late-check schedule after expiry and WatchAfterExpiry how long checks run.
	// A nil Schedule means the defaults (30m:1m,23h30m:1h and 23h30m); an empty one turns late
	// checks off (only "Check again" requests run).
	Schedule         scan.Schedule
	WatchAfterExpiry time.Duration
	// LateBatch bounds the entries one late pass checks (default: multicallBatch).
	LateBatch int
	// ReconcileWait is how long a balance the rescanned logs do not explain is retried before it
	// is stored anyway, with a Critical alert (default 10m; H-1).
	ReconcileWait time.Duration
	// BaselineReach bounds how far (in time, turned into blocks with BlockTime) before where a
	// customer address's rescan would start a stored balance still serves as the baseline that
	// must explain a reading (default 24h; H-1). An older one (a token the address's sessions
	// stopped watching for a while) would pull the tick's rescan back to it, every tick while the
	// reading is unexplained; past the reach the token's baseline counts as unknown.
	BaselineReach time.Duration
}

func (c *Config) defaults() {
	if c.Policy.Confirmations == 0 && !c.Policy.UseSafeHead {
		c.Policy.Confirmations = chainwatch.DefaultConfirmations
	}
	if c.MaxRange == 0 {
		c.MaxRange = chainwatch.DefaultMaxLogRange
	}
	if c.BlockTime == 0 {
		c.BlockTime = 2 * time.Second
	}
	if c.FinalityDelay == 0 {
		c.FinalityDelay = DefaultFinalityDelay
	}
	if c.FinalityRetry == 0 {
		c.FinalityRetry = time.Minute
	}
	if c.Schedule == nil {
		c.Schedule, _ = scan.ParseSchedule(scan.DefaultLateSchedule)
		c.WatchAfterExpiry = scan.DefaultWatchAfterExpiry
	}
	if len(c.Schedule) == 0 {
		c.WatchAfterExpiry = 0
	}
	if c.LateBatch == 0 {
		c.LateBatch = multicallBatch
	}
	if c.ReconcileWait == 0 {
		c.ReconcileWait = 10 * time.Minute
	}
	if c.BaselineReach == 0 {
		c.BaselineReach = 24 * time.Hour
	}
}

// Detector finds payments to the watched deposit addresses of one EVM chain and records them.
type Detector struct {
	cfg       Config
	q         *chainwatch.Quorum
	rot       *chainwatch.Rotor
	multicall evm.Address
	svc       *service.Service
	st        watchStore
	alerts    alert.Sink
	log       *slog.Logger
	tokens    map[evm.Address]chain.Asset
	// unexplained remembers since when a rescanned balance has not been explained by its logs.
	unexplained unexplained
}

// watchStore is what the detector reads and writes of the chain watch: the watched addresses and
// their cursors, the payments it tracks and the splits it records. *store.Store satisfies it.
type watchStore interface {
	chainsettle.Store
	AddressCursors(ctx context.Context, chainID string, addressIDs []string) (map[string]store.AddressCursor, error)
	ChainPaymentsOfSession(ctx context.Context, sessionID string) ([]store.TrackedPayment, error)
	ClearRecheck(ctx context.Context, sessionID string, requestedAt time.Time) (bool, error)
	CurrentAddressSession(ctx context.Context, addressID string) (string, error)
	LateEntries(ctx context.Context, q store.ScanQuery, limit int) ([]store.WatchEntry, error)
	MarkChecked(ctx context.Context, e store.WatchEntry, c store.EVMCheck) error
	OwnFlushJob(ctx context.Context, chainID, address string) (bool, error)
	PendingChainPayments(ctx context.Context, chainID string) ([]store.TrackedPayment, error)
	ReopenSettleJobs(ctx context.Context, chainID, sessionID, txHash string) (int, error)
	ResetLateCheck(ctx context.Context, chainID, sessionID string, block uint64) error
	SessionWatchEntries(ctx context.Context, chainID, sessionID string) ([]store.WatchEntry, error)
	SetAddressCursor(ctx context.Context, chainID, addressID, sessionID string, block uint64) error
	SetCreditHead(ctx context.Context, chainID string, head uint64) error
	SetPaymentFinalityCheck(ctx context.Context, paymentID string, at time.Time) error
	TakeWatch(ctx context.Context, e store.WatchEntry, firstBlock uint64) (bool, error)
	UpsertChainPayment(ctx context.Context, c store.ChainPayment) error
	WatchCheck(ctx context.Context, e store.WatchEntry) (checkedAt *time.Time, balance *money.Units, block *uint64, err error)
	WatchSet(ctx context.Context, chainID string, since time.Time) ([]store.WatchEntry, error)
}

// New returns a detector for the configured chain, filling unset config fields with their defaults.
func New(cfg Config, q *chainwatch.Quorum, svc *service.Service, st watchStore, alerts alert.Sink, log *slog.Logger) (*Detector, error) {
	cfg.defaults()
	if cfg.Chain.Family != chain.EVM {
		return nil, fmt.Errorf("detector: %s is not an EVM chain", cfg.Chain.ID)
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if alerts == nil {
		alerts = alert.Log{Logger: log}
	}
	mc := contracts.Multicall3Address
	switch {
	case cfg.Multicall != nil:
		mc = *cfg.Multicall
	case cfg.Chain.Multicall3 != "":
		a, err := evm.ParseAddressLoose(cfg.Chain.Multicall3)
		if err != nil {
			return nil, fmt.Errorf("detector: %s multicall3: %w", cfg.Chain.ID, err)
		}
		mc = a
	}
	d := &Detector{cfg: cfg, q: q, rot: q.Rotor(), multicall: mc, svc: svc, st: st, alerts: alerts, log: log,
		tokens: map[evm.Address]chain.Asset{}}
	for _, a := range cfg.Chain.Assets {
		t, err := evm.ParseAddressLoose(a.Token)
		if err != nil {
			return nil, fmt.Errorf("detector: %s %s token: %w", cfg.Chain.ID, a.Symbol, err)
		}
		d.tokens[t] = a
	}
	return d, nil
}

func (d *Detector) chainID() string { return d.cfg.Chain.ID }

var errInconsistent = errors.New("detector: logs and block hashes are inconsistent; retrying")

// Step runs one tick (callers without a pacer, tests). It returns the number of transfers the
// tick's rescans attributed to a session.
func (d *Detector) Step(ctx context.Context) (int, error) {
	return d.tick(ctx)
}

// Scan is the pacer's hot/warm step: one tick. It never asks to run again at once: a tick reads
// every watched entry (in multicallBatch chunks) and rescans what changed in full.
func (d *Detector) Scan(ctx context.Context, _ scan.Tick) (bool, error) {
	_, err := d.tick(ctx)
	return false, err
}

// tick reads the balances of the watched entries when there are any or a detected payment waits
// for confirmations (the aggregate is also the head; with no entry it carries no call), rescans
// the entries whose balance rose, credits what is deep enough and runs the finality checks that
// are due. A tick with nothing watched and nothing due makes no request.
func (d *Detector) tick(ctx context.Context) (int, error) {
	now := d.st.Now()
	entries, err := d.st.WatchSet(ctx, d.chainID(), now)
	if err != nil {
		return 0, err
	}
	pays, err := d.st.PendingChainPayments(ctx, d.chainID())
	if err != nil {
		return 0, err
	}
	waiting := false
	for _, p := range pays {
		waiting = waiting || p.Status == session.Detected
	}
	found := 0
	if len(entries) > 0 || waiting {
		head, n, err := d.observe(ctx, entries, now, false)
		if err != nil {
			return 0, err
		}
		found = n
		if n > 0 {
			if pays, err = d.st.PendingChainPayments(ctx, d.chainID()); err != nil {
				return found, err
			}
		}
		if err := d.credit(ctx, head, pays); err != nil {
			return found, err
		}
	}
	return found, d.finality(ctx, pays, now)
}

// Heartbeat is the idle probe: one latest header from one provider, alternating (chainwatch
// Rotor.Probe). A failing provider is failed over, cooled down and shown in the chain status;
// the probe fails when every provider does.
func (d *Detector) Heartbeat(ctx context.Context) error {
	return d.rot.Probe(ctx)
}

// Rest is called when the chain leaves the scanning modes. Lean scanning keeps no block cursor,
// so there is nothing to put to rest.
func (d *Detector) Rest(context.Context) error { return nil }
