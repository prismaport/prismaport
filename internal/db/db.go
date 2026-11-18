// Package db opens the Postgres pool and runs the embedded migrations.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/prismaport/prismaport/migrations"
)

// DefaultMaxConns sizes the pool when the URL has no pool_max_conns. pgx would otherwise size it
// by the host's CPU count, which differs between machines.
const DefaultMaxConns = 10

// MinWorkerConns is the smallest pool the worker starts with: its loops share four connections,
// below which they queue behind each other's queries, and the wake listener holds one more.
const MinWorkerConns = 5

// Timeouts bound the server side of a process's database work, so a hung query or a transaction
// left open fails (and the loop running it reports loop_failing) instead of stalling forever.
type Timeouts struct {
	// Statement is statement_timeout: the longest one statement may run.
	Statement time.Duration
	// IdleInTransaction is idle_in_transaction_session_timeout: the longest a transaction may sit
	// open with no statement running.
	IdleInTransaction time.Duration
}

// AppTimeouts bound the API's and the worker's connections. Every statement they run is short
// (indexed reads and writes of a few rows); a migration may run far longer and opens its pool
// without them.
var AppTimeouts = Timeouts{Statement: 30 * time.Second, IdleInTransaction: 60 * time.Second}

// Open opens a pool without timeouts: the migrator and the one-off commands.
func Open(ctx context.Context, url string) (*pgxpool.Pool, error) {
	return OpenWith(ctx, url, Timeouts{})
}

// OpenWith opens a pool whose every connection runs under t (a zero field sets nothing).
func OpenWith(ctx context.Context, url string, t Timeouts) (*pgxpool.Pool, error) {
	cfg, err := ParseConfig(url)
	if err != nil {
		return nil, err
	}
	t.apply(cfg)
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("db: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}
	return pool, nil
}

// ParseConfig reads the pool configuration from url, with DefaultMaxConns unless the url sets
// pool_max_conns.
func ParseConfig(url string) (*pgxpool.Config, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("db: parse url: %w", err)
	}
	if !strings.Contains(url, "pool_max_conns=") {
		cfg.MaxConns = DefaultMaxConns
	}
	return cfg, nil
}

// apply sets the timeouts with SET on every new connection (pgxpool AfterConnect) rather than as
// startup parameters: a pooler (Supavisor, PgBouncer) may refuse or drop startup options it does
// not know, while a SET reaches the server. The settings are session-level, so they hold for the
// connection's life on a direct connection and on a session-mode pooler (the only kind the worker
// accepts, RequireSessionConnection).
func (t Timeouts) apply(cfg *pgxpool.Config) {
	var set []string
	if t.Statement > 0 {
		set = append(set, fmt.Sprintf("SET statement_timeout = %d", t.Statement.Milliseconds()))
	}
	if t.IdleInTransaction > 0 {
		set = append(set, fmt.Sprintf("SET idle_in_transaction_session_timeout = %d", t.IdleInTransaction.Milliseconds()))
	}
	if len(set) == 0 {
		return
	}
	sql := strings.Join(set, "; ")
	prev := cfg.AfterConnect
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		if prev != nil {
			if err := prev(ctx, c); err != nil {
				return err
			}
		}
		// No arguments: one simple-protocol round trip for both.
		if _, err := c.Exec(ctx, sql); err != nil {
			return fmt.Errorf("db: set timeouts: %w", err)
		}
		return nil
	}
}

// RequireMaxConns refuses a pool configured with fewer than min connections.
func RequireMaxConns(cfg *pgxpool.Config, min int32) error {
	if n := cfg.MaxConns; n < min {
		return fmt.Errorf("db: pool_max_conns=%d is below the %d connections this process needs", n, min)
	}
	return nil
}

// RequireSessionConnection refuses a URL whose host is a transaction-mode pooler (Neon's
// "-pooler" endpoints). A session advisory lock taken through one does not stay with the client,
// so two workers could both believe they own the same chain loop.
func RequireSessionConnection(url string) error {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return fmt.Errorf("db: parse url: %w", err)
	}
	hosts := []string{cfg.ConnConfig.Host}
	for _, f := range cfg.ConnConfig.Fallbacks {
		hosts = append(hosts, f.Host)
	}
	for _, h := range hosts {
		if label, _, _ := strings.Cut(h, "."); strings.HasSuffix(label, "-pooler") {
			return fmt.Errorf("db: %s is a transaction pooler; the worker and migrations need a direct connection (the host without -pooler)", h)
		}
	}
	return nil
}

// Chain runs the goose migrations as they are: no lock and no floor on rolling back. Tests use it
// to stop at a version and check a backfill, and to roll migrations back and forth; everything
// else migrates through a Migrator.
type Chain struct {
	sqlDB    *sql.DB
	provider *goose.Provider
}

