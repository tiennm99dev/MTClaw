# Gateway / Telegram / cron review (fourth round, read-only)

Date: 2026-09-29. Tree: `main` @ `e8b0b74`, clean.
Scope: `internal/gateway`, `internal/channel`, `internal/channel/telegram`,
`internal/cron` (plus the `cron run` body in `internal/cli/cron_cmd.go`).
Baseline: the items fixed or accepted in
`fullstack-developer-260928-1041-final-review-fixes.md` and
`fullstack-developer-260928-1041-telegram-gateway-cron-fixes.md` are not
re-reported. Accepted decisions (telego poll not fatal, fs writes ungated,
no exec_audit requester, `sh -c` bypass, B16 no inbound persistence,
spring-forward skip in `TestScheduler_DST_SpringForward_*`) are left alone.

Method: read every non-test file in scope. Probes ran in a scratchpad copy
(`git archive HEAD`) and never in the working tree. `go vet` and
`go test -race -count=1` pass for all three packages on the current tree.
A 60s native fuzz plus 200k watchdog-guarded random inputs on
`renderChunks` checked these invariants: part <= limit, balanced tags,
valid UTF-8, no raw `<` in the text, no hang. There were no violations.

## High

None.

## Medium

### M1. `/new` and `/stop` lose to a message the worker has just dequeued. `/new` then stalls the whole Telegram pump and fails.

`dispatch.go:320-345` (dequeue), `:499-501` (where `w.cancel` is first set),
`:420-433` (`cancelSession`), `:447-480` (`runOnWorker`), `poll.go:24-36,80`.

When a worker is parked in its `select`, `dispatch`'s send hands the message
straight to it. The worker has then dequeued the message, but `w.cancel`
stays nil until `runTurn` runs a few statements later. A `/new` in that
window does the following:

1. `cancelSession` finds nothing to cancel.
2. `runOnWorker` blocks on the unbuffered `w.control` for the whole
   uncanceled turn.
3. Reset's 10s `commandDBTimeout` expires. The user gets "could not start a
   new conversation: context deadline exceeded" and the message runs against
   the old history.
4. During those 10s the Telegram update pump is blocked, because commands run
   synchronously on it. No update is processed for any chat, and that
   includes approval callbacks. If the racing turn asks for approval, the tap
   cannot land until Reset gives up.

`/stop` has the same window: it replies "nothing running." and the turn runs
anyway.

Realistic trigger: "do X" followed quickly by "/new" or "/stop", or both
arriving in one `getUpdates` batch. A batch is typical right after a restart,
when Telegram replays the backlog. The existing M1 test only covers a message
queued behind a turn that is already registered.

Confidence: **Confirmed by running.** The probe warms a worker, dispatches
"hello", then calls `Reset` with a 200ms ctx. Reset failed 50/50 times, 42/50
under `-race`. The runner was never canceled.

Minimal fix: make "dequeue + register cancel" and "cancel + mark reset
pending" one critical section under `d.mu`.
- Add `w.resetPending bool`.
- In the dequeue branch, inside the existing `d.mu` block that clears
  `busyNotified`: if `resetPending` is set, drop the message with
  `callOnDone(in, context.Canceled)`. Otherwise create `turnCtx`/`cancel`
  there and set `w.cancel`. `runTurn` takes the ctx as a parameter.
- `runOnWorker` sets `resetPending` and calls `w.cancel` under `d.mu`.
  `wrapped` clears the flag.
- Regression test: this probe, which reproduces the bug deterministically.

### M2. Replies from consecutive turns in one session are delivered out of order and can interleave

`dispatch.go:560-561` (`go d.reply(...)` detached per turn).

The worker starts turn N+1 as soon as turn N's reply goroutine is spawned.
Nothing orders reply N against reply N+1, or against N+1's approval prompt,
slow-tool notice or busy notice.

Example: a 10-chunk answer to Q1 takes at least 2.25s of `interChunkDelay`
alone. Q2's short answer, or Q2's approval prompt, lands between Q1's chunks.
The per-chat serialization model in `docs/architecture.md` promises ordered
turns, and today that holds for the store but not for what the user reads.

