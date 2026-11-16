# 8. Stop secrets where they enter, with kit.Secret

- **Status:** Accepted
- **Date:** 2026-11-14 (recorded 2026-11-17)

## Context

When a request fails, `kit.Handler` logs the full error with its request ID. That log line is the only record of why a request failed, so it has to keep the detail. But an error can carry anything it was built from. If a secret ever got into one, the secret would be written to the logs, where many more people and tools can read it than can read the secret's source.

A review of Phase 5.5 found exactly that risk. A test even required a password to appear in a logged error. Today the only secret is `DATABASE_URL`, which holds the account database's password, but every service that stores data will have one, and later phases add more.

We had to decide where to stop secrets: on their way out, as text is logged, or where they come in.

## Decision

**Every secret read from the environment is stored as a `kit.Secret`** ([secret.go](../../pkg/kit/secret.go)). It's a string type whose every way of becoming text shows `xxxxx` instead:

- `String` covers fmt's `%v`, `%s` and `%q`, including inside structs and wrapped errors;
- `GoString` covers `%#v`;
- `LogValue` covers `log/slog`;
- `MarshalText` covers `encoding/json` and other text encoders.

The only way to get the value is `Reveal()`, called where the value is used and nowhere else. Today that's one line, where the account service opens its database connection. The kit keeps logging full errors: secrets can't get into them, because they can't be printed.

### Alternatives considered

- **Redact secrets from log output with patterns** (such as anything that looks like `password=…` or a URL with credentials). We tried this, and removed it. It only catches the shapes someone thought of in advance. It can mangle log lines that happen to match. And it acts after the secret has already been built into a string, so anything else that prints that string still leaks it.
- **Filter by key name in the logger** (slog's `ReplaceAttr` dropping keys like `password` or `token`). It misses a secret inside a value, such as an error message or a URL, which is how a connection string usually leaks.
- **Stop logging errors in full.** It removes the risk, but leaves 500s nobody can explain. That cost is paid every time something goes wrong.
- **Fetch secrets from a secret manager.** It protects how secrets are stored and handed over, not what happens once one is in memory, so it would still need this. It also has no place on a local cluster today.

## Consequences

- Printing or logging a whole config struct is safe. That's the most common way secrets leak.
- Each use of a secret is a `Reveal()` call, easy to find and to review.
- A new secret must be declared as `kit.Secret`. That's a rule in AGENTS.md, because the compiler can't tell which strings are secret.
- After `Reveal()`, the value is an ordinary string again. Code that puts the revealed value into an error, or a plain conversion `string(s)`, bypasses the protection. Reviewers have to check every place a secret is revealed or converted.
- Secrets that reach third-party code are only as safe as that code. pgx, the Postgres driver, hides passwords in its own errors.

## Security

This decision is a security control: it stops secrets leaking through logs, errors and printed configs. It's in the controls table of [docs/security.md](../security.md), and the gaps it leaves are listed under Consequences above.
