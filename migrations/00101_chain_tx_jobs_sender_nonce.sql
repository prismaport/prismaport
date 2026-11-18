-- +goose NO TRANSACTION
-- +goose Up
-- MaxSenderNonce (internal/store/chain.go) reads the highest nonce a sender used on a chain. With
-- no index it read every job of the chain (ADR-0008 §F-2).
--
-- chain_tx_jobs is live, so the index is built CONCURRENTLY (outside a transaction): writes go on
-- during the build. A build that fails leaves an INVALID index and goose does not record the
-- version, so the next run drops that leftover first and builds it again. The same holds for
-- every CONCURRENTLY migration after this one (00102-00106, 00108).
DROP INDEX CONCURRENTLY IF EXISTS chain_tx_jobs_sender_nonce;
CREATE INDEX CONCURRENTLY IF NOT EXISTS chain_tx_jobs_sender_nonce ON chain_tx_jobs (chain, sender, nonce);

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS chain_tx_jobs_sender_nonce;
