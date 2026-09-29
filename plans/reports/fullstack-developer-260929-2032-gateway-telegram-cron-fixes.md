# Gateway / Telegram / cron fixes

Date: 2026-09-29. Source: `code-reviewer-260929-2032-gateway-telegram-cron-review.md`. Nothing committed.

Status: all of M1-M4, L1-L5 and C1-C4 are done. L6 is skipped as decided. L7 belongs to the CLI owner.
Every regression test was checked by reverting its fix and confirming the test fails.

## Per finding

| # | What changed | Regression test | Mutation check |
|---|---|---|---|
| M1 | `internal/gateway/dispatch.go`: `runWorker` now dequeues the message and registers `w.cancel` inside one `d.mu` critical section (new `newTurnContext`; `runTurn` takes `turnCtx` and `cancel` as parameters). `runOnWorker` claims the worker (`pendingControl++`) and cancels the in-flight turn under the same lock as the lookup. A worker with `pendingControl > 0` drops (`context.Canceled`) any message it dequeues. The claim is released exactly once, either when the worker picks up fn or when the caller gives up. | `TestRunOnWorker_MessageJustDequeuedIsCanceledNotAwaited` (30 iterations) | Cancel registered late and the drop case removed: fails at iteration 0 with `context deadline exceeded`. |
| M2 | `dispatch.go`: `worker.lastReply` chains reply goroutines. `reply(prev, done, ...)` waits on `prev`, then sends, and always closes `done`. The reply still runs on `replyWG`, not `wg`, so drain behavior is unchanged. | `TestDispatch_RepliesArriveInTurnOrder` | Removing the wait: the order becomes short, long, and the test fails. |
| M3 | `internal/channel/telegram/channel.go`: `newBot` sets an `http.Client{Timeout: apiRequestTimeout}` (60 s, above the 30 s long poll) through the caller in `caller.go`. | `TestChannelStart_ReturnsPromptlyOnCancelWhileLongPollIsHeld` (the server holds `getUpdates` for up to 20 s; `Start` must return within 1 s of cancel) | Removing the HTTP client: `Start` did not return within 5 s (the test ran 20 s). |
| M4 | New `internal/channel/telegram/caller.go` (about 55 lines, under the 60-line limit): a `net/http` `ta.Caller` that returns `&ta.Error{ErrorCode: status}` for HTTP >= 500 and otherwise decodes the body like telego. `newBot` installs it with `telego.WithAPICaller`. The phantom tests were replaced by tests that drive a real `telego.Bot` against httptest. | `TestSendOne_RetriesOnceOn5xxThenSucceeds` (502 then 200), `TestSendOne_GivesUpAfterOneTransientRetry` (503 gives a typed `*ta.Error`, 2 calls), `TestAPICaller_DecodesAPIErrorBodyOnNon5xx` | Default telego caller: the 5xx errors are untyped and the tests fail (`a 5xx must surface as a typed API error`). |
| L1 | `send.go`: a 429 `retry_after` up to `maxRetryAfter` (60 s) is honored even past the caller's deadline. The deadline is extended by wait plus 30 s of headroom, and `sendText` keeps using the extended context for the later chunks. Longer waits return the error at once. If a chunk fails after an earlier one was delivered, `notifyIncomplete` makes one best-effort send of "(the reply above is incomplete: the rest could not be delivered)" in the same thread, on a fresh 5 s deadline, and logs it. It is skipped when the cause is an outright cancellation, and when the first chunk fails. `sendText` now takes a logger (`Channel.Send` passes `c.log`, `SendOnce` passes `slog.Default()`). | `TestSendOne_HonorsRetryAfterBeyondCallerDeadline`, `TestSendOne_RetryAfterOverCapIsNotWaited`, `TestSendOne_CancelDuringRetryAfterStopsWaiting`, `TestSendText_LaterChunksSurviveAnExtendedFloodWait`, `TestSendText_MidReplyFailureSendsIncompleteNotice`, `TestSendText_FirstChunkFailureSendsNoNotice` | Extension disabled and notice removed: the first three named tests fail (deadline error, chunk 2 lost, no notice). |
| L2 | `internal/cron/scheduler.go`: `Start` no longer runs a free ticker. It re-aligns a timer to the next wall-clock minute every loop. `tickWithCatchUp(now, lastEvaluated)` evaluates each whole minute in (last, nowMinute], capped at `maxCatchUpMinutes` before now, and returns nowMinute. `missedTickThreshold` and the `Round(0)` gap logic are removed. Evaluations use whole-minute instants, so a wake a few seconds into its minute still finds the job due. The existing DST tests are green. | `TestScheduler_TickWithCatchUp_MinuteStraddledByWakePhaseIsEvaluated`, `..._LateWakeWithinMinuteStillFires`, `..._NotPastLastEvaluatedDoesNothing`. The existing catch-up tests were updated only for the new warning text. | Old `scheduler.go` restored: all three new tests fail. |
| L3 | `internal/gateway/progress.go`: a notice context canceled by `stop()`, plus `noticeMu` and `stopped`. The notice callback checks `stopped` and sends under `noticeMu`. `stop()` cancels the context, then takes the lock, so after `stop()` returns no notice is in flight or can start. | `TestProgressReporter_NoticeNeverSentAfterStopReturns` (300 racing iterations) | Lock and `stopped` check removed: fails at iteration 2. |
| L4 | Same `pendingControl` counter as M1. The idle-reap branch refuses to exit while it is above zero. | `TestRunOnWorker_PendingControlBlocksIdleReap` (white-box and deterministic; an interleaving probe would be too narrow to fail reliably) | Reap condition reverted: fails (`must not reap itself`). |
| L5 | `internal/channel/telegram/chunk.go`: an empty plain segment no longer flushes `pending`. When it fits, the blank line stays inside the pending chunk. | `TestSplit_FencesSeparatedByBlankLineShareOneChunk` | `chunk.go` restored from HEAD: fails (2 chunks). |

