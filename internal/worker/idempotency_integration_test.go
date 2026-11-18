//go:build integration

package worker_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/worker"
)

// Idempotency keys older than IdempotencyTTL are deleted in batches (ADR-0008 §F-6): a full batch
// runs again at once, the last partial one ends the pass. A key inside the TTL still replays its
// response; a purged one runs as a new request.
func TestIdempotencyKeysOlderThanTheTTLArePurgedInBatches(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	ctx := t.Context()
	open := func(key string) (string, bool) {
		t.Helper()
		r := app.Session(app.TestKey, `{"amount":"5.00"}`, "Idempotency-Key", key)
		var s struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(r.Body, &s); err != nil || r.Status != 200 {
			t.Fatalf("session %s: %d %s", key, r.Status, r.Body)
		}
		return s.ID, r.Header.Get("Idempotent-Replayed") == "true"
	}
	const ttl = 7 * 24 * time.Hour
	oldID, _ := open("old")
	// A backlog of expired keys, as a worker that never purged would leave.
	if _, err := app.Store.Pool().Exec(ctx, `INSERT INTO idempotency_keys (merchant_id, livemode, key, request_hash, response_status,
			response_body, created_at)
		SELECT $1, false, 'backlog-' || g, sha256(g::text::bytea), 200, '{}', $2 FROM generate_series(1, 2500) g`,
		app.MerchantID, app.Clock.Now()); err != nil {
		t.Fatal(err)
	}
	app.Clock.Advance(ttl - time.Hour)
	keptID, _ := open("kept")
	app.Clock.Advance(time.Hour + time.Second)

	var expired int
	if err := app.Store.Pool().QueryRow(ctx, `SELECT count(*) FROM idempotency_keys WHERE created_at < $1`,
		app.Clock.Now().Add(-ttl)).Scan(&expired); err != nil || expired < 2501 {
		t.Fatalf("expired keys %d %v", expired, err)
	}
	w := worker.New(app.Svc, app.Store, app.Dispatcher, worker.Config{IdempotencyTTL: ttl, IdempotencyBatch: 1000}, nil)
	full := 0
	for {
		n, err := w.PurgeIdempotencyKeys(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 { // a partial batch ends the pass
			break
		}
		if n != 1000 {
			t.Fatalf("a batch deleted %d keys, want 1000", n)
		}
		full++
	}
	if full != expired/1000 {
		t.Fatalf("%d full batches for %d expired keys", full, expired)
	}
	var left []string
	rows, err := app.Store.Pool().Query(ctx, `SELECT key FROM idempotency_keys WHERE merchant_id = $1 ORDER BY key`, app.MerchantID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatal(err)
		}
		left = append(left, k)
	}
	if rows.Err() != nil || len(left) != 1 || left[0] != "kept" { // created inside the TTL
		t.Fatalf("keys left %v %v, want [kept]", left, rows.Err())
	}
	if n, err := w.PurgeIdempotencyKeys(ctx); err != nil || n != 0 {
		t.Fatalf("nothing left to purge: %d %v", n, err)
	}
	if id, replayed := open("kept"); id != keptID || !replayed {
		t.Fatalf("a key inside the TTL: %s replayed=%v, want %s replayed", id, replayed, keptID)
	}
	if id, replayed := open("old"); id == oldID || replayed {
		t.Fatalf("a purged key: %s replayed=%v, want a new session", id, replayed)
	}
}
