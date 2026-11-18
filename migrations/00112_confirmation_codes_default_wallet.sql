-- +goose Up
-- Confirmation by emailed code (D58) and the default wallet of a network family (D57).

-- A sensitive Live action is confirmed with a 6-digit code emailed to the owner, one live code
-- per user and purpose, stored like the other emailed codes (HMAC, 15 minutes, 5 attempts).
ALTER TABLE dashboard_email_codes DROP CONSTRAINT dashboard_email_codes_purpose_check;
ALTER TABLE dashboard_email_codes ADD CONSTRAINT dashboard_email_codes_purpose_check CHECK (purpose = ANY (ARRAY[
    'verify_email', 'password_reset',
    'confirm_wallet_create', 'confirm_wallet_archive', 'confirm_api_key', 'confirm_webhook_endpoint', 'confirm_totp_enroll']));

-- The wallet new products of the family pay by default. At most one active default per merchant,
-- mode and family; an archived wallet is never the default. Without one, the oldest active
-- wallet of the family is the default, as before this migration.
ALTER TABLE wallets ADD COLUMN is_default boolean NOT NULL DEFAULT false;
ALTER TABLE wallets ADD CONSTRAINT wallets_default_active CHECK (NOT is_default OR archived_at IS NULL);
CREATE UNIQUE INDEX wallets_one_default ON wallets (merchant_id, livemode, family) WHERE is_default;

UPDATE wallets w SET is_default = true
WHERE w.archived_at IS NULL AND NOT EXISTS (
    SELECT 1 FROM wallets o
    WHERE o.merchant_id = w.merchant_id AND o.livemode = w.livemode AND o.family = w.family AND o.archived_at IS NULL
      AND (o.created_at, o.seq) < (w.created_at, w.seq));

-- +goose Down
DROP INDEX wallets_one_default;
ALTER TABLE wallets DROP CONSTRAINT wallets_default_active;
ALTER TABLE wallets DROP COLUMN is_default;
DELETE FROM dashboard_email_codes WHERE purpose LIKE 'confirm\_%';
ALTER TABLE dashboard_email_codes DROP CONSTRAINT dashboard_email_codes_purpose_check;
ALTER TABLE dashboard_email_codes ADD CONSTRAINT dashboard_email_codes_purpose_check CHECK (purpose = ANY (ARRAY['verify_email', 'password_reset']));
