-- +goose Up
-- Unlocking Live changes (D61): an emailed code of its own purpose opens the session's
-- confirmation window (D59) without an action.
ALTER TABLE dashboard_email_codes DROP CONSTRAINT dashboard_email_codes_purpose_check;
ALTER TABLE dashboard_email_codes ADD CONSTRAINT dashboard_email_codes_purpose_check CHECK (purpose = ANY (ARRAY[
    'verify_email', 'password_reset',
    'confirm_wallet_create', 'confirm_wallet_archive', 'confirm_api_key', 'confirm_webhook_endpoint', 'confirm_totp_enroll',
    'confirm_unlock']));

-- +goose Down
DELETE FROM dashboard_email_codes WHERE purpose = 'confirm_unlock';
ALTER TABLE dashboard_email_codes DROP CONSTRAINT dashboard_email_codes_purpose_check;
ALTER TABLE dashboard_email_codes ADD CONSTRAINT dashboard_email_codes_purpose_check CHECK (purpose = ANY (ARRAY[
    'verify_email', 'password_reset',
    'confirm_wallet_create', 'confirm_wallet_archive', 'confirm_api_key', 'confirm_webhook_endpoint', 'confirm_totp_enroll']));
