-- +goose Up
-- Which request created a transfer: its trace ID (to find the whole journey
-- in Jaeger) and its request ID (to find its log lines). Saved when the
-- transfer is, before the ledger is called, so a transfer whose ledger calls
-- all timed out still leads to its trace. Empty for transfers saved before
-- this migration, or by a request with no trace (tracing off).
ALTER TABLE transfers
    ADD COLUMN trace_id   text CHECK (trace_id ~ '^[0-9a-f]{32}$'),
    ADD COLUMN request_id text CHECK (request_id ~ '^[A-Za-z0-9_-]{1,64}$');

-- +goose Down
ALTER TABLE transfers DROP COLUMN trace_id, DROP COLUMN request_id;
