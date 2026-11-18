package app

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/prismaport/prismaport/internal/alert"
	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/chainsched"
	"github.com/prismaport/prismaport/internal/chainwatch"
	"github.com/prismaport/prismaport/internal/config"
	"github.com/prismaport/prismaport/internal/detector"
	"github.com/prismaport/prismaport/internal/evm"
	"github.com/prismaport/prismaport/internal/scan"
	"github.com/prismaport/prismaport/internal/sender"
	"github.com/prismaport/prismaport/internal/service"
	"github.com/prismaport/prismaport/internal/solana"
	"github.com/prismaport/prismaport/internal/soldetector"
	"github.com/prismaport/prismaport/internal/solsender"
	"github.com/prismaport/prismaport/internal/solwatch"
	"github.com/prismaport/prismaport/internal/store"
	"github.com/prismaport/prismaport/internal/worker"
)

// DefaultVerifyRetry is how long a loop waits after a failed provider verification before asking
// again; the detector loop ticks every active mode check (5s) and would otherwise ask on every tick.
const DefaultVerifyRetry = 30 * time.Second

// retryPolicy is when a failed provider verification is asked again: retry after the failure,
// read on now.
type retryPolicy struct {
	retry time.Duration
	now   func() time.Time
}

// verifyPolicy is the provider verification retry of the loops over st: Root.VerifyRetry (nil:
// DefaultVerifyRetry) on the store's clock.
func (e Root) verifyPolicy(st *store.Store) retryPolicy {
	retry := DefaultVerifyRetry
	if e.VerifyRetry != nil {
		retry = *e.VerifyRetry
	}
	return retryPolicy{retry: retry, now: st.Now}
}

// verifiedBy runs step only after verify succeeded once; until then the loop reports the failure
// and does nothing, retrying at most every p.retry (the ticks in between are skipped).
func verifiedBy(p retryPolicy, verify func(context.Context) error, step func(context.Context) (int, error)) func(context.Context) (int, error) {
	ok := false
	var failedAt time.Time
	return func(ctx context.Context) (int, error) {
		if !ok {
			if !failedAt.IsZero() && p.now().Sub(failedAt) < p.retry {
				return 0, worker.ErrSkipped
			}
			if err := verify(ctx); err != nil {
				failedAt = p.now()
				return 0, err
			}
			ok = true
		}
		return step(ctx)
	}
}

// verified runs step only after every provider of the chain proved it serves the expected
// chain id; until then the loop reports the mismatch and does nothing.
func verified(p retryPolicy, q *chainwatch.Quorum, want uint64, step func(context.Context) (int, error)) func(context.Context) (int, error) {
	return verifiedBy(p, func(ctx context.Context) error { return q.VerifyChainID(ctx, want) }, step)
}

// genesisVerified runs step only after every provider proved it serves the expected cluster.
func genesisVerified(p retryPolicy, q *solwatch.Quorum, want string, step func(context.Context) (int, error)) func(context.Context) (int, error) {
	return verifiedBy(p, func(ctx context.Context) error { return q.VerifyGenesis(ctx, want) }, step)
}

// locked makes the step a single writer across workers: it runs only while this process holds
// the named advisory lock, checked before every run (chainsched.ErrNotHeld while another process
// holds it). After the lock connection fails the step is skipped until the lock is taken again,
// which waits for any worker that took it meanwhile.
func locked(locks *store.Locker, name string, step func(context.Context) (int, error)) func(context.Context) (int, error) {
	return func(ctx context.Context) (int, error) {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		ok, err := locks.Hold(ctx, name)
		if err != nil {
			return 0, err
		}
		if !ok {
			return 0, chainsched.ErrNotHeld
		}
		return step(ctx)
	}
}

// detectorStep is a chain's pacer step for the scheduler: StepState with the state the scheduler
// read, inside guard (the chain's lock and provider verification).
func detectorStep(guard func(func(context.Context) (int, error)) func(context.Context) (int, error), pacer *scan.Pacer) func(context.Context, scan.State) (int, error) {
	// The scheduler never runs a chain's detector twice at once, so st is not shared.
	var st scan.State
	step := guard(func(ctx context.Context) (int, error) { return pacer.StepState(ctx, st) })
	return func(ctx context.Context, s scan.State) (int, error) {
		st = s
		return step(ctx)
	}
}

// scan is the scanner configuration, defaults when the Env was built without config.Load.
func (e Root) scan() config.ScanEnv {
	if e.Scan.Intervals.Hot == 0 {
		return config.DefaultScan()
	}
	return e.Scan
}

// modeCheck is the worker loop interval of the scan pacers, idle and active.
func modeCheck(s config.ScanEnv) (idle, active time.Duration) {
	idle, active = s.ModeCheck, s.ModeCheckActive
	if idle == 0 {
		idle = scan.DefaultModeCheck
	}
	if active == 0 {
		active = scan.DefaultModeCheckActive
	}
	return idle, active
}