Confidence: **Confirmed by running.** With a fake channel whose Send for Q1
takes 300ms, delivery order was `[short answer, long answer]`.

Minimal fix: keep delivery detached from `d.wg`, but chain it per worker.
Store `w.lastReply chan struct{}` under `d.mu`. Each reply goroutine waits on
the previous channel (bounded by its own `sendCtx`) before sending, then
closes its own. This keeps the drain-deadline property that B10 introduced.

### M3. telego's default fasthttp caller ignores ctx cancellation, so every shutdown waits for the in-flight long poll (up to 30s)

`channel.go:60-65` (`newBot` sets no HTTP client), `:191-206`; `gateway.go:254`
(`pumps.Wait()`); telego `bot.go:105` (`ta.FastHTTPCaller{Client: &fasthttp.Client{}}`),
`telegoapi/caller.go:59-64` (it honors only `ctx.Deadline()`, never
`ctx.Done()`).

What happens:
- On SIGINT/SIGTERM, `doLongPolling` cannot abort its in-flight `getUpdates`
  (`Timeout: 30`). The updates channel closes only when Telegram answers.
- `Channel.Start`, and with it `Gateway.Run`'s `pumps.Wait()`, therefore waits
  0-30s after drain has finished. While idle a long poll is almost always in
  flight, so nearly every shutdown pays this cost.
- Under `docker stop` (10s grace) this often ends in SIGKILL. At a terminal,
  Ctrl-C appears to hang.
- The same cause means `/stop` or shutdown cannot abort an in-flight
  `sendChatAction`, approval-prompt send or slow-tool notice. All of these
  use `turnCtx`, which has no deadline.

Confidence: **Confirmed by running.** Against an httptest Bot API that holds
`getUpdates` for 3s, the updates channel closed 2.80s after cancel, and
`sendTyping` returned after 3.0s despite a cancel at 100ms. The same server
with `telego.WithHTTPClient(&http.Client{})` closed the channel 159µs after
cancel.

Minimal fix: in `newBot`, pass `telego.WithHTTPClient(&http.Client{Timeout: 60 * time.Second})`.
The timeout must be longer than the 30s long poll. telego's `HTTPCaller` uses
`http.NewRequestWithContext`, so cancellation becomes immediate. Add a
shutdown-latency test against an httptest server that holds `getUpdates`.

### M4. The claimed 5xx retry in `sendOne` is dead code in production, and its tests are phantom

`send.go:273-279`; tests `send_test.go:115,155`; telego `telegoapi/caller.go:69-71`
(fasthttp) and `:116-118` (net/http).

Both telego callers turn HTTP >= 500 into
`fmt.Errorf("internal server error: %d")` before decoding any body. The
result is never a `*ta.Error`, so `errors.As` fails and the 5xx branch never
runs. A transient 502/503 from Telegram drops the reply, or the approval
prompt, on the first attempt.

The tests pass only because the fake returns
`&ta.Error{ErrorCode: 500}`, a value production can never produce. This is
the same class as the round-3 L5 phantom-coverage item.

Confidence: **Confirmed by running.** Against httptest returning 502 with a
JSON body: `err="...request call: internal server error: 502"`,
`isTaError=false`, 1 request, no retry.

Minimal fix, pick one:
- (a) Wrap the caller with `telego.WithAPICaller` in a small `ta.Caller`
  that returns `&ta.Error{ErrorCode: status}` for >= 500. The branch becomes
  live, and it composes with the M3 client.
- (b) Delete the branch and its two tests, and document "5xx not retried".

Either way, drive the test through a real `telego.Bot` against httptest,
not a hand-built `ta.Error`.

## Low

### L1. A 429 `retry_after` longer than the reply budget drops the reply, and a mid-reply failure truncates silently

`dispatch.go:611,628-638`; `send.go:75-77,260-265`.

A single-chunk reply gets a 10s `sendCtx`. Group flood control commonly
returns `retry_after` of 10-40s. `sleepCtx` then returns false and the whole
answer is logged as "send turn reply failed" and never reaches the user.

For a multi-chunk reply, a failure on chunk k leaves chunks 1..k-1 delivered
with no marker that anything is missing.

Confidence: Plausible, by reading.

