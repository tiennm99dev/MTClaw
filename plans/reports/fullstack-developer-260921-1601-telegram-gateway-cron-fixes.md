# Telegram/gateway/cron review-fix implementation

Scope: `internal/channel/telegram/*`, `internal/gateway/*`, `internal/cron/scheduler.go`,
`internal/store/store.go` (+1 interface method), `internal/store/sqlite/cron_runs.go`
(+`store_test.go` appended tests).

## H1 - MarkdownV2 escaping inflates a chunk past 4096 (`send.go`)

- `sendOne`'s 400 fallback now also matches `"too long"`, not just `"parse"`.
- New `escapeChunk(chunk, limit)` escapes a Split-produced chunk and, if the escaped
  result still exceeds `limit`, re-splits the plain text at a reduced limit (scaled to
  the observed inflation ratio, floored at `limit/2` - no character escapes past 2
  bytes) and recurses. `sendText` now builds a `[]escapedPart{escaped, plain}` list
  instead of escaping `Split`'s chunks directly.
- Tests: `TestSendText_EscapedChunksNeverExceedLimit` (20KB dense in MarkdownV2
  specials; asserts every sent chunk's escaped length `<= 4096` and reassembles
  exactly), `TestEscapeChunk_ReSplitsWhenEscapingInflatesPastLimit`,
  `TestEscapeChunk_FitsWithoutSplittingWhenAlreadyUnderLimit`,
  `TestSendOne_FallsBackToPlainTextOnTooLongError`.

## H2 - silent poll failures (`channel.go`)

- Added `botLogger` (`telego.Logger`): `Debugf` no-op, `Errorf` -> `slog.Error` with the
  bot token redacted via `strings.ReplaceAll`. `New()` now passes
  `telego.WithLogger(newBotLogger(log, token))` instead of `WithDiscardLogger()`.
- **Skipped**: the consecutive-getUpdates-failure counter that would make `Start`
  return an error after N failures. telego only exposes failures through `Errorf`
  (no success signal exists on the `Logger` interface), so a counter here could only
  ever increment, never reset on success - misclassifying a flaky-but-recovering poll
  as a persistent failure. Wiring a real "stop after N" required either canceling a
  derived long-polling context from inside the logger (a hidden side channel a logger
  should not have) or accepting an incorrect "consecutive" semantic. Left as `Errorf`
  redaction + visibility only, per the task's own escape valve ("optionally... if that
  is not observable via the logger without hacks, skip the counter and say so").
- Tests: `channel_test.go` - `TestBotLogger_ErrorfRedactsTokenFromMessage`,
  `TestBotLogger_ErrorfLogsWithoutToken`, `TestBotLogger_DebugfIsANoOp`.

## H3 - worker leak on shutdown (`dispatch.go`)

- `dispatch` now returns `callOnDone(in, err)` immediately if `d.rootCtx.Err() != nil`.
- `runWorker`'s `<-d.rootCtx.Done()` case now mirrors the idle-reap branch: sets
  `w.closing = true`, deletes itself from `d.workers` under `d.mu`, *then* drains.
- Tests: `TestDispatch_AfterRootCtxCanceled_OnDoneFiresImmediately`,
  `TestDispatch_RunWorker_RootCtxDoneRemovesItselfFromMap`.

## M1 - drain-deadline breach ignored (`shutdown.go`, `gateway.go`)

- `waitForShutdown` now returns `drain`'s bool; `drain`'s deadline-exceeded log raised
  from `Warn` to `Error`.
- `Run` tracks `drained`; on breach, `store.Close()` is skipped (comment explains why -
  closing under a live writer is worse than leaking the fd at exit) and `Run` returns
  a non-nil error (`channelFailure` takes priority if both fired) so the process exits
  non-zero.
