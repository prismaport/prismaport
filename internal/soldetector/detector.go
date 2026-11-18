// Package soldetector turns agreed Solana transactions into session payments. Lean scanning
// (D49): each tick of a scanning cluster reads the
// token balance of every watched deposit account (the classic-SPL-Token ATA of each session PDA
// for each configured mint) with one getMultipleAccounts per 100 accounts, from one provider,
// alternating providers tick by tick. Only a balance that rose costs more: the account's
// signatures from its cursor (two providers, merged), each transaction and its block agreed by
// two providers. A transaction counts only when two providers return it identically; amounts
// come from its SPL Token instructions, cross-checked against the recorded token balance
// changes. A session's reference key (connected wallet and Solana Pay) is listed from one
// provider only while a wallet transaction built for it can land (WalletWindow). Payments go
// detected -> confirmed (credit policy; at confirmed the agreed transaction is enough) ->
// finalized: FinalityDelay after detection, then one FinalityRetry per check, one finalized
// slot from one provider decides whether the agreed block hash and transaction re-check runs. A
// payment whose slot was skipped or replaced on the finalized fork is reversed.
//
// Expired sessions' deposit accounts get late checks on a schedule (the same batched read)
// until their watch ends; a dashboard "Check again" rescans them whatever their balance.
//
// The same read also holds the rent receivers of the watched deposit addresses. Flush is
// permissionless and closing the deposit account pays its rent to the rent receiver, so a
// deposit someone else flushed and closed between two reads still raises the rent receiver's
// lamports: that rise lists its new signatures and rescans any watched deposit account a
// foreign transaction touched (rentProbe).
package soldetector

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/prismaport/prismaport/internal/alert"
	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/chainsettle"
	"github.com/prismaport/prismaport/internal/scan"
	"github.com/prismaport/prismaport/internal/service"
	"github.com/prismaport/prismaport/internal/solana"
	"github.com/prismaport/prismaport/internal/solrpc"
	"github.com/prismaport/prismaport/internal/solwatch"
	"github.com/prismaport/prismaport/internal/store"
)

// DefaultFinalityDelay is how long after detection the first finalized-slot read of a payment is
// due when the configuration sets none (finality trails confirmation by about 13 s).
const DefaultFinalityDelay = 20 * time.Second

const (
	// signaturesPage is the getSignaturesForAddress page size.
	signaturesPage = 1000
	// rentPage bounds the rent receiver signatures one probe lists.
	rentPage = 50
)

// Config holds the settings of a Detector; zero values fall back to defaults.
type Config struct {
	Chain chain.Chain
	// Credit is the commitment at which a detected payment counts: solrpc.Confirmed (default)
	// or solrpc.Finalized.
	Credit solrpc.Commitment
	// Schedule and WatchAfterExpiry are the late checks after expiry (nil Schedule: the
	// defaults; an empty one turns them off, only "Check again" requests run).
	Schedule         scan.Schedule
	WatchAfterExpiry time.Duration
	// LateBatch bounds the entries one late pass checks; AccountBatch the accounts of one
	// getMultipleAccounts (at most 100).
	LateBatch    int
	AccountBatch int
	// FinalityDelay is how long after detection a payment's first finalized-slot read is due
	// (default 20s); FinalityRetry the wait after a read that did not cover it (default 1m, the
	// warm interval in the worker).
	FinalityDelay time.Duration
	FinalityRetry time.Duration
	// WalletWindow is how long a wallet transaction built for a session can land (default
	// store.SolanaWalletWindow): the reference key is scanned until WatchEntry.WalletScanEnd.
	WalletWindow time.Duration
}

