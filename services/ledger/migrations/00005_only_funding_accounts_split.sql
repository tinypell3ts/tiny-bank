-- +goose Up
-- A post locks only the one shard it picks, so a split account's balance is
-- never checked as a whole. That's fine for funding accounts, which have no
-- overdraft rule, and wrong for a customer account. The ledger never splits
-- one; this is the backstop if a migration or a hand-run UPDATE ever tries,
-- as customer_balance_not_negative is for overdrafts.
ALTER TABLE accounts ADD CONSTRAINT only_funding_accounts_split CHECK (kind = 'funding' OR shards = 1);

-- +goose Down
ALTER TABLE accounts DROP CONSTRAINT only_funding_accounts_split;