// NewChain opens the migrations of fsys (migrations.FS) on pool.
func NewChain(pool *pgxpool.Pool, fsys fs.FS) (*Chain, error) {
	sqlDB := stdlib.OpenDBFromPool(pool)
	p, err := goose.NewProvider(goose.DialectPostgres, sqlDB, fsys)
	if err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("db: goose provider: %w", err)
	}
	return &Chain{sqlDB: sqlDB, provider: p}, nil
}

// Close closes the underlying database handle.
func (c *Chain) Close() error { return c.sqlDB.Close() }

// Up applies every pending migration of the chain.
func (c *Chain) Up(ctx context.Context) error {
	_, err := c.provider.Up(ctx)
	return err
}

// UpTo applies pending migrations up to and including version v.
func (c *Chain) UpTo(ctx context.Context, v int64) error {
	_, err := c.provider.UpTo(ctx, v)
	return err
}

// DownTo rolls migrations back to version v.
func (c *Chain) DownTo(ctx context.Context, v int64) error {
	_, err := c.provider.DownTo(ctx, v)
	return err
}

// Version returns the chain's current migration version.
func (c *Chain) Version(ctx context.Context) (int64, error) {
	return c.provider.GetDBVersion(ctx)
}

// Versions lists the chain's migration versions in order.
func (c *Chain) Versions() []int64 { return versions(c.provider) }

func versions(p *goose.Provider) []int64 {
	var out []int64
	for _, s := range p.ListSources() {
		out = append(out, s.Version)
	}
	return out
}

// Migrator migrates the database to the embedded migrations: the baseline (00100) and those
// after it. Every change runs under one session advisory lock (migrate.go), so two processes
// migrating at once take turns.
type Migrator struct {
	pool  *pgxpool.Pool
	sqlDB *sql.DB
	main  *goose.Provider
	log   *slog.Logger
}

// NewMigrator returns a Migrator for the pool's database.
func NewMigrator(pool *pgxpool.Pool) (*Migrator, error) {
	sqlDB := stdlib.OpenDBFromPool(pool)
	main, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations.FS)
	if err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("db: goose provider: %w", err)
	}
	return &Migrator{pool: pool, sqlDB: sqlDB, main: main, log: slog.New(slog.DiscardHandler)}, nil
}

// SetLogger logs each migration step to l: every migration applied or rolled back.
func (m *Migrator) SetLogger(l *slog.Logger) { m.log = l }

// Close closes the underlying database handle.
func (m *Migrator) Close() error { return m.sqlDB.Close() }

// Up applies every pending migration.
func (m *Migrator) Up(ctx context.Context) error {
	return m.locked(ctx, func() error {
		if _, err := m.status(ctx); err != nil {
			return err
		}
		res, err := m.main.Up(ctx)
		m.logResults(res)
		return err
	})
}

// UpTo applies the pending migrations up to v.
func (m *Migrator) UpTo(ctx context.Context, v int64) error {
	if v < migrations.BaselineVersion {
		return fmt.Errorf("db: version %d is below the baseline; migrate to %d or later", v, migrations.BaselineVersion)
	}
	return m.locked(ctx, func() error {
		if _, err := m.status(ctx); err != nil {
			return err
		}
		res, err := m.main.UpTo(ctx, v)
		m.logResults(res)
		return err
	})
}

// ErrBelowBaseline refuses a rollback below the baseline, which builds the whole schema at once.
var ErrBelowBaseline = fmt.Errorf("db: refusing to roll back below the baseline (version %d): it would drop the whole schema; restore a backup instead",
	migrations.BaselineVersion)

// DownTo rolls back the migrations above v. It refuses v below the baseline.
func (m *Migrator) DownTo(ctx context.Context, v int64) error {
	if v < migrations.BaselineVersion {
		return ErrBelowBaseline
	}
	return m.locked(ctx, func() error {
		if _, err := m.status(ctx); err != nil {
			return err
		}
		res, err := m.main.DownTo(ctx, v)
		m.logResults(res)
		return err
	})
}

// Version is the highest migration version applied.
func (m *Migrator) Version(ctx context.Context) (int64, error) {
	st, err := m.status(ctx)
	return st.Version, err
}

// Versions lists the embedded migration versions in order: the baseline and those after it.
func (m *Migrator) Versions() []int64 { return versions(m.main) }

// logResults logs each migration goose ran.
func (m *Migrator) logResults(res []*goose.MigrationResult) {
	for _, r := range res {
		if r.Error != nil {
			continue
		}
		msg := "migrate: applied"
		if r.Direction == "down" {
			msg = "migrate: rolled back"
		}
		m.log.Info(msg, "version", r.Source.Version, "file", path.Base(r.Source.Path), "duration", r.Duration.String())
	}
}

// Migrate applies every pending migration.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	m, err := NewMigrator(pool)
	if err != nil {
		return err
	}
	defer m.Close()
	return m.Up(ctx)
}
