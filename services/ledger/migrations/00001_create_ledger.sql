-- +goose Up
-- The ledger's view of an account: which rules it follows, and its balance.
-- The account service owns everything else about an account; the IDs match.
-- A customer account's row is created by its first journal.
CREATE TABLE accounts (
    id         uuid        PRIMARY KEY,
    kind       text        NOT NULL CHECK (kind IN ('customer', 'funding')),
    -- Only the currencies pkg/money supports, like the account service's table.
    currency   char(3)     NOT NULL CHECK (currency IN ('GBP', 'EUR', 'USD', 'JPY')),
    -- In minor units (pence). It always equals the sum of the account's
    -- entries; it's kept here so a post can lock and check one row instead
    -- of adding up the account's whole history.
    balance    bigint      NOT NULL DEFAULT 0,
    -- The no-overdraft rule is checked in Go first, for a clear error. This
    -- is the backstop if a bug ever skips that check.
    CONSTRAINT customer_balance_not_negative CHECK (kind = 'funding' OR balance >= 0)
);

-- One funding account per currency. The IDs are fixed, and each ends in the
-- currency's ISO 4217 number, so callers can name them (funding.Account in pkg/funding).
INSERT INTO accounts (id, kind, currency) VALUES
    ('00000000-0000-4000-8000-000000000826', 'funding', 'GBP'),
    ('00000000-0000-4000-8000-000000000978', 'funding', 'EUR'),
    ('00000000-0000-4000-8000-000000000840', 'funding', 'USD'),
    ('00000000-0000-4000-8000-000000000392', 'funding', 'JPY');

-- A journal is one movement of money.
CREATE TABLE journals (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    -- Chosen by the caller. UNIQUE is what makes a retried request post once:
    -- a second insert with the same key finds the first one instead.
    idempotency_key text        NOT NULL UNIQUE CHECK (length(idempotency_key) BETWEEN 1 AND 100),
    currency        char(3)     NOT NULL CHECK (currency IN ('GBP', 'EUR', 'USD', 'JPY')),
    created_at      timestamptz NOT NULL DEFAULT now()
);

-- An entry adds amount to one account's balance (negative amounts take from it).
-- A journal's entries add up to zero, which Go checks before inserting them.
CREATE TABLE entries (
    journal_id uuid   NOT NULL REFERENCES journals (id),
    account_id uuid   NOT NULL REFERENCES accounts (id),
    amount     bigint NOT NULL CHECK (amount <> 0),
    PRIMARY KEY (journal_id, account_id)
);

-- +goose Down
DROP TABLE entries;
DROP TABLE journals;
DROP TABLE accounts;
