# MTClaw Full Implementation Review

Reviewer: code-reviewer | Date: 2026-08-01
Scope: entire uncommitted working tree on top of abffab0 (~16,900 LOC Go across internal/, main.go, docs/, CI). Advisory only; no code modified.

## Gate Results

- `gofmt -l .` — clean
- `go vet ./...` — clean
- `CGO_ENABLED=0 go build ./...` — clean
- `go test ./...` — all packages pass

## Overall Assessment

The security-critical core is genuinely well built. The policy pipeline, path guard, SSRF guard, Telegram gating, and secret handling all do what the spec requires and fail closed on every path I traced. The cross-phase seams (Meta threading, approver mux, approvals-row ownership, turn buffering, reap/enqueue locking) are consistent with each other. The findings below are real but concentrated in two areas: a session-poisoning gap in the agent loop's multi-tool-call abort paths, and dropped `OnDone` callbacks that can permanently wedge a cron job.

## Critical Issues

None. No trust-boundary bypass, no secret leak, no approval bypass found.

## High Priority

### H1. Mid-batch tool abort persists orphaned `tool_calls`, poisoning the session
`internal/agent/loop.go:169-196` — when `resp.Message.ToolCalls` contains N > 1 calls and call k < N returns a Go error, the loop appends a tool message **only for call k** and immediately flushes the buffer (via `abortForCancellation` at line 189, or the direct flush at line 192). Calls k+1..N in the already-buffered assistant `tool_calls` message get no tool-role responses. The store's atomic Append and `HardTrim`'s whole-turn guarantee then faithfully preserve and replay this internally inconsistent turn.

Failure scenario: model issues two `exec` calls; user sends `/stop` (or the cron `Timeout` fires) while the first runs → `run` returns `ctx.Err()` → flush persists `assistant(tool_calls=[1,2]) + tool(1)`. Every subsequent turn in that session sends history with tool_call id 2 unanswered → OpenAI rejects with 400 → `ErrBadRequest` → user gets "sorry, something went wrong" on every message until enough new (user-only) turns push the poisoned turn out of the `max_history_turns` window. The comment at loop.go:179 ("it is internally consistent - no orphaned tool_calls") is false for multi-call batches; the only cancellation test (`TestRun_CancelMidTurn_PersistsPartialTurn`, loop_test.go:293) uses a single tool call and does not cover this.

Fix: before flushing in both abort paths, backfill a synthetic tool message ("canceled before execution") for every call ID in the current batch that has no buffered result. Add a multi-call cancellation test.

### H2. Dropped `Inbound` never invokes `OnDone`; a cron job can be wedged until restart
`internal/gateway/dispatch.go` — `channel.Inbound.OnDone` is documented as "invoked ... once the turn finishes", but four paths drop the message without ever calling it:

1. `dispatch` line 185-190: `trySend` fails (session queue full) → `replySessionBusy`, `OnDone` never called.
2. `runTurn` line 284-288: `Sessions().Ensure` fails → early return, no `OnDone`, no reply.
3. `runTurn` line 277-281: semaphore wait aborted by `rootCtx.Done()` (shutdown).
4. `runWorker` line 237-238: `rootCtx.Done()` with messages still queued.

For cron this is not cosmetic: `internal/cron/scheduler.go` sets `running[job]` true **before** enqueueing and clears it only in `OnDone`. Path 2 is reachable in steady state (transient SQLite error, e.g. a busy_timeout expiry under `sessions rm` contention): the flag stays true forever, every later fire records `skipped`, and the job is silently dead until gateway restart. Paths 3/4 also leave a `cron_runs` row in `started` forever (the startup sweep only expires approvals, not cron runs).

Fix: guarantee `in.OnDone(err)` on every terminal path of `dispatch`/`runTurn`/`runWorker` (including drops), or have the scheduler pair the running flag with a deadline slightly above `job.Timeout`.

Sub-note: for a dropped cron message, `replySessionBusy` calls `channel.Send` with `in.ChatID == "job:<name>"`, which fails `strconv.ParseInt` — harmless but confirms cron inbounds were never considered on this path.

