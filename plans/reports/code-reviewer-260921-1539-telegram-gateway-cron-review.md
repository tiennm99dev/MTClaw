# Re-review: channel/telegram + gateway + cron

Date: 2026-09-21. Scope: `internal/channel/channel.go`, `internal/channel/telegram/*`, `internal/gateway/*`, `internal/cron/*` (5,719 LOC incl. tests). Advisory only, no files modified.

## Gate results

- `go test -race ./internal/channel/... ./internal/gateway/... ./internal/cron/...` — PASS (telegram 1.2s, gateway 10.7s, cron 2.7s).
- `go vet` clean, `gofmt -l` clean.
- Coverage: telegram 60.5%, gateway 57.5%, cron 86.3%.

## Prior-report status (verified this pass)

- **H2 (drop path never calls OnDone) — fixed and holding.** `dispatch.go:184` `in = wrapOnDone(in)` + `callOnDone` on the queue-full path (`:196`), Ensure failure (`:341`), sem-wait shutdown (`:333`), and queue drain (`:289`). One residual hole remains — see H3 below.
- **M2 (prompt sent before approvals row) — fixed and holding.** `approver.go:119` `a.approvals.Create(...)` precedes `sendOne` at `:141`; send failure marks the row expired (`:146`).
- **M3 (late callback vs timeout) — still present**, see M3 below; still fails in the safe direction (deny) but produces contradictory records.
- **M4 (synchronous update pump) — still present**, see M5; impact is larger than the prior report implies because the 429 sleep at `send.go:84` runs inside the pump.
- **L4 (lock stale-removal race) — still present**, see L1; narrower than feared but cheaply fixable.

---

## High

### H1. MarkdownV2 escaping inflates a full chunk past 4096 → Telegram 400, reply dropped

`send.go:44-46`:
```go
chunks := Split(text, DefaultChunkLimit)
for i, chunk := range chunks {
    params := tu.Message(tu.ID(id), EscapeMarkdownV2(chunk)).WithParseMode(telego.ModeMarkdownV2)
```
`Split` bounds the **pre-escape** text at 4096; `EscapeMarkdownV2` then inserts a `\` before every char in `mdV2Special` (`format.go:12` — includes `.` `-` `(` `)` `!` `#` `+` `=`). Measured on this repo's own prose (`head -c 4096 docs/architecture.md`): **283 special chars per 4096 bytes ≈ 7% inflation**. Any chunk within ~93% of the limit exceeds 4096 after escaping.

Failure: model answer > 4096 chars → first chunk escapes to ~4380 → Telegram returns `400 "Bad Request: message is too long"`. `sendOne`'s fallback only matches `strings.Contains(..., "parse")` (`send.go:89`), so this is not caught; `sendText` returns an error, `dispatch.reply` only logs it (`dispatch.go:424`). **The user gets nothing for a long answer** — and it's deterministic, not flaky, for any long reply.

Fix (smallest): broaden the fallback condition at `send.go:89` to also match `"too long"` so the message degrades to unescaped plain text (`fallback.Text = plain` is ≤ 4096 by construction). Better, additionally: in `sendText`, after escaping, if `len(escaped) > DefaultChunkLimit`, re-`Split` that chunk with a reduced limit (`limit*limit/len(escaped)`) and send the sub-pieces — keeps formatting instead of dropping to plain.

Test gap: `chunk_test.go` only asserts pre-escape lengths; nothing asserts `len(EscapeMarkdownV2(chunk)) <= 4096`.

### H2. All getUpdates failures are invisible and retried forever — the "channel stopped unexpectedly" fatal path is unreachable

`channel.go:61` `telego.NewBot(token, telego.WithDiscardLogger())` + `channel.go:152` `telego.WithLongPollingRetryTimeout(8*time.Second)`.

