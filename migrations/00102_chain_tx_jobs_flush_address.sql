-- +goose NO TRANSACTION
-- +goose Up
-- The flush jobs of one deposit account (ADR-0008 §F-2): OwnFlushJob (internal/store/scan.go)
-- matches lower(address), because an EVM flush job stores the checksummed address and the
-- detector asks with the lowercase one; FlushSiblingInFlight and OtherSessionFlushes
-- (internal/store/chain.go) match the address exactly and the asset, and also state
-- lower(address) = lower($2), which keeps their result and lets this index serve them. Each
-- query names kind = 'flush', so the index holds flush jobs only. Built CONCURRENTLY, as 00101.
DROP INDEX CONCURRENTLY IF EXISTS chain_tx_jobs_flush_address;
CREATE INDEX CONCURRENTLY IF NOT EXISTS chain_tx_jobs_flush_address ON chain_tx_jobs (chain, lower(address), asset)
    WHERE kind = 'flush';

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS chain_tx_jobs_flush_address;