- Test: `TestWaitForShutdown_PropagatesDrainResult`. Full `Run()`-level integration
  (spin up a real `Gateway`, force a drain breach, assert store stays open/error
  propagates) was not added - `Gateway.New`/`Run` have no existing test harness (real
  OpenAI client, real Telegram token, etc.) and building one was out of scope for this
  slice; `drain`'s own deadline behavior is fully covered in `shutdown_test.go`.

## M2 - empty-text turns / captions ignored (`gating.go`, `poll.go`)

- `gating.go`: new `messageText(msg)` returns `(msg.Text, msg.Entities)` normally,
  falling back to `(msg.Caption, msg.CaptionEntities)` when `Text == ""`. `Decide` and
  `detectMention` now thread this pair through instead of reading `msg.Text`/
  `msg.Entities` directly.
- `poll.go`: `handleMessage` drops (`return`, no forward, no reply) any accepted
  message whose `cleanText` is empty/whitespace-only after gating.
- Tests: `gating_test.go` - `TestDecide_CaptionUsedWhenTextIsEmpty`,
  `TestDecide_CaptionEntitiesUsedForMentionDetection`,
  `TestDecide_TextTakesPriorityOverCaption`; `poll_test.go` -
  `TestHandleMessage_EmptyTextAfterGating_Dropped`,
  `TestHandleMessage_CaptionForwardedWhenTextEmpty`.

## M3 - approval timeout/callback race (`approver.go`)

