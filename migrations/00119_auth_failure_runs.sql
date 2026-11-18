-- +goose Up
-- Wrong second-factor codes count in two runs per user: totp_failures/totp_retry_at before a
-- session (sign-in, password reset), totp_step_failures/totp_step_retry_at inside one (step-ups,
-- confirmations). Wrong codes from someone who only knows the password never make the signed-in
-- owner wait. password_failures counts wrong passwords in a row at sign-in (and on a lost
-- authenticator link); it slows nothing down, it only tells the owner by email. A lost link dies
-- after a few wrong passwords (dashboard_totp_lost_links.password_failures).
ALTER TABLE dashboard_users
    ADD COLUMN totp_step_failures integer DEFAULT 0 NOT NULL CONSTRAINT dashboard_users_totp_step_failures_check CHECK (totp_step_failures >= 0),
    ADD COLUMN totp_step_retry_at timestamp with time zone,
    ADD COLUMN password_failures integer DEFAULT 0 NOT NULL CONSTRAINT dashboard_users_password_failures_check CHECK (password_failures >= 0);

ALTER TABLE dashboard_totp_lost_links
    ADD COLUMN password_failures integer DEFAULT 0 NOT NULL CHECK (password_failures >= 0);

-- +goose Down
ALTER TABLE dashboard_totp_lost_links DROP COLUMN password_failures;
ALTER TABLE dashboard_users DROP COLUMN password_failures, DROP COLUMN totp_step_retry_at, DROP COLUMN totp_step_failures;
