// Package testdb gives each integration test its own freshly migrated database on the Postgres
// named by PRISMAPORT_TEST_DATABASE_URL (see Taskfile.yml: task db:up).
package testdb

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prismaport/prismaport/internal/db"
	"github.com/prismaport/prismaport/migrations"
)

// EnvURL names the environment variable with the URL of the database integration tests use.
const EnvURL = "PRISMAPORT_TEST_DATABASE_URL"

// DefaultURL matches the container started by `task db:up`.
const DefaultURL = "postgres://prismaport:prismaport@127.0.0.1:55434/prismaport?sslmode=disable"

// templatePrefix names the migrated databases that Migrated copies (see template).
const templatePrefix = "prismaport_tmpl_"

// templateLock is the advisory lock a test process holds while it builds a template.
const templateLock = 0x70726973_6d617470

var (
	templateMu sync.Mutex
	templates  = map[string]string{} // admin URL -> the template built or found on that server
)

func adminURL() string {
	if u := os.Getenv(EnvURL); u != "" {
		return u
	}
	return DefaultURL
}

func connect(ctx context.Context, t testing.TB, admin string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatalf("testdb: connect to %s: %v (start it with `task db:up`)", admin, err)
	}
	return conn
}

func databaseURL(t testing.TB, admin, name string) string {
	t.Helper()
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatalf("testdb: parse url: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}

// Empty creates an empty database and returns its URL. It is dropped when the test ends.
func Empty(t testing.TB) string {
	t.Helper()
	u, err := create(t, "")
	if err != nil {
		t.Fatalf("testdb: create database: %v", err)
	}
	return u
}

// create makes a database, a copy of template when one is named, and drops it when the test ends.
func create(t testing.TB, template string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin := adminURL()
	conn := connect(ctx, t, admin)
	defer conn.Close(ctx)
	var b [6]byte
	_, _ = rand.Read(b[:])
	name := "prismaport_test_" + hex.EncodeToString(b[:])
	stmt := "CREATE DATABASE " + name
	if template != "" {
		stmt += " TEMPLATE " + template
	}
	if _, err := conn.Exec(ctx, stmt); err != nil {
		return "", err
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		c, err := pgx.Connect(ctx, admin)
		if err != nil {
			t.Logf("testdb: cleanup connect: %v", err)
			return
		}
		defer c.Close(ctx)
		if _, err := c.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Logf("testdb: drop %s: %v", name, err)
		}
	})
	return databaseURL(t, admin, name), nil
}

// Pool returns a pool on a new, fully migrated database.
func Pool(t testing.TB) *pgxpool.Pool {
	t.Helper()
	return Open(t, Migrated(t))
}

// Migrated creates a new, fully migrated database and returns its URL. It is a copy of a template
// migrated once (see template): a test pays a file copy, not every migration.
func Migrated(t testing.TB) string {
	t.Helper()
	u, err := create(t, template(t))
	// A run with other migrations on this server dropped the template (see template): build it
	// again.
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "3D000" {
		templateMu.Lock()
		delete(templates, adminURL())
		templateMu.Unlock()
		u, err = create(t, template(t))
	}
	if err != nil {
		t.Fatalf("testdb: create database: %v", err)
	}
	return u
}

// template returns a database that holds every migration and accepts no connections, so CREATE
// DATABASE can copy it. Its name carries a hash of the embedded migrations, so the test processes
// of one run (go test runs packages in parallel) and later runs on the same server share it, and
// changed migrations get a new one. The first process to need it builds it under an advisory lock,
// then drops the templates of other migration sets last taken over staleTemplate ago. A newer one may
// belong to a run from another branch or worktree on the same server; should one go anyway,
// Migrated builds its own again.
func template(t testing.TB) string {
	t.Helper()
	admin := adminURL()
	templateMu.Lock()
	defer templateMu.Unlock()
	if name, ok := templates[admin]; ok {
		return name
	}
	name := templatePrefix + migrationsHash(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	conn := connect(ctx, t, admin)
	// Closing the session releases the advisory lock.
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", int64(templateLock)); err != nil {
		t.Fatalf("testdb: lock: %v", err)
	}
	// Refusing connections marks a finished template: a build that died half way left its
	// database open, so it starts over.
	var ready bool
	err := conn.QueryRow(ctx, "SELECT NOT datallowconn FROM pg_database WHERE datname = $1", name).Scan(&ready)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("testdb: look up template: %v", err)
	}
	if !ready {
		if _, err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Fatalf("testdb: drop template: %v", err)
		}
		if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
			t.Fatalf("testdb: create template: %v", err)
		}
		pool, err := db.Open(ctx, databaseURL(t, admin, name))
		if err != nil {
			t.Fatalf("testdb: %v", err)
		}
		err = db.Migrate(ctx, pool)
		pool.Close()
		if err != nil {
			t.Fatalf("testdb: migrate: %v", err)
		}
		if _, err := conn.Exec(ctx, "ALTER DATABASE "+name+" WITH ALLOW_CONNECTIONS false"); err != nil {
			t.Fatalf("testdb: close template: %v", err)
		}
		dropStaleTemplates(ctx, t, conn, name)
	}
	// The comment holds when a run last took the template (Unix seconds), which decides when it is
	// stale for a run with other migrations.
	if _, err := conn.Exec(ctx, fmt.Sprintf("COMMENT ON DATABASE %s IS '%d'", name, time.Now().Unix())); err != nil {
		t.Fatalf("testdb: date template: %v", err)
	}
	templates[admin] = name
	return name
}

// staleTemplate is how long a template of other migrations is kept after a run last took it.
const staleTemplate = 24 * time.Hour

// dropStaleTemplates drops the templates other than keep last taken over staleTemplate ago, or
// never dated (one whose build died). A drop that fails is left for the next build.
func dropStaleTemplates(ctx context.Context, t testing.TB, conn *pgx.Conn, keep string) {
	t.Helper()
	rows, err := conn.Query(ctx, `SELECT datname, coalesce(shobj_description(oid, 'pg_database'), '')
		FROM pg_database WHERE datname LIKE 'prismaport\_tmpl\_%' AND datname <> $1`, keep)
	if err != nil {
		t.Fatalf("testdb: list templates: %v", err)
	}
	type tmpl struct{ name, built string }
	others, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (tmpl, error) {
		var o tmpl
		return o, r.Scan(&o.name, &o.built)
	})
	if err != nil {
		t.Fatalf("testdb: list templates: %v", err)
	}
	for _, o := range others {
		if built, err := strconv.ParseInt(o.built, 10, 64); err == nil && time.Since(time.Unix(built, 0)) < staleTemplate {
			continue
		}
		_, _ = conn.Exec(ctx, "DROP DATABASE IF EXISTS "+o.name)
	}
}

// migrationsHash identifies the embedded migrations: every file of migrations.FS.
func migrationsHash(t testing.TB) string {
	t.Helper()
	h := sha256.New()
	err := fs.WalkDir(migrations.FS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(migrations.FS, path)
		if err != nil {
			return err
		}
		h.Write([]byte(path + "\x00"))
		h.Write(data)
		return nil
	})
	if err != nil {
		t.Fatalf("testdb: hash migrations: %v", err)
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// Open opens a pool on url, closed when the test ends.
func Open(t testing.TB, url string) *pgxpool.Pool {
	t.Helper()
	pool, err := db.Open(context.Background(), url)
	if err != nil {
		t.Fatalf("testdb: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}
