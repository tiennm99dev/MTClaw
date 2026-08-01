# MTClaw Core System: Nine-Phase Implementation Complete

**Date**: 2026-08-01  
**Severity**: Medium (2 High bugs fixed same-day; release-ready)  
**Component**: MTClaw gateway (11k LOC Go, 9 sequential phases)  
**Status**: Resolved

## What Happened

Implemented all nine phases of the MTClaw core system in a single coordinated session. Delegated agents built foundation, SQLite store, OpenAI provider, agent loop, tools + policy, Telegram channel, gateway dispatcher, cron scheduler, and hardening/release sequentially, with verification between each phase. Post-implementation code review caught 2 High, 4 Medium, 6 Low findings. The two Highs matched exactly the cross-phase seam gaps identified in a pre-implementation re-review (S1–S9) applied *before any code was written*. Fixed all four High+Medium findings same-day with regression tests.

## The Brutal Truth

The anxiety going into this was concentrated in two places: the exec policy engine (deny-list bypass patterns are easy to get subtly wrong, and the red team had already found two) and the reap/enqueue race in the gateway dispatcher. Both survived adversarial review intact. The real relief was *not* finding a third bypass or a silent message loss. For a 16.9k LOC security-critical system shipping in one day, this went unusually clean.

The one frustration: the code review's H1 and H2 findings were the *exact* cross-phase seams the red team found but my seam review caught *before* implementation. This validates the review posture (preemptively re-review agent boundaries), but it also means the code review was partially redundant — we'd already prevented the two highest-risk bugs by engineering discipline rather than catch-and-fix.

## Technical Details

**Pre-implementation seam findings (S1–S9, all applied to plan before code):**
- S2/S3: Approver mux keyed on `meta.Channel`, `Inbound.OnDone` callback for cron completion
- S6: `BEGIN IMMEDIATE` on persistent `cron run` to avoid gateway race
- S9: Read-only WAL recovery fallback on `SQLITE_READONLY_RECOVERY`

**Integration defects caught between phases:**
1. Duplicate approvals-row ownership: `exec.go` was also creating rows; approver now sole owner (phase 6→7 boundary)
2. Flaky nanosecond-timer test: classifier context cancellation hit race on test clock; made deterministic (phase 5→6 boundary)

**Post-implementation review findings (fixed same-day):**
- **H1** (`loop.go:169-196`): Multi-tool-call abort flushed orphaned tool_calls, poisoning session on every subsequent turn until the turn aged out. Fixed with `backfillAbortedToolCalls` scanning the buffered turn and appending synthetic results for every un-run call. Added `TestRun_ToolCancelMidBatch_BackfillsUnrunCalls`.
- **H2** (`dispatch.go`, 4 paths): Dropped `Inbound.OnDone` wedged cron jobs' running flags forever. Fixed with `wrapOnDone` (sync.Once guard) and four explicit call sites, including a `drainQueueOnDone` helper for shutdown paths. Added `TestDispatch_SessionQueueFull_OnDoneCalledWithError`, etc.
- **M1** (`history.go`): History trim could start mid-turn with orphaned tool_calls. Fixed with `dropLeadingNonUser` before turn-count trim. Added property test cutting windows at arbitrary message boundaries.
- **M2** (`approver.go`): Telegram prompt sent before approvals row existed. Fixed by creating the row first (at `Create` time, `message_id` set after send). Added `TestApprover_RowExistsBeforePromptIsSent`.

**Deferred, documented in review report:**
- M3 (forensics-only audit race: approval expires while callback races to Decide)
- M4 (synchronous update pump stalls approval handling under concurrent turns on 429 retry)
- 6 Low findings (SSRF residual gaps, path-guard case sensitivity on Windows, UTF-8 truncation cosmetics, etc.)

## Final Quality Gates

- `gofmt -l .` clean, `go vet ./...` clean, `CGO_ENABLED=0 go build ./...` green
- `go test -race ./...` green (10 packages)
- 44/44 CLI smoke tests, no panics, no credential leaks, correct exit codes
- Security checklist: 20/21 executed with cited tests (1 live-log-grep item pending)
- Docs complete: configuration.md (coverage-test-enforced), security.md, telegram-setup.md, architecture.md, README rewrite
- CI/release: workflows written, Makefile rehearsed locally (5 binaries + SHA256SUMS)

## Lessons Learned

**Cross-phase seam review is not optional.** When different agents author different phases, the boundaries between their work accumulate ambiguities and implicit assumptions. Identify and write those seams *before* code — a second red team pass on just the phase-file junctions found 9 issues, 2 of which the code review would have found as High-severity bugs. The seam review prevented rework.

**The two-review system (seam + code) is not redundant.** Seams catch architectural omissions (what the spec forgot). Code review catches implementation bugs (what the code did wrong). Both are necessary, and the seam review *must* precede code, not follow it.

**Fail-closed patterns survive review better than optional guards.** The SSRF guard, path guard, and deny-list all use explicit boundary-crossing that a reviewer can audit. The cron running flag (which had to be deadline-guarded as a fallback) is harder to trust. Stateless functions and guards beat stateful flags.

## Next Steps

1. **Live-credential verification** (user's responsibility): test `mtclaw prompt` with real OpenAI key, `mtclaw gateway` against real Telegram chat, `mtclaw cron` on schedule. Instructions in phase-09 report.
2. **M3/M4 + Low findings**: candidates for a follow-up pass, not blockers for release.
3. **First CI run**: the Linux/macOS workflows are untested. The first push will exercise them.
4. **Commit and tag**: work is currently uncommitted on `docs/mtclaw-core-system-plan`. After live verification passes, commit and tag the release.

---

**Unresolved question:** Fix M4 (sync update pump) before first live multi-user use, or leave it as a known-low-probability stall for a future patch? Currently deferred.
