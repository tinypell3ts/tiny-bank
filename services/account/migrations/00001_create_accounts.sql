-- +goose Up
-- An account belongs to one holder and holds money in exactly one currency.
-- There is no balance column on purpose: balances come from the ledger
-- (Phase 7), the single record of every movement of money.
CREATE TABLE accounts (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    holder_name text        NOT NULL CHECK (length(holder_name) BETWEEN 1 AND 200),
    -- Only the currencies pkg/money supports (money.GBP() and friends). Adding
    -- one there means a new migration here that widens this check.
    currency    char(3)     NOT NULL CHECK (currency IN ('GBP', 'EUR', 'USD', 'JPY')),
    status      text        NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'closed')),
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE accounts;
