-- +goose Up
-- chain_checkpoints held the EVM detector's block cursor. Lean scanning (D49, legacy 00038-00044)
-- stopped writing it and chain_credit_heads (legacy 00044) replaced its last reader, so no code
-- reads or writes it (ADR-0008 §F-7). The 0.2.0 binary still does: production's first deploy of
-- the baseline, which carries this migration, runs with the api and worker stopped, and 0.2.0
-- must not run again after it. Nothing else locks the table.
DROP TABLE chain_checkpoints;

-- +goose Down
CREATE TABLE chain_checkpoints (
    chain text NOT NULL,
    block_number bigint NOT NULL,
    block_hash text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT chain_checkpoints_block_hash_check CHECK ((block_hash ~ '^0x[0-9a-f]{64}$'::text)),
    CONSTRAINT chain_checkpoints_block_number_check CHECK ((block_number >= 0)),
    CONSTRAINT chain_checkpoints_pkey PRIMARY KEY (chain, block_number)
);
