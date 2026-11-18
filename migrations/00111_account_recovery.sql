-- +goose Up
-- Account recovery (D56): recovery codes, the emailed link that asks to remove a lost
-- authenticator, the 72 hour wait before the removal, and the lock on live credentials after it.

-- One row per code, stored only as an HMAC with a key derived from the service secret, like the
-- emailed codes. used_at is set atomically, which is what makes a code single use.
CREATE TABLE dashboard_recovery_codes (
    user_id text NOT NULL REFERENCES dashboard_users (id) ON DELETE CASCADE,
    code_hash bytea NOT NULL CHECK (length(code_hash) = 32),
    used_at timestamptz,
    created_at timestamptz NOT NULL,
    PRIMARY KEY (user_id, code_hash)
);

-- Like a password reset link: the URL carries a random token, the row keeps only its SHA-256 and
-- is found by that hash alone. One live link per user; a newer one replaces it.
CREATE TABLE dashboard_totp_lost_links (
    user_id text PRIMARY KEY REFERENCES dashboard_users (id) ON DELETE CASCADE,
    token_hash bytea NOT NULL UNIQUE CHECK (length(token_hash) = 32),
    created_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL
);

-- A pending removal, one per user: removal_at never moves once the row exists.
CREATE TABLE dashboard_totp_removals (
    user_id text PRIMARY KEY REFERENCES dashboard_users (id) ON DELETE CASCADE,
    requested_at timestamptz NOT NULL,
    removal_at timestamptz NOT NULL,
    cancel_token_hash bytea NOT NULL UNIQUE CHECK (length(cancel_token_hash) = 32),
    reminded_at timestamptz,
    CHECK (removal_at > requested_at)
);
CREATE INDEX dashboard_totp_removals_due ON dashboard_totp_removals (removal_at);

-- While in the future, live wallets and live API keys cannot be added or rolled.
ALTER TABLE dashboard_users ADD COLUMN recovery_locked_until timestamptz;

-- +goose Down
ALTER TABLE dashboard_users DROP COLUMN recovery_locked_until;
DROP TABLE dashboard_totp_removals;
DROP TABLE dashboard_totp_lost_links;
DROP TABLE dashboard_recovery_codes;