func (c *Config) defaults() {
	if c.Credit == "" {
		c.Credit = solrpc.Confirmed
	}
	if c.Schedule == nil {
		c.Schedule, _ = scan.ParseSchedule(scan.DefaultLateSchedule)
		c.WatchAfterExpiry = scan.DefaultWatchAfterExpiry
	}
	if len(c.Schedule) == 0 {
		c.WatchAfterExpiry = 0
	}
	if c.LateBatch == 0 {
		c.LateBatch = 100
	}
	if c.AccountBatch <= 0 || c.AccountBatch > 100 {
		c.AccountBatch = 100
	}
	if c.FinalityDelay == 0 {
		c.FinalityDelay = DefaultFinalityDelay
	}
	if c.FinalityRetry == 0 {
		c.FinalityRetry = time.Minute
	}
	if c.WalletWindow == 0 {
		c.WalletWindow = store.SolanaWalletWindow
	}
}

// Detector watches Solana forwarder accounts and reports the payments it finds to the service.
type Detector struct {
	// mu serializes the steps that read or move the in-memory state below (ticks, late passes
	// and the sender's RescanAccount, which runs in the sender's goroutine).
	mu     sync.Mutex
	cfg    Config
	q      *solwatch.Quorum
	rot    *solwatch.Rotor
	svc    *service.Service
	st     watchStore
	alerts alert.Sink
	log    *slog.Logger
	mints  map[string]chain.Asset
	// slot is the highest context slot a balance read returned: the next read asks for at least
	// that slot, so a provider behind it fails over instead of answering an older balance.
	slot uint64
	// finalized is the highest finalized slot a finality check read. A cursor only moves past
	// transactions at or below it.
	finalized uint64
	// rent is what the rent receiver probe saw per rent receiver (in memory: a restart starts
	// from the next read).
	rent map[string]rentSeen
	// reorged holds deposit accounts whose payment was reversed, rescanned every tick until the
	// time given (the transaction may land again while the account is already flushed and
	// closed, which no balance shows) or until a payment is found on them.
	reorged map[string]time.Time
}

// ReorgRescan is how long the deposit account of a reversed payment is rescanned every tick.
const ReorgRescan = 10 * time.Minute

// rentSeen is a rent receiver's lamports at the last read and the newest signature of its
// history already looked at.
type rentSeen struct {
	lamports uint64
	sig      string
	slot     uint64
}

// watchStore is what the detector reads and writes of the Solana watch: the watched accounts and
// their cursors, the payments it tracks and the splits it records. *store.Store satisfies it.
type watchStore interface {
	chainsettle.Store
	ChainPaymentsOfSession(ctx context.Context, sessionID string) ([]store.TrackedPayment, error)
	ClearRecheck(ctx context.Context, sessionID string, requestedAt time.Time) (bool, error)
	CurrentAddressSession(ctx context.Context, addressID string) (string, error)
	EnsureSolanaWatch(ctx context.Context, rows []store.SolanaWatch) (map[string]store.SolanaWatch, error)
	LateEntries(ctx context.Context, q store.ScanQuery, limit int) ([]store.WatchEntry, error)
	MarkSolanaChecked(ctx context.Context, chainID, address string, c store.SolanaCheck) error
	OwnTxHashes(ctx context.Context, chainID string, hashes []string) (map[string]bool, error)
	PendingChainPayments(ctx context.Context, chainID string) ([]store.TrackedPayment, error)
	ResetSolanaCursors(ctx context.Context, chainID, sessionID string) error
	SessionWatchEntries(ctx context.Context, chainID, sessionID string) ([]store.WatchEntry, error)
	SetPaymentFinalityCheck(ctx context.Context, paymentID string, at time.Time) error
	SetSolanaCursor(ctx context.Context, chainID, address, sig string, slot uint64) error
	SolanaDepositRows(ctx context.Context, chainID string, addrs []string) ([]store.SolanaWatch, error)
	SolanaWatchSet(ctx context.Context, chainID string, now time.Time, window time.Duration) ([]store.WatchEntry, error)
	TakeSolanaWatch(ctx context.Context, chainID, sessionID string, addrs []string) error
	UpsertChainPayment(ctx context.Context, c store.ChainPayment) error
}

