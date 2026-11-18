-- +goose Up
-- The confirmation window of a dashboard session (D59): a right code opens it, and until
-- confirmed_until the session's next covered Live actions run without another code.
-- confirmed_wallets counts the Live wallets added in the window, which has a cap. Using the window
-- never extends it; signing out drops the session and the window with it.
ALTER TABLE dashboard_sessions ADD COLUMN confirmed_until timestamp with time zone;
ALTER TABLE dashboard_sessions ADD COLUMN confirmed_wallets integer NOT NULL DEFAULT 0;
ALTER TABLE dashboard_sessions ADD CONSTRAINT dashboard_sessions_confirmed_wallets_check CHECK (confirmed_wallets >= 0);

-- +goose Down
ALTER TABLE dashboard_sessions DROP CONSTRAINT dashboard_sessions_confirmed_wallets_check;
ALTER TABLE dashboard_sessions DROP COLUMN confirmed_wallets;
ALTER TABLE dashboard_sessions DROP COLUMN confirmed_until;
