package store

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
	"github.com/prismaport/prismaport/internal/ids"
)

// sequencerLockKey is the advisory lock that makes the sequencer a single writer even when
// several worker processes run.
const sequencerLockKey = 0x70726973 // "pris"

// SequenceEvents assigns the next seq values to committed, unsequenced events in insertion
// order and fans each one out to the matching enabled webhook endpoints, all in one transaction.
// An endpoint disabled at that moment gets no delivery of the event, not even once enabled again.
func (s *Store) SequenceEvents(ctx context.Context, batch int) (int, error) {
	n := 0
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(sequencerLockKey)); err != nil {
			return err
		}
		var next int64
		if err := tx.QueryRow(ctx, `SELECT coalesce(max(seq), 0) + 1 FROM events`).Scan(&next); err != nil {
			return err
		}
		type pending struct {
			pk         int64
			id         string
			merchantID string
			livemode   bool
			typ        string
		}
		rows, err := tx.Query(ctx, `SELECT pk, id, merchant_id, livemode, type FROM events WHERE seq IS NULL
			ORDER BY pk LIMIT $1 FOR UPDATE`, batch)
		if err != nil {
			return err
		}
		list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (pending, error) {
			var p pending
			err := r.Scan(&p.pk, &p.id, &p.merchantID, &p.livemode, &p.typ)
			return p, err
		})
		if err != nil {
			return err
		}
		now := s.now()
		for _, e := range list {
			if _, err := tx.Exec(ctx, `UPDATE events SET seq = $2, sequenced_at = $3 WHERE pk = $1`, e.pk, next, now); err != nil {
				return err
			}
			next++
			erows, err := tx.Query(ctx, `SELECT id FROM webhook_endpoints WHERE merchant_id = $1 AND livemode = $2
				AND status = 'enabled' AND deleted_at IS NULL AND (cardinality(enabled_events) = 0 OR $3 = ANY(enabled_events)) ORDER BY id`,
				e.merchantID, e.livemode, e.typ)
			if err != nil {
				return err
			}
			endpoints, err := pgx.CollectRows(erows, pgx.RowTo[string])
			if err != nil {
				return err
			}
			for _, ep := range endpoints {
				if _, err := tx.Exec(ctx, `INSERT INTO webhook_deliveries (id, merchant_id, livemode, endpoint_id, event_id, status,
					next_attempt_at, created_at) VALUES ($1, $2, $3, $4, $5, 'pending', $6, $6) ON CONFLICT (endpoint_id, event_id) DO NOTHING`,
					ids.NewAt(ids.WebhookDelivery, now), e.merchantID, e.livemode, ep, e.id, now); err != nil {
					return err
				}
			}
		}
		n = len(list)
		return nil
	})
	return n, opErr("sequence events", err)
}

// FeedEvent is one sequenced event as delivered to the event feed.
type FeedEvent struct {
	Seq     int64
	ID      string
	Type    string
	Payload json.RawMessage
}

// ListEvents returns sequenced events after the cursor, ordered by seq. Only sequenced events
// are visible, so the feed has no holes that fill in later.
func (s *Store) ListEvents(ctx context.Context, merchantID string, livemode bool, after int64, limit int) ([]FeedEvent, bool, error) {
	rows, err := s.db.Query(ctx, `SELECT seq, id, type, payload::text FROM events
		WHERE merchant_id = $1 AND livemode = $2 AND seq IS NOT NULL AND seq > $3 ORDER BY seq LIMIT $4`,
		merchantID, livemode, after, limit+1)
	if err != nil {
		return nil, false, opErr("list events", err)
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (FeedEvent, error) {
		var e FeedEvent
		var payload string
		err := r.Scan(&e.Seq, &e.ID, &e.Type, &payload)
		e.Payload = json.RawMessage(payload)
		return e, err
	})
	if err != nil {
		return nil, false, opErr("list events", err)
	}
	more := len(list) > limit
	if more {
		list = list[:limit]
	}
	return list, more, nil
}

// LatestEventSeq is the seq of the merchant's newest sequenced event in a mode, or 0. Every event
// sequenced later has a greater seq, so it is a cursor for "from now on".
func (s *Store) LatestEventSeq(ctx context.Context, merchantID string, livemode bool) (int64, error) {
	var n int64
	err := s.db.QueryRow(ctx, `SELECT coalesce(max(seq), 0) FROM events WHERE merchant_id = $1 AND livemode = $2 AND seq IS NOT NULL`,
		merchantID, livemode).Scan(&n)
	return n, opErr("latest event seq", err)
}

// UnsequencedCount is used by health checks and tests.
func (s *Store) UnsequencedCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRow(ctx, `SELECT count(*) FROM events WHERE seq IS NULL`).Scan(&n)
	return n, opErr("unsequenced count", err)
}

// SessionEvents lists a session's events in version order (diagnostics and tests).
func (s *Store) SessionEvents(ctx context.Context, sessionID string) ([]EventRecord, error) {
	rows, err := s.db.Query(ctx, `SELECT id, type, session_version, payload::text FROM events WHERE session_id = $1
		ORDER BY session_version`, sessionID)
	if err != nil {
		return nil, opErr("session events", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (EventRecord, error) {
		var e EventRecord
		var p string
		err := r.Scan(&e.ID, &e.Type, &e.SessionVersion, &p)
		e.Payload = []byte(p)
		return e, err
	})
	return out, opErr("session events", err)
}
