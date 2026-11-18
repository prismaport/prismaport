package store

import (
	"context"
	"time"
)

// AddressCursor is how far the detector read a customer address's history on one EVM chain.
type AddressCursor struct {
	// Block is the last block every transfer into the address was read up to; nil when nothing
	// was read yet (reading then starts where the address could first receive anything).
	Block     *uint64
	CreatedAt time.Time
}

// AddressCursors reads the cursors of customer addresses on a chain, with each address's creation
// time.
func (s *Store) AddressCursors(ctx context.Context, chainID string, addressIDs []string) (map[string]AddressCursor, error) {
	rows, err := s.db.Query(ctx, `SELECT a.id, a.created_at, c.block_number FROM customer_addresses a
		LEFT JOIN customer_address_cursors c ON c.customer_address_id = a.id AND c.chain = $1 WHERE a.id = ANY($2)`, chainID, addressIDs)
	if err != nil {
		return nil, opErr("address cursors", err)
	}
	out := map[string]AddressCursor{}
	for rows.Next() {
		var id string
		var c AddressCursor
		var block *int64
		if err := rows.Scan(&id, &c.CreatedAt, &block); err != nil {
			rows.Close()
			return nil, opErr("address cursors", err)
		}
		if block != nil {
			b := uint64(*block)
			c.Block = &b
		}
		out[id] = c
	}
	return out, opErr("address cursors", rows.Err())
}

// SetAddressCursor records that the address's history was read up to block by its current
// session (sessionID). The cursor never moves back here, and an earlier session's reading does not
// move it: a newer session reads from the cursor again.
func (s *Store) SetAddressCursor(ctx context.Context, chainID, addressID, sessionID string, block uint64) error {
	_, err := s.db.Exec(ctx, `INSERT INTO customer_address_cursors (customer_address_id, chain, block_number, updated_at)
		SELECT $1, $2, $4, $5 FROM customer_addresses WHERE id = $1 AND current_session_id = $3
		ON CONFLICT (customer_address_id, chain) DO UPDATE SET block_number = EXCLUDED.block_number, updated_at = EXCLUDED.updated_at
			WHERE customer_address_cursors.block_number < EXCLUDED.block_number`, addressID, chainID, sessionID, int64(block), s.now())
	return opErr("set address cursor", err)
}

// AdvanceAddressCursors moves the cursors of the given (address, session) pairs to to after the
// range [from, to] was read for them: only a cursor that already reached from-1 (no gap is ever
// skipped) and only while the session is still the address's current one.
func (s *Store) AdvanceAddressCursors(ctx context.Context, chainID string, addressIDs, sessionIDs []string, from, to uint64) error {
	if len(addressIDs) == 0 {
		return nil
	}
	_, err := s.db.Exec(ctx, `UPDATE customer_address_cursors c SET block_number = $5, updated_at = $6
		FROM unnest($2::text[], $3::text[]) AS t(aid, sid) JOIN customer_addresses a ON a.id = t.aid
		WHERE c.customer_address_id = t.aid AND c.chain = $1 AND a.current_session_id = t.sid
			AND c.block_number >= $4::bigint - 1 AND c.block_number < $5`, chainID, addressIDs, sessionIDs, int64(from), int64(to), s.now())
	return opErr("advance address cursors", err)
}

// RewindAddressCursors moves every cursor of the chain above keep back to keep: the blocks above
// it are read again (a reorg, or a payment found there that turned out not canonical).
func (s *Store) RewindAddressCursors(ctx context.Context, chainID string, keep uint64) error {
	_, err := s.db.Exec(ctx, `UPDATE customer_address_cursors SET block_number = $2, updated_at = $3
		WHERE chain = $1 AND block_number > $2`, chainID, int64(keep), s.now())
	return opErr("rewind address cursors", err)
}
