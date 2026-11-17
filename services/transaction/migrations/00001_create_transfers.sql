-- +goose Up
-- A transfer is one request to move money, and what has happened to it. It's
-- saved, pending, before anything is sent to the ledger, so a crash or a
-- timeout never leaves a movement of money nobody knows about.
CREATE TABLE transfers (
    -- Also the ledger's idempotency key for this transfer: every attempt sends
    -- the same one, so the ledger posts it once however many times it's tried.
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    -- The caller's key: the same request again gets this transfer back.
    idempotency_key text        NOT NULL UNIQUE CHECK (idempotency_key ~ '^[A-Za-z0-9._:-]{1,100}$'),
    kind            text        NOT NULL CHECK (kind IN ('transfer', 'deposit')),
    -- Empty for a deposit, whose money comes from outside the bank.
    from_account    uuid,
    to_account      uuid        NOT NULL,
    -- Only the currencies pkg/money supports, like the other services' tables.
    currency        char(3)     NOT NULL CHECK (currency IN ('GBP', 'EUR', 'USD', 'JPY')),
    -- In minor units (pence); never zero or negative, at most £10 billion.
    amount          bigint      NOT NULL CHECK (amount BETWEEN 1 AND 1000000000000),
    state           text        NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'completed', 'failed')),
    journal_id      uuid,          -- the ledger's journal, once completed
    failure_reason  text,          -- the ledger's terror code, once failed
    attempts        integer     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),

    -- The rules the Go code keeps, checked again here in case a bug doesn't.
    CONSTRAINT deposit_has_no_from CHECK ((kind = 'deposit') = (from_account IS NULL)),
    CONSTRAINT not_to_itself CHECK (from_account IS DISTINCT FROM to_account),
    CONSTRAINT completed_has_journal CHECK ((state = 'completed') = (journal_id IS NOT NULL)),
    CONSTRAINT failed_has_reason CHECK ((state = 'failed') = (failure_reason IS NOT NULL))
);

-- Completed and failed are final. Once a transfer leaves pending, nothing
-- about it may change: not its state, not its money, not its journal.
-- +goose StatementBegin
CREATE FUNCTION refuse_change_to_finished_transfer() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.state <> 'pending' THEN
        RAISE EXCEPTION 'transfer % is %, and a finished transfer never changes', OLD.id, OLD.state;
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER finished_transfers_never_change BEFORE UPDATE ON transfers
    FOR EACH ROW EXECUTE FUNCTION refuse_change_to_finished_transfer();

-- +goose Down
DROP TRIGGER finished_transfers_never_change ON transfers;
DROP FUNCTION refuse_change_to_finished_transfer();
DROP TABLE transfers;
