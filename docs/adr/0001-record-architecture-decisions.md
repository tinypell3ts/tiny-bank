# 1. Record architecture decisions

- **Status:** Accepted
- **Date:** 2026-11-11

## Context

tiny-bank will make many significant choices: transport, data ownership, messaging and runtime. Months later we'll want to know *why* a choice was made, not just what was chosen. Commit messages and PR descriptions get buried.

## Decision

We record each significant decision as an Architecture Decision Record (ADR) in `docs/adr/`, following Michael Nygard's format.

- Files are named `NNNN-short-title.md`, numbered in order, and never renumbered.
- Each ADR has: **Status**, **Date**, **Context**, **Decision**, **Consequences**.
- To reverse a decision, add a new ADR that supersedes the old one, and mark the old one `Superseded by NNNN`. Never rewrite history.
- A PR that makes a significant decision includes its ADR.

Copy [template.md](template.md) to start a new one.

## Consequences

- Decisions and their reasons stay next to the code and are reviewed in PRs.
- Writing an ADR costs a few minutes, so we only write them for decisions that are hard to reverse or would surprise a newcomer.
