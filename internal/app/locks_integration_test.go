//go:build integration

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prismaport/prismaport/internal/alert"
	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/chainsched"
	"github.com/prismaport/prismaport/internal/config"
	"github.com/prismaport/prismaport/internal/evm"
	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/scan"
	"github.com/prismaport/prismaport/internal/service"
	"github.com/prismaport/prismaport/internal/solana"
	"github.com/prismaport/prismaport/internal/solrpc"
	"github.com/prismaport/prismaport/internal/store"
	"github.com/prismaport/prismaport/internal/testdb"
	"github.com/prismaport/prismaport/internal/testutil"
	"github.com/prismaport/prismaport/internal/worker"
)

// lockerLocks lists the advisory locks held by lock connections; the sequencer's transaction
// locks are advisory too and are left out.
const lockerLocks = `SELECT l.pid FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
	WHERE l.locktype = 'advisory' AND a.datname = current_database() AND a.application_name = '` + store.LockerApplicationName + `'`

func newLocker(t *testing.T, url string) *store.Locker {
	t.Helper()
	l, err := store.NewLocker(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Close)
	return l
}

func TestLockedSkipsTheStepOnceTheLockConnectionDies(t *testing.T) {
	url := testdb.Empty(t)
	ctx := t.Context()
	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	mine, other := newLocker(t, url), newLocker(t, url)
	ran := 0
	step := locked(mine, "sender:base", func(context.Context) (int, error) { ran++; return 0, nil })
	if _, err := step(ctx); err != nil || ran != 1 {
		t.Fatalf("step with a free lock: ran %d, %v", ran, err)
	}

	var killed bool
	if err := admin.QueryRow(ctx, `SELECT pg_terminate_backend(pid, 5000) FROM (`+lockerLocks+`) l`).Scan(&killed); err != nil || !killed {
		t.Fatalf("terminate the lock backend: %v %v", killed, err)
	}
	_, lostErr := step(ctx)
	if ok, err := other.Hold(ctx, "sender:base"); err != nil || !ok {
		t.Fatalf("second worker could not take the freed lock: %v %v", ok, err)
	}
	_, otherErr := step(ctx)

	if lostErr == nil || !errors.Is(otherErr, chainsched.ErrNotHeld) || ran != 1 {
		t.Fatalf("step ran without the lock: ran %d, lost %v, other %v", ran, lostErr, otherErr)
	}
	other.Close()
	if _, err := step(ctx); err != nil || ran != 2 {
		t.Fatalf("step did not resume once the lock was free again: ran %d, %v", ran, err)
	}
}

// rpcDown answers every JSON-RPC call with an error and counts calls per chain (the first path
// segment).
type rpcDown struct {
	mu    sync.Mutex
	calls map[string]int
}

func (r *rpcDown) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	chain, _, _ := strings.Cut(strings.TrimPrefix(req.URL.Path, "/"), "/")
	r.mu.Lock()
	r.calls[chain]++
	r.mu.Unlock()
	http.Error(w, "down", http.StatusServiceUnavailable)
}

func (r *rpcDown) count(chain string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[chain]
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	testutil.Eventually(t, 20*time.Second, what, cond)
}