// New returns a Detector for a Solana chain, or an error if cfg.Chain is not Solana.
func New(cfg Config, q *solwatch.Quorum, svc *service.Service, st watchStore, alerts alert.Sink, log *slog.Logger) (*Detector, error) {
	cfg.defaults()
	if cfg.Chain.Family != chain.Solana {
		return nil, fmt.Errorf("soldetector: %s is not a Solana chain", cfg.Chain.ID)
	}
	if cfg.Credit != solrpc.Finalized && cfg.Credit != solrpc.Confirmed {
		return nil, fmt.Errorf("soldetector: credit commitment must be confirmed or finalized, got %q", cfg.Credit)
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if alerts == nil {
		alerts = alert.Log{Logger: log}
	}
	d := &Detector{cfg: cfg, q: q, rot: q.Rotor(), svc: svc, st: st, alerts: alerts, log: log, mints: map[string]chain.Asset{},
		rent: map[string]rentSeen{}, reorged: map[string]time.Time{}}
	for _, a := range cfg.Chain.Assets {
		if _, err := solana.ParsePublicKey(a.Token); err != nil {
			return nil, fmt.Errorf("soldetector: %s %s mint: %w", cfg.Chain.ID, a.Symbol, err)
		}
		d.mints[a.Token] = a
	}
	return d, nil
}

func (d *Detector) chainID() string { return d.cfg.Chain.ID }

// Step runs one lean tick (Scan without the pacer) and returns the number of payments it
// recorded.
func (d *Detector) Step(ctx context.Context) (int, error) {
	return d.tick(ctx)
}

// Scan is the pacer's hot/warm step: one tick.
func (d *Detector) Scan(ctx context.Context, _ scan.Tick) (bool, error) {
	_, err := d.tick(ctx)
	return false, err
}

// tick reads the balances of the open sessions' deposit accounts (and the rent receivers),
// rescans the accounts whose balance rose, lists the reference keys inside their wallet window,
// credits and runs the finality checks that are due. A tick with nothing watched and nothing due
// makes no request.
func (d *Detector) tick(ctx context.Context) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.st.Now()
	entries, err := d.st.SolanaWatchSet(ctx, d.chainID(), now, d.cfg.WalletWindow)
	if err != nil {
		return 0, err
	}
	var deps, refs []target
	for _, tg := range d.targets(entries) {
		e := tg.entries[0]
		switch {
		case tg.kind == store.SolanaDeposit && e.ExpiresAt.After(now):
			deps = append(deps, tg)
		case tg.kind == store.SolanaReference && d.walletScan(e, now):
			refs = append(refs, tg)
		}
	}
	force := map[string]bool{}
	for addr, until := range d.reorged {
		if now.Before(until) {
			force[addr] = true
		} else {
			delete(d.reorged, addr)
		}
	}
	found := 0
	checks, err := d.balances(ctx, deps, force)
	for _, c := range checks {
		found += c.found
		if c.found > 0 {
			delete(d.reorged, c.target.address)
		}
	}
	if err != nil {
		return found, fmt.Errorf("soldetector: read balances: %w", err)
	}
	n, err := d.scanTargets(ctx, refs)
	found += n
	if err != nil {
		return found, fmt.Errorf("soldetector: scan deposit accounts: %w", err)
	}
	if err := d.settle(ctx, now); err != nil {
		return found, fmt.Errorf("soldetector: settle: %w", err)
	}
	return found, nil
}

// Heartbeat is the idle probe: one getSlot from one provider, alternating (solwatch Rotor.Probe).
// A failing provider is failed over, cooled down and shown in the chain status; the probe fails
// when every provider does.
func (d *Detector) Heartbeat(ctx context.Context) error {
	return d.rot.Probe(ctx)
}

// Rest does nothing on Solana: the per-account cursors need no continuity.
func (d *Detector) Rest(context.Context) error { return nil }