telego v1.11.1 `long_polling.go:125-135`:
```go
updates, err := b.GetUpdates(ctx, params)
if err != nil {
    b.log.Errorf("Getting updates: %s", err)
    if lp.retryTimeout == 0 || errors.Is(err, context.Canceled) { return }
    b.log.Errorf("Retrying getting updates in %s...", ...)
    time.Sleep(lp.retryTimeout)
    continue
}
```
With a discarded logger and a non-zero retry timeout, a revoked token (401), a wrong token (404), or **409 Conflict from a second poller** is logged nowhere and retried every 8s forever. The updates channel never closes, `pumpUpdates` never returns, `Start` never returns, and `gateway.go:168-183` —
```go
channelFailure = fmt.Errorf("gateway: channel stopped unexpectedly: %w", err)
```
— is dead code for exactly the failure modes its comment names ("a revoked token, a sustained network failure"). Production symptom: process alive, zero log output, bot silently deaf. Also defeats the instance lock's purpose: a second gateway on another host 409s against the first and neither reports it.

Fix: replace `WithDiscardLogger()` with a small `telego.Logger` adapter (`Debugf` no-op, `Errorf` → `slog.Error` after `strings.ReplaceAll(msg, token, "<redacted>")`). The token-leak warning applies to telego's debug logging; the API error string carries no token (`telegoapi.Error.Error()` formats only code + description). Optionally also count consecutive getUpdates failures and `stop()` the gateway after N, restoring the intended fatal path.

### H3. A worker that exits on shutdown stays in `d.workers`; messages dispatched afterwards vanish (and `wg.Add` races `wg.Wait`)

`dispatch.go:274-277`:
```go
case <-d.rootCtx.Done():
    d.drainQueueOnDone(w)
    return
```
Unlike the idle-reap branch (`:269-270` sets `w.closing = true` and `delete(d.workers, w.key)`), this path leaves the worker in the map. `dispatch` (`:189`) only rejects `!ok || w.closing`, so it hands the message to a queue with **no receiver**: no OnDone, no reply, no log. For a cron fire that means the `running` flag stays set and the `cron_runs` row stays `started` (compounds M6).

Reachable: `pump` (`:150-161`) and `relayInbound` (`queue.go:44-58`) both `select` over `ctx.Done()` and the message case; Go picks randomly when both are ready, so dispatch after cancellation is normal, not exotic.

Second consequence: if all workers already exited (`d.wg` counter 0) while `drain`'s goroutine sits in `wg.Wait()` (`shutdown.go:48`), `spawnWorkerLocked`'s `d.wg.Add(1)` (`:235`) violates the documented WaitGroup rule ("calls with a positive delta that start when the counter is zero must happen before a Wait") and can panic `sync: WaitGroup misuse: Add called concurrently with Wait`. If it doesn't panic, the new worker may pick its queued message over `rootCtx.Done()` and run a full turn *after* drain returned, i.e. while `Run`'s deferred `g.store.Close()` (`gateway.go:142`) executes.

Fix (both at once), at the top of `dispatch`:
```go
if err := d.rootCtx.Err(); err != nil {
    callOnDone(in, err)
    return
}
```
and mirror the reap branch in the `rootCtx.Done()` case (set `closing`, `delete`, unlock, then drain).

Test gap: `dispatch_test.go:257` builds a `worker` by hand and calls `drainQueueOnDone` directly — it never exercises `runWorker`'s ctx-done branch, which is where this lives.

---

## Medium

### M1. Drain-deadline breach is logged, then ignored — store closes under running workers

`shutdown.go:42` `drain(...) bool` returns whether workers finished, but `waitForShutdown` (`:34-36`) discards it and `gateway.go:199-201` ignores it too. After 30s, `Run` proceeds to `pumps.Wait()` and the deferred `g.store.Close()` while turns are still mid-`Append` — the exact loss the comment at `gateway.go:137-139` says the ordering prevents.

Fix: propagate the bool; on false, either return a non-nil error from `Run` (so the process exits non-zero and the operator sees it) or skip `store.Close()` and let process exit close the fd — closing a database out from under a writer is worse than leaking an fd at exit.

### M2. Any non-text message from an allowlisted user starts a full LLM turn on an empty prompt

