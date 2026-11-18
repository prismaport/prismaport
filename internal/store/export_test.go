package store

import "time"

var LockKey = lockKey

// The flush checks' SQL fragments, for the whole-history oracle in flushscope_integration_test.go.
const (
	SettledExpr  = settledExpr
	WatchFrom    = watchFrom
	RevealedCond = revealedCond
	OwnerCond    = ownerCond
	LateHorizon  = lateHorizon
	FlushHorizon = flushHorizon
)

var (
	WalletScanCond    = walletScanCond
	FlushHistoryEvery = flushHistoryEvery
)

// Flush check scopes for SetFlushScope.
const (
	FlushScopeAuto    = 0
	FlushScopeHistory = 1
	FlushScopeHot     = 2
)

// SetFlushScope forces the flush checks of s (and every copy of it) to read the whole history,
// only the hot scope, or to decide by time again (FlushScopeAuto, forgetting earlier reads).
func SetFlushScope(s *Store, scope int) {
	s.flush.mu.Lock()
	defer s.flush.mu.Unlock()
	s.flush.scope = scope
	if scope == FlushScopeAuto {
		s.flush.last = map[string]time.Time{}
	}
}

// The queries the audit's indexes serve (migrations 00101-00106), for the plan checks in
// indexes_integration_test.go.
const (
	MaxSenderNonceSQL      = maxSenderNonceSQL
	OwnFlushJobSQL         = ownFlushJobSQL
	FlushSiblingSQL        = flushSiblingSQL
	OtherSessionFlushesSQL = otherSessionFlushesSQL
	OwnTxHashesSQL         = ownTxHashesSQL
	DeliveriesForEventSQL  = deliveriesForEventSQL
	ListDeliveriesSQL      = listDeliveriesSQL
	RecentPaymentsSQL      = recentPaymentsSQL
)
