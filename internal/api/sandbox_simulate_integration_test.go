//go:build integration

package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/service"
	"github.com/prismaport/prismaport/internal/webhooksig"
)

func newTestSession(t *testing.T, app *apptest.App, key, amount string) map[string]any {
	t.Helper()
	r := app.Session(key, map[string]any{"amount": amount}, "Idempotency-Key", "sim-"+amount+"-"+time.Now().Format(time.RFC3339Nano))
	if r.Status != http.StatusOK {
		t.Fatalf("create session: %d %s", r.Status, r.Body)
	}
	return r.JSON(t)
}

type simResult struct {
	Events []struct {
		ID             string `json:"id"`
		Type           string `json:"type"`
		SessionVersion int64  `json:"session_version"`
	} `json:"events"`
	Session struct {
		ID             string `json:"id"`
		Status         string `json:"status"`
		Version        int64  `json:"version"`
		AmountReceived string `json:"amount_received"`
		ExpiresAt      string `json:"expires_at"`
		Payments       []struct {
			ID, Asset, Amount, Status string
			TxHash                    string `json:"tx_hash"`
			Simulated                 bool   `json:"simulated"`
		} `json:"payments"`
	} `json:"session"`
}

func simulate(t *testing.T, app *apptest.App, id string, body map[string]any) simResult {
	t.Helper()
	r := app.Do("POST", "/v1/test_helpers/sessions/"+id+"/simulate", app.TestKey, body)
	if r.Status != http.StatusOK {
		t.Fatalf("simulate %v: %d %s", body, r.Status, r.Body)
	}
	var res simResult
	if err := json.Unmarshal(r.Body, &res); err != nil {
		t.Fatal(err)
	}
	return res
}

func types(res simResult) string {
	var out []string
	for _, e := range res.Events {
		out = append(out, e.Type)
	}
	return strings.Join(out, ",")
}

