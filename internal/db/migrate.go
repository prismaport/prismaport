package db

import (
	"context"
	"fmt"
	"hash/fnv"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prismaport/prismaport/migrations"
)

// migrateLockKey is the session advisory lock a Migrator holds while it changes the schema.
var migrateLockKey = func() int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("prismaport:migrate"))
	return int64(h.Sum64())
}()

// lockRetry is how often a Migrator that found the migration lock taken asks for it again.
const lockRetry = 250 * time.Millisecond

// locked runs fn holding the migration lock. The lock lives on a connection taken out of the pool
// and closed at the end, which releases it; fn's own queries use the pool.
//
// A Migrator that finds the lock taken asks again every lockRetry rather than blocking in
// pg_advisory_lock: a statement blocked there keeps its snapshot, and CREATE INDEX CONCURRENTLY
// (migrations after the baseline) waits for every older snapshot in the database, so the holder's
// index build and the waiter would wait for each other forever.
func (m *Migrator) locked(ctx context.Context, fn func() error) error {
	c, err := m.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("db: migration lock: %w", err)
	}
	conn := c.Hijack()
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	for waited := false; ; waited = true {
		var free bool
		if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, migrateLockKey).Scan(&free); err != nil {
			return fmt.Errorf("db: migration lock: %w", err)
		}
		if free {
			break
		}
		if !waited {
			m.log.Info("migrate: waiting for another migrator to finish")
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("db: migration lock: %w", ctx.Err())
		case <-time.After(lockRetry):
		}
	}
	return fn()
}

// Status is where a database stands against the embedded migrations.
type Status struct {
	// Version is the highest version applied, 0 for an empty database.
	Version int64
	// Pending lists the migrations Up applies.
	Pending []int64
}

func (s Status) String() string {
	pending := "up to date"
	if len(s.Pending) > 0 {
		pending = "pending " + versionList(s.Pending)
	}
	if s.Version == 0 {
		return "empty: `migrate up` applies the baseline; " + pending
	}
	return fmt.Sprintf("baseline chain (from version %d); %s", migrations.BaselineVersion, pending)
}

// versionList prints versions as ranges of consecutive ones: 101-117, or 101, 103-117.
func versionList(vs []int64) string {
	var parts []string
	for i := 0; i < len(vs); {
		j := i
		for j+1 < len(vs) && vs[j+1] == vs[j]+1 {
			j++
		}
		if j > i {
			parts = append(parts, strconv.FormatInt(vs[i], 10)+"-"+strconv.FormatInt(vs[j], 10))
		} else {
			parts = append(parts, strconv.FormatInt(vs[i], 10))
		}
		i = j + 1
	}
	return strings.Join(parts, ", ")
}

// Status reports the database's version and the migrations still to apply. It changes nothing
// and takes no lock.
func (m *Migrator) Status(ctx context.Context) (Status, error) { return m.status(ctx) }

func (m *Migrator) status(ctx context.Context) (Status, error) {
	applied, err := appliedVersions(ctx, m.pool)
	if err != nil {
		return Status{}, err
	}
	st := Status{}
	if len(applied) > 0 {
		st.Version = applied[len(applied)-1]
	}
	if below := slices.DeleteFunc(slices.Clone(applied), func(v int64) bool { return v >= migrations.BaselineVersion }); len(below) > 0 {
		return st, fmt.Errorf("db: goose_db_version holds versions %s, below the baseline (%d); this binary only migrates databases created from the baseline",
			versionList(below), migrations.BaselineVersion)
	}
	st.Pending = missing(versions(m.main), applied)
	return st, nil
}

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// appliedVersions lists the versions in goose_db_version, ascending, without goose's version 0
// row; none when the table does not exist yet.
func appliedVersions(ctx context.Context, q querier) ([]int64, error) {
	var exists bool
	if err := q.QueryRow(ctx, `SELECT to_regclass('goose_db_version') IS NOT NULL`).Scan(&exists); err != nil {
		return nil, fmt.Errorf("db: read goose_db_version: %w", err)
	}
	if !exists {
		return nil, nil
	}
	rows, err := q.Query(ctx, `SELECT version_id, is_applied FROM goose_db_version WHERE version_id <> 0 ORDER BY version_id`)
	if err != nil {
		return nil, fmt.Errorf("db: read goose_db_version: %w", err)
	}
	var out []int64
	for rows.Next() {
		var v int64
		var ok bool
		if err := rows.Scan(&v, &ok); err != nil {
			rows.Close()
			return nil, err
		}
		if !ok {
			rows.Close()
			return nil, fmt.Errorf("db: goose_db_version marks version %d as not applied; this goose never writes such rows, so remove them first", v)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// missing lists the versions of chain not in applied.
func missing(chain, applied []int64) []int64 {
	var out []int64
	for _, v := range chain {
		if !slices.Contains(applied, v) {
			out = append(out, v)
		}
	}
	return out
}
