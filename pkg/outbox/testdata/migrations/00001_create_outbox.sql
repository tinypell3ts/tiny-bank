-- +goose Up
-- The outbox table every service using pkg/outbox creates (see its package
-- doc), for pkg/outbox's own tests.
CREATE TABLE outbox (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    topic       text NOT NULL,
    key         bytea NOT NULL,
    value       bytea NOT NULL,
    traceparent text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE outbox;
