-- +goose Up
-- The runtime pause (D64): a whole chain (asset NULL) or one asset on it takes nothing new
-- (sessions, payment methods, customer addresses, reveals, wallet payments) while the worker keeps
-- watching, crediting and flushing it. A row is a pause in force; resuming deletes it. Who paused
-- and resumed, and when, is in audit_log (chain.paused, chain.resumed). The api and the worker
-- read the table again about every 30 s.
CREATE TABLE chain_pauses (
    chain text NOT NULL,
    asset text,
    paused_at timestamp with time zone NOT NULL,
    paused_by text NOT NULL
);
CREATE UNIQUE INDEX chain_pauses_key ON chain_pauses (chain, COALESCE(asset, ''));

-- +goose Down
DROP TABLE chain_pauses;
