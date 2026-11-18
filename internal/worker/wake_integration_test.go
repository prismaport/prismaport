//go:build integration

package worker_test

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/service"
	"github.com/prismaport/prismaport/internal/store"
	"github.com/prismaport/prismaport/internal/testutil"
	"github.com/prismaport/prismaport/internal/worker"
)

// fastWake makes the listener notice a lost connection and listen again within a test's patience.
var fastWake = store.WakeConfig{Ping: 200 * time.Millisecond, MinBackoff: 20 * time.Millisecond, MaxBackoff: 200 * time.Millisecond}

// manualClock is the activity's clock: tests move it to make the worker idle without waiting.
type manualClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *manualClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *manualClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// topics records the payloads the listener delivered.
type topics struct {
	mu   sync.Mutex
	seen []string
}

func (r *topics) add(p string) { r.mu.Lock(); r.seen = append(r.seen, p); r.mu.Unlock() }
func (r *topics) has(p string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Contains(r.seen, p)
}
func (r *topics) reset() { r.mu.Lock(); r.seen = nil; r.mu.Unlock() }

// listenerPID is the backend of this database's wake listener (0: none listening).
func listenerPID(t *testing.T, app *apptest.App) int {
	t.Helper()
	var pid int
	err := app.Store.Pool().QueryRow(t.Context(), `SELECT coalesce(max(pid), 0) FROM pg_stat_activity
		WHERE datname = current_database() AND application_name = $1`, store.WakeApplicationName).Scan(&pid)
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

func unsequenced(t *testing.T, app *apptest.App) (pending, all int) {
	t.Helper()
	err := app.Store.Pool().QueryRow(t.Context(), `SELECT count(*) FILTER (WHERE seq IS NULL), count(*) FROM events`).Scan(&pending, &all)
	if err != nil {
		t.Fatal(err)
	}
	return pending, all
}

// idleEventsWorker runs the events loops with an hour between idle ticks and 50ms between
// active ones, listening for wakes; it returns once the listener listens and the process is idle.
// The expiry loop ticks hourly in both modes, so only a wake ticks it: the open session a test
// pays into would otherwise keep the process active (as it should in production).
func idleEventsWorker(t *testing.T, app *apptest.App) (*worker.Activity, *manualClock, *topics) {
	t.Helper()
	clk := &manualClock{t: time.Now()}
	act := worker.NewActivity(10*time.Minute, clk.now, nil)
	got := &topics{}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		worker.New(app.Svc, app.Store, app.Dispatcher, worker.Config{Role: worker.RoleEvents,
			SequenceEvery: time.Hour, SequenceActive: 50 * time.Millisecond, DispatchEvery: time.Hour, DispatchActive: 50 * time.Millisecond,
			ExpireEvery: time.Hour, ExpireActive: time.Hour, NoticeEvery: time.Hour, TOTPRemovalEvery: time.Hour, Activity: act,
			Listen: func(ctx context.Context, wake func(string)) {
				app.Store.ListenWake(ctx, fastWake, func(p string) { got.add(p); wake(p) }, nil)
			}}, nil).Run(ctx)
	}()
	t.Cleanup(func() { cancel(); <-done })
	testutil.Eventually(t, 10*time.Second, "the wake listener to listen", func() bool { return listenerPID(t, app) != 0 })
	idle(t, act, clk)
	return act, clk, got
}

// idle moves the activity clock past IdleAfter and lets every loop arm its idle (one hour) timer.
func idle(t *testing.T, act *worker.Activity, clk *manualClock) {
	t.Helper()
	clk.advance(11 * time.Minute)
	if act.Active() {
		t.Fatal("still active after IdleAfter")
	}
	time.Sleep(300 * time.Millisecond) // the loops' last active timers (50ms) fire and re-arm idle
}

type revealedSession struct {
	ID    string                            `json:"id"`
	PayTo []struct{ Chain, Address string } `json:"pay_to"`
}

func revealed(t *testing.T, app *apptest.App, key string) revealedSession {
	t.Helper()
	r := app.RevealedSession(app.TestKey, `{"amount":"5.00"}`, "Idempotency-Key", key)
	var s revealedSession
	if err := json.Unmarshal(r.Body, &s); err != nil || s.ID == "" || len(s.PayTo) == 0 {
		t.Fatalf("revealed session: %v %s", err, r.Body)
	}
	return s
}

