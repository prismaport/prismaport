-- +goose Up
-- ADR-0008 §F-8: a chain payment's session_id is its payment's session. The detectors write the
-- chain row with the session they recorded the payment on (RecordPayment returns only a payment
-- of that session), so existing rows should agree; this query must return no rows before a
-- deploy:
--   SELECT c.payment_id FROM chain_payments c JOIN payments p ON p.id = c.payment_id
--   WHERE c.session_id <> p.session_id;
--
-- Both statements take a brief lock on payments (ACCESS EXCLUSIVE to attach the index 00108
-- built, SHARE ROW EXCLUSIVE for the foreign key) and scan nothing: the key is added NOT VALID,
-- so it checks new rows only, and 00110 validates the existing ones under a lock that lets writes
-- go on. lock_timeout stops this migration (and the deploy) rather than queue every payment
-- write behind a long transaction; run it again.
SET LOCAL lock_timeout = '10s';
ALTER TABLE payments ADD CONSTRAINT payments_id_session_id_key UNIQUE USING INDEX payments_id_session_id_key;
ALTER TABLE chain_payments ADD CONSTRAINT chain_payments_payment_session
    FOREIGN KEY (payment_id, session_id) REFERENCES payments (id, session_id) NOT VALID;

-- +goose Down
-- Dropping the constraint drops its index; the index is built again as 00108 left it, without
-- CONCURRENTLY (inside this transaction), which blocks payment writes while it builds.
ALTER TABLE chain_payments DROP CONSTRAINT chain_payments_payment_session;
ALTER TABLE payments DROP CONSTRAINT payments_id_session_id_key;
CREATE UNIQUE INDEX payments_id_session_id_key ON payments (id, session_id);
