# PM Report — MTClaw Core System: Implementation Complete

Date: 2026-08-01 | Plan: `plans/260731-2219-mtclaw-core-system/` | Branch: `docs/mtclaw-core-system-plan`

## Status

| Phase | Status | Verified by |
|---|---|---|
| 1 Foundation + Config | Completed | config table tests, CLI smoke (8/8 criteria) |
| 2 SQLite Store | Completed | 16+ store tests incl. FK/atomicity/WAL (8/8) |
| 3 OpenAI Provider | Completed | converter round-trips, classification table, SDK confinement (7/7) |
| 4 Agent Loop | Completed | mock-provider suite, trim property tests (6/8 — 2 live-key) |
| 5 Tools + Policy | Completed | deny corpus both OSes, SSRF/path-guard matrices, kill-tree (15/16 — 1 live-key) |
| 6 Telegram Channel | Completed | gating matrix, chunker, approver auth, thread routing (10/15 — 5 live-bot) |
| 7 Gateway | Completed | -race reap hammer, serialization, lock, shutdown (11/13 — 2 live-bot) |
| 8 Cron | Completed | DST/injected-clock, overlap+minute guards, gronx contract (13/13) |
| 9 Hardening + Release | Completed | doctor/onboard tests, docs coverage test, release rehearsal (27/32 — 5 live/CI) |

## Pre-implementation review (this session)

9 findings (S1–S9) applied to the plan before any code: thread routing, approver
mux, cron OnDone plumbing, semaphore-during-approval risk, default-group
validation hole, writer invariant + BEGIN IMMEDIATE, Windows deny-list,
classifier client, WAL recovery fallback. Recorded in plan.md Red Team Review.

## Post-implementation review + fix pass

- code-reviewer: 2 High, 4 Medium, 6 Low. Security boundaries verified sound.
- Fixed same-day with regression tests: H1 (multi-tool-call abort backfill),
  H2 (OnDone guaranteed on all dispatcher drop paths), M1 (mid-turn history
  window trim), M2 (approval row created before buttons sent).
- Deferred, documented in review report: M3 (forensics-only audit race),
  M4 (synchronous update pump stall on 429), 6 Low.
- tester: 44/44 CLI smoke tests pass; no panics, no credential leaks, correct
  exit codes.
- Additional fix during integration: duplicate approvals-row bookkeeping
  (exec.go vs telegram approver) — approver now sole owner; flaky
  classifier ctx test made deterministic.

## Quality gates (final state)

- `gofmt -l` clean, `go vet` clean, `CGO_ENABLED=0 go build ./...` green
- `go test -race ./...` green — 10 packages
- Security checklist: 20/21 executed with cited tests; #14 (live log grep)
  manual pending
- Docs: configuration.md (coverage-test-enforced), security.md,
  telegram-setup.md, architecture.md, README rewrite
- CI + release workflows written; Makefile release target rehearsed locally
  (5 binaries + SHA256SUMS)

## Outstanding

1. Live-credential verification items — see plan.md "Remaining manual
   verification" (needs user's OpenAI key + bot token).
2. Deferred review findings M3/M4 + 6 Low — candidates for a small follow-up.
3. First push will exercise CI on Linux/macOS for the first time.
4. Work is uncommitted on `docs/mtclaw-core-system-plan`.

## Unresolved questions

- Whether to fix M4 (sync update pump) before first live use — a chunked long
  reply hitting 429 delays approval callbacks; low probability single-user.
- Release tagging deferred until live verification passes.
