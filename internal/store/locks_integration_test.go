//go:build integration

package store_test

import (
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/prismaport/prismaport/internal/store"
	"github.com/prismaport/prismaport/internal/testdb"
)

func newLocker(t *testing.T, url string) *store.Locker {
	t.Helper()
	l, err := store.NewLocker(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Close)
	return l
}

// killHolder terminates the backend holding the named lock, as a database restart or a dropped
// connection would, and waits until it is gone.
func killHolder(t *testing.T, admin *pgx.Conn, name string) {
	t.Helper()
	key := uint64(store.LockKey(name))
	var killed bool
	err := admin.QueryRow(t.Context(), `SELECT pg_terminate_backend(pid, 5000) FROM pg_locks
		WHERE locktype = 'advisory' AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
		AND classid = $1::bigint::oid AND objid = $2::bigint::oid AND objsubid = 1`, int64(key>>32), int64(key&0xffffffff)).Scan(&killed)
	if err != nil || !killed {
		t.Fatalf("terminate the holder of %s: %v %v", name, killed, err)
	}
}

func hold(t *testing.T, l *store.Locker, name string) bool {
	t.Helper()
	ok, err := l.Hold(t.Context(), name)
	if err != nil {
		t.Fatalf("hold %s: %v", name, err)
	}
	return ok
}

func TestLockerIsExclusiveAcrossWorkers(t *testing.T) {
	t.Parallel()
	url := testdb.Empty(t) + "&pool_max_conns=4"
	a, b := newLocker(t, url), newLocker(t, url)

	if !hold(t, a, "sender:base") {
		t.Fatal("free lock refused")
	}
	if hold(t, b, "sender:base") {
		t.Fatal("lock taken by two workers")
	}
	if !hold(t, b, "sender:base-sepolia") {
		t.Fatal("independent lock refused")
	}
	if !hold(t, a, "sender:base") {
		t.Fatal("held lock not reported as held")
	}
	a.Close()
	if !hold(t, b, "sender:base") {
		t.Fatal("closing did not release the lock")
	}
}

func TestLockerDropsEveryLockWhenItsConnectionDies(t *testing.T) {
	t.Parallel()
	url := testdb.Empty(t)
	ctx := t.Context()
	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	a, b := newLocker(t, url), newLocker(t, url)
	if !hold(t, a, "sender:base") || !hold(t, a, "detector:base") {
		t.Fatal("free locks refused")
	}

	killHolder(t, admin, "sender:base")

	if ok, err := a.Hold(ctx, "sender:base"); ok || err == nil {
		t.Fatalf("lost lock reported as held: %v %v", ok, err)
	}
	if !hold(t, b, "sender:base") || !hold(t, b, "detector:base") {
		t.Fatal("locks of the dead connection were not freed")
	}
	if hold(t, a, "sender:base") || hold(t, a, "detector:base") {
		t.Fatal("reconnected locker took locks another worker holds")
	}
	b.Close()
	if !hold(t, a, "sender:base") {
		t.Fatal("locker did not take the lock back once it was free")
	}
}
