//go:build integration

package service_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/store"
)

// Sessions held by a pending payment no longer fill the expiry batch: before, the oldest `limit`
// due sessions were listed and skipped, and no later session ever expired (ADR-0008 A-2).
func TestExpiryNotStalledByPendingPayments(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	ctx := t.Context()
	var pending []sessionInfo
	for _, k := range []string{"p1", "p2"} {
		s := createSession(t, app, app.TestKey, `{"amount":"10.00"}`, k)
		pay(t, app, s, "base-sepolia", "0x"+k, usdc("10"), false)
		pending = append(pending, s)
		app.Clock.Advance(time.Second)
	}
	free := createSession(t, app, app.TestKey, `{"amount":"10.00"}`, "free")
	app.Clock.Advance(2 * time.Hour)

	if n, err := app.Svc.ExpireDue(ctx, 2); err != nil || n != 1 {
		t.Fatalf("expired %d (want 1): %v", n, err)
	}
	if st := sessionJSON(t, app, free.ID)["status"]; st != "expired" {
		t.Fatalf("session behind pending ones: %v", st)
	}
	for _, s := range pending {
		if st := sessionJSON(t, app, s.ID)["status"]; st != "open" {
			t.Fatalf("pending session %s: %v", s.ID, st)
		}
	}
}

// A session whose expiry fails is skipped and reported; the sessions after it still expire
// (ADR-0008 A-2: the pass used to stop at the first unexpected error, every tick).
func TestExpirySkipsAFailingSession(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	ctx := t.Context()
	bad := createSession(t, app, app.TestKey, `{"amount":"10.00"}`, "bad")
	app.Clock.Advance(time.Second)
	good := createSession(t, app, app.TestKey, `{"amount":"10.00"}`, "good")
	app.Clock.Advance(2 * time.Hour)
	if _, err := app.Store.Pool().Exec(ctx, `CREATE FUNCTION fail_expiry() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'injected expiry failure'; END $$;
		CREATE TRIGGER fail_expiry BEFORE UPDATE ON sessions FOR EACH ROW
			WHEN (NEW.id = '`+bad.ID+`' AND NEW.status = 'expired') EXECUTE FUNCTION fail_expiry()`); err != nil {
		t.Fatal(err)
	}

	n, err := app.Svc.ExpireDue(ctx, 100)
	if err == nil || n != 1 {
		t.Fatalf("expired %d, err %v: want 1 and the failure reported", n, err)
	}
	if st := sessionJSON(t, app, good.ID)["status"]; st != "expired" {
		t.Fatalf("session after the failing one: %v", st)
	}
	if st := sessionJSON(t, app, bad.ID)["status"]; st != "open" {
		t.Fatalf("failing session: %v", st)
	}
}

// A failed confirm leaves no simulated payment behind: it would stay detected forever (no chain
// loop confirms it) and keep the session from expiring (ADR-0008 A-3).
func TestSimulatedPaymentIsAtomic(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	ctx := t.Context()
	s := createSession(t, app, app.TestKey, `{"amount":"10.00"}`, "sim")
	if _, err := app.Store.Pool().Exec(ctx, `CREATE FUNCTION fail_confirm() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'injected confirm failure'; END $$;
		CREATE TRIGGER fail_confirm BEFORE UPDATE ON payments FOR EACH ROW
			WHEN (NEW.status = 'confirmed') EXECUTE FUNCTION fail_confirm()`); err != nil {
		t.Fatal(err)
	}
	r := app.Do("POST", "/v1/test_helpers/sessions/"+s.ID+"/simulate", app.TestKey, map[string]any{"action": "pay_exact"})
	if r.Status == http.StatusOK {
		t.Fatalf("simulate succeeded through a failing confirm: %s", r.Body)
	}
	ses, err := app.Store.LoadSession(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ses.State.Payments) != 0 {
		t.Fatalf("simulated payment left behind: %+v", ses.State.Payments)
	}
}

