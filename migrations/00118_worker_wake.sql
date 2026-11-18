-- +goose NO TRANSACTION
-- +goose Up
-- Worker plan B (2026-11-17): the worker polls slowly while idle and listens on prismaport_wake
-- (store.ListenWake) to wake its loops at once. The payload names what changed:
--   sessions        a new session (the events loops: expiry)
--   events          a new event to sequence and deliver
--   session_reveals a network revealed to a payer (the chain scheduler: the payment comes next)
--   recheck         a "Check again" request the merchant waits for (the chain scheduler)
-- Statement triggers notify once per statement, and Postgres folds identical notifications of a
-- transaction into one, delivered at commit. Rows the worker writes itself need no wake.
-- No transaction: each CREATE TRIGGER takes its table's lock alone and releases it, so the
-- migration cannot deadlock with a reveal (session_reveals then sessions) or a session creation
-- (sessions then session_reveals) running while it applies; OR REPLACE makes a rerun safe, and the
-- lock timeout keeps a waiting trigger from queueing every writer of the table behind it.
SET lock_timeout = '5s';
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION prismaport_wake() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_notify('prismaport_wake', TG_ARGV[0]);
    RETURN NULL;
END
$$;
-- +goose StatementEnd
CREATE OR REPLACE TRIGGER sessions_wake AFTER INSERT ON sessions FOR EACH STATEMENT EXECUTE FUNCTION prismaport_wake('sessions');
CREATE OR REPLACE TRIGGER events_wake AFTER INSERT ON events FOR EACH STATEMENT EXECUTE FUNCTION prismaport_wake('events');
CREATE OR REPLACE TRIGGER session_reveals_wake AFTER INSERT ON session_reveals FOR EACH STATEMENT
    EXECUTE FUNCTION prismaport_wake('session_reveals');
CREATE OR REPLACE TRIGGER sessions_recheck_wake AFTER UPDATE OF recheck_requested_at ON sessions FOR EACH ROW
    WHEN (NEW.recheck_requested_at IS NOT NULL AND NEW.recheck_requested_at IS DISTINCT FROM OLD.recheck_requested_at)
    EXECUTE FUNCTION prismaport_wake('recheck');
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP TRIGGER IF EXISTS sessions_recheck_wake ON sessions;
DROP TRIGGER IF EXISTS session_reveals_wake ON session_reveals;
DROP TRIGGER IF EXISTS events_wake ON events;
DROP TRIGGER IF EXISTS sessions_wake ON sessions;
DROP FUNCTION IF EXISTS prismaport_wake();
RESET lock_timeout;