// senderEvery is how often a chain's gas-wallet loop looks for jobs. The sender reads the
// database first and makes requests only when a job needs one.
func senderEvery(hot time.Duration) time.Duration {
	return min(2*time.Second, hot)
}

// newPacer builds a chain's scan pacer: the mode is read from the database every ModeCheck.
func newPacer(s config.ScanEnv, st *store.Store, id string, fam chain.Family, in scan.Intervals, sc scan.Scanner, log *slog.Logger) *scan.Pacer {
	state := scan.FromStore(st, id, fam, s.PayerActive, s.LateSchedule, s.WatchAfterExpiry)
	return scan.NewPacer(scan.Config{Chain: id, Intervals: in, QuietAfter: s.HeartbeatQuietAfter, Now: st.Now, Log: log}, state, sc)
}

// ChainLoops builds the chain scheduler (chainsched): one worker loop that, every mode check
// (ModeCheck idle, ModeCheckActive while the worker is active), reads every chain's state in one query and runs the paced detectors (scan modes, late checks,
// heartbeat) and gas-wallet senders that have something due, each under its advisory lock in
// locks. Requests are made only when a mode's interval, a late check, a job or the heartbeat is
// due.
func (e Root) ChainLoops(svc *service.Service, st *store.Store, locks *store.Locker, alerts alert.Sink, log *slog.Logger) ([]worker.Loop, error) {
	sched, err := e.Scheduler(svc, st, locks, alerts, log)
	if err != nil {
		return nil, err
	}
	return []worker.Loop{sched.Loop(modeCheck(e.scan()))}, nil
}

// Scheduler builds the chain scheduler of every chain with providers (ChainLoops).
func (e Root) Scheduler(svc *service.Service, st *store.Store, locks *store.Locker, alerts alert.Sink, log *slog.Logger) (*chainsched.Scheduler, error) {
	onStatus := chainStatusSink(st, alerts, log)
	chains, err := e.evmChains(svc, st, locks, alerts, log, onStatus)
	if err != nil {
		return nil, err
	}
	sol, err := e.solanaChains(svc, st, locks, alerts, log, onStatus)
	if err != nil {
		return nil, err
	}
	chains = append(chains, sol...)
	ids := make([]string, len(chains))
	for i, c := range chains {
		ids[i] = c.ID
	}
	sc := e.scan()
	q := scan.Query(ids, sc.PayerActive, sc.LateSchedule, sc.WatchAfterExpiry)
	return chainsched.New(chainsched.Config{Chains: chains, Alerts: alerts, Log: log, Now: st.Now, StepTimeout: e.StepTimeout, Activity: e.Activity,
		State: func(ctx context.Context) (map[string]store.ChainScanState, error) {
			q.Now = st.Now()
			return st.ChainStates(ctx, q)
		}}), nil
}