Race runs: `go test -race -count=20` on the M1, L3, L4 and M2 tests (`-run 'TestRunOnWorker_|TestProgressReporter_NoticeNeverSent|TestDispatch_RepliesArriveInTurnOrder'`) passed.

## Cleanup

- **C1:** `worker.closing` and every `|| w.closing` check are removed, along with the comments that depended on it (`dispatch.go`).
- **C2:** `channel.Inbound.UserID` and `channel.DeliverTarget.Channel` are removed. The setters in `poll.go` and `cron/job.go` and the affected test literals and assertions (`dispatch_test.go`, `cron/job_test.go`) are updated.
- **C3:** one `groupAllowFrom(cfg, group)` in `gating.go` is used by both `decide` and `Approver.authorized`.
- **C4:** the stale "parse or too long" comments in `send.go` are fixed. The garbled `isCommand` comment is fixed. The `CaptureSenders` doc block now sits directly on the function.
- **Fake Bot API:** `internal/testsupport/fakeapi/telegram.go`. The stale MarkdownV2 comment is fixed. `pushLocked` is now `push` and `nextMessageIDLocked` is now `nextMessageID`, which takes its own lock; the backing field was renamed to `lastMessageID` to avoid a name clash. `push` assigns the callback query id together with the update id under the lock.

## Docs impact

The docs agent should cover these user-visible changes:

- **Ordered replies.** Replies within one session (chat and thread) now reach the chat strictly in turn order. A slow multi-chunk answer is no longer overtaken by the next turn's reply. Approval prompts, slow-tool notices and busy notices are not part of that ordering. Replies are still delivered detached from the turn, and shutdown still waits for them up to the reply drain deadline.
- **Shutdown latency.** Shutdown no longer waits up to 30 s for the in-flight `getUpdates`. Telegram API calls go through a net/http client with a 60 s timeout, and they abort on context cancellation (shutdown, `/stop`).
- **5xx retry.** A Telegram HTTP 5xx on `sendMessage` is now retried once after 1 s, for replies and approval prompts. Network-level errors are still not retried.
- **`retry_after` and incomplete replies.**
  - A 429 `retry_after` of up to 60 s is honored even if it outlasts the reply's send budget. Longer waits fail immediately. Shutdown still bounds how long the process waits for it.
  - If a multi-chunk reply fails after some chunks were sent, the chat gets one best-effort line: "(the reply above is incomplete: the rest could not be delivered)". The failure is logged at error level.
- **Cron minute evaluation.** The scheduler now wakes on each wall-clock minute boundary. It evaluates every whole minute between its last evaluation and now, up to 10 minutes before now, and logs a warning naming how many minutes were skipped. Evaluation happens at second 0 of each minute. There is no replay after a restart. A spring-forward gap still skips jobs scheduled inside the missing hour. A fall-back repeat still fires once.
- **`/new` and `/stop` timing.** A `/new` or `/stop` sent right after a message no longer waits behind that message's turn. The turn is cancelled, and a queued message is dropped instead of run against history that is being reset.
- **Blank line between fenced blocks.** Two fenced code blocks separated by a blank line are now sent as one Telegram message when they fit, instead of two.

## Not done / concerns

- **L6 (UTF-16 post-parse length) is deferred** as decided. Code-heavy replies are still over-split by byte length.
- **L7 (`cron run` timeout) is not mine.**
- **A reply that hits a flood wait extends its own deadline and drops the original context's cancellation.** Reply contexts are already detached from shutdown, so this is harmless for them. The approval prompt path (`turnCtx`, no deadline) never triggers the extension.
- **No timeout on the reply chain wait.** Each reply waits for the previous one without a bound of its own, because every send is bounded by its own timeout. A wedged first reply therefore delays later replies in that session. Shutdown is still capped by the reply drain deadline.
- **`/stop` while the message is still only in the queue** still says "nothing running", as before. Only the dequeue-to-registration gap was closed.
- **The `telegram` package tests take about 25 s.** That is not new; the shutdown-latency test adds under 1 s.
- **An unrelated stray file** appeared in the tree: `internal/gateway/lock_unix_test.go` (the lock owner's).

## Validation

```
gofmt -l internal/gateway internal/channel internal/cron internal/testsupport
```
No output.

```
go vet ./internal/gateway/... ./internal/channel/... ./internal/cron/... ./internal/testsupport/...
```
No output (pass).

```
go test -race -count=1 ./internal/gateway/... ./internal/channel/... ./internal/cron/... ./internal/testsupport/...
```
- `internal/gateway`: ok
- `internal/channel/telegram`: ok
- `internal/cron`: ok
- `internal/testsupport/fakeapi`: ok

```
go test -race -count=20 -run 'TestRunOnWorker_|TestProgressReporter_NoticeNeverSent|TestDispatch_RepliesArriveInTurnOrder' ./internal/gateway/
```
ok.

```
go build ./...
```
Clean.
