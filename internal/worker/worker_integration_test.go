//go:build integration

package worker_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/alert"
	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/service"
	"github.com/prismaport/prismaport/internal/testutil"
	"github.com/prismaport/prismaport/internal/worker"
)

// TestWorkerEndToEnd runs the real loops: a confirmed payment must reach the merchant endpoint
// as a signed webhook without any manual sequencing or dispatching.
func TestWorkerEndToEnd(t *testing.T) {
	app := apptest.New(t, apptest.Options{})
	var mu sync.Mutex
	var types []string
	got := make(chan struct{}, 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var e struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(body, &e)
		mu.Lock()
		types = append(types, e.Type)
		mu.Unlock()
		got <- struct{}{}
	}))
	defer srv.Close()
	if r := app.Do("POST", "/v1/webhook_endpoints", app.TestKey, map[string]any{"url": srv.URL, "enabled_events": []string{"session.paid", "session.expired"}}); r.Status != 200 {
		t.Fatal(string(r.Body))
	}
	r := app.RevealedSession(app.TestKey, `{"amount":"5.00"}`, "Idempotency-Key", "w")
	var s struct {
		ID    string                            `json:"id"`
		PayTo []struct{ Chain, Address string } `json:"pay_to"`
	}
	_ = json.Unmarshal(r.Body, &s)
	// Never revealed: it expires at the end of the selection window.
	expiring := app.Session(app.TestKey, `{"amount":"5.00"}`, "Idempotency-Key", "w2")
	if expiring.Status != 200 {
		t.Fatal(string(expiring.Body))
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		worker.New(app.Svc, app.Store, app.Dispatcher, worker.Config{SequenceEvery: 20 * time.Millisecond, DispatchEvery: 20 * time.Millisecond,
			ExpireEvery: 20 * time.Millisecond}, nil).Run(ctx)
		close(done)
	}()

	id, _, err := app.Svc.RecordPayment(ctx, service.Observation{SessionID: s.ID, Chain: s.PayTo[0].Chain, Asset: "USDC",
		Address: s.PayTo[0].Address, TxHash: "0x1", Amount: money.FromUint64(5_000_000)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.Svc.ConfirmPayment(ctx, s.ID, id); err != nil {
		t.Fatal(err)
	}
	app.Clock.Advance(2 * time.Hour)
	for i := 0; i < 2; i++ {
		select {
		case <-got:
		case <-time.After(10 * time.Second):
			t.Fatalf("webhook %d not delivered", i+1)
		}
	}
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	seen := map[string]bool{}
	for _, ty := range types {
		seen[ty] = true
	}
	if !seen["session.paid"] || !seen["session.expired"] || len(types) != 2 {
		t.Fatalf("delivered %v", types)
	}
}

// A loop that fails on every step for FailingAfter is worth a page; one failed step is not, and
// a loop_recovered alert follows when it works again.
func TestLoopFailingForFailingAfterAlertsOnceAndRecovers(t *testing.T) {
	app := apptest.New(t, apptest.Options{})
	var mu sync.Mutex
	broken := true
	steps := 0
	step := func(context.Context) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		steps++
		if broken {
			return 0, errors.New("rpc: connection refused")
		}
		return 0, nil
	}
	rec := &alert.Recorder{}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		worker.New(app.Svc, app.Store, app.Dispatcher, worker.Config{Alerts: rec, FailingAfter: 150 * time.Millisecond,
			Loops: []worker.Loop{{Name: "detector:solana", Every: 10 * time.Millisecond, Step: step}}}, nil).Run(ctx)
		close(done)
	}()
	defer func() {
		if t.Failed() {
			t.Logf("alerts %v", rec.Kinds())
		}
	}()
	stepCount := func() int { mu.Lock(); defer mu.Unlock(); return steps }

	testutil.Eventually(t, 5*time.Second, "a few failed steps", func() bool { return stepCount() >= 3 })
	early := rec.Has("loop_failing")
	testutil.Eventually(t, 5*time.Second, "loop_failing", func() bool { return rec.Has("loop_failing") })
	// Ten more failed steps (FailingAfter again) must not raise it a second time.
	failedAt := stepCount()
	testutil.Eventually(t, 5*time.Second, "ten more failed steps", func() bool { return stepCount() >= failedAt+10 })
	mu.Lock()
	broken = false
	mu.Unlock()
	testutil.Eventually(t, 5*time.Second, "loop_recovered", func() bool { return rec.Has("loop_recovered") })
	cancel()
	<-done

	if early {
		t.Fatal("loop_failing raised before FailingAfter")
	}
	all := rec.All()
	if kinds := rec.Kinds(); len(kinds) != 2 || kinds[0] != "loop_failing" || kinds[1] != "loop_recovered" {
		t.Fatalf("alerts %v, want loop_failing once then loop_recovered", kinds)
	}
	if a := all[0]; a.Severity != alert.Critical || a.Chain != "solana" || a.Fields["loop"] != "detector:solana" ||
		a.Fields["err"] != "rpc: connection refused" {
		t.Fatalf("loop_failing %+v", a)
	}
}

