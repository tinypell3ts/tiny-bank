-- +goose Up
-- Events waiting to be sent to Kafka: a TransferChanged, saved in the same
-- transaction as the change it announces, and deleted once Kafka has stored
-- it. A copy of pkg/outbox's table (pkg/outbox/testdata/migrations), which
-- its relay reads by column name.
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
