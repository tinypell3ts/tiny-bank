-- +goose Up
-- Who is attempting a pending transfer right now. Store.Claim and ClaimDue
-- set a new random lease when they claim a due transfer, and only the holder
-- may start an attempt or record a refusal, so two attempts never post the
-- same transfer at once. Empty until a transfer is first claimed.
ALTER TABLE transfers ADD COLUMN lease uuid;

-- +goose Down
ALTER TABLE transfers DROP COLUMN lease;