## Medium Priority

### M1. History window can start mid-turn and produce an unrecoverable-shaped request
`internal/agent/loop.go:239-254` + `internal/agent/history.go` — `Recent(sessionID, maxTurns*8)` truncates at message granularity. When a single turn holds more messages than the tail of that window (tool-heavy turns easily exceed 8 messages/turn on average; `max_iterations` allows up to ~200 messages in one turn), the oldest surviving turn is partial and starts with tool/assistant rows. `SegmentTurns` keeps them as a leading turn and `HardTrim` does not drop it when `len(turns) <= maxTurns` — so the request begins with a tool message that has no preceding `tool_calls`, which OpenAI rejects with 400. Same degraded-session symptom as H1, different cause. Fix: after trimming, drop any leading messages before the first user-role message.

### M2. Telegram approver: prompt sent before the approvals row exists
`internal/channel/telegram/approver.go:121-140` — the inline-button message is sent (line 121) before `approvals.Create` (line 138). A fast tap in that window hits `HandleCallback` → `Get` → `ErrNotFound` → "unknown or expired approval" (recoverable by tapping again). Worse: if `Create` fails, `Ask` returns an error but the already-sent prompt keeps live-looking buttons forever (never edited, never answerable). Fix: create the row first (update `message_id` after the send), or on `Create` failure best-effort edit the message to remove the keyboard.

### M3. Late callback vs. timeout race leaves contradictory records
`approver.go:149-158` vs `HandleCallback` — if the timer fires and a callback's `Decide("approved")` wins the DB race before `finishExpired`'s `Decide("expired")`, the approvals row reads `approved`, the message is edited twice (last write "timed out"), `Ask` has already returned `false, DeadlineExceeded`, and `exec_audit` records `expired`. Fail-closed (the command never runs), so this is a forensics inconsistency, not a security hole. Worth a comment or a `WHERE expires_at > now` guard on the callback path.

### M4. Telegram update pump is fully synchronous
`internal/channel/telegram/poll.go:17-26` — `HandleCallback` and `handleMessage` (including `deps.Status`/`Reset` DB calls, `sendOne` with its 429 `RetryAfter` sleeps that can be tens of seconds) run inline in the single update loop. One slow send stalls approval-button handling for a concurrently running turn — the approver waits on `wait` while the callback that would satisfy it sits behind the stalled pump; with a 5m approval timeout this can turn an approvable command into an expiry. Single-user impact is modest; a bounded worker or per-update goroutine for callbacks would remove it.

## Low Priority

- **L1. SSRF blocklist gaps** (`internal/tools/web_fetch.go:110-129`): 255.255.255.255, 192.0.0.0/24 (includes 192.0.0.170/171), 198.18.0.0/15, and NAT64 `64:ff9b::/96`-embedded private IPv4 are not blocked. The important ranges (loopback, RFC1918, link-local/169.254.169.254, ULA, 0/8, CGNAT, IPv4-mapped IPv6 via `Unmap`) are all covered; residual risk on a personal box is minimal.
- **L2. `validate.go` `isWithinRoot` is case-sensitive and symlink-blind** (`internal/config/validate.go:188-194`), unlike `path_guard.go`'s `withinRoot`: on Windows, `cwd: c:\work` vs root `C:\Work` fails validation spuriously (fail-closed), and a symlinked `exec.cwd` pointing outside a root passes validation (informational only — cwd is documented as "not a jail").
- **L3. Byte-boundary truncations can split UTF-8 mid-rune**: `RedactSecrets` display cap (`approver.go:146-148`), exec output cap (`exec.go:222-224`), `read_file` limit reads. Cosmetic mojibake only; `chunk.go` gets this right (`safeRuneCut`), so the pattern exists locally.
- **L4. Instance-lock stale-removal race** (`internal/gateway/lock.go:29-67`): two simultaneous starters can both judge the lock stale, and the loser can delete the winner's freshly created lock → two gateways. Explicitly documented as cooperative; the common case (double `mtclaw gateway`) is handled.
- **L5. `shellwords.Parse` gate is POSIX-shaped on Windows**: PowerShell-specific syntax that go-shellwords cannot tokenize fails closed to an approval prompt — friction, not a vulnerability. Worth a docs note.
- **L6. web_fetch ignores Content-Type**: binary bodies are run through `htmlToText` and returned as garbage text within `max_bytes`. Wastes tokens only.

