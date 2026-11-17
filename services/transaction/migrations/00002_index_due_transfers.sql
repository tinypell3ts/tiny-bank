-- +goose Up
-- The finisher looks for pending transfers that are due, oldest first, every
-- second. A partial index holds only pending rows, so it stays small however
-- many finished transfers pile up, and the query never scans the table.
CREATE INDEX transfers_due ON transfers (next_attempt_at) WHERE state = 'pending';

-- +goose Down
DROP INDEX transfers_due;
