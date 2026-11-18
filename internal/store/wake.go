package store

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// WakeChannel is the channel migration 00118's triggers notify: the payload names what changed
// ("sessions", "events", "session_reveals", "recheck").
const WakeChannel = "prismaport_wake"

// WakeApplicationName names the listening connection in pg_stat_activity.
const WakeApplicationName = "prismaport-wake"

// WakeConfig paces ListenWake; zero values get the defaults.
type WakeConfig struct {
	// Ping is how long the connection may stay silent before it is pinged (default 1 minute), so
	// a connection a pooler dropped without a word is noticed.
	Ping time.Duration
	// MinBackoff and MaxBackoff bound the wait before listening again after a failure (default
	// 1 second doubling up to 1 minute).
	MinBackoff, MaxBackoff time.Duration
}

// ListenWake holds one connection of the store's pool with LISTEN prismaport_wake until ctx ends
// and calls wake with each notification's payload. The connection is the pool's, so it counts
// in pool_max_conns (it must be a session connection: a transaction pooler does not keep a
// LISTEN). When it fails it is dropped and taken again after a backoff, without stopping the
// caller; after each LISTEN but the first, wake("") reports that notifications may have been
// missed meanwhile.
func (s *Store) ListenWake(ctx context.Context, cfg WakeConfig, wake func(payload string), log *slog.Logger) {
	if cfg.Ping <= 0 {
		cfg.Ping = time.Minute
	}
	if cfg.MinBackoff <= 0 {
		cfg.MinBackoff = time.Second
	}
	if cfg.MaxBackoff < cfg.MinBackoff {
		cfg.MaxBackoff = max(time.Minute, cfg.MinBackoff)
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	backoff := cfg.MinBackoff
	first := true
	for ctx.Err() == nil {
		var since time.Time
		listening, err := s.listenOnce(ctx, cfg.Ping, func() {
			// The first LISTEN needs no wake: every loop ticks when the worker starts.
			if !first {
				wake("")
			}
			first = false
			since = time.Now()
		}, wake)
		if ctx.Err() != nil {
			return
		}
		// A connection that lived a ping interval resets the backoff; one dropped right after its
		// LISTEN does not, so a pooler that keeps dropping it is not asked again every second.
		if listening && time.Since(since) >= cfg.Ping {
			backoff = cfg.MinBackoff
		}
		if listening {
			log.Warn("wake listener lost; idle intervals apply until it listens again", "err", err, "retry_in", backoff.String())
		} else {
			log.Warn("wake listener could not listen", "err", err, "retry_in", backoff.String())
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, cfg.MaxBackoff)
	}
}

// listenOnce takes a pool connection, listens and waits for notifications until the connection
// fails or ctx ends; listening reports whether LISTEN succeeded. The connection is closed before
// it goes back to the pool, so the pool never hands out one still listening.
func (s *Store) listenOnce(ctx context.Context, ping time.Duration, listened func(), wake func(string)) (listening bool, err error) {
	pc, err := s.pool.Acquire(ctx)
	if err != nil {
		return false, err
	}
	conn := pc.Conn()
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = conn.Close(closeCtx)
		cancel()
		pc.Release()
	}()
	if _, err := conn.Exec(ctx, "SET application_name = '"+WakeApplicationName+"'; LISTEN "+WakeChannel); err != nil {
		return false, err
	}
	listened()
	for {
		wctx, cancel := context.WithTimeout(ctx, ping)
		n, err := conn.WaitForNotification(wctx)
		cancel()
		switch {
		case err == nil:
			wake(n.Payload)
		case ctx.Err() != nil:
			return true, ctx.Err()
		case errors.Is(err, context.DeadlineExceeded) && !conn.IsClosed():
			// Silent for a whole ping interval: make sure the connection is still there.
			pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := conn.Ping(pctx)
			cancel()
			if err != nil {
				return true, err
			}
		default:
			return true, err
		}
	}
}
