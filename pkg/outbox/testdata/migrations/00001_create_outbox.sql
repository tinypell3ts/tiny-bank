-- +goose Up
-- The outbox table, as pkg/outbox expects it: its tests use this, and every
-- service that uses pkg/outbox copies it into a migration of its own. One
-- row is an event waiting to be sent, with the trace of whatever added it.
-- Rows are deleted once Kafka has stored them.
CREATE TABLE outbox (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    topic       text NOT NULL,
    key         bytea NOT NULL,
    value       bytea NOT NULL,
    traceparent text NOT NULL,
    tracestate  text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- The relay picks the oldest waiting event of each topic and key.
CREATE INDEX outbox_key ON outbox (topic, key, id);

-- +goose Down
DROP TABLE outbox;