`gating.go:36` `return true, msg.Text, ""` — for a photo, sticker, document, or a group service message (`new_chat_members`), `msg.Text` is `""` (captions live in `Caption`/`CaptionEntities`, never read). `poll.go:60-67` then builds an `Inbound` with `Text: ""` and forwards it; nothing downstream guards it — `loop.go:109` is `provider.Message{Role: RoleUser, Content: userText}`. Result: a sticker costs a full model round-trip and writes an empty user row to history. In groups with `require_mention: true` this is filtered incidentally (no entities → no mention), so DMs and non-mention groups are the exposure.

Fix: in `handleMessage`, `if strings.TrimSpace(cleanText) == "" { return }` after gating. Optionally read `msg.Caption` + `msg.CaptionEntities` so captioned photos work at all (currently a captioned photo addressed to the bot is silently ignored in groups).

### M3. Approval timeout can win a race it already lost — DB says approved, tool is denied, message says "timed out"

`approver.go:159-173`:
```go
select {
case approved := <-wait:  return approved, nil
case <-timer.C:
    a.finishExpired(context.WithoutCancel(ctx), id, req.ChatID, msg.MessageID, "timed out waiting for a response")
    return false, context.DeadlineExceeded
```
Two windows: (a) a callback's `Decide` succeeds and pushes to `wait` at the same instant the timer fires — Go picks the timer arm at random; (b) the callback lands between `timer.C` firing and `finishExpired`'s `Decide` executing. In both, `HandleCallback` has already written state `approved` (`:244`) and edited the message to "✅ approved by user N" (`:268`), then `finishExpired`'s `Decide` returns `ErrAlreadyDecided`, is silently swallowed (`:180`), and `editOutcome` overwrites the message with "⏱ timed out" (`:183`) — while `Ask` returns `false`. The user is told their approval landed, then that it timed out, and the command does not run; the audit row says `approved` for a command that never ran.

Direction of failure is safe (deny), so Medium not High. Fix: drain `wait` before accepting the timeout, and don't edit when the row was already decided:
```go
case <-timer.C:
    select { case approved := <-wait: return approved, nil; default: }
    ...
```
plus have `finishExpired` skip `editOutcome` when `Decide` returned `ErrAlreadyDecided`.

Related, same class: after `Ask` returns, its deferred `delete(a.waiters, id)` (`:97-101`) runs, but a later tap still passes `Decide` and edits the message to "approved by user N" for a request nobody is waiting on. Worth a "no longer waiting" outcome edit when the waiter lookup at `:255` misses.

### M4. `Ensure` runs on `context.Background()` inside the worker, holding a semaphore slot

`dispatch.go:338`:
```go
sess, err := d.store.Sessions().Ensure(context.Background(), in.Channel, in.ChatID, in.ThreadID)
```
It executes *after* the semaphore is taken (`:331`) and before any cancelable turn context exists. A SQLite write blocked by another process (a manual `mtclaw cron run`, a backup holding the write lock) pins one of four global slots indefinitely, ignores `/stop`, and outlasts the 30s drain (→ M1). Fix: `ctx, cancel := context.WithTimeout(d.rootCtx, 10*time.Second)`.

### M5. Update pump is fully synchronous — a rate-limited send stalls approval buttons (prior M4, still open)

`poll.go:17-25` handles callbacks and messages inline in the range loop. `handleMessage` → `handleCommand` (`/status` does two DB queries) → `c.Send` → `sendOne`, which on a 429 does `sleepCtx(ctx, RetryAfter seconds)` (`send.go:84`) *inside the pump*. Telegram's per-chat retry_after is commonly 10-60s under group flood control. During that sleep no `CallbackQuery` is processed, so an approval button press is not answered (client spins) and can miss its `approval_timeout` entirely; telego's 100-buffer then fills and long-polling stalls (updates are not lost, but the offset stops advancing).

Fix: handle callbacks in their own goroutine (`go c.approver.HandleCallback(ctx, update.CallbackQuery)`) — the approver is already mutex-safe and the store is the serialization point — and/or move command replies off the pump. Keep `handleMessage`'s forward to `out` on the pump goroutine so backpressure semantics don't change.

### M6. No startup sweep for `cron_runs` rows stuck in `started` (prior unresolved question 2 — still unresolved)

