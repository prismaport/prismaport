package scan

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/rpcstat"
	"github.com/prismaport/prismaport/internal/store"
	"github.com/prismaport/prismaport/internal/worker"
)

// State is what the database says is pending on a chain at one moment.
type State struct {
	// PayerActive: an open session of the chain whose checkout page was seen within PayerActive.
	PayerActive bool
	// Detected: a payment detected and not yet confirmed.
	Detected bool
	// Submitted: a sender job submitted and not yet mined.
	Submitted bool
	// Open: a session of the chain is open (open/partially_paid, not expired).
	Open bool
	// Confirmed: a payment confirmed and not yet finalized.
	Confirmed bool
	// Mined: a sender job mined and waiting for finality.
	Mined bool
	// LateDue is the earliest due late check (nil: no expired session is inside its watch).
	LateDue *time.Time
	// Recheck: a "Check again" request waits.
	Recheck bool
	// LastReveal is the latest reveal of a session on the chain (nil: never). The idle
	// heartbeat is skipped once it is older than Config.QuietAfter.
	LastReveal *time.Time
}

// Select is the fastest mode the state needs and the reason for it.
func Select(s State) (Mode, string) {
	switch {
	case s.PayerActive:
		return Hot, "payer_active"
	case s.Detected:
		return Hot, "payment_detected"
	case s.Submitted:
		return Hot, "job_submitted"
	case s.Open:
		return Warm, "session_open"
	case s.Confirmed:
		return Warm, "payment_confirmed"
	case s.Mined:
		return Warm, "job_mined"
	case s.Recheck:
		return Late, "recheck"
	case s.LateDue != nil:
		return Late, "late_checks"
	}
	return Idle, "nothing_pending"
}

// lateDue reports whether a late pass has work now.
func (s State) lateDue(now time.Time) bool {
	return s.Recheck || (s.LateDue != nil && !s.LateDue.After(now))
}

// Tick tells a scanner what one scan step is for.
type Tick struct {
	Mode Mode
	// Full is set when the warm interval has passed since the last full pass: Solana then reads
	// every open deposit account's balance (the warm trigger) even while the chain is hot.
	Full bool
}

// Scanner is one chain's RPC work.
type Scanner interface {
	// Scan reads new chain data and moves pending payments along. more is true when it stopped
	// short of the head (a range cap) and should run again at once.
	Scan(ctx context.Context, t Tick) (more bool, err error)
	// Late runs the due late checks and "Check again" requests. more is true when a batch was
	// full.
	Late(ctx context.Context) (more bool, err error)
	// Heartbeat is the idle probe: one cheap read per provider so failures still surface.
	Heartbeat(ctx context.Context) error
	// Rest is called (database only) when the chain leaves the scanning modes.
	Rest(ctx context.Context) error
}

// StateFunc reads a chain's state.
type StateFunc func(ctx context.Context) (State, error)

// Config holds one chain's pacer settings; zero values get defaults in NewPacer.
type Config struct {
	Chain     string
	Intervals Intervals
	// StatsEvery is how often the RPC call counters are logged (default 10 minutes).
	StatsEvery time.Duration
	// QuietAfter skips the idle heartbeat of a chain with no session revealed for that long
	// (0: always beat). The first real tick still surfaces a provider failure.
	QuietAfter time.Duration
	Now        func() time.Time
	Log        *slog.Logger
}

// Pacer runs a chain's scanner at the speed its state needs. Step is meant to be called every
// ModeCheck by a worker loop; it returns 1 when the scanner asked to run again at once.
type Pacer struct {
	cfg   Config
	state StateFunc
	sc    Scanner

	mode      Mode
	reason    string
	lastScan  time.Time
	lastFull  time.Time
	lastProbe time.Time // last RPC of any kind that succeeded, or the last heartbeat attempt
	probeFail bool
	backlog   bool
	force     bool
	// lateRetry holds late passes back after one failed, until the warm interval has passed.
	lateRetry time.Time

	calls     [Hot + 1]atomic.Int64
	statsFrom time.Time
}