## Security-Critical Verification (all confirmed against code)

- **Policy pipeline** (`policy.go:107-136`): fixed deny → allow → mode order; deny returns `VerdictRefuse` before allow/classifier/approver can see the command; unknown mode, nil classifier, classifier error/timeout (10s bound), and invalid classifier risk values all fail closed to `VerdictAsk`. Tokenize failure fails closed ahead of the pipeline (`exec.go:120-126`). Approval timeout, `ErrNoApprover` (cron/CLI-less), and user denial all refuse with distinct audit labels; only explicit `approved==true` executes.
- **Path guard** (`path_guard.go`): NUL rejection, tilde expansion, symlink resolution on deepest existing ancestor, root symlink resolution, `filepath.Rel`-based containment (sibling `/data-evil` rejected), case-insensitive comparison on Windows/macOS. `write_file`'s `MkdirAll` operates only on the already-resolved path; `list_dir` never descends symlinked dirs.
- **SSRF guard** (`web_fetch.go`): `Dialer.Control` validates the actual resolved connect address (DNS-rebind- and redirect-proof), `Unmap()` defeats `::ffff:` encoding, redirects capped at 3 with per-hop scheme re-validation through the same transport; manually constructed Transport has no env proxy that could bypass the dialer.
- **Telegram gating** (`gating.go`): allowlist fail-closed (empty list matches no one, including `From.ID == 0`); bots rejected; unknown chat types rejected; mention detection is entity-only with correct UTF-16 offsets, never substring; empty group `allow_from` inherits the channel list rather than allowing everyone; validation refuses `enabled: true` with a would-accept-no-one config. Rejections are silent per spec.
- **Callback authorization** (`telegram/approver.go:179-256`): approval id is a 128-bit random nonce; presser must match the approval's originating chat AND pass the same allowlist that gates messages; `Decide`'s `WHERE state='pending'` CAS makes double-taps and timeout races single-winner.
- **Secrets**: inline `api_key`/`token` rejected at validation; resolved secrets live in unexported fields unreachable by `yaml.Marshal`; `config show` renders `<set:env:NAME>` placeholders; telego constructed with `WithDiscardLogger` (its debug logger can log the token); doctor prints sources, never values; onboard writes only env-var indirection at 0600 and prints the export line instead of persisting; approval prompts and `exec_audit.command` both receive `RedactSecrets`'d text while the executed command is untouched.
- **Approvals-row ownership**: the Telegram approver creates/decides/expires its own row; `exec.go` writes only `exec_audit` rows (`ask` on the deny/expire paths, `execute`/`finishExecute` on the run path) — no double bookkeeping found.
- **Concurrency**: reap/enqueue race correctly serialized under one mutex with the `closing` flag and post-timer queue re-check; cancel registry (`worker.cancel`) read/written only under `d.mu`; semaphore bounded and released via defer; approver waiter map mutex-guarded with buffered signal channel; cron minute-guard (wall-clock key, DST-correct) and CAS overlap guard are correct on the happy path (see H2 for the unhappy path); SQLite writer pool capped at 1 with `_txlock=immediate`; message Append is atomic per turn.
- **Contracts**: `docs/configuration.md` matches `config/types.go` key-for-key including defaults and failure modes spot-checked (`tools.exec`, `channels.telegram`, `cron`); README CLI commands match `internal/cli/root.go`'s registered tree; the result-string-vs-error tool contract is honored by all four tools (Go errors only for turn-ending ctx events).

## Recommended Actions (priority order)