func count(t *testing.T, pool *pgxpool.Pool, sql string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(t.Context(), sql).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestChainLoopsOfFourChainsRunOnAFourConnectionPool runs the real worker with the chain
// scheduler of all four pilot chains (a paced detector and a sender per chain, each under its
// lock when it runs) on a pool of four connections: the locks must not starve the queries, so
// every chain keeps running and events keep being sequenced.
func TestChainLoopsOfFourChainsRunOnAFourConnectionPool(t *testing.T) {
	url := testdb.Migrated(t) + "&pool_max_conns=4"
	app := apptest.New(t, apptest.Options{DatabaseURL: url})
	pool := app.Store.Pool()
	if pool.Config().MaxConns != 4 {
		t.Fatalf("pool has %d connections", pool.Config().MaxConns)
	}
	var everyTick time.Duration // the providers are down: verify on every tick
	rpc := &rpcDown{calls: map[string]int{}}
	srv := httptest.NewServer(rpc)
	defer srv.Close()
	urls := func(id string) []string { return []string{srv.URL + "/" + id + "/1", srv.URL + "/" + id + "/2"} }
	contract, err := evm.ParseAddress("0x1111111111111111111111111111111111111111")
	if err != nil {
		t.Fatal(err)
	}
	program := solana.MustPublicKey("DEb5yphxEaPc5BN118svVN4R3GFu9jKs31Gcv5yekjZx")
	sch, _ := scan.ParseSchedule(scan.DefaultLateSchedule)
	fast := scan.Intervals{Hot: 20 * time.Millisecond, Warm: 20 * time.Millisecond, Heartbeat: 20 * time.Millisecond}
	env := config.Env{
		// The providers are down: every chain retries its heartbeat (or scan) at the warm
		// interval, so each keeps making requests.
		Scan: config.ScanEnv{Intervals: fast, ModeCheck: 20 * time.Millisecond, PayerActive: 30 * time.Second, LateSchedule: sch,
			WatchAfterExpiry: 23 * time.Hour},
		Chains: map[string]config.ChainEnv{
			"base":         {ID: "base", RPCURLs: urls("base"), Factory: &contract, Gateway: &contract, Confirmations: 3},
			"base-sepolia": {ID: "base-sepolia", RPCURLs: urls("base-sepolia"), Factory: &contract, Gateway: &contract, Confirmations: 3},
		},
		Solana: map[string]config.SolanaEnv{
			"solana": {ID: "solana", RPCURLs: urls("solana"), Program: &program, RentReceiver: apptest.SolanaTreasury,
				Genesis: "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d", Credit: solrpc.Confirmed},
			"solana-devnet": {ID: "solana-devnet", RPCURLs: urls("solana-devnet"), Program: &program, RentReceiver: apptest.SolanaTreasury,
				Genesis: "EtWTRABZaYq6iMfeYKouRu166VU2xqa1wcaWoxPkrZBG", Credit: solrpc.Confirmed},
		},
	}
	t.Setenv("PRISMAPORT_GAS_KEY", "0x"+strings.Repeat("11", 32))
	feePayer, err := solana.KeypairFromSeed(bytes.Repeat([]byte{3}, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PRISMAPORT_SOLANA_GAS_KEY", feePayer.JSON())
	locks := newLocker(t, url)
	log := slog.New(slog.DiscardHandler)
	loops, err := Root{Env: env, VerifyRetry: &everyTick}.ChainLoops(app.Svc, app.Store, locks, alert.Log{Logger: log}, log)
	if err != nil {
		t.Fatal(err)
	}
	if len(loops) != 1 || loops[0].Name != "chains" {
		t.Fatalf("chain loops %+v, want the scheduler alone", loops)
	}
	for i := range loops {
		loops[i].Every = 20 * time.Millisecond
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		worker.New(app.Svc, app.Store, app.Dispatcher, worker.Config{SequenceEvery: 20 * time.Millisecond, DispatchEvery: 20 * time.Millisecond,
			ExpireEvery: 20 * time.Millisecond, Loops: loops}, log).Run(ctx)
		close(done)
	}()
	defer func() { cancel(); <-done }()

	// Every detector is due (its providers fail verification, retried each tick); no sender has a
	// job, so no sender runs or takes its lock.
	eventually(t, "every detector lock to be held", func() bool {
		return count(t, pool, `SELECT count(*) FROM (`+lockerLocks+`) l`) == 4
	})
	before := map[string]int{}
	for _, id := range []string{"base", "base-sepolia", "solana", "solana-devnet"} {
		before[id] = rpc.count(id)
	}
	r := app.RevealedSession(app.TestKey, `{"amount":"5.00"}`, "Idempotency-Key", "small-pool")
	var s struct {
		ID    string                            `json:"id"`
		PayTo []struct{ Chain, Address string } `json:"pay_to"`
	}
	if err := json.Unmarshal(r.Body, &s); err != nil || r.Status != 200 {
		t.Fatalf("session: %d %s", r.Status, r.Body)
	}
	if _, _, err := app.Svc.RecordPayment(ctx, service.Observation{SessionID: s.ID, Chain: s.PayTo[0].Chain, Asset: "USDC",
		Address: s.PayTo[0].Address, TxHash: "0x1", Amount: money.FromUint64(5_000_000)}); err != nil {
		t.Fatal(err)
	}

	eventually(t, "the payment's event to be sequenced", func() bool {
		return count(t, pool, `SELECT count(*) FROM events WHERE seq IS NOT NULL`) > 0 && count(t, pool, `SELECT count(*) FROM events WHERE seq IS NULL`) == 0
	})
	eventually(t, "every chain loop to keep running", func() bool {
		for id, n := range before {
			if rpc.count(id) <= n {
				return false
			}
		}
		return true
	})
	if n := count(t, pool, `SELECT count(DISTINCT pid) FROM (`+lockerLocks+`) l`); n != 1 {
		t.Fatalf("chain locks spread over %d connections, want one", n)
	}
}
