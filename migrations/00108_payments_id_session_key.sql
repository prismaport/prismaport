-- +goose NO TRANSACTION
-- +goose Up
-- ADR-0008 §F-8: chain_payments.session_id must be its payment's session. The composite foreign
-- key that says so (00109) needs a unique key on payments (id, session_id). id is already the
-- primary key, so every row satisfies it; the index is built CONCURRENTLY first (as 00101) and
-- 00109 turns it into the constraint without another scan.
DROP INDEX CONCURRENTLY IF EXISTS payments_id_session_id_key;
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS payments_id_session_id_key ON payments (id, session_id);

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS payments_id_session_id_key;