1. Fix H1: backfill synthetic tool results for un-run calls in both abort paths of `Loop.Run`; add a multi-tool-call cancellation test.
2. Fix H2: invoke `OnDone` on every drop/early-return path in the dispatcher (or deadline-bound the cron running flag).
3. Fix M1: drop leading pre-first-user messages after history trim.
4. Fix M2: create the approvals row before sending the prompt (or clean up the keyboard on Create failure).
5. Consider M4 (async callback handling) before any multi-user use.

## Metrics

- gofmt/vet/build: clean | Tests: 14/14 packages pass
- Test coverage: not measured (no coverage gate in CI observed); test quality is high — policy corpus tests, gating tables, dispatcher race tests, approver races all present. The one materially missing case is the multi-tool-call abort (H1).

## Unresolved Questions

1. Is the "any group + `*` wildcard" behavior intended to let an allowlisted user operate the bot in arbitrary groups where non-allowlisted members can read the output? It is documented, but worth an explicit note in `docs/security.md` if intended.
2. Should `cron_runs` rows stuck in `started` (crash or H2 paths) get a startup sweep like `ExpirePending` does for approvals?

Status: DONE_WITH_CONCERNS
Summary: Production-quality security core with all fail-closed paths verified; 2 High, 4 Medium, 6 Low findings — the High pair (session-poisoning multi-tool abort, dropped OnDone wedging cron jobs) should be fixed before release.
Concerns/Blockers:
- H1: mid-batch tool abort flushes orphaned tool_calls, breaking the session on every subsequent turn until the turn ages out (loop.go:169-196).
- H2: dispatcher drop paths never call Inbound.OnDone, permanently wedging a cron job's overlap flag until restart (dispatch.go:185/284; scheduler.go running flag).

## Fix Pass 2026-08-01

All four findings below (H1, H2, M1, M2) were fixed and verified; M3/M4/Low were left untouched per scope. `gofmt -l .`, `go vet ./...`, `CGO_ENABLED=0 go build ./...`, and `go test -race ./...` are all clean/green after these changes.

### H1 — Mid-batch tool abort persists orphaned `tool_calls`
- Fix: `internal/agent/loop.go` adds `backfillAbortedToolCalls`, called once inside the `runErr != nil` branch of the tool-call loop, before either abort path (the `context.Canceled`/`DeadlineExceeded` branch that calls `abortForCancellation`, and the generic Go-error branch that flushes directly). It scans the buffered turn for tool messages already present and appends a synthetic `tool` result ("tool execution aborted: <reason>") - plus a `toolNames` entry - for every call in the current assistant message's `ToolCalls` that has none, so a partially-run batch is never persisted with an orphaned id. The stale "internally consistent - no orphaned tool_calls" comment (false for batches > 1) was removed and replaced with an accurate one.
- Test: `internal/agent/loop_test.go`'s `TestRun_ToolCancelMidBatch_BackfillsUnrunCalls` - a 2-call batch where call 1 returns `context.Canceled` and call 2 must never run; asserts the persisted transcript has a tool row for both ids (call 2's content contains "aborted"), then converts the persisted rows back to `provider.Message` and feeds them through `HardTrim`, asserting `assertNoOrphans` holds - the same round-trip a later turn would depend on.