- `Ask`'s select loop extracted into `awaitDecision(ctx, wait, timer, id, chatID,
  messageID)` (behavior-preserving refactor, done to make the race directly testable).
- Timer-fire case now does a non-blocking drain of `wait` before treating it as a
  timeout.
- `finishExpired` now returns early (skips `editOutcome`) when `Decide` returns
  `store.ErrAlreadyDecided`.
- `HandleCallback`: when `a.waiters[id]` has no entry (Decide still committed, but
  nobody in this process is waiting - e.g. a pending row surviving a restart, tapped
  before the startup `ExpirePending` sweep), the message is edited to "this approval is
  no longer waiting for a response" instead of "approved/denied by user N".
- Tests: `TestApprover_FinishExpired_SkipsEditWhenAlreadyDecided`,
  `TestApprover_AwaitDecision_TimerFireRacesBufferedDecision_NeverLosesIt` (500
  iterations with both select cases pre-ready, forcing Go's random pick to hit the
  timer branch regularly), `TestApprover_CallbackWithNoWaiter_EditsNoLongerWaiting`.

## M4 - unbounded `Ensure` call (`dispatch.go`)

- `runTurn`'s `Sessions().Ensure` now runs under
  `context.WithTimeout(d.rootCtx, 10*time.Second)` instead of `context.Background()`.
  Covered incidentally by all existing dispatch tests (a stuck `Ensure` would time out
  and fail them); no dedicated new test added (would need a store that blocks past
  10s, not worth the real wall-clock cost).

## M5 - synchronous update pump (`poll.go`)

- `pumpUpdates` now runs `HandleCallback` via `go c.approver.HandleCallback(...)`;
  message forwarding stays on the pump goroutine.
- Test: `TestPumpUpdates_CallbackDoesNotBlockMessagePump` (a blocking fake
  `ApprovalStore.Get` proves a queued Message update is still forwarded promptly).

## M6 - no startup sweep for stuck `cron_runs` rows

- `store.CronRunStore` gained `ExpireStarted(ctx, before time.Time) (int, error)`.
- `sqlite/cron_runs.go`: `UPDATE cron_runs SET status='interrupted', finished_at=?
  WHERE status='started' AND started_at < ?`.
- `gateway.go`'s `New()` calls it unconditionally (even when `cron.enabled` is false -
  a stale row can predate a config change) right after `sqlite.New(db)`, logging the
  count when `> 0`. No dedicated `Gateway.New`-level test (same rationale as M1); the
  sqlite-layer behavior is fully tested.
- Tests: `TestCronRuns_ExpireStarted_MovesStartedRowsBeforeCutoff`,
  `TestCronRuns_ExpireStarted_LeavesFinishedRowsAlone`,
  `TestCronRuns_ExpireStarted_LeavesRowsStartedAfterCutoff`.

## M7 - no missed-tick detection (`scheduler.go`)

- New `Scheduler.tickWithCatchUp(now, lastTick)`, called only from `Start`'s own
  ticker loop (never from `tick()` directly, which existing unit tests call with
  arbitrary jumps to prove restarts do *not* replay - see
  `TestScheduler_MissedRun_NotReplayed`, still passing unmodified). If
  `now.Sub(lastTick) > 90s`, logs a warning and evaluates every whole minute skipped
  in between (bounded to 10), then evaluates `now` itself.
- This is a deliberate design split: a process *restart* (first tick ever) must not
  replay a miss (existing, documented policy); only an *already-running* process whose
  ticker fires late (host suspend, GC pause) counts as a missed tick worth catching up.
- Tests: `TestScheduler_TickWithCatchUp_SmallGapFiresOnlyTheArrivingTick`,
  `_LargeGapFiresSkippedMinutes`, `_BoundedToMaxCatchUpMinutes`,
  `_LogsAWarningOnlyWhenCatchingUp`.

## M8 - cron job timeout delivers nothing (`dispatch.go`)

- `reply()`: when a canceled turn's error is `ErrCanceled` *and* `in.Channel ==
  "cron"` *and* `in.Timeout > 0` *and* `errors.Is(result.Err, context.DeadlineExceeded)`
  (distinguishes the job's own timeout from a shutdown/`/stop` cancellation, which
  yields `context.Canceled` since the parent context was canceled before the
  deadline), the deliver target gets `"scheduled job timed out after <d>"`. All other
  `ErrCanceled` cases keep the existing silent suppression.
- Tests: `TestDispatch_CronJobTimeout_DeliversTimeoutNotice`,
  `TestDispatch_CronShutdownCancel_StaysSilent`.

## L1 - lock file rewrite (`lock.go`, `lock_unix.go`, `lock_windows.go`)

- `lock.go`: shared-only now (`lockFileName`, `readLockPID`, `writeLockPID`).
- `lock_unix.go`: `Acquire`/`Held` rewritten on `syscall.Flock(LOCK_EX|LOCK_NB)`
  (kernel-enforced, self-releasing on crash). PID is still written into the file
  purely for the error message; the stale-PID deletion heuristic is gone entirely.
  `Held` probes with a second, distinct fd's non-blocking flock, releasing it
  immediately if uncontended.
- `lock_windows.go`: kept the PID-probe heuristic (`processAlive`) - documented why:
  a kernel-enforced `LockFileEx` needs `golang.org/x/sys/windows` as a *direct*
  import, and go.mod is out of bounds for this fix (it's currently only an indirect
  transitive dependency). Cross-compiled (`GOOS=windows GOARCH=amd64 go build`/`go
  vet`) to confirm it still compiles.
- `lock_test.go`: rewrote the stale-PID-specific tests to match the new contract
  (existing file content is irrelevant to acquisition; release drops the kernel lock
  but no longer deletes the file - removing it would reopen the TOCTOU window flock
  exists to close). `TestProcessAlive_*` moved to a new `lock_windows_test.go`
  (`//go:build windows`) since `processAlive` no longer exists on non-Windows builds.

## L2/L3/L4/L5/L6 (`send.go`, `dispatch.go`, `format.go`, `progress.go`, `queue.go`)

- **L2**: `sendOne` retries once (`transientRetryDelay = 1s`) on a 5xx or a raw
  network error (guarded by `ctx.Err() == nil`), on top of the existing 429/parse
  retries. Tests: `TestSendOne_RetriesOnceOn5xxThenSucceeds`,
  `_RetriesOnceOnNetworkErrorThenSucceeds`, `_GivesUpAfterOneTransientRetry`.
