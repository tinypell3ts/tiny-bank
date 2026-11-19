# 13. Announce changes through a transactional outbox

- **Status:** Accepted
- **Date:** 2026-11-19

## Context

Other services need to hear when a transfer is created, completed or failed: the notification service to tell a customer (a payer must be told when a payment is refused), a feed to show pending payments, later fraud checks. The transaction service saves each change in Postgres and the news goes out on Kafka ([ADR-0012](0012-send-events-on-kafka-with-franz-go.md)). It can't do both atomically:

- **Save, then send:** a crash or a Kafka outage between the two loses the event, and nothing would ever send it.
- **Send, then save (or send inside the transaction):** if the save rolls back, the event announces something that didn't happen, and every request waits on Kafka.

Constraints:

- An event exists if and only if its change was saved. Losing one is worse than sending one twice.
- One transfer's events arrive in the order they happened: pending before completed.
- A Kafka outage must not stop money moving.
- Events carry no personal details, as anything on Kafka can be read by any consumer.
- It runs on what we already have (Postgres, Kafka) and fits the next services that publish, such as the customer service.

## Decision

**A transactional outbox, in `pkg/outbox`, with one event type per change, `TransferChanged`.**

- **Saved with the change.** `outbox.Add` writes the event (binary protobuf, with the request's `traceparent` and `tracestate`) into an `outbox` table in the service's own database, using the transaction that saves the change. Both commit, or neither does. The transaction service announces through an `Announce` hook passed to its store, so only the call that changes a transfer announces it: a retry or a repeated answer adds nothing.
- **Sent by a relay beside the service.** `outbox.Relay` runs on every pod (through `db.Deps.Background`). Each batch is one Postgres transaction that takes an advisory lock, so one pod sends at a time. It picks the oldest waiting event of each topic and key, sends them together with `Producer.ProduceAll`, deletes those Kafka stored and commits. A key's next event waits for a later batch, so a stuck event holds back only its own key. Each batch's sends have a 10-second limit, and failures back off up to 5 seconds.
- **At least once.** A relay that stops after sending but before committing sends those events again, so each event carries an `event_id` for consumers to skip one they've seen.
- **Ordered per key.** Events are keyed by transfer ID. The row lock taken by each change means one transfer's events never come from overlapping transactions, so insert order is commit order for that key. Events for different transfers have no order between them.
- **Deleted once sent.** The outbox holds only what's waiting. Kafka keeps the sent copy for a week, and the record is the transfers table and the ledger's journals, not the events.
- **One event type, `TransferChanged`, carrying the whole transfer**, sent on creation, completion and failure, rather than one type per state. A new state later is a new enum value, not a new event. The caller's `idempotency_key` is left out: it's their own text.
- **Measured.** `outbox_waiting_events` (counted up to 10,000, by index), `outbox_oldest_waiting_seconds` and `outbox_sent_events_total`, on the transaction dashboard.

No new dependencies: `pkg/outbox` uses pgx, franz-go (through `pkg/kafka`) and protobuf, which we already had.

Alternatives we didn't choose:

- **Change data capture (Debezium reading Postgres's write-ahead log):** no table of our own and no relay to write, but Kafka Connect and Debezium to run, a replication slot that fills the database's disk if the connector stops, and events shaped like table rows, not like a contract.
- **Every pod sends with `FOR UPDATE SKIP LOCKED`:** spreads the work, but two pods could send one transfer's events out of order.
- **A leader chosen by Kubernetes (a `Lease`):** one sender, but a moving part outside the database, and the relay would still need the database to be sure what it sent.
- **Keep sent events, marked as sent:** a local history, but a table that grows without end, and a second record of what the transfers table already says.
- **An outbox per service, written by hand:** the customer service is planned to publish too, so the relay's ordering, limits and metrics belong in shared code once.

## Consequences

- Moving money never waits on Kafka. During an outage, events wait in the outbox and go once Kafka is back, in order.
- Consumers must deduplicate by `event_id` (the inbox table, with the notification service), and treat events as news, checking the transfer itself when it matters.
- A service that publishes has to copy the outbox table into a migration of its own (`pkg/outbox/testdata/migrations` is the one definition). The relay reads it by column name, so a copy that has drifted fails loudly.
- While Kafka is down, the relay keeps one of the service's database connections busy for most of each 10-second send window.
- An event that Kafka always refuses holds back the rest of its key until someone fixes it. It shows in `outbox_oldest_waiting_seconds` and the relay's log lines.
- Events reach Kafka within a fraction of a second normally (the relay polls every 200ms), not at the moment of the change.

## Security

Events are saved in the service's own database and sent to Kafka, which has no authentication, TLS or topic ACLs yet ([docs/security.md](../security.md)): any pod in the cluster can read them, or write a fake one that a consumer would believe. That stays acceptable only while Kafka is reachable only inside the cluster and events carry no personal details: `TransferChanged` has IDs, amounts, states and terror codes, and the caller's idempotency key is left out because it's text they chose. Anyone with Kafka UI on a developer's machine can read every event too; it's local only and read-only. The outbox table adds no new secrets or inputs: only the service writes to it, in its own transactions.