### H2 — Dropped `Inbound` never invokes `OnDone`
- Fix: `internal/gateway/dispatch.go` adds `wrapOnDone` (applied once, in `dispatch`, to guard the caller's `OnDone` with a `sync.Once` so any of several terminal paths can call it exactly once) and `callOnDone` (nil-safe invoke). All four drop paths now call it: (1) `dispatch`'s queue-full branch calls `callOnDone(in, errSessionQueueFull)` before `replySessionBusy`; (2) `runTurn`'s semaphore-wait `<-d.rootCtx.Done()` branch calls `callOnDone(in, d.rootCtx.Err())`; (3) `runTurn`'s `Sessions().Ensure` failure branch calls `callOnDone(in, fmt.Errorf("gateway: ensure session: %w", err))`; (4) `runWorker`'s `<-d.rootCtx.Done()` exit now calls a new `drainQueueOnDone(w)` helper that drains `w.queue` and fires `OnDone` for every message still sitting in it. The normal-completion call site was also switched to `callOnDone` for consistency. Bonus fix: `replySessionBusy` now returns immediately (no `Send`) when `in.Channel == "cron"`, since a cron inbound's `ChatID` ("job:\<name\>") is not a real chat - `OnDone`'s error is the only signal needed.
- Tests (`internal/gateway/dispatch_test.go`): `TestDispatch_SessionQueueFull_OnDoneCalledWithError` (queue-full drop fires `OnDone` with a non-nil error), `TestDispatch_SessionQueueFull_CronInbound_NoSendAttempted` (same drop, cron channel, asserts zero `Send` calls), `TestDispatch_EnsureSessionFailure_OnDoneCalledWithError` (closes the store early to force `Ensure` to fail, asserts `OnDone` still fires), `TestDispatch_WorkerExitWithQueuedItem_OnDoneCalledForEachQueued` (calls `drainQueueOnDone` directly against a worker with 3 queued messages and an already-canceled `rootCtx`, asserting all 3 get `OnDone(context.Canceled)` - a direct, deterministic test of the drain helper rather than racing Go's non-deterministic `select` on a live `runWorker` goroutine).

### M1 — History window can start mid-turn
- Fix: `internal/agent/history.go`'s `HardTrim` now calls a new `dropLeadingNonUser` helper first, which discards any prefix of the input before the first `role=="user"` message (returning `nil` if there is none), before doing its turn-count trim. This runs regardless of `maxTurns`, so the "trimmed history always starts at a user boundary" invariant holds unconditionally, not just when trimming is active. `SegmentTurns`'s doc comment was updated to clarify it still preserves a leading non-user segment as its own `Turn` (useful for tests/introspection) but that `HardTrim` is what actually discards it.
- Test: `internal/agent/history_test.go`'s `TestHardTrim_MidTurnWindow_NeverOrphansOrStartsWithNonUser` extends the existing property test with 200 randomized cases that additionally cut a structurally-valid generated history at an arbitrary message index (not a turn boundary) before trimming - simulating exactly what `Loop.loadHistory`'s `Recent(maxTurns*8)` fetch can hand `HardTrim` - and asserts both `assertNoOrphans` and that the trimmed output's first message (when non-empty) is always `role=="user"`.

### M2 — Telegram approver: prompt sent before the approvals row exists
- Fix: `internal/channel/telegram/approver.go`'s `Ask` now creates the approvals row (via `a.approvals.Create`) before building the keyboard or sending anything; a `Create` failure returns the fail-closed deny immediately with no `Send` attempted. `MessageID` is no longer set at `Create` time (it isn't known yet); after `sendOne` succeeds, a new `store.ApprovalStore.SetMessageID` call records it on the existing row. If `sendOne` itself fails, the already-created row is marked `"expired"` (best-effort) instead of being left pending with nothing that could ever answer it. `SetMessageID` was added to the `ApprovalStore` interface, implemented in `internal/store/sqlite/approvals.go` (a plain `UPDATE ... WHERE id = ?`, no-op if the row is gone by then), and implemented on the `fakeApprovalStore` test double.
- Tests: `internal/channel/telegram/approver_test.go`'s `TestApprover_RowExistsBeforePromptIsSent` (a `fakeBotAPI.sendFunc` hook checks the row already exists at the moment `SendMessage` is actually called, via a channel to avoid a data race with the polling test goroutine; also asserts `message_id` is recorded on the row after the send) and `TestApprover_CreateFailure_NoPromptSentFailClosed` (forces `Create` to fail via a new `fakeApprovalStore.failCreate` flag, asserts `Ask` denies and `api.lastSent()` is nil). `internal/store/sqlite/store_test.go` adds `TestApprovals_SetMessageID_RecordsAfterCreate` and `TestApprovals_SetMessageID_UnknownIDIsANoOp` for the store-level method.
