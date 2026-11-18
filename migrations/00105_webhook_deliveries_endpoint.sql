-- +goose NO TRANSACTION
-- +goose Up
-- ListDeliveries (internal/store/dashboard_data.go) pages one endpoint's deliveries newest first:
-- WHERE endpoint_id = $1 AND id < $2 ORDER BY id DESC LIMIT $3. This index returns them in that
-- order, so a page reads its rows and stops instead of sorting the endpoint's whole log
-- (ADR-0008 §F-5). Built CONCURRENTLY, as 00101.
DROP INDEX CONCURRENTLY IF EXISTS webhook_deliveries_endpoint;
CREATE INDEX CONCURRENTLY IF NOT EXISTS webhook_deliveries_endpoint ON webhook_deliveries (endpoint_id, id DESC);

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS webhook_deliveries_endpoint;