func TestSimulatorLifecycle(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	rc := newReceiver(t, http.StatusOK, "")
	_, secret := createEndpoint(t, app, app.TestKey, rc.srv.URL)
	ses := newTestSession(t, app, app.TestKey, "25.00")
	id := ses["id"].(string)

	steps := []struct {
		body     map[string]any
		events   string
		status   string
		received string
	}{
		{map[string]any{"action": "pay_partial", "amount": "10.00"}, "session.payment_detected,session.partially_paid", "partially_paid", "10.00"},
		{map[string]any{"action": "pay_partial"}, "session.payment_detected,session.partially_paid", "partially_paid", "17.50"},
		{map[string]any{"action": "pay_exact"}, "session.payment_detected,session.paid", "paid", "25.00"},
		{map[string]any{"action": "reverse_last_payment"}, "payment.reversed", "partially_paid", "17.50"},
		{map[string]any{"action": "overpay", "amount": "8.000001"}, "session.payment_detected,session.overpaid", "overpaid", "25.500001"},
		{map[string]any{"action": "under_review"}, "session.under_review", "under_review", "25.500001"},
	}
	version := int64(1)
	for _, st := range steps {
		t.Run(st.events, func(t *testing.T) {
			res := simulate(t, app, id, st.body)
			if got := types(res); got != st.events || res.Session.Status != st.status || res.Session.AmountReceived != st.received {
				t.Fatalf("%v: events %s status %s received %s", st.body, got, res.Session.Status, res.Session.AmountReceived)
			}
			for _, e := range res.Events {
				version++
				if e.SessionVersion != version || !strings.HasPrefix(e.ID, "evt_") {
					t.Fatalf("%v: event %+v (want version %d)", st.body, e, version)
				}
			}
			if res.Session.Version != version {
				t.Fatalf("%v: session version %d, events end at %d", st.body, res.Session.Version, version)
			}
		})
	}
	got := simulate(t, app, newTestSession(t, app, app.TestKey, "30.00")["id"].(string), map[string]any{"action": "pay_exact"})
	for _, p := range got.Session.Payments {
		if !p.Simulated || p.Status != "confirmed" || p.Amount != "30.000000" || !strings.HasPrefix(p.TxHash, service.SimulatedHashPrefix) {
			t.Fatalf("synthetic payment: %+v", p)
		}
	}

	// Expiry and late payments.
	exp := newTestSession(t, app, app.TestKey, "12.00")["id"].(string)
	res := simulate(t, app, exp, map[string]any{"action": "expire"})
	if types(res) != "session.expired" || res.Session.Status != "expired" || res.Session.ExpiresAt != app.Clock.Now().Format(time.RFC3339) {
		t.Fatalf("expire: %s %+v", types(res), res.Session)
	}
	res = simulate(t, app, exp, map[string]any{"action": "late_payment"})
	if types(res) != "session.payment_detected,session.late_payment" || res.Session.Status != "late_payment" || res.Session.AmountReceived != "12.00" {
		t.Fatalf("late payment: %s %+v", types(res), res.Session)
	}
	direct := newTestSession(t, app, app.TestKey, "12.00")["id"].(string)
	res = simulate(t, app, direct, map[string]any{"action": "late_payment", "amount": "5.00"})
	if types(res) != "session.expired,session.payment_detected,session.late_payment" || res.Session.AmountReceived != "5.00" {
		t.Fatalf("late payment on an open session: %s %+v", types(res), res.Session)
	}

	// The events reach the feed and the webhooks exactly as detector events do.
	if _, err := app.Store.SequenceEvents(t.Context(), 1000); err != nil {
		t.Fatal(err)
	}
	for {
		n, err := app.Dispatcher.RunOnce(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}
	feed := app.Do("GET", "/v1/events?limit=200", app.TestKey, nil)
	var page struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(feed.Body, &page); err != nil {
		t.Fatal(err)
	}
	delivered := rc.all()
	if len(page.Data) != len(delivered) || len(delivered) < 15 {
		t.Fatalf("feed %d, delivered %d", len(page.Data), len(delivered))
	}
	byID := map[string][]byte{}
	for _, d := range delivered {
		if err := webhooksig.Verify(d.body, d.header.Get("Signature"), secret, 0, app.Clock.Now()); err != nil {
			t.Fatal(err)
		}
		byID[d.header.Get("Prismaport-Event-Id")] = d.body
	}
	for _, raw := range page.Data {
		var e struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(raw, &e)
		if string(byID[e.ID]) != string(raw) {
			t.Fatalf("feed and webhook differ for %s", e.ID)
		}
	}

	// Synthetic payments have no chain record, so they are never confirmed by a detector or flushed.
	var chainRows int
	if err := app.Store.Pool().QueryRow(t.Context(), `SELECT count(*) FROM chain_payments`).Scan(&chainRows); err != nil || chainRows != 0 {
		t.Fatalf("chain_payments %d %v", chainRows, err)
	}
	for _, c := range []string{"base-sepolia", "solana-devnet"} {
		if cands, err := app.Store.FlushCandidates(t.Context(), c, 100); err != nil || len(cands) != 0 {
			t.Fatalf("flush candidates on %s: %v %v", c, cands, err)
		}
	}
	var simulated int
	if err := app.Store.Pool().QueryRow(t.Context(), `SELECT count(*) FROM payments WHERE simulated`).Scan(&simulated); err != nil || simulated != 7 {
		t.Fatalf("simulated payments stored: %d %v", simulated, err)
	}
}

func TestSimulatorPaysTheChosenNetwork(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	id := newTestSession(t, app, app.TestKey, "25.00")["id"].(string)
	res := simulate(t, app, id, map[string]any{"action": "pay_partial", "amount": "1.000001", "chain": "solana-devnet", "asset": "USDC"})
	ses, err := app.Store.LoadSession(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	p := ses.State.Payments[0]
	if p.Chain != "solana-devnet" || !p.Simulated || !strings.HasPrefix(p.TxHash, "11111111") || res.Session.AmountReceived != "1.000001" {
		t.Fatalf("solana payment: %+v", p)
	}
}

func TestSimulatorRefusals(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	id := newTestSession(t, app, app.TestKey, "25.00")["id"].(string)
	live := newTestSession(t, app, app.LiveKey, "25.00")["id"].(string)
	_, readOnly, err := app.Svc.CreateAPIKey(t.Context(), app.MerchantID, false, []string{"sessions:read"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, id, key string
		body          any
		status        int
		code          string
	}{
		{"live key", live, app.LiveKey, map[string]any{"action": "pay_exact"}, 403, "test_mode_only"},
		{"live key on a test session", id, app.LiveKey, map[string]any{"action": "pay_exact"}, 403, "test_mode_only"},
		{"live session with a test key", live, app.TestKey, map[string]any{"action": "pay_exact"}, 404, "session_not_found"},
		{"unknown session", "ses_01JB0000000000000000000000", app.TestKey, map[string]any{"action": "pay_exact"}, 404, "session_not_found"},
		{"scope", id, readOnly, map[string]any{"action": "pay_exact"}, 403, "insufficient_scope"},
		{"no action", id, app.TestKey, map[string]any{}, 400, "invalid_request"},
		{"unknown action", id, app.TestKey, map[string]any{"action": "refund"}, 400, "invalid_request"},
		{"unknown field", id, app.TestKey, map[string]any{"action": "pay_exact", "tx_hash": "0x1"}, 400, "invalid_request"},
		{"amount on pay_exact", id, app.TestKey, map[string]any{"action": "pay_exact", "amount": "1.00"}, 400, "invalid_request"},
		{"amount on expire", id, app.TestKey, map[string]any{"action": "expire", "amount": "1.00"}, 400, "invalid_request"},
		{"partial not below what is due", id, app.TestKey, map[string]any{"action": "pay_partial", "amount": "25.00"}, 400, "invalid_request"},
		{"overpay not above what is due", id, app.TestKey, map[string]any{"action": "overpay", "amount": "25.00"}, 400, "invalid_request"},
		{"bad amount", id, app.TestKey, map[string]any{"action": "pay_partial", "amount": "ten"}, 400, "invalid_request"},
		{"too many places", id, app.TestKey, map[string]any{"action": "pay_partial", "amount": "1.0000001"}, 400, "invalid_request"},
		{"zero", id, app.TestKey, map[string]any{"action": "pay_partial", "amount": "0"}, 400, "invalid_request"},
		{"unknown network", id, app.TestKey, map[string]any{"action": "pay_exact", "chain": "base"}, 400, "invalid_request"},
		{"reverse with nothing to reverse", id, app.TestKey, map[string]any{"action": "reverse_last_payment"}, 409, "invalid_state"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := app.Do("POST", "/v1/test_helpers/sessions/"+c.id+"/simulate", c.key, c.body)
			if r.Status != c.status || r.ErrorCode(t) != c.code {
				t.Errorf("%s: %d %s", c.name, r.Status, r.Body)
			}
		})
	}
	if ses, _ := app.Store.LoadSession(t.Context(), live); ses.State.Version != 1 || len(ses.State.Payments) != 0 {
		t.Fatal("the live session changed")
	}
	if ses, _ := app.Store.LoadSession(t.Context(), id); ses.State.Version != 1 {
		t.Fatal("a refused simulation changed the session")
	}

	// State conflicts.
	simulate(t, app, id, map[string]any{"action": "pay_exact"})
	for _, body := range []map[string]any{{"action": "pay_exact"}, {"action": "pay_partial"}, {"action": "expire"}, {"action": "late_payment"}} {
		if r := app.Do("POST", "/v1/test_helpers/sessions/"+id+"/simulate", app.TestKey, body); r.Status != 409 || r.ErrorCode(t) != "invalid_state" {
			t.Errorf("%v on a paid session: %d %s", body, r.Status, r.Body)
		}
	}
	simulate(t, app, id, map[string]any{"action": "under_review"})
	if r := app.Do("POST", "/v1/test_helpers/sessions/"+id+"/simulate", app.TestKey, map[string]any{"action": "under_review"}); r.Status != 409 {
		t.Fatalf("under review twice: %d", r.Status)
	}

	// A real detected payment keeps the session from expiring, and is never reversed by the simulator.
	pending := newTestSession(t, app, app.TestKey, "25.00")
	ses, _ := app.Store.LoadSession(t.Context(), pending["id"].(string))
	pt := ses.PayTo[0]
	if _, _, err := app.Svc.RecordPayment(t.Context(), service.Observation{SessionID: ses.ID, Chain: pt.Chain, Asset: pt.Asset,
		Address: pt.Address, TxHash: "0x" + strings.Repeat("ab", 32), Amount: pt.Amount}); err != nil {
		t.Fatal(err)
	}
	if r := app.Do("POST", "/v1/test_helpers/sessions/"+ses.ID+"/simulate", app.TestKey, map[string]any{"action": "expire"}); r.Status != 409 {
		t.Fatalf("expire with a pending payment: %d %s", r.Status, r.Body)
	}
	if r := app.Do("POST", "/v1/test_helpers/sessions/"+ses.ID+"/simulate", app.TestKey, map[string]any{"action": "reverse_last_payment"}); r.Status != 409 {
		t.Fatalf("reverse a real payment: %d %s", r.Status, r.Body)
	}
}

func TestDashboardSimulate(t *testing.T) {
	t.Parallel()
	app, _ := dashApp(t, nil)
	c := login(t, app, "owner@example.com")
	id := newTestSession(t, app, app.TestKey, "25.00")["id"].(string)
	res := c.send("POST", "/v1/dashboard/sessions/"+id+"/simulate?mode=test", map[string]any{"action": "pay_exact"})
	if res["session"].(map[string]any)["status"] != "paid" || len(res["events"].([]any)) != 2 {
		t.Fatalf("dashboard simulate: %v", res)
	}
	detail := c.get("/v1/dashboard/sessions/" + id + "?mode=test")
	if detail["payments"].([]any)[0].(map[string]any)["simulated"] != true || len(detail["events"].([]any)) != 2 {
		t.Fatalf("detail: %v", detail)
	}
	// A synthetic hash has no explorer page; the deposit addresses keep theirs.
	for _, l := range detail["explorer_links"].([]any) {
		if l.(map[string]any)["type"] == "payment" {
			t.Fatalf("explorer link to a simulated payment: %v", l)
		}
	}
	rp := c.get("/v1/dashboard/overview?mode=test")["recent_payments"].([]any)[0].(map[string]any)
	if rp["simulated"] != true || rp["explorer_url"] != nil {
		t.Fatalf("overview row: %v", rp)
	}
	if r := c.do("POST", "/v1/dashboard/sessions/"+id+"/simulate?mode=live", map[string]any{"action": "under_review"}); r.Status != 403 || r.ErrorCode(t) != "test_mode_only" {
		t.Fatalf("live: %d %s", r.Status, r.Body)
	}
	secondMerchant(t, app, "other@example.com")
	intruder := login(t, app, "other@example.com")
	if r := intruder.do("POST", "/v1/dashboard/sessions/"+id+"/simulate?mode=test", map[string]any{"action": "under_review"}); r.Status != 404 {
		t.Fatalf("intruder: %d %s", r.Status, r.Body)
	}
}

// after=latest answers an empty page whose cursor skips the history, for pollers starting now.
func TestFeedLatestCursor(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	if got := app.Do("GET", "/v1/events?after=latest", app.TestKey, nil).JSON(t); got["next_cursor"] != "0" || len(got["data"].([]any)) != 0 {
		t.Fatalf("empty feed: %v", got)
	}
	id := newTestSession(t, app, app.TestKey, "25.00")["id"].(string)
	simulate(t, app, id, map[string]any{"action": "pay_exact"})
	if _, err := app.Store.SequenceEvents(t.Context(), 100); err != nil {
		t.Fatal(err)
	}
	latest := app.Do("GET", "/v1/events?after=latest", app.TestKey, nil).JSON(t)
	if len(latest["data"].([]any)) != 0 || latest["has_more"] != false {
		t.Fatalf("latest: %v", latest)
	}
	if page := app.Do("GET", "/v1/events?after="+latest["next_cursor"].(string), app.TestKey, nil).JSON(t); len(page["data"].([]any)) != 0 {
		t.Fatalf("events after the latest cursor: %v", page)
	}
	simulate(t, app, id, map[string]any{"action": "reverse_last_payment"})
	if _, err := app.Store.SequenceEvents(t.Context(), 100); err != nil {
		t.Fatal(err)
	}
	page := app.Do("GET", "/v1/events?after="+latest["next_cursor"].(string), app.TestKey, nil).JSON(t)
	if len(page["data"].([]any)) != 1 || page["data"].([]any)[0].(map[string]any)["type"] != "payment.reversed" {
		t.Fatalf("new events: %v", page)
	}
	// Live mode has its own feed; another merchant's events never move this cursor.
	if got := app.Do("GET", "/v1/events?after=latest", app.LiveKey, nil).JSON(t); got["next_cursor"] != "0" {
		t.Fatalf("live cursor: %v", got)
	}
	_, readOnly, _ := app.Svc.CreateAPIKey(t.Context(), app.MerchantID, false, []string{"sessions:read"}, "test")
	if r := app.Do("GET", "/v1/events?after=latest", readOnly, nil); r.Status != http.StatusForbidden {
		t.Fatalf("scope: %d", r.Status)
	}
	if r := app.Do("GET", "/v1/events?after=earliest", app.TestKey, nil); r.Status != http.StatusBadRequest {
		t.Fatalf("bad cursor: %d", r.Status)
	}
}
