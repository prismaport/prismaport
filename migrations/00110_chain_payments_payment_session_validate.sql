-- +goose Up
-- Validates 00109's foreign key over the existing chain payments. VALIDATE CONSTRAINT takes
-- SHARE UPDATE EXCLUSIVE on chain_payments and ROW SHARE on payments, so reads and writes go on
-- while it scans. A chain payment whose session is not its payment's fails it, and the deploy
-- stops with the key still NOT VALID (the check query is in 00109).
ALTER TABLE chain_payments VALIDATE CONSTRAINT chain_payments_payment_session;

-- +goose Down
-- A constraint cannot be marked not validated again: drop it and add it back NOT VALID, as 00109
-- left it.
ALTER TABLE chain_payments DROP CONSTRAINT chain_payments_payment_session;
ALTER TABLE chain_payments ADD CONSTRAINT chain_payments_payment_session
    FOREIGN KEY (payment_id, session_id) REFERENCES payments (id, session_id) NOT VALID;
