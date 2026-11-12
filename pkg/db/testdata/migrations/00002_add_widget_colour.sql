-- +goose Up
-- Real migrations take time. The sleep makes pods that start together
-- overlap, so TestMigrateIsSafeWhenPodsStartTogether really tests the lock.
SELECT pg_sleep(0.5);
ALTER TABLE widgets ADD COLUMN colour text NOT NULL DEFAULT 'blue';

-- +goose Down
ALTER TABLE widgets DROP COLUMN colour;
