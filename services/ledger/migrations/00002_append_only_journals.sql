-- +goose Up
-- The audit trail: journals and entries are the history of every movement of
-- money, so once written they never change. A mistake is corrected by posting
-- another journal that moves the money back, never by editing the first, so
-- the history always shows what happened and when.

-- Which request posted the journal: kit.Handler's request ID, which is in
-- every log line of that request (and of the calls it made to other
-- services). Empty for journals posted before this migration.
ALTER TABLE journals ADD COLUMN request_id text CHECK (request_id ~ '^[A-Za-z0-9_-]{1,64}$');

-- +goose StatementBegin
CREATE FUNCTION refuse_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% is append-only: % is not allowed', TG_TABLE_NAME, TG_OP;
END;
$$;
-- +goose StatementEnd

-- Row triggers catch UPDATE and DELETE; TRUNCATE skips them, so it gets its
-- own statement trigger.
CREATE TRIGGER journals_append_only BEFORE UPDATE OR DELETE ON journals
    FOR EACH ROW EXECUTE FUNCTION refuse_change();
CREATE TRIGGER journals_no_truncate BEFORE TRUNCATE ON journals
    FOR EACH STATEMENT EXECUTE FUNCTION refuse_change();
CREATE TRIGGER entries_append_only BEFORE UPDATE OR DELETE ON entries
    FOR EACH ROW EXECUTE FUNCTION refuse_change();
CREATE TRIGGER entries_no_truncate BEFORE TRUNCATE ON entries
    FOR EACH STATEMENT EXECUTE FUNCTION refuse_change();

-- +goose Down
DROP TRIGGER entries_no_truncate ON entries;
DROP TRIGGER entries_append_only ON entries;
DROP TRIGGER journals_no_truncate ON journals;
DROP TRIGGER journals_append_only ON journals;
DROP FUNCTION refuse_change();
ALTER TABLE journals DROP COLUMN request_id;
