// Package store is the Postgres persistence layer. Money crosses the boundary as base-unit
// integer strings (cast to/from numeric in SQL), never as floats.
package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prismaport/prismaport/internal/money"
)

// Sentinel errors returned by store methods.
var (
	ErrNotFound            = errors.New("store: not found")
	ErrIdempotencyConflict = errors.New("store: idempotency key reused with a different request")
	ErrConflict            = errors.New("store: conflicting write")
)

// opErr names the store operation that failed, so a log line says which query broke. An error
// that already carries store context (a sentinel, an error built here, or one an inner store
// call wrapped) passes through unchanged: errors.Is still matches and the prefix never repeats.
// A nil error stays nil.
func opErr(op string, err error) error {
	if err == nil || strings.HasPrefix(err.Error(), "store: ") {
		return err
	}
	return fmt.Errorf("store: %s: %w", op, err)
}

// EventMeta identifies an event while its payload is rendered.
type EventMeta struct {
	ID        string
	Type      string
	CreatedAt time.Time
}

// Renderer turns a session snapshot into the event envelope stored in the outbox. The stored
// bytes are what the feed serves and what webhooks sign.
type Renderer interface {
	EventPayload(meta EventMeta, s *Session) ([]byte, error)
}

// dbtx is what both the pool and a transaction offer; a Store bound to a transaction runs every
// query on it.
type dbtx interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Store is the Postgres-backed data layer; it runs on a pool or, once bound, inside a transaction.
type Store struct {
	pool     *pgxpool.Pool
	db       dbtx
	renderer Renderer
	now      func() time.Time
	flush    *flushPasses
}

// New returns a Store on the given pool that renders event payloads with r.
func New(pool *pgxpool.Pool, r Renderer) *Store {
	return (&Store{pool: pool, db: pool, renderer: r, flush: newFlushPasses()}).WithClock(time.Now)
}

// Bind returns a store whose reads and writes run inside tx (nested transactions become
// savepoints). Code holding a transaction uses it instead of the pool: waiting for a second
// pool connection while holding a lock can deadlock once every connection waits the same way.
func (s *Store) Bind(tx pgx.Tx) *Store {
	c := *s
	c.db = tx
	return &c
}

// WithClock replaces the clock; tests use it to make time deterministic. Times are truncated
// to the microsecond precision Postgres stores, so values read back compare equal.
func (s *Store) WithClock(now func() time.Time) *Store {
	c := *s
	c.now = func() time.Time { return now().UTC().Truncate(time.Microsecond) }
	return &c
}

// Now is the store clock.
func (s *Store) Now() time.Time { return s.now() }

// Pool returns the underlying connection pool.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

func (s *Store) inTx(ctx context.Context, fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, s.db, fn)
}

func parseUnits(col, v string) (money.Units, error) {
	u, err := money.ParseInteger(v)
	if err != nil {
		return money.Zero, fmt.Errorf("store: column %s holds %q: %w", col, v, err)
	}
	return u, nil
}

func parseOptUnits(col string, v *string) (*money.Units, error) {
	if v == nil {
		return nil, nil
	}
	u, err := parseUnits(col, *v)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func optUnitsArg(u *money.Units) *string {
	if u == nil {
		return nil
	}
	s := u.String()
	return &s
}

func unitsArrayArg(us []money.Units) []string {
	if us == nil {
		return nil
	}
	out := make([]string, len(us))
	for i, u := range us {
		out[i] = u.String()
	}
	return out
}

func metadataArg(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func isUniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

func isForeignKeyViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23503"
}

func notFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
