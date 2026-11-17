-- +goose Up
-- An idempotency key gets one final answer. A posted journal keeps its key in
-- journals; a refused post used to leave nothing, so the same key could post
-- later (once the account had money, say), even after its caller had been
-- told "refused" and acted on it. Now a refusal keeps the key too, and every
-- repeat of the request gets the same refusal.
CREATE TABLE refusals (
    idempotency_key text        PRIMARY KEY,
    -- A fingerprint of the request (sha256 of its sorted entries), so a
    -- repeat can be told apart from the key reused for something else.
    request         text        NOT NULL CHECK (request ~ '^[0-9a-f]{64}$'),
    -- The terror the caller got, such as precondition_failed.insufficient_funds.
    code            text        NOT NULL,
    message         text        NOT NULL,
    request_id      text        CHECK (request_id ~ '^[A-Za-z0-9_-]{1,64}$'),
    created_at      timestamptz NOT NULL DEFAULT now()
);

-- Part of the audit trail, like journals: never changed once written.
CREATE TRIGGER refusals_append_only BEFORE UPDATE OR DELETE ON refusals
    FOR EACH ROW EXECUTE FUNCTION refuse_change();
CREATE TRIGGER refusals_no_truncate BEFORE TRUNCATE ON refusals
    FOR EACH STATEMENT EXECUTE FUNCTION refuse_change();

-- +goose Down
DROP TABLE refusals;