// detectedSequenced records a payment, as the chain worker would (its session.payment_detected
// event notifies prismaport_wake), and waits for the worker under test to sequence the event,
// returning how long that took from the insert.
func detectedSequenced(t *testing.T, app *apptest.App, s revealedSession, tx string) time.Duration {
	t.Helper()
	_, before := unsequenced(t, app)
	start := time.Now()
	if _, _, err := app.Svc.RecordPayment(t.Context(), service.Observation{SessionID: s.ID, Chain: s.PayTo[0].Chain, Asset: "USDC",
		Address: s.PayTo[0].Address, TxHash: tx, Amount: money.FromUint64(5_000_000)}); err != nil {
		t.Fatal(err)
	}
	testutil.EventuallyEvery(t, 5*time.Second, 5*time.Millisecond, "the new event to be sequenced", func() bool {
		pending, all := unsequenced(t, app)
		return all > before && pending == 0
	})
	return time.Since(start)
}

// A new event wakes an idle worker (hour-long idle ticks): it is sequenced within one active
// tick of the insert, and the process turns active. A new session wakes it too.
func TestInsertWakesAnIdleWorker(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	act, clk, got := idleEventsWorker(t, app)
	s := revealed(t, app, "wake-1")
	testutil.Eventually(t, 5*time.Second, "the session wake", func() bool { return got.has("sessions") })
	if !act.Active() {
		t.Fatal("the session wake did not turn the process active")
	}
	idle(t, act, clk)
	took := detectedSequenced(t, app, s, "0x1")
	if took > 2*time.Second {
		t.Fatalf("sequenced %s after the insert, want within an active tick", took)
	}
	if !got.has("events") {
		t.Fatalf("notifications %v, want events", got.seen)
	}
	if !act.Active() {
		t.Fatal("the wake did not turn the process active")
	}
	t.Logf("wake latency (insert to sequenced): %s", took)
}

// A dropped listening connection is taken again with a backoff, without stopping the worker; the
// listener wakes the loops on its return (notifications may have been missed) and new inserts
// wake them again.
func TestListenerReconnectsAfterItsConnectionDrops(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	act, clk, got := idleEventsWorker(t, app)
	s := revealed(t, app, "wake-2")
	pid := listenerPID(t, app)
	got.reset()
	var killed bool
	if err := app.Store.Pool().QueryRow(t.Context(), `SELECT pg_terminate_backend($1, 5000)`, pid).Scan(&killed); err != nil || !killed {
		t.Fatalf("terminate the listener: %v %v", killed, err)
	}
	testutil.Eventually(t, 10*time.Second, "a new listener", func() bool {
		p := listenerPID(t, app)
		return p != 0 && p != pid
	})
	testutil.Eventually(t, 5*time.Second, "the reconnect wake", func() bool { return got.has("") })
	idle(t, act, clk)
	if took := detectedSequenced(t, app, s, "0x2"); took > 2*time.Second {
		t.Fatalf("after the reconnect: sequenced %s after the insert", took)
	}
}

// Each trigger of migration 00118 notifies its topic: a session and its event, a revealed network
// and a "Check again" request; the worker clearing the request does not.
func TestEveryWakeTopicIsNotified(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	got := &topics{}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); app.Store.ListenWake(ctx, fastWake, got.add, nil) }()
	t.Cleanup(func() { cancel(); <-done })
	testutil.Eventually(t, 10*time.Second, "the wake listener to listen", func() bool { return listenerPID(t, app) != 0 })

	r := app.Session(app.TestKey, `{"amount":"5.00"}`, "Idempotency-Key", "topics")
	if r.Status != 200 {
		t.Fatal(string(r.Body))
	}
	id := r.JSON(t)["id"].(string)
	testutil.Eventually(t, 5*time.Second, "sessions", func() bool { return got.has("sessions") })
	app.Reveal(app.TestKey, id)
	testutil.Eventually(t, 5*time.Second, "session_reveals", func() bool { return got.has("session_reveals") })
	var s revealedSession
	if err := json.Unmarshal(app.Do("GET", "/v1/sessions/"+id, app.TestKey, nil).Body, &s); err != nil {
		t.Fatal(err)
	}
	if _, _, err := app.Svc.RecordPayment(t.Context(), service.Observation{SessionID: id, Chain: s.PayTo[0].Chain, Asset: "USDC",
		Address: s.PayTo[0].Address, TxHash: "0x3", Amount: money.FromUint64(1_000_000)}); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, 5*time.Second, "events", func() bool { return got.has("events") })
	if ok, err := app.Store.RequestRecheck(t.Context(), app.MerchantID, id, "test", nil); err != nil || !ok {
		t.Fatalf("request recheck: %v %v", ok, err)
	}
	testutil.Eventually(t, 5*time.Second, "recheck", func() bool { return got.has("recheck") })
	got.reset()
	if _, err := app.Store.Pool().Exec(t.Context(), `UPDATE sessions SET recheck_requested_at = NULL WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Store.Pool().Exec(t.Context(), `SELECT pg_notify($1, 'marker')`, store.WakeChannel); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, 5*time.Second, "the marker", func() bool { return got.has("marker") })
	if got.has("recheck") {
		t.Fatal("clearing the recheck notified")
	}
}
