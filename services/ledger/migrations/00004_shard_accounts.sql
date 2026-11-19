-- +goose Up
-- A busy account can be split into shards, so posts to it don't all queue
-- for one locked row: each post that adds to it picks one shard at random,
-- and the account's balance is the sum of its shards.
--
-- Shard 0 is the account's own row in accounts, as before, and shards 1 to
-- shards-1 are rows in account_shards. So code that knows nothing of shards
-- (still running while this rolls out) keeps posting to shard 0, and every
-- shard's balance stays equal to its entries. Two things are off until every
-- pod runs the new code: an old pod reads a funding account's balance as
-- shard 0 alone, and an old pod's deposit (FOR UPDATE on the funding row)
-- and a new pod's (a foreign-key check on it) can deadlock. Postgres aborts
-- one, and the caller retries it with the same idempotency key, so no money
-- is lost or moved twice.
--
-- The ledger reads a funding account's shard count once and keeps it. Adding
-- shards is safe while it runs (new ones go unused until a restart);
-- removing any needs every ledger pod restarted first, or a pod picks a
-- shard that's gone and its posts fail.

-- How many shards an account has: 1 (only its own row) unless split.
ALTER TABLE accounts ADD COLUMN shards smallint NOT NULL DEFAULT 1 CHECK (shards BETWEEN 1 AND 64);

-- Shards 1 and up. Each one's balance equals the sum of its entries, like an
-- account's own row.
CREATE TABLE account_shards (
    account_id uuid     NOT NULL REFERENCES accounts (id),
    shard      smallint NOT NULL CHECK (shard >= 1),
    balance    bigint   NOT NULL DEFAULT 0,
    PRIMARY KEY (account_id, shard)
);

-- Which shard an entry moved. Every entry so far moved shard 0.
ALTER TABLE entries ADD COLUMN shard smallint NOT NULL DEFAULT 0 CHECK (shard >= 0);

-- Funding accounts are in every deposit, so each gets 8 shards. They have no
-- overdraft rule, so no post ever needs all their shards at once.
UPDATE accounts SET shards = 8 WHERE kind = 'funding';
INSERT INTO account_shards (account_id, shard)
    SELECT id, s FROM accounts, generate_series(1, 7) AS s WHERE kind = 'funding';

-- +goose Down
-- Folds every shard back into shard 0 first, so no money is lost.
UPDATE accounts AS a SET balance = a.balance + s.total
    FROM (SELECT account_id, sum(balance) AS total FROM account_shards GROUP BY account_id) s
    WHERE a.id = s.account_id;
DROP TABLE account_shards;
ALTER TABLE accounts DROP COLUMN shards;
ALTER TABLE entries DROP COLUMN shard;
