-- +goose NO TRANSACTION
-- +goose Up
-- DeliveriesForEvent (internal/store/webhooks.go) reads an event's deliveries by event_id; the
-- unique index on (endpoint_id, event_id) cannot serve it, so it read the whole table
-- (ADR-0008 §F-3). Built CONCURRENTLY, as 00101.
DROP INDEX CONCURRENTLY IF EXISTS webhook_deliveries_event;
CREATE INDEX CONCURRENTLY IF NOT EXISTS webhook_deliveries_event ON webhook_deliveries (event_id);

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS webhook_deliveries_event;
