//go:build integration

package db_test

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prismaport/prismaport/internal/db"
	"github.com/prismaport/prismaport/internal/testdb"
	"github.com/prismaport/prismaport/migrations"
)

// lockedBuffer collects one migrator's log lines.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func newMigrator(t *testing.T, pool *pgxpool.Pool) (*db.Migrator, *lockedBuffer) {
	t.Helper()
	m, err := db.NewMigrator(pool)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	logs := &lockedBuffer{}
	m.SetLogger(slog.New(slog.NewTextHandler(logs, nil)))
	return m, logs
}

// gooseVersions lists the versions in goose_db_version, without the version 0 row.
func gooseVersions(t *testing.T, pool *pgxpool.Pool) []int64 {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT version_id FROM goose_db_version WHERE version_id <> 0 ORDER BY version_id`)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestFreshDatabaseAppliesTheBaseline: an empty database gets the baseline and every migration
// after it; a second Up on the migrated database changes nothing.
func TestFreshDatabaseAppliesTheBaseline(t *testing.T) {
	ctx := t.Context()
	pool := testdb.Open(t, testdb.Empty(t))
	m, logs := newMigrator(t, pool)
	st, err := m.Status(ctx)
	if err != nil || st.Version != 0 || !slices.Equal(st.Pending, m.Versions()) {
		t.Fatalf("status of an empty database: %+v %v", st, err)
	}
	if err := m.Up(ctx); err != nil {
		t.Fatal(err)
	}
	if v := m.Versions(); v[0] != migrations.BaselineVersion {
		t.Fatalf("first embedded version %d, want the baseline", v[0])
	}
	if got := gooseVersions(t, pool); !slices.Equal(got, m.Versions()) {
		t.Fatalf("goose_db_version %v, want %v", got, m.Versions())
	}
	last := m.Versions()[len(m.Versions())-1]
	if v, err := m.Version(ctx); err != nil || v != last {
		t.Fatalf("version %d %v", v, err)
	}
	if !strings.Contains(logs.String(), fmt.Sprintf("version=%d", migrations.BaselineVersion)) {
		t.Fatalf("the baseline was not logged:\n%s", logs)
	}

	schema := readSchema(t, pool)
	again, againLogs := newMigrator(t, pool)
	if err := again.Up(ctx); err != nil {
		t.Fatal(err)
	}
	if st, err := again.Status(ctx); err != nil || st.Version != last || len(st.Pending) != 0 {
		t.Fatalf("status after a second up: %+v %v", st, err)
	}
	if strings.Contains(againLogs.String(), "migrate: applied") {
		t.Fatalf("a migrated database was migrated again:\n%s", againLogs)
	}
	if diff := db.DiffSchemas(schema, readSchema(t, pool)); diff != "" {
		t.Fatalf("a second up changed the schema:\n%s", diff)
	}
}

// TestConcurrentMigrators: two migrators (two replicas deploying at once) on one empty database.
// Both wait while another holds the lock; then one migrates and the other finds nothing to do.
// The first also builds the indexes after the baseline CONCURRENTLY while the second waits: the
// wait must not hold a snapshot the index build waits for, or neither finishes.
func TestConcurrentMigrators(t *testing.T) {
	ctx := t.Context()
	url := testdb.Empty(t)
	pool := testdb.Open(t, url)

	// A third party holds the lock first: both wait.
	holder, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close(ctx)
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_lock($1)`, db.MigrateLockKey); err != nil {
		t.Fatal(err)
	}

	var ms []*db.Migrator
	var logs []*lockedBuffer
	for range 2 {
		m, l := newMigrator(t, testdb.Open(t, url))
		ms, logs = append(ms, m), append(logs, l)
	}
	errs := make(chan error, len(ms))
	for _, m := range ms {
		go func() { errs <- m.Up(ctx) }()
	}
	select {
	case err := <-errs:
		t.Fatalf("a migrator ran while another held the lock: %v", err)
	case <-time.After(700 * time.Millisecond):
	}
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('merchants') IS NOT NULL`).Scan(&exists); err != nil || exists {
		t.Fatalf("migrated while the lock was held: %v %v", exists, err)
	}
	for _, l := range logs {
		if !strings.Contains(l.String(), "waiting for another migrator") {
			t.Errorf("no wait logged:\n%s", l)
		}
	}
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_unlock($1)`, db.MigrateLockKey); err != nil {
		t.Fatal(err)
	}
	for range ms {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	applied := 0
	for _, l := range logs {
		applied += strings.Count(l.String(), fmt.Sprintf("version=%d", migrations.BaselineVersion))
	}
	if applied != 1 {
		t.Fatalf("baseline applied %d times:\n%s\n---\n%s", applied, logs[0], logs[1])
	}
	if got, want := gooseVersions(t, pool), ms[0].Versions(); !slices.Equal(got, want) {
		t.Fatalf("goose_db_version %v, want %v", got, want)
	}
}

// TestNoRollbackBelowTheBaseline: rolling back below the baseline is refused, and so is
// migrating to a version below it.
func TestNoRollbackBelowTheBaseline(t *testing.T) {
	ctx := t.Context()
	pool := testdb.Pool(t)
	m, _ := newMigrator(t, pool)
	for _, v := range []int64{0, 46, migrations.BaselineVersion - 1} {
		if err := m.DownTo(ctx, v); !errors.Is(err, db.ErrBelowBaseline) {
			t.Fatalf("down to %d: %v", v, err)
		}
	}
	if err := m.UpTo(ctx, 46); err == nil {
		t.Fatal("up to a version below the baseline accepted")
	}
	if !slices.Contains(gooseVersions(t, pool), migrations.BaselineVersion) {
		t.Fatal("baseline row gone")
	}
	if !strings.Contains(readSchema(t, pool), "table merchants\n") {
		t.Fatal("schema gone")
	}
}

// TestRefusesVersionsBelowTheBaseline: a goose_db_version holding a version below the baseline
// is not migrated, rolled back or guessed at.
func TestRefusesVersionsBelowTheBaseline(t *testing.T) {
	ctx := t.Context()
	pool := testdb.Pool(t)
	exec(t, pool, `INSERT INTO goose_db_version (version_id, is_applied) VALUES (46, true)`)
	m, _ := newMigrator(t, pool)
	if err := m.Up(ctx); err == nil || !strings.Contains(err.Error(), "below the baseline") {
		t.Fatalf("up: %v", err)
	}
	if err := m.DownTo(ctx, migrations.BaselineVersion); err == nil || !strings.Contains(err.Error(), "below the baseline") {
		t.Fatalf("down: %v", err)
	}
	if _, err := m.Status(ctx); err == nil {
		t.Fatal("status accepted it")
	}
}
