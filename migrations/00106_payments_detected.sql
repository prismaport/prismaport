-- +goose NO TRANSACTION
-- +goose Up
-- The dashboard's recent payments (RecentPayments, internal/store/dashboard_data.go) are a
-- merchant's latest payments: ORDER BY detected_at DESC, id DESC LIMIT n over the payments of
-- the merchant's sessions in a mode. ADR-0008 §F-5 suggested (session_id, detected_at), but the
-- list spans many sessions, so that index would still feed every payment of the merchant to a
-- sort. This one is in the list's order: the planner reads payments newest first, keeps those of
-- the merchant's sessions (sessions_pkey) and stops after n. A session's own payments (loading a
-- session) stay on payments_session, and sorting those few rows costs nothing. Built
-- CONCURRENTLY, as 00101.
DROP INDEX CONCURRENTLY IF EXISTS payments_detected;
CREATE INDEX CONCURRENTLY IF NOT EXISTS payments_detected ON payments (detected_at DESC, id DESC);

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS payments_detected;
