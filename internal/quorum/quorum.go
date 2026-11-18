// Package quorum is the provider health and failover core shared by chainwatch (EVM) and
// solwatch (Solana). A Core holds the independent providers of one chain in priority order,
// cools down a provider that fails (doubling per consecutive failure, up to a cap), reports the
// chain's status (ok, degraded while a provider cools down, halted when two healthy providers
// cannot answer or disagree) and runs the request patterns both chains use: two providers asked
// in parallel (Ask), the first healthy one (First), one of the first two in turn (Rotor) and every
// healthy one (Broadcast). What counts as a provider failure, and how two answers are compared,
// stays with each chain.
package quorum

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/prismaport/prismaport/internal/safe"
)

var (
	// ErrNoQuorum means fewer than two providers are healthy: nothing may be credited.
	ErrNoQuorum = errors.New("chainwatch: fewer than two healthy providers")
	// ErrDisagreement means two healthy providers returned different chain data.
	ErrDisagreement = errors.New("chainwatch: providers disagree")
)

// State is a chain's health as the quorum sees it.
type State string

// The chain health states, from healthy to stopped.
const (
	OK       State = "ok"
	Degraded State = "degraded"
	Halted   State = "halted"
)

// Status is the chain's health as the quorum last saw it.
type Status struct {
	Chain  string
	State  State
	Reason string
	Since  time.Time
}

// Options tune how the quorum backs off failing providers and when it reports a chain degraded or
// halted.
type Options struct {
	// Cooldown is the first pause after a provider failure; it doubles per consecutive
	// failure up to MaxCooldown.
	Cooldown    time.Duration
	MaxCooldown time.Duration
	Now         func() time.Time
	// OnStatus is called whenever the chain's state or reason changes.
	OnStatus func(Status)
}

// Provider cooldown defaults (Options.Cooldown, Options.MaxCooldown). MaxCooldownShift caps the
// doubling exponent, so the shift cannot overflow a Duration before MaxCooldown applies.
const (
	DefaultCooldown    = 15 * time.Second
	DefaultMaxCooldown = 5 * time.Minute
	MaxCooldownShift   = 16
)

// Provider is the part of a chain's RPC client the core needs.
type Provider interface {
	Name() string
}

// Classify sorts a chain's provider errors.
type Classify struct {
	// Transient reports a transport failure (the provider's fault). First and Broadcast cool the
	// provider down on it and First fails over; any other error is returned as the answer.
	Transient func(error) bool
	// Failed reports an error that is not an answer about the chain. Ask and Rotate cool the
	// provider down on it and fail over; anything else is an answer, compared or returned.
	Failed func(error) bool
	// Behind, when set, reports a provider that has not caught up with the request: Rotate asks
	// the next provider instead, without cooling this one down.
	Behind func(error) bool
}

// Member is one provider with its health.
type Member[P Provider] struct {
	P        P
	until    time.Time
	failures int
	lastErr  string
}

// Core is the provider bookkeeping a chain's quorum shares: cooldowns, health state and its
// changes.
type Core[P Provider] struct {
	chain       string
	cls         Classify
	mu          sync.Mutex
	members     []*Member[P]
	cooldown    time.Duration
	maxCooldown time.Duration
	now         func() time.Time
	status      Status
	onChange    func(Status)
	// seq numbers the status changes (under mu). cbMu runs one onChange at a time and delivered
	// is the newest change it ran, so a change that lost the race to a later one is dropped
	// instead of being persisted after it.
	seq       uint64
	cbMu      sync.Mutex
	delivered uint64
}