// evmChains builds the scheduled work of every EVM chain with providers.
func (e Root) evmChains(svc *service.Service, st *store.Store, locks *store.Locker, alerts alert.Sink, log *slog.Logger, onStatus func(chainwatch.Status)) ([]chainsched.Chain, error) {
	quorums := e.Quorums(onStatus)
	ids := make([]string, 0, len(quorums))
	for id := range quorums {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var key *evm.PrivateKey
	var out []chainsched.Chain
	for _, id := range ids {
		c, q := e.Chains[id], quorums[id]
		reg, ok := svc.Registry().Get(id)
		if !ok {
			return nil, fmt.Errorf("chain %s is not in the registry", id)
		}
		policy := chainwatch.CreditPolicy{Confirmations: c.Confirmations, UseSafeHead: c.UseSafeHead}
		sc := e.scan()
		in := c.Scan
		if in.Hot == 0 {
			in = sc.Intervals
		}
		margin := c.ReanchorMargin
		if c.BlockTime == 0 && margin == 0 {
			margin = config.DefaultReanchorMargin // an Env built without Load
		}
		det, err := detector.New(detector.Config{Chain: reg, Gateway: c.Gateway, Policy: policy, MaxRange: c.MaxLogRange,
			Multicall: c.Multicall3, BlockTime: c.BlockTime, ReanchorMargin: margin, FinalityDelay: c.FinalityDelay,
			FinalityRetry: in.Warm, Schedule: sc.LateSchedule, WatchAfterExpiry: sc.WatchAfterExpiry}, q, svc, st, alerts, log)
		if err != nil {
			return nil, err
		}
		pacer := newPacer(sc, st, id, chain.EVM, in, det, log)
		sch := chainsched.Chain{ID: id, Due: pacer.Due, Next: pacer.Next, SenderEvery: senderEvery(in.Hot),
			Detector: detectorStep(func(step func(context.Context) (int, error)) func(context.Context) (int, error) {
				return locked(locks, "detector:"+id, verified(e.verifyPolicy(st), q, reg.EVMID, step))
			}, pacer)}
		if c.Factory == nil && c.Gateway == nil {
			out = append(out, sch)
			continue
		}
		if key == nil {
			if key, err = e.GasKey(); err != nil {
				return nil, fmt.Errorf("chain %s sends transactions but the gas wallet is not configured (PRISMAPORT_GAS_KEY_FILE): %w", id, err)
			}
		}
		var factory evm.Address
		if c.Factory != nil {
			factory = *c.Factory
		}
		snd := sender.New(sender.Config{Chain: reg, Factory: factory, MaxFeePerGas: c.MaxFeePerGas, MinGasBalance: c.MinGasBalance,
			MaxLogRange: c.MaxLogRange, FinalityDelay: c.FinalityDelay, FinalityRetry: in.Warm, Recorder: det}, q, st, key, alerts, log)
		log.Info("gas wallet", "chain", id, "address", snd.Wallet().Hex())
		sch.Sender = locked(locks, "sender:"+id, verified(e.verifyPolicy(st), q, reg.EVMID,
			func(ctx context.Context) (int, error) {
				planned := 0
				if c.Factory != nil {
					n, err := snd.Plan(ctx)
					if err != nil {
						return 0, err
					}
					planned = n
				}
				n, err := snd.Step(ctx)
				return planned + n, err
			}))
		out = append(out, sch)
	}
	return out, nil
}

// solanaChains builds the paced detector and fee-payer loops of every Solana cluster with
// providers.
func (e Root) solanaChains(svc *service.Service, st *store.Store, locks *store.Locker, alerts alert.Sink, log *slog.Logger, onStatus func(chainwatch.Status)) ([]chainsched.Chain, error) {
	quorums := e.SolanaQuorums(onStatus)
	ids := make([]string, 0, len(quorums))
	for id := range quorums {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var key *solana.Keypair
	var out []chainsched.Chain
	for _, id := range ids {
		c, q := e.Solana[id], quorums[id]
		reg, ok := svc.Registry().Get(id)
		if !ok {
			return nil, fmt.Errorf("chain %s is not in the registry", id)
		}
		sc := e.scan()
		in := c.Scan
		if in.Hot == 0 {
			in = sc.Intervals
		}
		det, err := soldetector.New(soldetector.Config{Chain: reg, Credit: c.Credit,
			Schedule: sc.LateSchedule, WatchAfterExpiry: sc.WatchAfterExpiry, FinalityDelay: c.FinalityDelay, FinalityRetry: in.Warm},
			q, svc, st, alerts, log)
		if err != nil {
			return nil, err
		}
		pacer := newPacer(sc, st, id, chain.Solana, in, det, log)
		sch := chainsched.Chain{ID: id, Due: pacer.Due, Next: pacer.Next, SenderEvery: senderEvery(in.Hot),
			Detector: detectorStep(func(step func(context.Context) (int, error)) func(context.Context) (int, error) {
				return locked(locks, "detector:"+id, genesisVerified(e.verifyPolicy(st), q, c.Genesis, step))
			}, pacer)}
		if !c.Addresses() {
			out = append(out, sch)
			continue
		}
		if key == nil {
			if key, err = e.SolanaGasKey(); err != nil {
				return nil, fmt.Errorf("chain %s flushes deposits but the fee payer is not configured (PRISMAPORT_SOLANA_GAS_KEY_FILE): %w", id, err)
			}
		}
		snd := solsender.New(solsender.Config{Chain: reg, PriorityFee: c.PriorityFee, MinGasBalance: c.MinGasBalance,
			FinalityDelay: c.FinalityDelay, FinalityRetry: in.Warm, Recorder: det}, q, st, key, alerts, log)
		log.Info("fee payer", "chain", id, "address", snd.Wallet().String())
		sch.Sender = locked(locks, "sender:"+id, genesisVerified(e.verifyPolicy(st), q, c.Genesis,
			func(ctx context.Context) (int, error) {
				planned, err := snd.Plan(ctx)
				if err != nil {
					return 0, err
				}
				n, err := snd.Step(ctx)
				return planned + n, err
			}))
		out = append(out, sch)
	}
	return out, nil
}

// chainStatusSink persists chain health and alerts when a chain halts.
func chainStatusSink(st *store.Store, alerts alert.Sink, log *slog.Logger) func(chainwatch.Status) {
	return func(s chainwatch.Status) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := st.SetChainStatus(ctx, store.ChainStatus{Chain: s.Chain, State: string(s.State), Reason: s.Reason, Since: s.Since}); err != nil {
			log.Error("chain status not saved", "chain", s.Chain, "err", err)
		}
		if s.State == chainwatch.Halted {
			alerts.Alert(ctx, alert.Alert{Severity: alert.Critical, Chain: s.Chain, Kind: "chain_halted", Message: s.Reason})
		}
	}
}
