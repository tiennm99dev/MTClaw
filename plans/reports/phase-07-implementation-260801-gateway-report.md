# Phase 7 Implementation Report: Gateway Orchestration

## Executed Phase
- Phase: phase-07-gateway-orchestration
- Plan: `plans/260731-2219-mtclaw-core-system`
- Status: completed

## Files Created
- `internal/gateway/gateway.go` (215 lines) - `New`/`Run`, `telegramDeps`
- `internal/gateway/dispatch.go` (280 lines) - worker map, `dispatch`, `runWorker`, cancel registry, semaphore, reply/overflow logic
- `internal/gateway/queue.go` (60 lines) - `trySend`, `relayInbound`, queue-size constants
- `internal/gateway/progress.go` (110 lines) - typing refresh + slow-tool notice
- `internal/gateway/lock.go` (75 lines) - `Acquire`/stale detection (shared)
- `internal/gateway/lock_unix.go` / `lock_windows.go` - platform `processAlive`
- `internal/gateway/shutdown.go` (55 lines) - `notifyContext`, `waitForShutdown`/`drain`
- `internal/gateway/approver.go` (45 lines) - `approverMux` (not in the phase file's original list; see Deviations)
- `internal/gateway/dispatch_test.go`, `queue_test.go`, `lock_test.go`, `shutdown_test.go`, `gateway_test_helpers_test.go`
- `internal/cli/gateway_cmd.go` (55 lines) - `mtclaw gateway`

## Files Modified
- `internal/agent/loop.go` - `Meta.MessageID`; `Loop.Run` gains a `messageID string` parameter
- `internal/agent/loop_test.go` - 11 call sites updated for the new parameter
- `internal/cli/prompt_cmd.go` - `loop.Run` call updated (passes `""`)
- `internal/cli/root.go` - registers `newGatewayCmd`
- `internal/channel/channel.go` - `Inbound` gains `DeliverTo` (`DeliverTarget`), `Timeout`, `OnDone` (cron-ready plumbing, phase 8 fills them)
- `internal/tools/approver.go` - `Request.MessageID`
- `internal/tools/exec.go` - `ask()` passes `meta.MessageID` into `Request`
- `internal/tools/exec_test.go` - `recordingApprover` records the last `Request`; added `TestExec_MessageIDPassedThroughToApprovalRequest`
- `internal/channel/telegram/approver.go` - `Ask` sets `WithReplyParameters` from `req.MessageID` when present
- `internal/channel/telegram/approver_test.go` - two new tests for the reply-quoting behavior
- `plans/260731-2219-mtclaw-core-system/phase-07-gateway-orchestration.md` - frontmatter status, success-criteria checkboxes
- `plans/260731-2219-mtclaw-core-system/plan.md` - phase 7 row -> Completed

`internal/channel/telegram/commands.go` was **not** modified: its `Deps` interface (`Status`/`Reset`/`Cancel`) already existed from phase 6 unchanged; phase 7 supplies the real implementation (`telegramDeps` in `gateway.go`) rather than needing to touch the command-dispatch code itself. `internal/cli/doctor_cmd.go` does not exist yet (phase 9) and was correctly left untouched per the task's explicit instruction.

## Tasks Completed
- [x] Per-session workers keyed `channel:chatID:threadID`, buffered queue 8; global inbound buffered 256
- [x] Reap/enqueue race fixed by serializing the worker's exit decision and the dispatcher's enqueue under one mutex (`dispatcher.mu`); `-race` hammer test (1ms idle timeout, 1500 iterations) asserts every dispatched message is processed
- [x] Idle reap after 5 min (`idleSessionTimeout` constant)
- [x] Overflow: session-queue-full -> one "still working..." reply + drop; global-queue-full -> warn log + drop (via a two-stage relay so `channel.Start`'s blocking send never becomes the enforcement point)
- [x] Global concurrency cap via a plain buffered channel (`sem chan struct{}`), no `x/sync` dependency added
- [x] Cancel registry folded into each `worker.cancel` field (guarded by the same dispatcher mutex) rather than a second parallel map; `/stop` wired end to end via `telegramDeps.Cancel`
- [x] Approver mux (`approverMux`) selects `"telegram"` -> the Telegram channel's approver, everything else -> `DenyAllApprover`; registry built once with the mux, mux filled in after the channel is constructed
- [x] MessageID threading: `agent.Meta.MessageID`, `tools.Request.MessageID`, `exec.go`'s `ask()`, and the Telegram approver's reply-to-quote, all wired; `Loop.Run` signature updated and all call sites fixed
- [x] Startup order: lock -> store (migrations) -> provider -> registry+mux -> channel -> (loop) -> dispatcher fields filled; any `New()` failure tears down what it already opened (store closed, lock released) before returning
- [x] Shutdown order: channel context canceled first (via `signal.NotifyContext` wrapping the caller's ctx), then drain (30s deadline, `drainDeadline` package constant), then `defer`-ordered store close and lock release
- [x] Instance lock: `O_CREATE|O_EXCL`, PID-based stale detection via `processAlive` (POSIX: `Signal(0)`; Windows: `os.FindProcess`/`OpenProcess` semantics), remove-and-retry-once on stale
- [x] Progress: typing refresh every 4s while a turn runs; "running `<tool>`..." once per tool call still running after 8s
- [x] Cron-ready `Inbound` fields (`DeliverTo`, `Timeout`, `OnDone`) plumbed through `runTurn`/`reply` without the dispatcher knowing cron exists
- [x] `gateway_cmd.go`: requires `channels.telegram.enabled`, logs a startup banner (version/model/exec-mode/workspace/db-path, warns on `auto`), runs, returns the error for a non-zero exit
- [x] No HTTP listener anywhere (confirmed via `grep -r "ListenAndServe\|http.Serve\|net.Listen"` across `internal/` - no matches)

## Bug found and fixed during manual verification
Manual testing (fake token, `mtclaw gateway`) surfaced a real defect: if `channel.Start` returned an error *after* Run had started (e.g., a bad/revoked token causing `getMe` to fail with 401), the gateway kept running forever with a dead channel - no path for new messages, but the process never exited. Fixed by having the channel goroutine call `stop()` (the `signal.NotifyContext` cancel func) and record the error when `Start` fails while `sigCtx` is not yet done; `Run` now returns that error (surfaced to `main` as a non-zero exit) instead of idling. Verified manually: the process now logs the failure, drains, releases the lock, and exits 1. Also fixed a cosmetic double-prefixed error (`"gateway: gateway: another instance..."`) in the same manual pass.

## Tests Status
- Type check / build: pass (`go build ./...`, `CGO_ENABLED=0 go build ./...`)
- `gofmt -l .`: clean
- `go vet ./...`: clean
- Unit tests: pass, `go test ./...` (all packages)
- Race: pass, `go test -race ./internal/gateway/...` (repeated 3x plus the full run at the end, all green)
- `go mod tidy`: no changes (no new dependency added, per the "avoid `x/sync`" instruction)
- Manual: `mtclaw gateway` with `channels.telegram.enabled: false` refuses with a clear non-zero exit; with an invalid token format, a clean non-zero exit; with a well-formed-but-unauthorized token, the new bugfix path drains and exits 1 with the lock released; second-instance-while-running refusal confirmed (names the PID, lock file still present); the earlier double-prefix bug confirmed fixed. Full Telegram round-trip and real SIGINT/SIGTERM delivery to a native Windows binary were not exercised (git-bash's `kill` does not reliably deliver POSIX signals to a Windows console binary; this needs a real terminal Ctrl-C or a Linux box) - the shutdown *logic* itself is covered by direct unit tests (`drain`/`waitForShutdown`) that cancel a context programmatically instead of relying on OS signal delivery.

## Design notes worth flagging
- `approverMux` lives in its own `internal/gateway/approver.go`, one file beyond the phase file's original list (`gateway/dispatch/queue/lock/shutdown/progress.go`). It's small and single-purpose (mux construction + `setTelegram` + `Ask`); folding it into `gateway.go` seemed worse for readability, so it stays separate. No file-ownership conflict since it's entirely inside the phase's exclusive `internal/gateway` package.
- The reap/enqueue race test (`TestDispatch_ReapEnqueueRace_NoMessageIsEverDropped`) synchronizes one-message-at-a-time via a buffered-1 `processedCh` rather than blasting N messages into one session queue unpaced. Unpaced dispatch at a 1ms idle timeout would routinely overflow the 8-slot session queue (a *different*, correctly-triggered feature) and turn the race assertion into a flaky false failure. The synchronized version still genuinely races the exact mutex-protected boundary (worker reap vs. next dispatch) every iteration, 1500 times, under `-race`.
- `lock_test.go`'s original approach for "a guaranteed-dead PID" spawned and waited on a short-lived re-exec of the test binary; under `-race` on Windows this was flaky because the just-exited PID was sometimes reused almost immediately by another process spawned by the test tooling. Replaced with a fixed out-of-range sentinel PID (2,000,000,000), which is simpler and deterministic.
- The startup banner's "bot username" requirement is satisfied by composition rather than a new log line: `telegram.Channel.Start` already logs `"telegram: bot ready", "username", ...` (existing phase 6 code) once `getMe` resolves, right after `gateway_cmd.go`'s own banner (version/model/exec-mode/workspace/db-path). Synchronizing on the username before logging a single combined banner would have required either a new network call in `Gateway.New` (moving a real Telegram API call into wiring, changing failure semantics) or a new `Channel.Username()` getter plus fragile cross-goroutine synchronization with `Start()`'s own resolution. Two log lines were the smaller, safer change.

## Unresolved Questions
1. Full Telegram round-trip (`mtclaw gateway` against a real bot) and real OS-signal (SIGINT/SIGTERM) delivery to the compiled Windows binary were not exercised in this environment - both are called out in the phase file itself as "Manual end-to-end" items. Recommend a manual pass with a real bot token before shipping, and/or a Linux CI job for signal-delivery confidence.
2. Success criterion "Shutdown with a pending approval completes in seconds" is not covered by a dedicated new automated test in this phase - it holds by composition (phase 6's `Approver.Ask` already selects on ctx, tested there; phase 7 derives every turn ctx from the same root ctx signal cancels). If this composition is ever refactored, consider adding a phase-7-level test that spins up a real pending approval and confirms shutdown still completes quickly, rather than relying on the two phases' tests staying consistent by inspection.