// A step that panics (bad data in one row, say) costs only that step: the worker keeps running
// every loop, step_panicked is raised with the loop's name, and the panics count as failed steps
// (loop_failing after FailingAfter).
func TestAPanickingStepIsRecoveredAndTheWorkerGoesOn(t *testing.T) {
	app := apptest.New(t, apptest.Options{})
	var mu sync.Mutex
	panics, healthy := 0, 0
	bad := func(context.Context) (int, error) {
		mu.Lock()
		panics++
		mu.Unlock()
		panic(errors.New("bad row"))
	}
	good := func(context.Context) (int, error) { mu.Lock(); defer mu.Unlock(); healthy++; return 0, nil }
	rec := &alert.Recorder{}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		worker.New(app.Svc, app.Store, app.Dispatcher, worker.Config{Alerts: rec, FailingAfter: 100 * time.Millisecond,
			Loops: []worker.Loop{{Name: "detector:base", Every: 10 * time.Millisecond, Step: bad},
				{Name: "detector:solana", Every: 10 * time.Millisecond, Step: good}}}, nil).Run(ctx)
		close(done)
	}()
	testutil.Eventually(t, 5*time.Second, "loop_failing after panicking steps", func() bool { return rec.Has("loop_failing") })
	counts := func() (int, int) { mu.Lock(); defer mu.Unlock(); return healthy, panics }
	before, _ := counts()
	// Both loops keep stepping after the panics: the healthy one goes on, the panicking one is retried.
	testutil.Eventually(t, 5*time.Second, "both loops to keep stepping", func() bool {
		after, n := counts()
		return n >= 3 && after > before
	})
	cancel()
	<-done

	var p alert.Alert
	for _, a := range rec.All() {
		if a.Kind == "step_panicked" {
			p = a
			break
		}
	}
	if p.Severity != alert.Critical || p.Chain != "base" || p.Fields["loop"] != "detector:base" || p.Fields["panic"] == "" {
		t.Fatalf("step_panicked %+v in %v", p, rec.Kinds())
	}
}

// The worker runs the removal sweep: a due removal is carried out without any manual step.
func TestWorkerRunsTOTPRemovals(t *testing.T) {
	app := apptest.New(t, apptest.Options{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	u, err := app.Svc.CreateDashboardUser(ctx, app.MerchantID, "owner@example.com", "Correct-horse-battery-1", "test")
	if err != nil {
		t.Fatal(err)
	}
	now := app.Clock.Now()
	if _, err := app.Store.Pool().Exec(ctx, `UPDATE dashboard_users SET totp_secret_ciphertext = 'x' WHERE id = $1`, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Store.Pool().Exec(ctx, `INSERT INTO dashboard_totp_removals (user_id, requested_at, removal_at, cancel_token_hash) VALUES ($1, $2, $3, $4)`,
		u.ID, now.Add(-time.Hour), now.Add(-time.Minute), make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		worker.New(app.Svc, app.Store, app.Dispatcher, worker.Config{TOTPRemovalEvery: 10 * time.Millisecond}, nil).Run(ctx)
	}()
	testutil.Eventually(t, 20*time.Second, "the worker to carry out the removal", func() bool {
		fresh, err := app.Store.DashboardUser(ctx, u.ID)
		if err != nil {
			t.Fatal(err)
		}
		return !fresh.TOTPEnabled() && fresh.RecoveryLockedUntil != nil
	})
	cancel()
	<-done
}