`scheduler.go:223-226` writes the `started` row; only `onDone` (`:249-267`) ever finishes it. `grep` across `internal/store/sqlite/cron_runs.go` and `internal/cli/cron_cmd.go` shows `Append`, `Finish`, `List` and **no reconciliation query**. Any SIGKILL, OOM, drain-deadline breach, or H3 drop leaves the row `started` forever, so `mtclaw cron runs` reports a job as perpetually in flight. Approvals already have the pattern (`Approver.ExpirePending`, `approver.go:68`).

Fix: add `CronRunStore.ExpireStarted(ctx, before time.Time) (int, error)` and call it from `Gateway.New`/`Run` startup, marking them `interrupted`. Same 10 lines as `ExpirePending`, same rationale.

### M7. Cron has no missed-tick detection or catch-up

`scheduler.go:124-133` is a plain `time.NewTicker(time.Minute)` whose handler evaluates only `s.now()`. If a tick is delayed past the next minute (host suspend/resume, heavy load, SIGSTOP, a long GC pause on a small VM), that minute's due job never fires and **no row records the miss** — `tryFire` is never reached, so not even a `skipped` row. `Start`'s doc comment states the no-replay policy for startup, which is deliberate; an unrecorded runtime miss is not.

Fix (small): keep a `lastTick` and, when `now.Sub(lastTick) > 90*time.Second`, log a warning and evaluate each whole minute in the gap (bounded to, say, 10) — or, minimum viable, log the gap so the operator can see it happened.

### M8. A cron job that times out delivers nothing to its chat

`dispatch.go:396-402` suppresses the reply for `provider.ErrCanceled`, on the reasoning that "whoever triggered the cancellation" already explained it. True for `/stop`; false for a cron `job.timeout` (`dispatch.go:348` `context.WithTimeout(d.rootCtx, in.Timeout)`) — nobody is watching, and the configured `deliver_to` chat gets silence. Only `mtclaw cron runs` shows the `error` row. Fix: suppress only when the cancellation came from an interactive `/stop` — e.g. keep the suppression for `in.Channel == "telegram"` and, for cron, send `"scheduled job timed out after <d>"` to the deliver target.

---

## Low

- **L1. Lock stale-removal race (prior L4).** `lock.go:31-42` creates the file, *then* writes the PID. A second starter whose `O_EXCL` fails in that window reads a zero-length file, `readLockPID` returns a parse error, and the `// Stale (dead PID) or unreadable/corrupt` branch (`:62`) **deletes the winner's live lock** and takes its own. Both gateways then run (→ H2's silent 409). Also `release` (`:43-48`) removes `path` unconditionally, so the loser's release deletes whatever lock is there. Fix: `syscall.Flock(fd, LOCK_EX|LOCK_NB)` on unix / `LockFileEx` on windows — the platform split already exists (`lock_unix.go`/`lock_windows.go`), kernel-enforced, and self-releasing on crash (which also removes the whole stale-PID heuristic). Keep the PID in the file purely for the error message.
- **L2. Narrow 429 handling, no transient retry.** `send.go:83`: `apiErr.ErrorCode == 429 && apiErr.Parameters != nil && apiErr.Parameters.RetryAfter > 0`. A 429 without `parameters`, a 5xx, or a TCP reset returns immediately and the reply is lost (logged only). Add one bounded backoff retry for 5xx/network errors.
- **L3. 10s reply budget vs. multi-chunk sends.** `dispatch.go:421` `context.WithTimeout(..., 10*time.Second)` covers *all* chunks including `interChunkDelay` (250ms each, `send.go:20`) and any 429 wait. A 6-chunk answer plus one `retry_after: 5` exceeds it, truncating a delivered reply mid-way. Scale the budget with `len(chunks)` or set it in `sendText`.
- **L4. Backslash inside code spans breaks the whole message's formatting.** `format.go:26-38` copies fenced/inline code byte-for-byte; Telegram requires `\` and `` ` `` to be escaped *inside* `code`/`pre` entities too. Any code block containing `\` (regex, Windows path, `\n`) returns 400 "can't parse entities", and the fallback sends the entire message unformatted. Fix: inside the copied span, escape `\` and `` ` `` only.
- **L5. progressReporter timer hygiene.** `progress.go:97-104` overwrites `p.timer[ev.ToolCallID]` without stopping a prior timer for the same id (leaks a stray "running X..." message if a provider ever reuses a call id). Separately, a timer that fires between `progress.stop()` (`dispatch.go:373`) and `cancel()` (`:375`) sends a "running" notice *after* the turn reply — `p.ctx` is still live in that window.
- **L6. Global-queue drop is user-invisible.** `queue.go:51-54` warns and drops; unlike the per-session overflow there is no reply and (for non-cron) no OnDone. Documented as "should never happen"; if it does, the user sees the bot ignore them. Consider reusing `sessionBusyReply` here.
- **L7. Cron can't deliver into a forum topic.** `job.go:335` `channel.DeliverTarget{Channel: ..., ChatID: j.DeliverTo.ChatID}` — `ThreadID` is never populated, and `grep -rn "ThreadID|thread_id" internal/config/*.go` returns nothing, so the config has no field for it either. `DeliverTarget.ThreadID` is dead weight until that's wired.
- **L8. `/reset` is not a command.** `commands.go:17-24` defines `start help new status whoami stop`; a user typing `/reset` gets it forwarded to the model as a prompt. Fine if intentional — worth an alias since the task brief called it `/reset`.
- **L9. Test gaps.** No test file exists for `poll.go`, `commands.go`, or `channel.go`: gating is well covered as a pure function, but the pump (command interception, empty-text forwarding, callback routing) is entirely untested — M2 and M5 both live there. Also missing: escaped-length assertion (H1), `runWorker` ctx-done map removal (H3), dispatch-after-shutdown.

