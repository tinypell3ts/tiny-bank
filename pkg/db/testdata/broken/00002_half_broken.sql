-- +goose Up
-- The first statement works, the second fails. Because the migration runs in
-- one transaction, the first must be rolled back too.
ALTER TABLE widgets ADD COLUMN size int;
ALTER TABLE no_such_table ADD COLUMN oops int;

-- +goose Down
ALTER TABLE widgets DROP COLUMN size;