Fix: have `sendOne` honor `retry_after` beyond the caller's deadline, up to a
cap (for example 60s) and still bounded by `drainReplies` at shutdown. Or
size `sendCtx` from the first 429.

### L2. Cron can skip a whole minute without triggering catch-up when the ticker's phase straddles a minute boundary

`scheduler.go:128-175`.

The ticker runs free once aligned, and a minute counts as "missed" only when
the gap exceeds 90s. Once the tick phase sits near `:59.99x`, a normal tick
(gap 60.004s) can go from 10:00:59.999 to 10:02:00.003. Minute 10:01 is then
never evaluated and nothing is logged.

The phase moves after a host suspend/resume, because the monotonic ticker is
not re-aligned to the wall clock. It also moves after an NTP step, or when a
first tick is slower than 60s.

Confidence: **Confirmed at unit level.** The probe calls
`tickWithCatchUp(10:02:00.003, last=10:00:59.999)` and a job `1 10 * * *`
fired 0 times. It is plausible in production (it needs a phase shift).

Fix, which also simplifies the code: track `lastEvaluatedMinute` (wall-clock
truncated) and evaluate every minute in `(last, nowMinute]`, capped at
`maxCatchUpMinutes`. Re-align with a timer each loop. This removes
`missedTickThreshold` and the `Round(0)` subtlety.

### L3. A slow-tool notice can be sent after the turn has finished, even after its reply

`progress.go:88-98,104-117`; `dispatch.go:544-548,502-507`.

The guard checks `p.ctx.Done()`, but `turnCtx` is canceled only in
`runTurn`'s defer, after `stop()` and after the reply goroutine starts. So
the guard never sees "stop already ran". `t.Stop()` does not wait for a
callback that is already running.

Result: when a tool finishes at about 8s, "running `exec`..." can arrive
after the answer.

Confidence: **Confirmed by running.** The probe sent the notice after
`stop()` returned in 1947/2000 iterations at a 50µs delay.

Fix: add a `stopped bool` under `p.mu`, set it in `stop()`, and check it
under `p.mu` in the callback. Alternatively give the reporter its own child
ctx and cancel it in `stop()`.

### L4. `runOnWorker` can hand off to a worker that has just reaped itself, then block for 10s

`dispatch.go:350-364` vs `:455-472`.

`runOnWorker` reads `w` under `d.mu` and releases the lock before sending on
`w.control`. An idle reap in that gap checks only `len(w.queue)`, so the
worker exits and the send blocks until Reset's ctx expires. This produces the
same pump stall as M1, but only at the exact idle-timeout instant.

Confidence: Plausible, by reading; the window is narrow.

Fix: count pending control senders under `d.mu`, next to M1's flag, and have
the reap branch refuse to exit while that count is above zero.

### L5. Two fenced blocks separated by a blank line are sent as two Telegram messages

`chunk.go:42-58,202-205`.

The blank line becomes a plain segment `""`. `splitPlain("")` returns nil, so
`len(pieces) != 1` flushes `pending`. This forces a message boundary that
nothing needed, which means an extra send and more exposure to rate limits.

Confidence: **Confirmed by running.** `"```a\nx\n```\n\n```b\ny\n```"`
became 2 chunks, well under the limit.

Fix: treat `len(pieces) == 0` as a one-piece empty candidate, or skip the
flush when the segment is empty.

### L6. Rendered HTML bytes are compared to Telegram's post-parse character limit, so code-heavy replies are over-split

`send.go:147-150`; `chunk.go:8-13`.

Telegram counts 4096 characters *after* entity parsing: tags do not count and
`&lt;` counts as 1. `fitHTML` counts markup and entity bytes instead.

Example: 50,000 `&` produced 74 messages where about 13 would fit. C++ or
HTML snippets inflate the same way. It is never unsafe, only wasteful, and it
feeds L1.

Confidence: Confirmed by probe (the counts above).

Fix, optional: measure the length after stripping tags and decoding entities
in UTF-16 units, and keep bytes as the fallback.

### L7. `mtclaw cron run` ignores the job's `timeout`