// With an expiry hold, a session past expires_at stays open until the scanner read its deposit
// addresses once more (the first late check), so a transfer mined just before expiry and found by
// that check is on time, not late; a session whose check ran expires; and the hold is bounded
// (ADR-0008 H-3).
func TestExpiryWaitsForTheFirstLateCheck(t *testing.T) {
	t.Parallel()
	const hold = 5 * time.Minute
	app := apptest.New(t, apptest.Options{ExpiryHold: hold})
	ctx := t.Context()
	onTime := createSession(t, app, app.TestKey, `{"amount":"10.00"}`, "h1")
	checked := createSession(t, app, app.TestKey, `{"amount":"10.00"}`, "h2")
	unchecked := createSession(t, app, app.TestKey, `{"amount":"10.00"}`, "h3")
	ses, err := app.Store.LoadSession(ctx, onTime.ID)
	if err != nil {
		t.Fatal(err)
	}
	app.Clock.Set(ses.State.ExpiresAt.Add(30 * time.Second))

	if n, err := app.Svc.ExpireDue(ctx, 100); err != nil || n != 0 {
		t.Fatalf("expired %d before the first late check: %v", n, err)
	}

	// The late check of onTime finds a transfer mined before expiry; checked's finds nothing.
	pay(t, app, onTime, "base-sepolia", "0xontime", usdc("10"), true)
	entries, err := app.Store.SessionWatchEntries(ctx, "base-sepolia", checked.ID)
	if err != nil || len(entries) == 0 {
		t.Fatalf("watch entries %v %v", entries, err)
	}
	next := app.Clock.Now().Add(time.Minute)
	for _, e := range entries {
		if err := app.Store.MarkChecked(ctx, e, store.EVMCheck{Next: &next}); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := app.Svc.ExpireDue(ctx, 100); err != nil || n != 1 {
		t.Fatalf("expired %d after one late check (want 1): %v", n, err)
	}
	if st := sessionJSON(t, app, checked.ID)["status"]; st != "expired" {
		t.Fatalf("checked session: %v", st)
	}
	if st := sessionJSON(t, app, unchecked.ID)["status"]; st != "open" {
		t.Fatalf("unchecked session inside the hold: %v", st)
	}
	paid, err := app.Store.LoadSession(ctx, onTime.ID)
	if err != nil {
		t.Fatal(err)
	}
	if paid.State.Status != "paid" || len(paid.State.Payments) != 1 || paid.State.Payments[0].Late {
		t.Fatalf("on-time payment: status %s payments %+v", paid.State.Status, paid.State.Payments)
	}

	app.Clock.Advance(hold)
	if n, err := app.Svc.ExpireDue(ctx, 100); err != nil || n != 1 {
		t.Fatalf("expired %d once the hold ran out (want 1): %v", n, err)
	}
	if st := sessionJSON(t, app, unchecked.ID)["status"]; st != "expired" {
		t.Fatalf("unchecked session after the hold: %v", st)
	}
}

// Sessions that fail to expire every time sort first and used to fill every batch: a full batch
// that left one unexpired is followed by the next, so the sessions behind them still expire
// (ADR-0008 A-2).
func TestExpiryPagesPastFailingSessions(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	ctx := t.Context()
	var bad []string
	for _, k := range []string{"bad1", "bad2"} {
		bad = append(bad, "'"+createSession(t, app, app.TestKey, `{"amount":"10.00"}`, k).ID+"'")
		app.Clock.Advance(time.Second)
	}
	good := createSession(t, app, app.TestKey, `{"amount":"10.00"}`, "good")
	app.Clock.Advance(2 * time.Hour)
	if _, err := app.Store.Pool().Exec(ctx, `CREATE FUNCTION fail_expiry() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'injected expiry failure'; END $$;
		CREATE TRIGGER fail_expiry BEFORE UPDATE ON sessions FOR EACH ROW
			WHEN (NEW.id IN (`+strings.Join(bad, ",")+`) AND NEW.status = 'expired') EXECUTE FUNCTION fail_expiry()`); err != nil {
		t.Fatal(err)
	}

	n, err := app.Svc.ExpireDue(ctx, 2)
	if err == nil || n != 1 {
		t.Fatalf("expired %d, err %v: want 1 and the failures reported", n, err)
	}
	if st := sessionJSON(t, app, good.ID)["status"]; st != "expired" {
		t.Fatalf("session behind a full batch of failing ones: %v", st)
	}
}

// A Gateway settle job that landed (mined) may not be credited yet: on Solana the reference-key
// scan records it after the job turns mined. The session stays held until the job is done or the
// hold runs out (ADR-0008 H-3).
func TestExpiryWaitsForALandedSettleJob(t *testing.T) {
	t.Parallel()
	const hold = 5 * time.Minute
	app := apptest.New(t, apptest.Options{ExpiryHold: hold})
	ctx := t.Context()
	s := createSession(t, app, app.TestKey, `{"amount":"10.00"}`, "settle")
	ses, err := app.Store.LoadSession(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	app.Clock.Set(ses.State.ExpiresAt.Add(2 * time.Minute))
	entries, err := app.Store.SessionWatchEntries(ctx, "base-sepolia", s.ID)
	if err != nil || len(entries) == 0 {
		t.Fatalf("watch entries %v %v", entries, err)
	}
	next := app.Clock.Now().Add(time.Minute)
	for _, e := range entries { // the deposit addresses had their first late check
		if err := app.Store.MarkChecked(ctx, e, store.EVMCheck{Next: &next}); err != nil {
			t.Fatal(err)
		}
	}
	j, _, err := app.Store.InsertTxJob(ctx, store.TxJob{Chain: "base-sepolia", Kind: store.JobSettle, Ref: "settle|" + s.ID,
		SessionID: s.ID, Asset: "USDC", To: "gateway", Calldata: []byte{1}, Sender: "sender"})
	if err != nil {
		t.Fatal(err)
	}
	setStatus := func(st string) {
		t.Helper()
		if _, err := app.Store.Pool().Exec(ctx, `UPDATE chain_tx_jobs SET status = $2, last_valid_height = 100 WHERE id = $1`, j.ID, st); err != nil {
			t.Fatal(err)
		}
	}

	setStatus(store.JobMined)
	if n, err := app.Svc.ExpireDue(ctx, 100); err != nil || n != 0 {
		t.Fatalf("expired %d with a landed settle job not credited yet: %v", n, err)
	}
	setStatus(store.JobDone)
	if n, err := app.Svc.ExpireDue(ctx, 100); err != nil || n != 1 {
		t.Fatalf("expired %d once the settle job was done (want 1): %v", n, err)
	}
}