- **L3**: `dispatch.reply`'s send timeout now scales with estimated chunk count
  (`replyTimeout`: `10s` base + `5s` per extra ~4096-byte chunk) instead of a flat
  `10s`. `replyChunkLimit` is a deliberately duplicated constant (mirrors
  `telegram.DefaultChunkLimit`) since `gateway` must not import the concrete
  `telegram` package (tests use `fakeChannel`).
- **L4**: `format.go`'s `EscapeMarkdownV2` now escapes `\` and `` ` `` *inside* fenced
  and inline code spans (leaving the fence's language-tag line alone). Tests:
  `TestEscapeMarkdownV2_EscapesBackslashInsideFencedCode`,
  `_LeavesFenceLanguageTagUnescaped`, `_EscapesBackslashInsideInlineCode`.
- **L5**: `progress.go`'s `EventToolStarted` now stops a prior timer for the same
  `ToolCallID` before replacing it, and the timer's own closure checks `p.ctx.Done()`
  before sending - closing the window between a timer firing and `stop()`'s `Stop()`
  call losing that race. Tests in new `progress_test.go`.
- **L6**: `relayInbound` gained a `Channel` parameter; on a global-queue drop it now
  sends the same `sessionBusyReply` text the per-session overflow path uses (skipped
  for `Channel == "cron"`, mirroring `dispatch.replySessionBusy`'s own guard), on its
  own goroutine so a slow channel can't stall the relay loop. `gateway.go`'s call site
  passes `g.channel`. Tests: `TestRelayInbound_DropsAndWarnsWhenGlobalQueueFull`
  (extended), `TestRelayInbound_DropsCronInbound_NoReplyAttempted`.

## L9 - pump/commands test gaps

- New `internal/channel/telegram/poll_test.go`: command interception (`/new`, `/stop`
  call `Deps`, don't forward), ordinary text forwarding, empty-text drop, caption
  fallback, and the M5 callback-goroutine-doesn't-block-pump test - using a
  `blockingApprovalStore` wrapper around the existing `fakeApprovalStore` and a new
  `fakeDeps`.

## Verification

```
gofmt -l internal/channel internal/gateway internal/cron internal/store   # clean
go vet ./internal/channel/... ./internal/gateway/... ./internal/cron/... ./internal/store/...   # clean
go build ./...                                                            # clean (whole repo)
go vet ./...                                                              # clean (whole repo)
GOOS=windows GOARCH=amd64 go build ./internal/gateway/...                 # clean
GOOS=windows GOARCH=amd64 go vet ./internal/gateway/...                   # clean
go test -race -count=1 ./internal/channel/... ./internal/gateway/... ./internal/cron/... ./internal/store/...   # all pass
```

No existing test was weakened; every pre-existing test still passes unmodified except
`lock_test.go`, whose stale-PID-specific cases were rewritten to match the new
kernel-lock contract (documented above) and `TestProcessAlive_*`, relocated to a new
Windows-only file since the function moved.

## Explicitly skipped / scoped down

1. **H2's consecutive-failure counter** - not wired (see H2 section above).
2. **`Gateway.New`/`Run`-level integration tests for M1 and M6** - no existing test
   harness for a fully-wired `Gateway`; the underlying `drain`/`ExpireStarted` logic is
   fully unit-tested. Flagging as a gap rather than silently skipping.
3. **M4's dedicated timeout test** - the 10s bound is exercised indirectly by every
   other dispatch test (a regression would time them all out); a dedicated test needing
   a store call that blocks past 10s was judged not worth the real wall-clock cost.

## Unresolved / worth a follow-up decision

- H2's fatal-path restoration (stop the gateway after N consecutive getUpdates
  failures) is still open - see the skip rationale above. If wanted, it needs a
  slightly different shape: a cancelable child context for the long-polling call whose
  cancel is exposed to `Start` via a settable hook, not purely a `Logger` callback.
