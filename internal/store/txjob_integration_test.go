//go:build integration

package store_test

import (
	"errors"
	"testing"

	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/store"
)

// A tx job is written only as it was read (ADR-0008 §A-4): a worker that lost the chain lock
// mid-step and still holds the job as it read it cannot overwrite what the lock holder wrote
// since, even when both writes happen at the same clock reading. The writer that succeeded can
// keep writing its copy.
func TestUpdateTxJobIsFencedAgainstAStaleCopy(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	ctx := t.Context()
	id := newSession(t, app, "5.00", "fence")
	j, _, err := app.Store.InsertTxJob(ctx, store.TxJob{Chain: "base-sepolia", Kind: store.JobSettle, Ref: "payer|nonce", SessionID: id,
		Asset: "USDC", To: "0x01", Calldata: []byte{1}})
	if err != nil {
		t.Fatal(err)
	}
	holder, stale := j, j
	nonce := uint64(7)
	holder.Status, holder.Nonce, holder.TxHashes = store.JobSubmitted, &nonce, []string{"0xaa"}
	if err := app.Store.UpdateTxJob(ctx, &holder); err != nil {
		t.Fatal(err)
	}
	other := uint64(8)
	stale.Status, stale.Nonce, stale.TxHashes = store.JobSubmitted, &other, []string{"0xbb"}
	if err := app.Store.UpdateTxJob(ctx, &stale); !errors.Is(err, store.ErrTxJobChanged) {
		t.Fatalf("stale write: %v", err)
	}
	holder.TxHashes = append(holder.TxHashes, "0xcc")
	if err := app.Store.UpdateTxJob(ctx, &holder); err != nil {
		t.Fatalf("second write of the holder: %v", err)
	}
	got, err := app.Store.GetTxJob(ctx, j.ID)
	if err != nil || got.Nonce == nil || *got.Nonce != 7 || len(got.TxHashes) != 2 || got.TxHashes[0] != "0xaa" ||
		!got.UpdatedAt.Equal(holder.UpdatedAt) || !got.UpdatedAt.After(j.UpdatedAt) {
		t.Fatalf("job %+v %v", got, err)
	}
	missing := got
	missing.ID = "txj_missing"
	if err := app.Store.UpdateTxJob(ctx, &missing); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing job: %v", err)
	}
}