// NewPacer returns a pacer that runs sc at the interval matching the mode state reports.
func NewPacer(cfg Config, state StateFunc, sc Scanner) *Pacer {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if cfg.StatsEvery == 0 {
		cfg.StatsEvery = 10 * time.Minute
	}
	return &Pacer{cfg: cfg, state: state, sc: sc, mode: Unknown}
}

// Mode is the mode of the last evaluation.
func (p *Pacer) Mode() Mode { return p.mode }

func (p *Pacer) count(ctx context.Context, m Mode) context.Context {
	return rpcstat.With(ctx, &p.calls[m])
}

// Step reads the chain's state and runs StepState with it.
func (p *Pacer) Step(ctx context.Context) (int, error) {
	st, err := p.state(ctx)
	if err != nil {
		return 0, err
	}
	return p.StepState(ctx, st)
}

// scanDue reports whether a scan of mode is due now.
func (p *Pacer) scanDue(mode Mode, now time.Time) bool {
	return mode.Scanning() && (p.force || p.backlog || !now.Before(p.lastScan.Add(p.cfg.Intervals.of(mode))))
}

// heartbeatDue reports whether the idle probe is due now: an interval after the last request
// (the warm interval after a failed probe), unless the chain is quiet.
func (p *Pacer) heartbeatDue(st State, now time.Time) bool {
	if p.cfg.QuietAfter > 0 && (st.LastReveal == nil || !now.Before(st.LastReveal.Add(p.cfg.QuietAfter))) {
		return false
	}
	wait := p.cfg.Intervals.Heartbeat
	if p.probeFail {
		wait = p.cfg.Intervals.Warm
	}
	return p.lastProbe.IsZero() || !now.Before(p.lastProbe.Add(wait))
}

// Due reports whether StepState with st would do anything now: change the mode, scan, run late
// checks or probe an idle chain. It reads nothing, so a scheduler can ask it of every chain every
// ModeCheck and take a chain's lock only when it is due.
func (p *Pacer) Due(st State, now time.Time) bool {
	mode, _ := Select(st)
	if mode != p.mode || p.scanDue(mode, now) || (st.lateDue(now) && !now.Before(p.lateRetry)) {
		return true
	}
	return !mode.Scanning() && p.heartbeatDue(st, now)
}

