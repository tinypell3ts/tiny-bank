# 12. Send events on Kafka, run by Strimzi, with franz-go

- **Status:** Accepted
- **Date:** 2026-11-18

## Context

Services are about to tell each other what happened ("transfer completed") without calling each other and waiting: the notification service, and later others, react to events. That needs a durable log that many readers can read at their own pace, keeps each account's events in order, and survives a reader crashing halfway.

Constraints:

- It runs on the local kind cluster today, the way production would run it: an operator, declared objects in git, nothing clicked together.
- Every service sends and receives the same way, with the same delivery guarantee, without each one choosing its own client settings.
- Events about money must never be lost or skipped. Seeing one twice is acceptable, as consumers deduplicate.
- The trace must carry on from the request that caused an event into whatever handles it ([ADR-0011](0011-trace-requests-with-opentelemetry-and-jaeger.md)).
- Tests must run without Docker or a real broker, like the unit tests today.

## Decision

**Kafka, run by the Strimzi operator, used through a thin `pkg/kafka` on franz-go.**

- **Kafka 4.3 in KRaft mode** (no ZooKeeper), one node that is both broker and controller, run by **Strimzi** from a `Kafka` object in `deploy/kafka/`. Topics are `KafkaTopic` objects in git, and auto-creating topics is off.
- **`pkg/kafka`** is the only place services touch Kafka:
  - `Producer.Produce` waits until Kafka has stored the message. The producer is idempotent, so franz-go's own retries never write a message twice.
  - `Consume` joins a consumer group and commits a message's offset only after its handler succeeds: **at least once**. A failed handler is retried every second, holding its partition, so order holds.
  - Messages are keyed (an account ID, say): one key always lands on one partition, in order.
  - The trace travels as a `traceparent` header, with a producer span (`send <topic>`) and a consumer span (`process <topic>`) under it.
  - `KAFKA_BROKERS` says where Kafka is, and a service that uses Kafka refuses to start without it.
- **franz-go** (`github.com/twmb/franz-go` v1.22.3) as the client, with **kfake**, its in-memory Kafka, for tests.

New direct dependencies, pinned in `go.mod`: `github.com/twmb/franz-go` v1.22.3, and `github.com/twmb/franz-go/pkg/kfake` (tests only) at the commit tagged with that release, as kfake has no tagged versions of its own. `kmsg` comes with them, indirectly.

Alternatives we didn't choose:

- **confluent-kafka-go:** wraps the C library librdkafka, so it needs cgo. Our images are static distroless binaries built with `CGO_ENABLED=0`; this would mean a C toolchain in the build and a libc in the image.
- **sarama (IBM):** pure Go and widely used, but its consumer group API is harder to get right (handler sessions, manual claim loops), and it has no in-memory broker for tests.
- **segmentio/kafka-go:** pure Go and simple, but slower to follow new Kafka protocol versions, and also no in-memory broker.
- **NATS or RabbitMQ:** simpler to run, but neither keeps a replayable, partitioned log that many groups read independently, which the outbox and inbox pattern relies on.

## Consequences

- Handlers must be idempotent: a crash after handling, before the commit, means the message is handled again. The inbox table, when consumers arrive, is how they deduplicate.
- A message that always fails holds its partition forever, by design: skipping it could lose money. It shows up in the logs and as a failed `process` span; a dead-letter topic can come once there's a real case for it.
- Tests run against kfake in-process, in milliseconds. kfake is a fake: what it doesn't model (disk limits, a real rebalance under load) is checked on the local cluster.
- One node means no replication: losing its volume loses every message. Fine locally; production needs three or more.

## Security

Kafka has no authentication, TLS or topic ACLs yet: any pod in the cluster can read or write any topic. It's a known gap in [docs/security.md](../security.md), and stays acceptable only while nothing outside the cluster can reach it and no service sends personal data on it. Messages must carry IDs, not names or other personal details. The client adds no secrets: `KAFKA_BROKERS` is an address, not a credential.
