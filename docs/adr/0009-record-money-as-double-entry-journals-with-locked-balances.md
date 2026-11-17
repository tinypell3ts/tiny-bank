# 9. Record money as double-entry journals, with a stored balance per account updated under a row lock

- **Status:** Accepted
- **Date:** 2026-11-17

## Context

The ledger (Phase 7) is the one record of every movement of money. Whatever else goes wrong, it must keep these promises:

- **Money is never created or lost.** Every movement takes from somewhere and adds somewhere else.
- **A customer balance never goes below zero**, even when many requests spend from the same account at the same moment.
- **A request sent twice moves money once.** Callers retry after timeouts, and a retry must not pay twice.
- **History can't be rewritten.** What happened stays recorded, so it can be checked and explained later.
- **It's fast enough to check a balance on every post**, without adding up an account's whole history each time.

The ledger runs as several pods on one Postgres database, so the guarantees have to come from the database, not from locks in one process.

## Decision

**Every movement of money is a journal of entries that add up to zero, and each account's balance is stored next to its entries and updated in the same transaction, under a row lock.**

- **Double entry.** A journal has 2 to 10 entries in one currency. Each one adds to one account (positive) or takes from it (negative), and together they sum to zero, checked in `NewJournal.Validate`. A £3 transfer is −300 from Alice and +300 to Bob. Each entry is capped at 10¹² minor units (£10 billion). The cap limits one entry, not a balance: a balance still grows with every deposit, but reaching the `int64` limit (about 9.2 × 10¹⁸) takes over nine million maximum-size journals into one account. A post that would go past it fails, as a 500, instead of wrapping around.
- **Funding accounts are where outside money comes from.** The migration creates one per currency, with fixed IDs ending in the currency's ISO number (`…000000000826` for GBP), so callers can name them. A funding account may go negative: −£100 means £100 has arrived and been moved into customer accounts. Customer accounts can't go below zero, which is checked in Go for a clear error, and by a `CHECK` constraint as a backstop.
- **A stored balance, updated under `SELECT … FOR UPDATE`.** Each post locks the rows of the accounts it touches, checks the new balances, then writes the balances and entries, in one transaction. Locks are taken in account ID order, so two posts that share accounts queue instead of deadlocking. The rule that "a balance equals the sum of its entries" is checked at the end of every store test.
- **Idempotency keys.** The caller picks a key per movement of money and sends it again on every retry. A unique index on the key means the money moves once. A retry gets the first journal back (200), and the same key for a different request is refused (412 `precondition_failed.idempotency_key`; terrors has no 409). A key gets one final answer: a refused post (insufficient funds, say) keeps its key too, in a `refusals` table, and the same request gets the same refusal even once it would go through, so a late or repeated post with that key can never move money its caller was told didn't move. Posts with the same key take a transaction lock on it, so the first to decide wins. An answer that says nothing (the account service down, a timeout) is not kept, so the key stays free for a retry. (Amended 2026-11-17, from "a refused post doesn't use up its key", after a stalled transaction-service worker was shown able to post a transfer already marked failed.)
- **Accounts are checked with the account service first**, outside any transaction: each must exist, be open and hold the journal's currency. A retry skips the checks and gets the first journal, even one that overlapped it (found by looking the key up again if the checks fail).
- **Append-only.** Database triggers refuse `UPDATE`, `DELETE` and `TRUNCATE` on journals and entries. A mistake is corrected by a new journal that moves the money back. Each journal records the request ID that posted it.

### Alternatives considered

- **Only a balance column, changed in place.** Simplest, but there's no history to explain a balance or check it against, and two requests that read then write can each overwrite the other.
- **Only entries, with the balance worked out by `SUM()` on every read and post.** Always consistent by construction, but every post gets slower as an account's history grows, and it still needs a lock to stop two posts both seeing enough money. Keeping both, and checking they agree, gives speed and a cross-check.
- **Optimistic concurrency** (a version number on each balance, and retry on conflict). Avoids holding locks, but under contention on a busy account most posts retry, and every caller of the store needs retry logic. Row locks are simpler and correct, and Postgres queues the waiters.
- **`SERIALIZABLE` isolation instead of explicit locks.** Postgres would detect the conflicts itself, but posts would fail with serialization errors that every caller has to retry, and which conflicts fail is harder to reason about than "locks are taken in ID order".

## Consequences

- **Correctness is checkable.** Two invariants can be tested at any time with one query each: every currency's balances sum to zero, and every balance equals the sum of its entries. The tests check both, and the Phase 13 alert rules can too.
- **Posts on the same account queue.** Throughput on one account is limited by how long a post holds its lock. That's why the account checks run before the transaction, and why the writes go in one batch.
- **The funding account is a hot row.** Every deposit or withdrawal in a currency locks the same funding row, so all of them queue behind each other. That's fine at this scale. At real volume the fix is several funding accounts per currency (shards), picked at random, or updating the funding row with `balance = balance + $1` and no read-check, since it has no overdraft rule. This is a known limit, not an accident.
- **Callers must keep their idempotency keys.** A caller that makes a new key on every retry gets no protection. The transaction service (Phase 8) stores each transfer's key before its first attempt.
- **History only grows.** Journals and entries are never deleted, so storage grows with use. Archiving old history would be a deliberate project, not a cleanup job.

## Security

- **Closes the gap "money doesn't move, so nothing is idempotent or audited"**, and adds controls to [docs/security.md](../security.md): zero-sum journals with capped amounts, row locks, idempotency keys, account checks with the account service, and the append-only audit trail with a request ID on every journal.
- **New data stored:** journals and entries (account IDs, which are random UUIDs, and amounts), and the request ID of each post. No personal data: holder names stay in the account service.
- **Remaining gaps**, all listed in security.md: any pod in the cluster can post a journal (Phase 12, then NetworkPolicies in Phase 13), and the ledger's database user owns its tables, so someone with its password could drop the triggers (Phase 13: separate database roles).
