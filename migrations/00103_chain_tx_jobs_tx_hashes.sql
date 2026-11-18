-- +goose NO TRANSACTION
-- +goose Up
-- OwnTxHashes (internal/store/solana.go) asks which of a list of transactions a job of ours sent.
-- It unnested every job's tx_hashes (ADR-0008 §F-2); it now first keeps the jobs whose
-- tx_hashes overlap the list (tx_hashes && $2), which this GIN index answers. Built
-- CONCURRENTLY, as 00101.
DROP INDEX CONCURRENTLY IF EXISTS chain_tx_jobs_tx_hashes;
CREATE INDEX CONCURRENTLY IF NOT EXISTS chain_tx_jobs_tx_hashes ON chain_tx_jobs USING gin (tx_hashes);

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS chain_tx_jobs_tx_hashes;