`internal/cli/cron_cmd.go:157`: `loop.Run(ctx, ...)` runs with the command
ctx only, while the gateway bounds the same job with `in.Timeout`
(`dispatch.go:489-490`). A manual run of a job that hangs runs until Ctrl-C,
and its `cron_runs` row cannot say "timed out".

Confidence: Confirmed by reading.

Fix: `ctx, cancel := context.WithTimeout(ctx, job.Timeout.Std())` around the
run.

## Cleanup (below real defects)

- **C1. `worker.closing` is dead state** (`dispatch.go:84,227,361,377,456`).
  It is set only together with `delete(d.workers, ...)` under the same
  `d.mu`, so any lookup under `d.mu` can never find a worker whose `closing`
  is true. Drop the field and the `|| w.closing` checks, and trim the
  comments that depend on it.
- **C2. Fields that are written and never read:** `channel.Inbound.UserID`
  (`channel.go:30`, set at `poll.go:94`) and `DeliverTarget.Channel`
  (`channel.go:18`, set at `job.go:49`). The dispatcher always delivers via
  its single `d.channel` (`dispatch.go:531-534`). Remove them, or honor
  `Channel` before a second channel exists.
- **C3. The allowlist resolution for a security check is written twice:**
  `gating.go:39-54` and `approver.go:366-379` both implement "group entry,
  else `*`, empty group list inherits the channel list". Extract one
  `effectiveAllowFrom(cfg, chat)` so the message gate and the approval gate
  cannot drift apart.
- **C4. Stale or garbled comments:**
  - `send.go:41-44` and `:95-98` still describe the old "400 naming parse or
    too long" condition. `sendOne` now falls back on any 400.
  - `commands.go:33-34` contains the garbled phrase "and known's caller".
  - `capture.go:17-27`: the `CaptureSenders` doc block is separated from the
    function by a const and another func, so godoc shows no doc for the
    exported function.

## Verified non-issues

- **Callback authorization** (`approver.go:272-313`): data is parsed, the
  message is required, the row's chat must match the press, and the presser
  must pass the chat's effective allowlist. All of this happens before
  `Decide`. An unauthorized press can only cause one `Get` and a generic
  answer.
- **Every inbound message path goes through `decide` before command
  parsing** (`poll.go`). Commands aimed at another bot (`/cmd@other`) are
  dropped. Bot and anonymous-admin senders (`IsBot`) are rejected. Channel
  auto-forwards (777000) fail the allowlist. Unsupported chat types are
  rejected. Edited messages, channel posts and media without a caption are
  ignored by design. Edits being silently ignored could get one line in
  `docs/telegram-setup.md`.
- **Cron exec approval:** `agent.Loop` takes `Meta.Channel` from the session
  (`loop.go:127`), which is "cron" for a cron turn, so `approverMux` uses
  `DenyAllApprover`. `cron run` wires the same approver.
- **Cron guards:** the minute de-dup and the overlap flag share one critical
  section. `fire` runs without `s.mu`, so a synchronous `OnDone` (dispatcher
  closed) cannot deadlock on `clearRunning`. `wrapOnDone` guarantees at most
  one `Finish`. The DST fall-back de-dup behaves as documented and tested.
- **Dispatcher reap/enqueue race:** one mutex correctly serializes it. The
  `closed` flag prevents `wg.Add` after `Wait`. `replyWG.Add` happens before
  `drainReplies` on the non-breach path.
- **Chunk/render safety:** the byte limit is never below Telegram's UTF-16
  length, cuts are rune-safe, and fuzzing found no over-limit or unbalanced
  output and no hang. Worst-case render of a pathological 4 KiB chunk took
  19ms.
- **Memory bounds:** each session queue holds 8 messages and the global
  inbound queue holds 256 with backpressure. Idle workers are reaped after 5
  minutes. There is at most one busy-notice goroutine per overflow burst.
  Callback goroutines are bounded by Telegram's update delivery rate.

## Unresolved questions

1. M2: should replies be strictly ordered per session, or is interleaving
   accepted as a cost of detaching replies (B10)? The fix keeps B10's drain
   property either way.
2. M4: make the 5xx retry real (custom caller), or delete it? This is a
   product call on whether a 502 counts as "not delivered".
