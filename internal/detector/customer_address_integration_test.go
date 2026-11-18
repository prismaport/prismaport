//go:build integration

package detector_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/derive"
	"github.com/prismaport/prismaport/internal/evm"
	"github.com/prismaport/prismaport/internal/money"
	"github.com/prismaport/prismaport/internal/service"
	"github.com/prismaport/prismaport/internal/session"
)

// openOn opens a session on a customer address (amount "" opens an open-amount session).
func (e *env) openOn(addr, amount, key string) sessionView {
	e.t.Helper()
	body := map[string]any{"customer_address_id": addr}
	if amount != "" {
		body["amount"] = amount
	}
	r := e.app.Do("POST", "/v1/sessions", e.app.TestKey, body, "Idempotency-Key", key)
	if r.Status != 200 {
		e.t.Fatalf("session on %s: %d %s", addr, r.Status, r.Body)
	}
	var s sessionView
	_ = json.Unmarshal(r.Body, &s)
	return s
}

// customerAddress creates a customer address on the test link's product and returns its id and
// its anvil deposit address.
func (e *env) customerAddress(customer string) (string, evm.Address) {
	e.t.Helper()
	r := e.app.Do("POST", "/v1/customer_addresses", e.app.TestKey, map[string]any{"customer_id": customer, "product_id": e.app.Link(e.app.TestKey)})
	if r.Status != 201 {
		e.t.Fatalf("customer address: %d %s", r.Status, r.Body)
	}
	var a struct {
		ID    string `json:"id"`
		PayTo []struct {
			Chain, Asset, Address string
		} `json:"pay_to"`
	}
	_ = json.Unmarshal(r.Body, &a)
	for _, p := range a.PayTo {
		if p.Chain == "anvil" && p.Asset == "USDC" {
			to, err := evm.ParseAddress(p.Address)
			if err != nil {
				e.t.Fatal(err)
			}
			return a.ID, to
		}
	}
	e.t.Fatalf("no anvil pay_to: %s", r.Body)
	return "", evm.Address{}
}

// watchOwner is the session the address's chain_watch row belongs to, and its next late check.
func (e *env) watchOwner(to evm.Address) (string, *time.Time) {
	e.t.Helper()
	var sid string
	var next *time.Time
	if err := e.app.Store.Pool().QueryRow(e.t.Context(), `SELECT session_id, next_check_at FROM chain_watch WHERE chain = 'anvil'
		AND address = $1`, to.Lower()).Scan(&sid, &next); err != nil {
		e.t.Fatal(err)
	}
	return sid, next
}

