# 10. Move money as transfers saved before the ledger is called, retried with the same key

- **Status:** Accepted
- **Date:** 2026-11-17

## Context

The ledger records money once it moves ([ADR-0009](0009-record-money-as-double-entry-journals-with-locked-balances.md)), but something has to ask it to, and that call can go wrong in a way a plain error can't describe. If the ledger times out, or the reply is lost, the money may or may not have moved. A caller that gives up loses track of a real movement. A caller that retries with a new key moves the money twice. And a service that dies mid-call takes what it knew with it.

We need a service that takes a request to move money, sees it through to a definite answer, and survives crashes, timeouts and a ledger that's down for a while.

## Decision

The transaction service records each request as a **transfer**, a small state machine: `pending`, then `completed` (with the ledger's journal) or `failed` (with the ledger's reason). Completed and failed are final, and a database trigger refuses any change to them.

- **Save first.** The transfer is saved, pending, before anything is sent to the ledger. After a crash there's a pending transfer to finish, never a movement nobody knows about.
- **One key, for ever.** The transfer's own ID is the ledger's idempotency key on every attempt. However many times it's posted, and by whichever pod, the ledger moves the money once and answers a repeat with the first journal.
- **Only a refusal is final.** A 4xx the ledger means (insufficient funds, an account that isn't open) fails the transfer. A timeout, a 5xx or no connection says nothing about whether the money moved, so the transfer stays pending and is tried again. A 412 saying the transfer's own key was used for a different journal is a bug, not a refusal: it's logged and the transfer stays pending.
- **A request tries a few times, the finisher the rest.** A request tries the ledger up to 3 times with full-jitter backoff (a random wait of up to 50 ms, then up to 100 ms), within its caller's deadline, then answers 202 with the pending transfer. Every pod runs a **finisher** that claims due pending transfers (`FOR UPDATE SKIP LOCKED`, with a 30 second lease) and tries each again, waiting 10 s, then 20 s, doubling to 5 minutes between attempts.
- **Every answer is the transfer.** A caller gets 201 when it finished in their request, 202 while pending, and 200 for a retry of a finished one, always with the same body. A failed transfer is a transfer with `state: FAILED`, not an error.

Alternatives we didn't choose:

- **Call the ledger, then save.** Simpler, but a crash between the two loses the record of money that moved.
- **Call the ledger with the caller's own key.** It's chosen by the caller, and the ledger's keys are shared by everyone who posts, so two callers' keys could collide there. The transfer's ID is ours, random and unique.
- **A queue (Kafka) between the services.** It would make every transfer asynchronous and needs Phase 9's Kafka. The finisher gets the same "try until it's answered" from the table we already have, and most transfers still finish within the request.
- **A distributed transaction (two-phase commit) across both databases.** Needs both databases to take part and blocks when either is down. An idempotent call that's retried until answered gives the same result with neither.

## Consequences

- Money moves exactly once, however often a request is retried, a reply is lost or a pod dies, because the ledger keeps the key and the transfer keeps trying.
- Callers must handle 202 and poll `GET /transfers/:id`. Most transfers finish in the request, so most callers never see it.
- A transfer can stay pending for as long as the ledger is down. `transaction_pending_transfers` and `transaction_oldest_pending_seconds` show it, and Phase 13 alerts on them.
- The transfers table is a second record of each movement, beside the ledger's. The ledger stays the record of balances; the transfer is the record of the request.
- A transfer pending because of a key-reuse bug is retried for ever, every 5 minutes, until someone fixes it. That's deliberate: marking it failed could be wrong, as its money may have moved.

## Security

- **Closes a way to move money twice:** a retried request, a lost reply or a pod that dies mid-call can no longer move the same money twice, or lose track of money that moved. Recorded as a control in [docs/security.md](../security.md).
- **New data stored:** each transfer keeps its accounts, amount, the caller's idempotency key and the ledger's refusal code. No names or other personal details.
- **Unchanged gap:** any pod can still ask the transaction service to move money between any accounts, and can still post to the ledger directly, until sign-in (Phase 12) and NetworkPolicies (Phase 13).