// New takes the providers in priority order.
func New[P Provider](chain string, providers []P, cls Classify, o Options) *Core[P] {
	if o.Cooldown == 0 {
		o.Cooldown = DefaultCooldown
	}
	if o.MaxCooldown == 0 {
		o.MaxCooldown = DefaultMaxCooldown
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	c := &Core[P]{chain: chain, cls: cls, cooldown: o.Cooldown, maxCooldown: o.MaxCooldown, now: o.Now, onChange: o.OnStatus}
	for _, p := range providers {
		c.members = append(c.members, &Member[P]{P: p})
	}
	c.status = Status{Chain: chain, State: OK, Since: o.Now()}
	return c
}

// Chain returns the ID of the chain the quorum serves.
func (c *Core[P]) Chain() string { return c.chain }

// Status returns the chain's health as last seen.
func (c *Core[P]) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

// Members is every configured provider in priority order, healthy or not. The list is fixed
// at New.
func (c *Core[P]) Members() []*Member[P] { return c.members }

// SetStatus records the chain's state; a change is passed to OnStatus, in order: a change that
// lost the race to a later one is never delivered after it.
func (c *Core[P]) SetStatus(s State, reason string) {
	c.mu.Lock()
	changed := c.status.State != s || c.status.Reason != reason
	if changed {
		c.seq++
		c.status = Status{Chain: c.chain, State: s, Reason: reason, Since: c.now()}
	}
	st, cb, seq := c.status, c.onChange, c.seq
	c.mu.Unlock()
	if !changed || cb == nil {
		return
	}
	c.cbMu.Lock()
	defer c.cbMu.Unlock()
	if seq <= c.delivered {
		return // a later status was delivered already
	}
	c.delivered = seq
	cb(st)
}

// Healthy returns members not in cooldown, in priority order, excluding skip.
func (c *Core[P]) Healthy(skip map[*Member[P]]bool) []*Member[P] {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	var out []*Member[P]
	for _, m := range c.members {
		if !skip[m] && !now.Before(m.until) {
			out = append(out, m)
		}
	}
	return out
}

// Fail cools m down: Cooldown doubled per consecutive failure, at most MaxCooldown.
func (c *Core[P]) Fail(m *Member[P], err error) {
	c.mu.Lock()
	m.failures++
	d := c.cooldown << min(m.failures-1, MaxCooldownShift)
	if d > c.maxCooldown || d <= 0 {
		d = c.maxCooldown
	}
	m.until = c.now().Add(d)
	m.lastErr = err.Error()
	c.mu.Unlock()
}

// Succeed clears m's failures.
func (c *Core[P]) Succeed(m *Member[P]) {
	c.mu.Lock()
	m.failures, m.until, m.lastErr = 0, time.Time{}, ""
	c.mu.Unlock()
}

// AfterSuccess reports the chain as degraded while any provider is cooling down, ok otherwise.
func (c *Core[P]) AfterSuccess() {
	c.mu.Lock()
	now := c.now()
	cooling := ""
	for _, m := range c.members {
		if now.Before(m.until) {
			cooling = m.P.Name() + ": " + m.lastErr
			break
		}
	}
	c.mu.Unlock()
	if cooling != "" {
		c.SetStatus(Degraded, "provider in cooldown: "+cooling)
		return
	}
	c.SetStatus(OK, "")
}

// AfterProbe ends an idle heartbeat: the status shows a provider cooling down (AfterSuccess),
// but a halted chain stays halted until an agreed read.
func (c *Core[P]) AfterProbe() {
	c.mu.Lock()
	halted := c.status.State == Halted
	c.mu.Unlock()
	if !halted {
		c.AfterSuccess()
	}
}

// Disagree halts the chain on two providers' different answers to what.
func (c *Core[P]) Disagree(what string, a, b *Member[P]) error {
	c.SetStatus(Halted, fmt.Sprintf("%s: %s and %s differ", what, a.P.Name(), b.P.Name()))
	return fmt.Errorf("%s on %s: %w (%s vs %s)", what, c.chain, ErrDisagreement, a.P.Name(), b.P.Name())
}

// Reply is one provider's answer.
type Reply[P Provider, T any] struct {
	M   *Member[P]
	V   T
	Err error
}

// Ask gets answers from two healthy providers, asked in parallel, failing over on provider
// failures (Classify.Failed). Fewer than two healthy providers halt the chain (ErrNoQuorum).
func Ask[P Provider, T any](ctx context.Context, c *Core[P], what string, f func(P) (T, error)) ([2]Reply[P, T], error) {
	var out [2]Reply[P, T]
	tried := map[*Member[P]]bool{}
	n := 0
	for n < 2 {
		cands := c.Healthy(tried)
		need := 2 - n
		if len(cands) < need {
			c.SetStatus(Halted, fmt.Sprintf("%s: %v", what, ErrNoQuorum))
			return out, fmt.Errorf("%s on %s: %w", what, c.chain, ErrNoQuorum)
		}
		cands = cands[:need]
		res := make([]Reply[P, T], len(cands))
		var wg sync.WaitGroup
		for i, m := range cands {
			tried[m] = true
			wg.Add(1)
			go func() {
				defer wg.Done()
				// A provider whose answer makes the client panic counts as a failed provider.
				var v T
				err := safe.Call(func() (err error) { v, err = f(m.P); return err })
				res[i] = Reply[P, T]{M: m, V: v, Err: err}
			}()
		}
		wg.Wait()
		if err := ctx.Err(); err != nil {
			return out, err
		}
		for _, r := range res {
			if c.cls.Failed(r.Err) {
				c.Fail(r.M, r.Err)
				continue
			}
			c.Succeed(r.M)
			out[n] = r
			n++
		}
	}
	return out, nil
}

// First runs f on the first healthy provider, failing over on transport errors
// (Classify.Transient). It is for requests whose answer is not credited.
func First[P Provider, T any](ctx context.Context, c *Core[P], f func(P) (T, error)) (T, error) {
	var zero T
	tried := map[*Member[P]]bool{}
	for {
		cands := c.Healthy(tried)
		if len(cands) == 0 {
			return zero, fmt.Errorf("%s: %w", c.chain, ErrNoQuorum)
		}
		m := cands[0]
		tried[m] = true
		v, err := f(m.P)
		if err != nil && c.cls.Transient(err) {
			c.Fail(m, err)
			if ctx.Err() != nil {
				return zero, ctx.Err()
			}
			continue
		}
		c.Succeed(m)
		return v, err
	}
}

// Broadcast sends to every healthy provider. accepted reports a provider that took the
// transaction (or already had it); a transport error cools the provider down. The errors of
// the others are returned for logging.
func (c *Core[P]) Broadcast(send func(P) error, accepted func(error) bool) (n int, errs []error) {
	for _, m := range c.Healthy(nil) {
		err := send(m.P)
		switch {
		case accepted(err):
			c.Succeed(m)
			n++
		case c.cls.Transient(err):
			c.Fail(m, err)
			errs = append(errs, fmt.Errorf("%s: %w", m.P.Name(), err))
		default:
			c.Succeed(m)
			errs = append(errs, fmt.Errorf("%s: %w", m.P.Name(), err))
		}
	}
	return n, errs
}

// Rotor sends single-provider reads (D49) to the first two healthy providers in turn: each
// call starts at the other one than the call before, and fails over to the next healthy
// provider when that one errs.
type Rotor[P Provider] struct {
	c    *Core[P]
	mu   sync.Mutex
	next int
}

// NewRotor returns a new round-robin reader over c's providers.
func NewRotor[P Provider](c *Core[P]) *Rotor[P] { return &Rotor[P]{c: c} }

// Core is the quorum the rotor reads from.
func (r *Rotor[P]) Core() *Core[P] { return r.c }

// Rotate runs f on one provider: the next of the first two healthy ones, failing over on a
// provider failure (Classify.Failed, cooled down) or a provider behind the request
// (Classify.Behind, not cooled down). It returns the name of the provider that answered.
func Rotate[P Provider, T any](ctx context.Context, r *Rotor[P], f func(P) (T, error)) (T, string, error) {
	var zero T
	c := r.c
	cands := c.Healthy(nil)
	if len(cands) == 0 {
		return zero, "", fmt.Errorf("%s: %w", c.chain, ErrNoQuorum)
	}
	r.mu.Lock()
	start := r.next % min(2, len(cands))
	r.next++
	r.mu.Unlock()
	order := append([]*Member[P]{cands[start]}, slices.Delete(slices.Clone(cands), start, start+1)...)
	var last error
	for _, m := range order {
		v, err := f(m.P)
		if c.cls.Behind != nil && c.cls.Behind(err) {
			last = err
			continue
		}
		if c.cls.Failed(err) {
			c.Fail(m, err)
			last = err
			if ctx.Err() != nil {
				return zero, "", ctx.Err()
			}
			continue
		}
		c.Succeed(m)
		return v, m.P.Name(), err
	}
	return zero, "", fmt.Errorf("%s: every provider failed: %w", c.chain, last)
}