---

## Verified non-issues (checked, not findings)

- **Per-session serialization holds.** One worker per `sessionKey` (`dispatch.go:144`), one goroutine per worker, queue is the only entry; the reap/enqueue race is genuinely closed by holding `d.mu` across lookup+`trySend` (`:187-193`) against the reap's re-check (`:259-272`). `dispatch_test.go:345` exercises it 1500×.
- **DST minute guard is correct.** `wallClockMinuteLayout` (`scheduler.go:29`) keys on formatted local time, so the repeated fall-back hour de-dups and the spring-forward gap simply never appears as a `now`. Both directions are tested (`scheduler_test.go:248,272`).
- **`reply`'s cancel classification is sound.** Tool-level cancellation also routes through `loop.go:194 → abortForCancellation → provider.Classify` (`loop.go:314`), so `errors.As(&perr) && perr.Kind == ErrCanceled` in `dispatch.go:397` catches `/stop` during a tool call, not just during a provider call.
- **Approver authorization is doubly checked** (chat identity `approver.go:220`, presser allowlist `:227`, group inheritance `:284-288`) and every `HandleCallback` path answers the callback query. Deliberate design point: any allowlisted user may approve another's command — correct for an allowlist trust model, worth keeping explicit in docs.
- **`approverMux` fails closed** for cron/unknown channels (`approver.go:39-49`, tested `cron_approver_test.go:28`).
- **`running` map is immutable after construction** (`scheduler.go:69-72`), so the `atomic.Bool` per job is race-free; `-race` agrees.

---

## Keep / Refactor / Rewrite

