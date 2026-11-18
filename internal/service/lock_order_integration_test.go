//go:build integration

package service_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/service"
	"github.com/prismaport/prismaport/internal/testutil"
	"github.com/prismaport/prismaport/internal/view"
)

// raceErrors collects the errors of concurrent calls and fails the test with them, naming the
// deadlocks Postgres reported apart.
type raceErrors struct {
	mu   sync.Mutex
	errs []string
}

func (r *raceErrors) add(what string, err error) {
	if err == nil {
		return
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "40P01" {
		what = "DEADLOCK " + what
	}
	r.mu.Lock()
	r.errs = append(r.errs, fmt.Sprintf("%s: %v", what, err))
	r.mu.Unlock()
}

func (r *raceErrors) check(t *testing.T, round int) {
	t.Helper()
	if len(r.errs) > 0 {
		t.Fatalf("round %d: %v", round, r.errs)
	}
}

func payoutTo(app *apptest.App, wallet string) map[string][]service.PayoutItemInput {
	out := map[string][]service.PayoutItemInput{}
	for _, c := range app.Registry.ForMode(false) {
		if c.Family == chain.EVM {
			out[c.ID] = []service.PayoutItemInput{{WalletID: wallet, Share: "100"}}
		}
	}
	return out
}

// An API session opened while the product's payout changes never deadlocks: the session insert's
// foreign keys take KEY SHARE on the merchant and the product, the edit holds the product and
// then the merchant lock that serializes payout and wallet writes.
func TestAPISessionRacesProductPayoutEdit(t *testing.T) {
	app := apptest.New(t, apptest.Options{})
	ctx := t.Context()
	auth, err := app.Svc.Authenticate(ctx, app.TestKey)
	if err != nil {
		t.Fatal(err)
	}
	first := app.Wallet(app.TestKey, chain.EVM, "First", "0x1111111111111111111111111111111111111111")
	second := app.Wallet(app.TestKey, chain.EVM, "Second", "0x2222222222222222222222222222222222222222")
	name, price, fixed := "Seats", "25.00", "fixed"
	p, err := app.Svc.CreateProduct(ctx, auth, service.ProductInput{Name: service.Set(name), PriceType: service.Set(fixed), Amount: service.Set(price), Payout: payoutTo(app, first.ID)})
	if err != nil {
		t.Fatal(err)
	}
	const sessionsPerRound = 6
	for round := range 40 {
		var errs raceErrors
		var wg sync.WaitGroup
		wallet := first.ID
		if round%2 == 0 {
			wallet = second.ID
		}
		wg.Go(func() {
			_, err := app.Svc.UpdateProduct(ctx, auth, p.ID, service.ProductInput{Payout: payoutTo(app, wallet)})
			errs.add("payout edit", err)
		})
		for i := range sessionsPerRound {
			wg.Go(func() {
				body := fmt.Sprintf(`{"product_id":%q,"n":"%d-%d"}`, p.ID, round, i)
				_, _, err := app.Svc.CreateSession(ctx, auth, fmt.Sprintf("race-%d-%d", round, i), []byte(body), service.SessionInput{ProductID: &p.ID})
				errs.add("session", err)
			})
		}
		wg.Wait()
		errs.check(t, round)
	}
	var sessions int
	if err := app.Store.Pool().QueryRow(ctx, `SELECT count(*) FROM sessions WHERE product_id = $1`, p.ID).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if sessions != 40*sessionsPerRound {
		t.Fatalf("%d sessions, want %d", sessions, 40*sessionsPerRound)
	}
}

// Products created at the same time never deadlock: each insert takes KEY SHARE on the merchant
// through its foreign key before the payout write locks the merchant.
func TestConcurrentProductCreates(t *testing.T) {
	app := apptest.New(t, apptest.Options{})
	ctx := t.Context()
	auth, err := app.Svc.Authenticate(ctx, app.TestKey)
	if err != nil {
		t.Fatal(err)
	}
	price, fixed := "25.00", "fixed"
	for round := range 20 {
		var errs raceErrors
		var wg sync.WaitGroup
		for i := range 6 {
			wg.Go(func() {
				name := fmt.Sprintf("Seat %d-%d", round, i)
				_, err := app.Svc.CreateProduct(ctx, auth, service.ProductInput{Name: service.Set(name), PriceType: service.Set(fixed), Amount: service.Set(price)})
				errs.add("create", err)
			})
		}
		wg.Wait()
		errs.check(t, round)
	}
}

// waitForLockWaiters waits until n backends of the test database wait for a lock.
func waitForLockWaiters(t *testing.T, app *apptest.App, n int) {
	t.Helper()
	testutil.Eventually(t, 10*time.Second, fmt.Sprintf("%d backends to wait for a lock", n), func() bool {
		var waiting int
		if err := app.Store.Pool().QueryRow(t.Context(), `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		return waiting >= n
	})
}

// A session opened while a payout edit of its product is in flight waits for the edit and pays
// the payout it committed, never the one it replaced: the product is locked before it is read.
func TestSessionWaitingForPayoutEditPaysTheNewPayout(t *testing.T) {
	app := apptest.New(t, apptest.Options{})
	ctx := t.Context()
	auth, err := app.Svc.Authenticate(ctx, app.TestKey)
	if err != nil {
		t.Fatal(err)
	}
	old := app.Wallet(app.TestKey, chain.EVM, "Old", "0x1111111111111111111111111111111111111111")
	next := app.Wallet(app.TestKey, chain.EVM, "New", "0x2222222222222222222222222222222222222222")
	name, price, fixed := "Seats", "25.00", "fixed"
	p, err := app.Svc.CreateProduct(ctx, auth, service.ProductInput{Name: service.Set(name), PriceType: service.Set(fixed), Amount: service.Set(price), Payout: payoutTo(app, old.ID)})
	if err != nil {
		t.Fatal(err)
	}
	link, _, err := app.Svc.CreatePaymentLink(ctx, auth, "link", []byte(`{}`), service.LinkInput{ProductID: service.Set(p.ID)})
	if err != nil {
		t.Fatal(err)
	}

	// The edit as EditProductAs runs it: the product FOR UPDATE, then the payout rows.
	tx, err := app.Store.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM products WHERE id = $1 FOR UPDATE`, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE product_payout_items SET wallet_id = $2 WHERE product_id = $1 AND family = 'evm'`, p.ID, next.ID); err != nil {
		t.Fatal(err)
	}

	ids := make([]string, 2)
	var errs raceErrors
	var wg sync.WaitGroup
	wg.Go(func() {
		resp, _, err := app.Svc.CreateSession(ctx, auth, "api", []byte(`{}`), service.SessionInput{ProductID: &p.ID})
		errs.add("api session", err)
		var body struct {
			ID string `json:"id"`
		}
		if err == nil {
			errs.add("api session body", json.Unmarshal(resp.Body, &body))
		}
		ids[0] = body.ID
	})
	wg.Go(func() {
		id, _, err := app.Svc.OpenLinkSession(ctx, link.ID, "payer", []byte(`{}`), nil, nil)
		errs.add("link session", err)
		ids[1] = id
	})
	waitForLockWaiters(t, app, 2)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	errs.check(t, 0)

	for i, id := range ids {
		var wallets []string
		rows, err := app.Store.Pool().Query(ctx, `SELECT DISTINCT wallet_id FROM session_payouts WHERE session_id = $1 AND family = 'evm'`, id)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var w string
			if err := rows.Scan(&w); err != nil {
				t.Fatal(err)
			}
			wallets = append(wallets, w)
		}
		rows.Close()
		if len(wallets) != 1 || wallets[0] != next.ID {
			t.Errorf("session %d (%s) pays %v, want the committed payout %s (old %s)", i, id, wallets, next.ID, old.ID)
		}
	}
}

// A product edit that waits for another edit's lock judges, audits and returns the payout that
// edit committed, never the one it read before waiting: the product is locked before it is read.
func TestProductEditWaitingForPayoutEditSeesTheCommittedPayout(t *testing.T) {
	for _, tc := range []struct{ name, target string }{{"to a third wallet", "z"}, {"back to the replaced wallet", "x"}} {
		t.Run(tc.name, func(t *testing.T) {
			app := apptest.New(t, apptest.Options{})
			ctx := t.Context()
			auth, err := app.Svc.Authenticate(ctx, app.TestKey)
			if err != nil {
				t.Fatal(err)
			}
			wallets := map[string]string{
				"x": app.Wallet(app.TestKey, chain.EVM, "X", "0x1111111111111111111111111111111111111111").ID,
				"y": app.Wallet(app.TestKey, chain.EVM, "Y", "0x2222222222222222222222222222222222222222").ID,
				"z": app.Wallet(app.TestKey, chain.EVM, "Z", "0x3333333333333333333333333333333333333333").ID,
			}
			name, price, fixed := "Seats", "25.00", "fixed"
			p, err := app.Svc.CreateProduct(ctx, auth, service.ProductInput{Name: service.Set(name), PriceType: service.Set(fixed), Amount: service.Set(price),
				Payout: payoutTo(app, wallets["x"])})
			if err != nil {
				t.Fatal(err)
			}

			// Edit A as EditProductAs runs it: the product FOR UPDATE, then the payout rows, X to Y.
			tx, err := app.Store.Pool().Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if _, err := tx.Exec(ctx, `SELECT 1 FROM products WHERE id = $1 FOR UPDATE`, p.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(ctx, `UPDATE product_payout_items SET wallet_id = $2 WHERE product_id = $1 AND family = 'evm'`,
				p.ID, wallets["y"]); err != nil {
				t.Fatal(err)
			}

			var errs raceErrors
			var wg sync.WaitGroup
			var got view.Product
			wg.Go(func() {
				var err error
				got, err = app.Svc.UpdateProduct(ctx, auth, p.ID, service.ProductInput{Payout: payoutTo(app, wallets[tc.target])})
				errs.add("edit B", err)
			})
			waitForLockWaiters(t, app, 1)
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			wg.Wait()
			errs.check(t, 0)

			for id := range payoutTo(app, wallets[tc.target]) {
				if items := got.Payout[id]; len(items) != 1 || items[0].WalletID != wallets[tc.target] {
					t.Errorf("response pays %s to %+v, want %s", id, items, wallets[tc.target])
				}
			}
			var stored []string
			rows, err := app.Store.Pool().Query(ctx, `SELECT DISTINCT wallet_id FROM product_payout_items WHERE product_id = $1 AND family = 'evm'`, p.ID)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var w string
				if err := rows.Scan(&w); err != nil {
					t.Fatal(err)
				}
				stored = append(stored, w)
			}
			rows.Close()
			if len(stored) != 1 || stored[0] != wallets[tc.target] {
				t.Errorf("stored payout %v, want %s", stored, wallets[tc.target])
			}

			var details []byte
			if err := app.Store.Pool().QueryRow(ctx, `SELECT details FROM audit_log WHERE merchant_id = $1
				AND action = 'product.payout_changed' ORDER BY id DESC LIMIT 1`, app.MerchantID).Scan(&details); err != nil {
				t.Fatal(err)
			}
			var entry struct {
				Previous map[string][]struct {
					WalletID string `json:"wallet_id"`
				} `json:"previous"`
			}
			if err := json.Unmarshal(details, &entry); err != nil {
				t.Fatal(err)
			}
			for id := range payoutTo(app, wallets["y"]) {
				if items := entry.Previous[id]; len(items) != 1 || items[0].WalletID != wallets["y"] {
					t.Errorf("audit previous for %s is %+v, want the committed payout %s", id, items, wallets["y"])
				}
			}
		})
	}
}
