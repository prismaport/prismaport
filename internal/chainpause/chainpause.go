// Package chainpause holds the runtime pause of a chain, or of one asset on a chain (D64): the
// "enabled" layer on top of the configured registry (D62). A pause takes the chain or asset out
// of everything new (sessions, payment methods, customer addresses, reveals and wallet payments)
// while the worker keeps watching, crediting and flushing it, so money already sent still lands.
// The pauses live in the database (chain_pauses); Reader keeps a copy that it reads again at most
// every refresh interval, so a pause or a resume reaches every api and worker process within it.
package chainpause

import (
	"context"
	"sync"
	"time"
)

// DefaultRefresh is how long a Reader serves its copy before reading the pauses again.
const DefaultRefresh = 30 * time.Second

// Pause is one switch: a whole chain (Asset empty) or one asset on it.
type Pause struct {
	Chain    string
	Asset    string
	PausedAt time.Time
	PausedBy string
}

// Set is the pauses in force at one moment. The zero Set pauses nothing.
type Set struct {
	list []Pause
}

// NewSet returns the set of the given pauses.
func NewSet(list ...Pause) Set { return Set{list: append([]Pause(nil), list...)} }

// Paused reports whether asset on chainID takes nothing new: the chain or that asset is paused.
func (s Set) Paused(chainID, asset string) bool {
	for _, p := range s.list {
		if p.Chain == chainID && (p.Asset == "" || p.Asset == asset) {
			return true
		}
	}
	return false
}

// ChainPaused reports whether the whole chain is paused (a pause of one of its assets is not).
func (s Set) ChainPaused(chainID string) bool {
	for _, p := range s.list {
		if p.Chain == chainID && p.Asset == "" {
			return true
		}
	}
	return false
}

// Empty reports whether nothing is paused.
func (s Set) Empty() bool { return len(s.list) == 0 }

// List returns the pauses, oldest first as the source answered them.
func (s Set) List() []Pause { return append([]Pause(nil), s.list...) }

// Source reads every pause in force (the store's ChainPauses).
type Source func(ctx context.Context) ([]Pause, error)

// Reader serves the pauses from a copy read at most refresh ago. A failed read keeps serving the
// last copy (and tries again on the next call); only a reader that never read answers the error.
type Reader struct {
	src     Source
	refresh time.Duration
	now     func() time.Time
	// Stale, when set, hears about a failed read whose caller got the last copy instead.
	Stale func(err error)

	mu     sync.Mutex
	set    Set
	read   time.Time
	loaded bool
}

// NewReader reads src through a copy kept for refresh (0 reads on every call). now is the clock
// the age is measured with.
func NewReader(src Source, refresh time.Duration, now func() time.Time) *Reader {
	if now == nil {
		now = time.Now
	}
	return &Reader{src: src, refresh: refresh, now: now}
}

// Set returns the pauses in force, reading them again once the copy is older than refresh. A
// nil Reader pauses nothing.
func (r *Reader) Set(ctx context.Context) (Set, error) {
	if r == nil {
		return Set{}, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	if r.loaded && r.refresh > 0 && now.Sub(r.read) < r.refresh && !now.Before(r.read) {
		return r.set, nil
	}
	list, err := r.src(ctx)
	if err != nil {
		if r.loaded {
			if r.Stale != nil {
				r.Stale(err)
			}
			return r.set, nil
		}
		return Set{}, err
	}
	r.set, r.read, r.loaded = NewSet(list...), now, true
	return r.set, nil
}