| Package / unit | Verdict | Reason |
|---|---|---|
| `internal/channel` (iface) | **Keep** | 63 lines, stdlib-only, `DeliverTo`/`Timeout`/`OnDone` are exactly the seams cron needed. Right shape. |
| `telegram` — gating | **Keep** | Pure function, table-tested, fails closed. Add the empty-text/caption guard (M2) at the caller. |
| `telegram` — chunk + format | **Refactor (small)** | Chunking logic is sound; the escape/limit contract between them is broken (H1) and code-span escaping is incomplete (L4). Fix the seam, don't rewrite 350 lines of tested splitting. |
| `telegram` — send | **Refactor (small)** | Broaden 400 handling (H1), add transient retry (L2). ~15 lines. |
| `telegram` — poll/channel | **Refactor** | Two real defects (H2 silent poll death, M5 synchronous pump) both live in ~90 lines. Async callback dispatch + a redacting logger adapter. |
| `telegram` — approver | **Keep** | 358 lines doing persistence + auth + UI edit correctly; only the timeout/callback race (M3) needs a 6-line change. Not over-engineered — the persistence is what makes restart-safety possible. |
| `gateway` — dispatch/queue | **Keep + 2 fixes** | Design is right and not over-engineered: bounded global queue, per-session worker, idle reap, one mutex covering both the enqueue and reap decisions. Fix H3 and M4; leave the shape alone. The `raw`→`relayInbound`→`global` two-stage hop looks redundant but earns itself (it lets `Channel.Start` do a plain blocking send while overflow policy stays in one place). |
| `gateway` — progress | **Keep** | 120 lines, correctly scoped per turn. L5 is cosmetic. |
| `gateway` — shutdown | **Refactor (small)** | Drain works; its result is thrown away (M1). Propagate it. |
| `gateway` — lock | **Rewrite (small, ~40 lines)** | PID-file + liveness probe is the wrong primitive: it has an unavoidable TOCTOU window (L1) and cannot detect a peer on another host or in another container. `flock`/`LockFileEx` removes the stale-PID heuristic, the retry loop, and the race in one go. Smallest change that fixes the real problem. |
| `gateway` — approverMux | **Keep** | 49 lines, fails closed, exists for a real wiring-order reason. |
| `cron` | **Keep + 2 additions** | Two-guard design (wall-clock minute key + CAS overlap flag) is right and well tested; `job.go` is a thin config projection. Needs the startup sweep (M6) and missed-tick visibility (M7). No rewrite. |

No package in this slice warrants a rewrite; the defects are seam bugs and missing recovery paths, not structural. The only "rewrite" is the 98-line lock file, and it shrinks.

## Recommended actions (priority order)

1. H1 — broaden the 400 fallback to `"too long"`, then make `sendText` size-aware post-escape. Add a `len(escaped) <= 4096` property test.
2. H2 — redacting `telego.Logger` adapter instead of `WithDiscardLogger`; optionally fail fast after N consecutive getUpdates errors.
3. H3 — early `rootCtx.Err()` return in `dispatch`; mirror reap bookkeeping in the ctx-done branch. Add a test that dispatches after cancel and asserts OnDone fires.
4. M1 — propagate `drain`'s bool into `Run`'s exit path.
5. M2 — drop empty-text inbounds in `handleMessage`; decide whether captions should be read.
6. M4 — bound `Ensure`'s context.
7. M3 — drain `wait` in the timer arm; skip the outcome edit on `ErrAlreadyDecided`.
8. M6 — `ExpireStarted` sweep at startup, mirroring `ExpirePending`.
9. M5 — run `HandleCallback` in its own goroutine.
10. M7, M8, then Low items as convenient. L1 (flock) before any multi-container deployment.

## Unresolved questions

1. Is running a second gateway against the same bot token an intended-impossible state? If yes, H2 matters more (the 409 is the only signal and it's currently swallowed) and L1 should be `flock`. If deployments are single-host single-process by policy, L1 can stay as-is.
2. Should non-text messages (photo/sticker/voice) be ignored (M2's proposed fix), or is a future multimodal path intended to consume `msg.Caption`/`msg.Photo`? The answer decides whether the guard is `return` or a caption-extraction step.
3. Cron misses (M7): is silent skip acceptable (fire-and-forget briefings) or should a missed minute fire late with a `late` status row? Product call.
4. On drain-deadline breach (M1): prefer a non-zero exit with the store left open, or force-close and accept possible `database is closed` errors in the log? Both are defensible; the current behavior (close silently under running writers) is the one that isn't.
5. Approval semantics: should an approval be decidable only by the user who triggered the command, or by any allowlisted user (current behavior)? Currently unstated in docs; the `approvals` row has no requester user id to enforce the stricter rule if wanted.
