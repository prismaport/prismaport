//go:build integration

package soldetector_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/service"
	"github.com/prismaport/prismaport/internal/solana"
	"github.com/prismaport/prismaport/internal/solfake"
)

// customerAddress creates a customer address on the test link's product and returns its id and
// its Solana deposit address (the forwarder).
func (e *env) customerAddress(customer string) (string, solana.PublicKey) {
	e.t.Helper()
	r := e.app.Do("POST", "/v1/customer_addresses", e.app.TestKey, map[string]any{"customer_id": customer, "product_id": e.app.Link(e.app.TestKey)})
	if r.Status != 201 {
		e.t.Fatalf("customer address: %d %s", r.Status, r.Body)
	}
	var a struct {
		ID    string `json:"id"`
		PayTo []struct {
			Chain, Address string
		} `json:"pay_to"`
	}
	_ = json.Unmarshal(r.Body, &a)
	for _, p := range a.PayTo {
		if p.Chain == chainID {
			return a.ID, solana.MustPublicKey(p.Address)
		}
	}
	e.t.Fatalf("no %s pay_to: %s", chainID, r.Body)
	return "", solana.PublicKey{}
}

// openOn opens a session on a customer address (amount "" opens an open-amount session).
func (e *env) openOn(addr, amount, key string) string {
	e.t.Helper()
	body := map[string]any{"customer_address_id": addr}
	if amount != "" {
		body["amount"] = amount
	}
	r := e.app.Do("POST", "/v1/sessions", e.app.TestKey, body, "Idempotency-Key", key)
	if r.Status != 200 {
		e.t.Fatalf("session on %s: %d %s", addr, r.Status, r.Body)
	}
	return r.JSON(e.t)["id"].(string)
}

// owner is the session a Solana watch row belongs to, and its next late check.
func (e *env) owner(account solana.PublicKey) (string, *time.Time) {
	e.t.Helper()
	var sid string
	var next *time.Time
	if err := e.app.Store.Pool().QueryRow(e.t.Context(), `SELECT session_id, next_check_at FROM solana_watch WHERE chain = $1
		AND address = $2`, chainID, account.String()).Scan(&sid, &next); err != nil {
		e.t.Fatal(err)
	}
	return sid, next
}

func (e *env) expire() {
	e.t.Helper()
	if _, err := e.app.Svc.ExpireDue(e.t.Context(), 10); err != nil {
		e.t.Fatal(err)
	}
}

// delta is the requests of one method (both providers) since the counter's last reading.
func (c *counter) delta(method string) int { return c.read()[method] }

// expiresAt is a session's stored deadline.
func (e *env) expiresAt(id string) time.Time {
	e.t.Helper()
	ses, err := e.app.Store.LoadSession(e.t.Context(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return ses.State.ExpiresAt
}

// The latest session on a customer address owns its accounts' watch rows. Opening it reads the
// deposit account from the cursor the earlier session left, so a deposit sent between sessions is
// credited to it with its transaction; the earlier session is not checked any more and a late
// credit of it racing the open is refused; the new session's late checks keep their own schedule
// in its own row (not due on every pass). The first session is overpaid (closed, D50) so that it
// keeps its late checks (a session paid exactly leaves them once final, D47); the default late
// schedule checks every minute for 30 min after expiry (D46).
func TestSolanaCustomerAddressWatchBelongsToTheLatestSession(t *testing.T) {
	t.Parallel()
	e := setup(t, "")
	addrID, pda := e.customerAddress("user_1")
	dep := ata(t, pda)

	s1 := e.openOn(addrID, "25.00", "a")
	expires1 := e.expiresAt(s1)
	e.pay("d1", pda, 30_000_000)
	e.mustStep()
	if v := e.get(s1); v.Status != "overpaid" {
		t.Fatalf("first session: %+v", v)
	}
	// Expired and checked once in its late window: its row carries a schedule.
	e.app.Clock.Set(expires1.Add(90 * time.Second))
	e.expire()
	e.late()
	if owner, next := e.owner(dep); owner != s1 || next == nil {
		t.Fatalf("first session's row: %s %v", owner, next)
	}

	// A deposit nobody scans yet, then the next session opens (the first one is paid, D50). The
	// balance reads as the first session saw it (someone flushed 10.00 out since), so only reading
	// the account from its cursor finds the deposit.
	e.pay("d2", pda, 10_000_000)
	e.cl.SetTokenAccount(dep, usdc, pda, 30_000_000)
	s2 := e.openOn(addrID, "", "b")
	if s2 == s1 {
		t.Fatal("a paid session was answered")
	}
	e.scan()
	v := e.get(s2)
	if v.Status != "paid" || len(v.Payments) != 1 || v.Payments[0].TxHash != solfake.Signature("d2") || v.AmountReceived != "10.00" {
		t.Fatalf("second session: %+v", v)
	}
	if v := e.get(s1); len(v.Payments) != 1 || v.AmountReceived != "30.00" {
		t.Fatalf("first session: %+v", v)
	}
	if owner, next := e.owner(dep); owner != s2 || next != nil {
		t.Fatalf("after the open the deposit row belongs to %s (next %v)", owner, next)
	}
	// Lean scanning reads a reference key only inside a wallet window (D49), and a session on a
	// customer address has no wallet path: its reference key is never watched.
	var refs int
	if err := e.app.Store.Pool().QueryRow(t.Context(), `SELECT count(*) FROM solana_watch WHERE chain = $1 AND address = $2`,
		chainID, solana.SessionReference(e.salt(s2)).String()).Scan(&refs); err != nil || refs != 0 {
		t.Fatalf("reference rows %d %v", refs, err)
	}
	_, _, err := e.app.Svc.RecordPayment(t.Context(), service.Observation{SessionID: s1, Chain: chainID, Asset: "USDC",
		Address: pda.String(), TxHash: solfake.Signature("race"), Amount: money.FromUint64(1_000_000)})
	if !errors.Is(err, service.ErrSessionSuperseded) {
		t.Fatalf("credit of a superseded session: %v", err)
	}

	// The first session's next check (a minute later) does not run.
	c := e.counter()
	e.app.Clock.Set(expires1.Add(5 * time.Minute))
	e.late()
	if m := c.delta("MultipleAccounts"); m != 0 {
		t.Fatalf("%d balance reads for a superseded session", m)
	}

	// The second session's schedule is its own: nothing 30 s after its expiry, one check after a
	// minute, then not again on the next pass.
	expires2 := e.expiresAt(s2)
	e.app.Clock.Set(expires2.Add(30 * time.Second))
	e.expire()
	e.late()
	if m := c.delta("MultipleAccounts"); m != 0 {
		t.Fatalf("the second session inherited the first one's schedule: %d balance reads", m)
	}
	e.app.Clock.Set(expires2.Add(70 * time.Second))
	e.late()
	if m := c.delta("MultipleAccounts"); m != 1 {
		t.Fatalf("first late check of the second session: %d balance reads", m)
	}
	e.late()
	if m := c.delta("MultipleAccounts"); m != 0 {
		t.Fatalf("the second session's late check is due on every pass: %d balance reads", m)
	}
	if owner, next := e.owner(dep); owner != s2 || next == nil || !next.After(e.app.Clock.Now()) {
		t.Fatalf("second session's row: %s %v", owner, next)
	}
}

func (e *env) salt(id string) [32]byte {
	e.t.Helper()
	ses, err := e.app.Store.LoadSession(e.t.Context(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return ses.Salt
}
