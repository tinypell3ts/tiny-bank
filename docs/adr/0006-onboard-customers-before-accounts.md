# 6. Onboard customers before opening accounts, and take the customer ID from sign-in

- **Status:** Accepted
- **Date:** 2026-11-13

## Context

Phase 5 built the account service. It opens an account for a `customer_id` given in the request, but nothing yet says what a customer is, or who is allowed to name one. Trying the API made the gap obvious: to open Ada's first account, you have to invent an ID for her.

A bank has to answer two questions before it holds anyone's money:

- **Who is this person?** Regulation requires a bank to know its customers (KYC): identity checks and agreeing to the terms happen *before* an account exists. Someone who hasn't been onboarded isn't a customer, so an account can't be opened for them.
- **Who is asking?** If callers can say which customer they are, any caller can act as any customer, opening accounts in their name or reading their details.

These questions belong to different parts of the system. Who a person is (name, identity documents, verification status) changes rarely, and is sensitive. Which accounts they hold changes often, and is what everything that moves money needs.

## Decision

**A customer service owns customers, and onboards them before any account is opened** (Phase 11.5).

- **Onboarding** creates the customer, runs the identity checks (a stand-in step at first) and records the outcome. Then it asks the account service to open the customer's first account. To the person signing up this is one step; behind it are two services and two calls.
- **The account service never creates customers.** It opens accounts only for a customer ID it's given. It doesn't check that the customer exists, because customers live in another service's database, and services never read each other's databases ([architecture](../architecture.md)). Callers are trusted to pass real IDs: the customer service, and later staff tools.
- **Internal APIs name the customer explicitly.** `POST /accounts` takes `customer_id` in its body. That's the internal API other services call; customers don't call it.
- **Customer-facing APIs never take a customer ID from the customer.** The edge gateway (Phase 12) signs callers in and takes their customer ID from the sign-in token. It then calls internal APIs with that ID. A customer can only ever act as themselves.

Until Phase 12 there's no sign-in, so the account service stays internal: a ClusterIP Service, with no Ingress (deploy/AGENTS.md).

### Alternatives considered

- **The account service creates a customer when `customer_id` is missing.** It's the shortest path to "open Ada's first account", but it mixes two jobs and skips identity checks. Two retries would create two Adas, nothing could ever say which customer is real, and there would be no single place for KYC.
- **Customers send their own `customer_id`.** This is simple, but any caller who finds out or guesses an ID can act as that customer. Validating the ID doesn't help: the question is *who is asking*, and only sign-in can answer that.
- **One service for customers and accounts.** There's one less network call, but identity data (sensitive, rarely changed) would share a database and a release cycle with account data (changed constantly, needed by every money flow). Splitting them later is far harder than starting apart.
- **A foreign key from accounts to customers.** This would need both tables in one database, which the one-database-per-service rule rules out. The customer service is the one that guarantees a customer exists before an account is opened.

## Consequences

- Signing up needs the customer service, so the first end-to-end sign-up arrives with Phase 11.5, and sign-in with Phase 12. Until then, accounts are opened by calling the internal API directly, with a made-up customer ID.
- If onboarding fails after the customer is created but before the account is opened, it must retry or record the half-finished state. The outbox (Phase 10) and the `CustomerOnboarded` event give it a reliable way to do that.
- Because `customer_id` isn't a foreign key, a bad caller could open an account for a customer that doesn't exist. The defence is that only trusted internal callers can reach the account service, and later, that NetworkPolicies (Phase 13) only let the customer service and the gateway in.
- `holder_name` is on the account today. Once customers exist, the name on an account can default to the customer's name, and the account keeps its own copy only as the name shown on that account.
- Every customer-facing endpoint added later follows the same rule: the customer comes from sign-in, never from the request.
