package store

import (
	"sync"
	"time"
)

// flushHorizon is how long after its expiry a session stays in the flush checks' hot scope: a
// session closes (paid, or expired holding funds) around its expiry, and its deposits are final
// within minutes of that. SQL interval literal.
const flushHorizon = "1 day"

// flushHistoryEvery is how often each chain's flush checks still read the whole payment history.
// The hot scope misses a session only if it needs a flush and has been outside it since: a
// flush the sender skipped (unknown token, address mismatch), a sender down for longer than
// flushHorizon, a review cleared long after expiry. Those are found within about twice this.
const flushHistoryEvery = 5 * time.Minute

// flushScope bounds a flush check's payments p to the sessions that can still need a flush
// (ADR-0008 §F-1): sessions whose expiry is within flushHorizon of now (index
// sessions_expires_at) and sessions with a payment on the chains still moving to finality
// (index payments_pending). Every payment of a session in scope is read, so its totals are the
// same as a whole-history read. chains is the chain predicate's right side ("= ANY($1)"), now
// the placeholder of the current time. An empty result reads the whole history.
func flushScope(full bool, chains, now string) string {
	if full {
		return ""
	}
	return ` AND p.session_id IN (SELECT fs.id FROM sessions fs WHERE fs.expires_at > ` + now + `::timestamptz - interval '` + flushHorizon + `'
		UNION SELECT fy.session_id FROM payments fy WHERE fy.chain ` + chains + ` AND fy.status IN ('detected', 'confirmed'))`
}

// flushPasses decides, per query and chain, when the flush checks read the whole history
// (flushHistoryEvery) instead of the hot scope. It is shared by every copy of a Store.
type flushPasses struct {
	mu   sync.Mutex
	last map[string]time.Time
	// scope forces a mode (tests): 0 decides by time, 1 always reads the whole history, 2 never.
	scope int
}

func newFlushPasses() *flushPasses { return &flushPasses{last: map[string]time.Time{}} }

// due reports whether query's next read of chains covers the whole history: the first read of
// a chain, and one every flushHistoryEvery (or after the clock went back).
func (f *flushPasses) due(query string, chains []string, now time.Time) bool {
	if f == nil {
		return true
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch f.scope {
	case 1:
		return true
	case 2:
		return false
	}
	for _, c := range chains {
		last, ok := f.last[query+"|"+c]
		if !ok || now.Before(last) || now.Sub(last) >= flushHistoryEvery {
			return true
		}
	}
	return false
}

// done records a whole-history read of chains at now.
func (f *flushPasses) done(query string, chains []string, now time.Time) {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range chains {
		f.last[query+"|"+c] = now
	}
}
