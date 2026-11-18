package store

import (
	"context"
	"fmt"
	"hash/fnv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// LockerApplicationName names the lock connection in pg_stat_activity.
const LockerApplicationName = "prismaport-locks"

// lockTimeout bounds every statement on the lock connection, so a silently dropped connection
// is noticed instead of blocking every locked loop.
const lockTimeout = 10 * time.Second

// Locker holds this process's Postgres session advisory locks, which make one worker the single
// writer for a chain loop even when several workers run. All of them live on one connection
// opened outside the pool: a held lock never takes a connection from the queries, however many
// loops there are. When that connection fails, Postgres has released every lock it held; the
// locker forgets them all and takes them again, on a new connection, once they are free.
type Locker struct {
	cfg  *pgx.ConnConfig
	mu   sync.Mutex
	conn *pgx.Conn
	held map[int64]bool
}

// NewLocker prepares a locker on the database at url; it connects on first use.
func NewLocker(url string) (*Locker, error) {
	// The pool parser drops the pool_* parameters, which a plain connection would send to the
	// server as unknown settings.
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("store: parse lock url: %w", err)
	}
	cfg.ConnConfig.RuntimeParams["application_name"] = LockerApplicationName
	return &Locker{cfg: cfg.ConnConfig, held: map[int64]bool{}}, nil
}

func lockKey(name string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("prismaport:" + name))
	return int64(h.Sum64())
}

// Hold reports whether this process holds the named lock, taking it when it is free. A lock
// already held is verified on its connection first, so true means the lock was held at the time
// of the call. Any error drops the connection and with it every lock.
func (l *Locker) Hold(ctx context.Context, name string) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, lockTimeout)
	defer cancel()
	if l.conn == nil {
		conn, err := pgx.ConnectConfig(ctx, l.cfg.Copy())
		if err != nil {
			return false, fmt.Errorf("store: lock connection: %w", err)
		}
		l.conn = conn
	}
	key := lockKey(name)
	if l.held[key] {
		if err := l.conn.Ping(ctx); err != nil {
			l.drop()
			return false, fmt.Errorf("store: lock connection lost, %s released: %w", name, err)
		}
		return true, nil
	}
	var ok bool
	if err := l.conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&ok); err != nil {
		l.drop()
		return false, fmt.Errorf("store: take lock %s: %w", name, err)
	}
	if ok {
		l.held[key] = true
	}
	return ok, nil
}

// Close releases every lock and closes the connection. The explicit unlock frees the locks
// before Close returns; a closed socket alone frees them only once the server notices.
func (l *Locker) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conn != nil {
		ctx, cancel := context.WithTimeout(context.Background(), lockTimeout)
		_, _ = l.conn.Exec(ctx, `SELECT pg_advisory_unlock_all()`)
		cancel()
	}
	l.drop()
}

func (l *Locker) drop() {
	if l.conn != nil {
		ctx, cancel := context.WithTimeout(context.Background(), lockTimeout)
		_ = l.conn.Close(ctx)
		cancel()
		l.conn = nil
	}
	clear(l.held)
}