// Next is when StepState with st would next have RPC work, for a scheduler that evaluates less
// often than the scan intervals (worker plan B, idle mode check): the next scan of a scanning
// mode or heartbeat of a resting one, the earliest late check and a waiting "Check again". Like
// Due it reads nothing.
func (p *Pacer) Next(st State, now time.Time) time.Time {
	var next time.Time
	take := func(t time.Time) {
		if next.IsZero() || t.Before(next) {
			next = t
		}
	}
	mode, _ := Select(st)
	switch {
	case mode != p.mode || mode.Scanning() && (p.force || p.backlog):
		take(now)
	case mode.Scanning():
		take(p.lastScan.Add(p.cfg.Intervals.of(mode)))
	case p.cfg.QuietAfter > 0 && (st.LastReveal == nil || !now.Before(st.LastReveal.Add(p.cfg.QuietAfter))):
	case p.lastProbe.IsZero():
		take(now)
	case p.probeFail:
		take(p.lastProbe.Add(p.cfg.Intervals.Warm))
	default:
		take(p.lastProbe.Add(p.cfg.Intervals.Heartbeat))
	}
	if st.Recheck {
		take(maxTime(now, p.lateRetry))
	}
	if st.LateDue != nil {
		take(maxTime(*st.LateDue, p.lateRetry))
	}
	return next
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// StepState evaluates the mode from st and runs whatever RPC work is due. A step that ran nothing
// returns worker.ErrSkipped, so it neither clears nor extends a failing streak.
func (p *Pacer) StepState(ctx context.Context, st State) (int, error) {
	now := p.cfg.Now()
	p.stats(now)
	mode, reason := Select(st)
	if mode != p.mode {
		if p.mode != Unknown || mode != Idle {
			p.cfg.Log.Info("scan mode", "chain", p.cfg.Chain, "from", p.mode.String(), "to", mode.String(), "reason", reason)
		}
		if !mode.Scanning() && (p.mode == Unknown || p.mode.Scanning()) {
			if err := p.sc.Rest(ctx); err != nil {
				return 0, err
			}
		}
		if mode > p.mode {
			p.force = true // woken (or sped up): scan now, not at the next interval
		}
		p.mode, p.reason = mode, reason
	}
	ran := false
	more := false
	var errs []error
	if p.scanDue(mode, now) {
		tick := Tick{Mode: mode, Full: mode == Warm || !now.Before(p.lastFull.Add(p.cfg.Intervals.Warm))}
		p.force, p.lastScan = false, now
		if tick.Full {
			p.lastFull = now
		}
		m, err := p.sc.Scan(p.count(ctx, mode), tick)
		ran, p.backlog = true, m && err == nil
		more = more || p.backlog
		if err != nil {
			errs = append(errs, err)
		} else {
			p.lastProbe, p.probeFail = now, false
		}
	}
	if st.lateDue(now) && !now.Before(p.lateRetry) {
		m, err := p.sc.Late(p.count(ctx, Late))
		ran = true
		more = more || (m && err == nil)
		if err != nil {
			errs = append(errs, err)
			p.lateRetry = now.Add(p.cfg.Intervals.Warm)
		} else {
			p.lastProbe, p.probeFail = now, false
		}
	}
	// After a failed probe retry at the warm interval, so a provider outage on an idle chain still
	// turns into loop_failing within minutes.
	if !mode.Scanning() && !ran && p.heartbeatDue(st, now) {
		err := p.sc.Heartbeat(p.count(ctx, mode))
		ran, p.lastProbe, p.probeFail = true, now, err != nil
		if err != nil {
			errs = append(errs, err)
		}
	}
	if !ran {
		return 0, worker.ErrSkipped
	}
	if more {
		return 1, errors.Join(errs...)
	}
	return 0, errors.Join(errs...)
}

// stats logs the RPC calls per mode every StatsEvery and resets the counters.
func (p *Pacer) stats(now time.Time) {
	if p.statsFrom.IsZero() {
		p.statsFrom = now
		return
	}
	if now.Sub(p.statsFrom) < p.cfg.StatsEvery {
		return
	}
	p.cfg.Log.Info("scan rpc calls", "chain", p.cfg.Chain, "mode", p.mode.String(), "since", p.statsFrom.UTC().Format(time.RFC3339),
		"hot", p.calls[Hot].Swap(0), "warm", p.calls[Warm].Swap(0), "late", p.calls[Late].Swap(0), "idle", p.calls[Idle].Swap(0))
	p.statsFrom = now
}

// FromStore reads a chain's state from the database: payer presence within payerActive, open
// sessions, pending payments and jobs, due late checks (schedule and watch after expiry) and
// "Check again" requests.
func FromStore(st *store.Store, chainID string, _ chain.Family, payerActive time.Duration, sch Schedule, watch time.Duration) StateFunc {
	q := Query(nil, payerActive, sch, watch)
	return func(ctx context.Context) (State, error) {
		q.Chains, q.Now = []string{chainID}, st.Now()
		all, err := st.ChainStates(ctx, q)
		return FromChainState(all[chainID]), err
	}
}

// Query is the ChainStatesQuery of the chains under the global scan settings (Now is set by the
// caller at each read).
func Query(chains []string, payerActive time.Duration, sch Schedule, watch time.Duration) store.ChainStatesQuery {
	if len(sch) == 0 {
		watch = 0
	}
	// A Solana reference key is scanned past expiry (D49).
	return store.ChainStatesQuery{Chains: chains, PayerActive: payerActive, LateFirst: sch.First(), LateWatch: watch,
		SolanaWalletWindow: store.SolanaWalletWindow}
}

// FromChainState is the pacer's view of a chain's database state.
func FromChainState(cs store.ChainScanState) State {
	return State{PayerActive: cs.PayerActive, Detected: cs.Detected, Submitted: cs.Submitted, Open: cs.Open,
		Confirmed: cs.Confirmed, Mined: cs.Mined, LateDue: cs.LateDue, Recheck: cs.Recheck, LastReveal: cs.LastReveal}
}