// payments lists a session's payments as stored: transaction hash and late flag, in order.
func (e *env) payments(id string) []session.Payment {
	e.t.Helper()
	ses, err := e.app.Store.LoadSession(e.t.Context(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return ses.State.Payments
}

// Sessions on one customer address pay into the same forwarder, derived by the real Factory from
// the copied salt and inputs. A transfer sent while no session watches the address (after the
// first session's watch ended) is credited to the next session opened on it, with its real
// transaction and not as a late payment; the first session is left alone.
func TestCustomerAddressFundsBetweenSessionsGoToTheNextSession(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	addrID, to := e.customerAddress("user_1")

	s1 := e.openOn(addrID, "25.00", "a")
	if addr(t, s1, "USDC") != to {
		t.Fatalf("session pays %s, the address is %s", addr(t, s1, "USDC").Hex(), to.Hex())
	}
	e.chain.MineN(2)
	e.drain()
	e.chain.Mine(transfer(usdc, payer, to, 25_000_000))
	e.chain.MineN(2)
	e.drain()
	if got := e.get(s1.ID); got.Status != "paid" {
		t.Fatalf("first session: %+v", got)
	}

	// The first session's watch ends (23 h after expiry); then the customer sends again.
	e.app.Clock.Advance(26 * time.Hour)
	e.expire()
	e.late()
	between := e.chain.Mine(transfer(usdc, payer, to, 10_000_000))
	e.chain.MineN(2)
	e.drain()
	e.late()
	if got := e.get(s1.ID); len(got.Payments) != 1 {
		t.Fatalf("a session past its watch got a payment: %+v", got)
	}

	s2 := e.openOn(addrID, "", "b")
	if s2.ID == s1.ID || addr(t, s2, "USDC") != to {
		t.Fatalf("second session %+v", s2)
	}
	ses, err := e.app.Store.LoadSession(t.Context(), s2.ID)
	if err != nil {
		t.Fatal(err)
	}
	anvil, _ := e.app.Registry.Get("anvil")
	p := ses.PayTo[0]
	got, err := derive.Forwarders(factory).Address(anvil, ses.Salt, derive.Params{Treasury: p.Treasury, Fee: p.Fee, Recipients: p.Recipients, Tokens: p.BoundTokens})
	if err != nil || got != p.Address {
		t.Fatalf("Factory derivation %s (%v), stored %s", got, err, p.Address)
	}

	e.chain.MineN(2)
	e.drain()
	v := e.get(s2.ID)
	if v.Status != "paid" || len(v.Payments) != 1 || v.Received != "10.00" {
		t.Fatalf("second session after its first scan: %+v", v)
	}
	if ps := e.payments(s2.ID); ps[0].TxHash != between.Logs[0].TxHash.Hex() || ps[0].Late {
		t.Fatalf("funds sent between sessions: %+v (tx %s), want that transaction and not late", ps[0], between.Logs[0].TxHash.Hex())
	}
	if got := e.get(s1.ID); len(got.Payments) != 1 || got.Received != "25.00" {
		t.Fatalf("first session: %+v", got)
	}
	if owner, next := e.watchOwner(to); owner != s2.ID || next != nil {
		t.Fatalf("watch row belongs to %s (next check %v), want the second session with no late state", owner, next)
	}
	// Read again later, nothing is credited twice.
	e.chain.MineN(3)
	e.drain()
	if got := e.get(s2.ID); len(got.Payments) != 1 {
		t.Fatalf("second session read the history twice: %+v", got)
	}
}

// A customer may send to the address before any session is opened on it: the first session
// reads the address from its creation.
func TestCustomerAddressFundsBeforeTheFirstSession(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	addrID, to := e.customerAddress("user_2")
	e.chain.MineN(3)
	early := e.chain.Mine(transfer(usdc, payer, to, 30_000_000))
	e.chain.MineN(3)
	e.drain()

	e.app.Clock.Advance(10 * time.Minute)
	s := e.openOn(addrID, "", "a")
	e.chain.MineN(2)
	e.drain()
	if v := e.get(s.ID); v.Status != "paid" || len(v.Payments) != 1 || e.payments(s.ID)[0].TxHash != early.Logs[0].TxHash.Hex() {
		t.Fatalf("first session: %+v", v)
	}
}

// expiresAt is a session's stored deadline.
func (e *env) expiresAt(id string) time.Time {
	e.t.Helper()
	ses, err := e.app.Store.LoadSession(e.t.Context(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return ses.State.ExpiresAt
}

// Opening a session on a customer address ends the previous session's late window: from then on
// only the new session watches the address, a transfer goes to it, the old session is not checked
// or rechecked, and a late credit of the old session racing the open is refused. The new session's
// late checks run on its own schedule (nothing inherited from the old session's row). The default
// late schedule checks every minute for the first 30 min after expiry (D46), and a session paid
// exactly leaves late checks once final (D47), so the second session is overpaid here.
func TestOpeningTheNextSessionEndsThePreviousLateWindow(t *testing.T) {
	t.Parallel()
	e := setup(t, nil)
	addrID, to := e.customerAddress("user_3")

	s1 := e.openOn(addrID, "25.00", "a")
	e.chain.MineN(2)
	e.drain()
	e.chain.Mine(transfer(usdc, payer, to, 10_000_000))
	e.chain.MineN(2)
	e.drain()
	if got := e.get(s1.ID); got.Status != "partially_paid" {
		t.Fatalf("first session: %+v", got)
	}
	// Opening again while the first session waits for payment answers it (D50).
	if again := e.openOn(addrID, "", "a2"); again.ID != s1.ID {
		t.Fatalf("open while partially paid: %s, want %s", again.ID, s1.ID)
	}
	expires := e.expiresAt(s1.ID)

	// Expired with 10.00: its late window runs and a first check is made.
	e.app.Clock.Set(expires.Add(90 * time.Second))
	e.expire()
	calls := e.method("eth_call")
	e.late()
	if e.method("eth_call") == calls {
		t.Fatal("the first session's late check did not run")
	}
	if owner, next := e.watchOwner(to); owner != s1.ID || next == nil {
		t.Fatalf("first session's row: %s %v", owner, next)
	}

	s2 := e.openOn(addrID, "25.00", "b")
	if s2.ID == s1.ID {
		t.Fatal("an expired session was answered")
	}
	e.chain.MineN(2)
	e.drain()
	if owner, next := e.watchOwner(to); owner != s2.ID || next != nil {
		t.Fatalf("after the open the row belongs to %s (next %v)", owner, next)
	}
	e.chain.Mine(transfer(usdc, payer, to, 30_000_000))
	e.chain.MineN(2)
	e.drain()
	if got := e.get(s2.ID); got.Status != "overpaid" || len(got.Payments) != 1 {
		t.Fatalf("second session: %+v", got)
	}

	// The first session's next check was due a minute later: it is not checked any more.
	e.app.Clock.Set(expires.Add(5 * time.Minute))
	calls = e.method("eth_call")
	e.late()
	if e.method("eth_call") != calls {
		t.Fatal("the earlier session was still late-checked after the next session opened")
	}
	if got := e.get(s1.ID); got.Status != "expired" || len(got.Payments) != 1 || got.Received != "10.00" {
		t.Fatalf("first session: %+v", got)
	}
	// A late credit of the first session racing the open (read before it) is refused.
	_, _, err := e.app.Svc.RecordPayment(t.Context(), service.Observation{SessionID: s1.ID, Chain: "anvil", Asset: "USDC",
		Address: to.Hex(), TxHash: "0x" + strings.Repeat("ab", 32), LogIndex: 0,
		Amount: money.FromUint64(1_000_000)})
	if !errors.Is(err, service.ErrSessionSuperseded) {
		t.Fatalf("credit of a superseded session: %v", err)
	}

	// The second session's own schedule: 30 s after its expiry nothing is due (the first
	// session's row state is gone), a minute after it is checked once.
	expires2 := e.expiresAt(s2.ID)
	e.app.Clock.Set(expires2.Add(30 * time.Second))
	e.expire()
	calls = e.method("eth_call")
	e.late()
	if e.method("eth_call") != calls {
		t.Fatal("the second session inherited the first session's late schedule")
	}
	e.app.Clock.Set(expires2.Add(70 * time.Second))
	e.late()
	if e.method("eth_call") == calls {
		t.Fatal("the second session's first late check did not run")
	}
	calls = e.method("eth_call")
	e.late()
	if e.method("eth_call") != calls {
		t.Fatal("the second session's late check is due on every pass")
	}
	if owner, next := e.watchOwner(to); owner != s2.ID || next == nil || !next.After(e.app.Clock.Now()) {
		t.Fatalf("second session's row: %s %v", owner, next)
	}

	// "Check again" on the first session after its window: refused, the second one watches.
	e.app.Clock.Set(expires.Add(25 * time.Hour))
	r := e.app.Do("POST", "/v1/sessions/"+s1.ID+"/recheck", e.app.TestKey, nil)
	if r.Status != 409 || r.ErrorCode(t) != "session_superseded" {
		t.Fatalf("recheck of a superseded session: %d %s", r.Status, r.Body)
	}
}
