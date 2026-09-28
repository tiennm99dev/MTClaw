# Third-round review: channel/telegram, gateway, cron

Date: 2026-09-28. Branch `refactor/260928-full-review` @ cd7806d. Read-only review.

Scope: `internal/channel/**` (1,765 non-test LOC in telegram, 1,822 test LOC), `internal/gateway/**`,
`internal/cron/**`. Fixed items from rounds 1 and 2 are not re-reported. Accepted product decisions
(telego poll not fatal, no fs-write approval gate, no exec_audit requester columns) are not re-litigated.

Checks run:
- `go test -race -count=1` on all three packages: pass. `go vet`: clean.
- Coverage: telegram 71.0%, gateway 60.1%, cron 87.6%.
- Probes ran in a throwaway copy of the repo, not in the working tree: other-bot command interception,
  approval-prompt fence injection, and long-info-string fence amplification. All three reproduced.

## Verdict

Round 2 held: no Critical issues, and no regressions from round 2. Four new bugs are confirmed by
probes. One more is a catch-up feature that does nothing in its main use case, and passes tests only
because the tests use a fake clock. The telegram package's size is mostly justified. The one refactor
that clearly pays off is to stop using MarkdownV2 escaping and send entities instead. That change
deletes a whole group of bugs.

---

## Bugs

### High

**B1. The approval prompt can be spoofed by the command text itself.**
`internal/channel/telegram/approver.go:128`, `:410-417`

`formatApprovalPrompt` puts `req.Command` inside a ```` ``` ```` fence, then runs `EscapeMarkdownV2` on
the whole prompt. The escaper ends a fence at the first ```` ``` ```` it finds anywhere. So a
model-generated command (the model can be steered by prompt-injected `web_fetch` content) that contains
its own fence line breaks out of the code block. It can then print prose that looks like part of the
bot's UI. Probe output (valid MarkdownV2, so the plain-text fallback never runs):

```
Approval requested for exec:
```
echo hi
```
reason: read\-only listing, safe       <- attacker text, rendered as prose
```
rm -rf ~/work
```
reason: writes                          <- real classifier reason
```

This message is the only security control the user sees for `exec`. The attacker controls how the
command is laid out, and can fake a "reason" line or push the dangerous part into a block that looks
like a separate message. Fix: build the escaped prompt from its parts instead of escaping the finished
string:

```go
"Approval requested for " + EscapeMarkdownV2(req.Tool) + ":\n```\n" +
    escapeBackslashAndBacktick(req.Command) + "\n```" + "\nreason: " + EscapeMarkdownV2(req.Reason)
```

MarkdownV2 allows `` \` `` inside `pre`, so the command can no longer close the fence. A better fix is
to send a `pre` entity (see R1), which needs no escaping at all. Add a test that uses a command
containing ```` ``` ````.

**B2. A command meant for another bot is run by this bot in groups.**
`internal/channel/telegram/commands.go:64`, `poll.go:67`

`commandName` removes any `@suffix` without checking that the suffix is this bot's username. In a group
with `require_mention: false` (which needs privacy mode off, so the bot sees every command), any
allowlisted member typing `/new@SomeOtherBot` wipes this bot's session history. `/stop@OtherBot`
cancels its running turn, and `/start@OtherBot` posts the help text. Reproduced: `resetCalls=1`. Fix:
in `commandName`, or in `handleMessage`, treat `/cmd@X` as a command only when `X` equals `c.username`
(case-insensitive). Otherwise drop it quietly. This is a one-line check plus a test.

### Medium

**B3. Replies, the busy notice, and approval prompts fail if the user deleted the message being replied to.**
`send.go:68`, `approver.go:138`

`ReplyParameters` is set without `AllowSendingWithoutReply: true`. If the triggering message was
deleted while the model was thinking, Telegram returns 400 "message to be replied not found". That text
matches neither the parse nor the too-long fallback, so:
- the whole turn's reply is lost (logged only);
- `Ask` fails, so a command the user would have approved is refused.

Fix: set `AllowSendingWithoutReply: true` in both places.

**B4. Cron catch-up never triggers after a host suspend or a wall-clock jump.**
`internal/cron/scheduler.go:160` (plus `Start`'s `lastTick = s.now()`)

`now.Sub(lastTick)` compares two `time.Now()` values. Both carry monotonic readings, so `Sub` uses the
monotonic clock only (per the `time` package docs). That clock does not advance during suspend on
Linux (`CLOCK_MONOTONIC`, golang/go#24595), on macOS, or on Windows. After a 2-hour suspend, the gap
computed here is about 60s, so the catch-up path never runs. Suspend is the first use case the code
comments name. The same goes for an NTP forward step. The tests pass only because the injected `now`
returns `time.Date(...)` values with no monotonic reading. This is a textbook "passes CI, breaks in
prod" bug; the same bug is filed against another Go cron daemon (huketo/herdr-cron#21). Fix: compare
wall clocks, `now.Round(0).Sub(lastTick.Round(0))` (or strip once when storing `lastTick`). Add a test
that passes `time.Now()`-derived values through `Add`, which keeps the monotonic reading, to lock this
in.

**B5. A synchronous session-busy reply stalls the dispatcher pump for every session.**
`internal/gateway/dispatch.go:218-221`, `:517-529`

`dispatch` runs on `pump`, the only reader of the global queue. When a session queue overflows, it
calls `replySessionBusy`, which does a blocking Telegram `Send` for up to 10s. The busy notice lands in
the same chat that is already sending fast, so a 429 `retry_after` is likely and the 10s budget gets
used up. Say a user forwards 20 messages at once: 12 overflow, the pump stalls for up to about 2
minutes, and every other chat's messages wait in the global queue, or are dropped once it fills. The
comment at `:514` says the reply is bounded "so a slow or dead channel cannot stall the dispatcher
itself", but a bounded 10s synchronous call does stall it. `sessionBusyReply`'s comment says the notice
is "sent once", but it is sent once per dropped message. `TestDispatch_SessionQueueFull_OneHonestReplyNoLostMessages`
overflows only once, so its "exactly one reply" assertion proves nothing about the "once" claim. Fix:
add a `busyNotified bool` on `worker`, set under `d.mu` on first overflow and cleared when the worker
takes its next message, and send on a goroutine. Extend the test to overflow 5 times and assert 1 send.

**B6. A long fence info string turns one reply into hundreds of messages and cuts UTF-8 mid-character.**
`chunk.go:141-151` (`fenceOpenLang`), `:173-177` (budget clamp), `:278-280` (`safeRuneCut`)

Round 2 fixed the infinite recursion, but not what comes out. A fence whose info string is at least
4,090 bytes clamps `splitFence`'s budget to 1 byte, so each byte of the body becomes its own chunk of
more than 5,000 bytes. `hardCutParts` then cuts each chunk into 2 parts. Probe: 5,608 bytes of input
(5,000-byte info string, 300 × `é`) produced **1,200 messages, 600 of them invalid UTF-8**.
`safeRuneCut`'s `limit = 1` fallback cuts inside a 2-byte rune. With a non-ASCII body, Telegram rejects
the second part and the reply is lost after one garbage message. With an ASCII body, the chat gets
spammed until `replyTimeout` (15s here) cuts it off, and the reply is lost either way. Fix: in
`fenceOpenLang`, reject info strings over a small cap (say 64 bytes) so the line is treated as plain
text. In `safeRuneCut`, move forward to the end of the first rune instead of returning 1.

### Low

- **B7. `/new` during a running turn is not a clean reset.** `gateway.go:283-289`. `Reset` deletes
  messages on the Telegram pump goroutine, outside the session worker. The running turn flushes its
  buffered messages when it ends, so the "fresh" conversation starts with the old turn's user and
  assistant rows. Fix: have `Reset` refuse ("a turn is running, /stop first") when `cancelSession`'s
  registry shows a turn in flight, or run the reset through the session worker.
- **B8. `/stop` while a turn waits for a global slot answers "nothing running", and the turn then
  runs.** `dispatch.go:407`: `w.cancel` is set only after the semaphore and `Ensure`. Fix: create the
  turn context and register `cancel` before waiting on the semaphore, and wait on `turnCtx.Done()` in
  that select.
- **B9. The lock error tells the operator to delete a live flock file.** `lock_unix.go:31,33`: "remove
  %s if this is wrong". With flock, the lock can only be held by a live process. Deleting the file lets
  a second gateway flock a new inode, which gives two pollers (409 loops) and cron firing twice. The
  advice fits the Windows PID scheme only. Fix: on unix, say "stop pid N first".
- **B10. A reply budget longer than the drain window makes a clean shutdown look like a breach.**
  `dispatch.go:485` against `shutdown.go:14`. A turn that finished just before SIGTERM with about 40KB
  of text gets a 55s send budget, but the drain allows 30s. The store is then left open and the process
  exits non-zero, even though nothing is still writing. Fix: cap `sendCtx` at the time left in the
  drain, or run `callOnDone` and flush, then send without holding `wg` (the send uses no store).
- **B11. The startup approval sweep misses rows that have not expired yet.** `approver.go:68-77` with
  `sqlite/approvals.go:109` (`expires_at < now`). A crash with a prompt less than
  `approval_timeout` old leaves the row `pending` until the next restart, and the old message keeps
  live-looking buttons. It is safe because a later tap gets "no longer waiting", but the comment "stops
  those rows from sitting pending forever" is false. The instance lock means every pending
  `channel='telegram'` row at startup is orphaned. Fix: sweep them regardless of age, keeping CLI rows
  out.
- **B12. Commands run on the Telegram pump with an unbounded context.** `/new` and `/status` do
  `Ensure` and a delete or count on `ctx` with no timeout. A held SQLite write lock stalls every update,
  including approval callbacks. Round 2 bounded `Ensure` in `runTurn` for this exact reason. Apply the
  same 10s bound here. `cron.fire`, `recordRun`, and `onDone` also use `context.Background()`, which is
  the same inconsistency.
- **B13. `CaptureSenders` counts pending updates from before the window.** `capture.go`: getUpdates
  first returns up to 24h of pending updates, so a stranger who messaged the bot earlier shows up as a
  candidate owner, and those updates are acknowledged (consumed). The caller asks for confirmation, so
  this is not a bypass. Fix: drop messages with `msg.Date` before the window start.
- **B14. `replyTimeout` still ignores escape inflation** (round-1 L6, left open on purpose). This goes
  away with R1. Otherwise use `replyChunkLimit/2`.
- **B15. Shutdown logs errors for normal operation.** `runWorker`'s `select` can pick a queued message
  after `rootCtx` ends. `runTurn` then runs `Ensure` on a canceled context and logs `ERROR ensure
  session failed` for each queued message. Fix: check `d.rootCtx.Err()` at the top of each loop and
  drain.
- **B16. Queued Telegram messages are lost silently on restart.** The global queue (up to 256
  messages) and the worker queues are dropped at shutdown after Telegram has already acknowledged those
  updates, and the senders get no reply. Persisting them is out of scope. A one-line "restarting, please
  resend" to each dropped chat is cheap, or at least a note in the docs.

---

## Refactors

Only refactors that clearly reduce complexity are listed, in order of value.

**R1. Replace MarkdownV2 escaping with `entities` for code spans.** Medium effort, highest value.
`EscapeMarkdownV2` escapes every special character outside code. So the model's `**bold**`, `_it_`, and
links are never rendered; they show up as literal characters. MarkdownV2 is used only to get monospace
code blocks. The cost of that one feature:
- `format.go` (99 LOC);
- `escapeChunk` and `hardCutParts`, with their recursion and termination proof (`send.go:85-149`);
- the 400 parse and too-long fallback branch;
- the mismatch between the escaper's fence detection and the splitter's (the escaper closes a fence at
  any ```` ``` ````, the splitter only at a line that is exactly ```` ``` ````);
- B1, B6's second step, and B14.

Sending plain text plus `pre`/`code` `MessageEntity` offsets (UTF-16; `gating.go` already has the
helper) removes all of that. `chunk.go` already knows where every fence starts and ends, so each chunk
can emit its own entities. Benefit: about 200 LOC less, no parse-error round trips, no escape
inflation, and the approval prompt becomes safe by construction. Risk: UTF-16 offset bugs, covered by
table tests using emoji and CJK. Rendering stays the same, since today only code renders anyway.
Product check: confirm that "code blocks only" is the intended rendering. If the user wants real
bold/italic, that is a new feature, and neither the current approach nor this one provides it.

**R2. Remove the global queue and `relayInbound`.** Small effort. `dispatch` is already non-blocking
once B5 is fixed. Per-session queues (8) already bound memory, and the number of sessions is bounded by
the allowlist. The raw → relay → global → pump chain adds a second drop path,
unbounded notify goroutines (round-1 L2, still open), `errGlobalQueueFull`, `queue.go`'s drop tests, and
a goroutine. With `Channel.Start` writing straight to the channel `pump` reads, the Telegram pump's
blocking send pushes back on telego's 100-slot buffer and on Telegram's own server-side queue, which
holds messages better than dropping them. Benefit: about 60 LOC and one goroutine less, no silent-drop
path. Risk: no hard cap on total messages in flight. That is fine for a single-user bot. Keep
`trySend` for worker queues.

**R3. Build the Telegram channel before the tool registry, and remove `approverMux`'s mutex and
setter.** Small effort. `telegram.New` needs only `st.Approvals()` and `deps`, not the registry.
Construct it first and pass `newApproverMux(tgChannel.Approver())` into `tools.New`. That removes the
`RWMutex`, `setTelegram`, and the nil path, which today quietly falls back to deny-all if the wiring
order ever regresses (`gateway.go:99-121`, `gateway/approver.go`). Risk: none.

**R4. Give the dispatcher lifecycle one entry point.** `rootCtx` is assigned in `Run`
(`gateway.go:186`), `closeForShutdown` runs in a hand-written goroutine, and `dispatch` checks both
`rootCtx.Err()` and `d.closed`. Add `d.start(ctx)`, which sets `rootCtx` and calls
`context.AfterFunc(ctx, d.closeForShutdown)`, and drop the unsynchronized fast path, since `d.closed`
already gives the guarantee. Benefit: one shutdown flag, and no field written after construction.
Risk: low.

**R5. Split `approver_test.go` (945 LOC).** Lines 700-945 test `sendOne`, `sendText`, and
`escapeChunk`, not the approver. Move them to `send_test.go`. Pure move, zero risk. This accounts for a
large part of the "3,600 lines" impression. The non-test package is 1,765 LOC across 10 files, the
largest being `approver.go` at 427. That size is justified by the approval-race handling. The bulk that
is not justified is the comment prose; see R7.

**R6. Small DRY and dead-code cleanups.** Each is trivial and zero risk.
- `commandNames` map plus two separate ordered arrays (`commands.go:74`, `:143`): replace with one
  ordered `[]struct{name, desc}`.
- `SendOnce`, `GetMe`, and `CaptureSenders` repeat the empty-token check and `NewBot(…WithDiscardLogger)`:
  replace with one `oneShotBot(token)` helper. `CaptureSenders` also skips the empty-token check.
- The nil-`Deps` branches and the "not available yet" text in `handleCommand`: the only production
  caller always passes deps.
- Exported symbols used only inside the package: `NewApprover`, `Split`, `EscapeMarkdownV2`, `Decide`.
  Unexport them.
- `recordRun(started, finished)`: every caller passes the same value twice.
- cron `running map[string]*atomic.Bool` mixed with `s.mu`: use a `map[string]bool` under `s.mu` and a
  single synchronization primitive.
- `waitForShutdown` is a one-line wrapper around `drain`: tests can call `drain` with the constant.

**R7. Remove plan and phase references and audit codes from comments and test names.** 22 non-test
comments cite "phase 6/7/8/9" or a plan file path, and test comments cite "the H1/M2/C1 regression
test". This breaks the user's rule on stable code artifacts. The codes are also ambiguous: "H1" refers
to three different bugs across `approver_test.go`. `api.go`'s comment points to the fakes in
`gating_test.go` and `chunk_test.go`, but they are in `approver_test.go`. More generally, many doc
comments run 10-20 lines about history ("the prior behavior", "unblocked once …") rather than about
invariants. Cut them to the invariant. Benefit: a large drop in comment LOC and less drift. Risk: none.

---

## Test gaps on real behavior

- `Gateway.Run` has no test at all (gateway coverage is 60%). Untested: the channel-failure path
  (`stop()`, then a non-zero return), the drained versus breached store-close decision, and `pumps.Wait`
  before close. A fake `Channel` plus the real sqlite temp store would cover it. The shutdown order is
  the most important invariant in the package.
- `telegramDeps` (Status, Reset, Cancel) has no tests. B7 lives there.
- Cron catch-up with monotonic-bearing times (B4).
- Repeated session-queue overflow (B5).
- Command-for-another-bot (B2), approval prompt containing fences (B1), deleted reply target (B3).
- `escapeChunk` long-info-string output: the existing C1 test checks only that the call terminates, not
  the part count or UTF-8 validity (B6).

## Checked and found fine

These were checked and are not issues:
- Callback authorization: both the chat match and the allowlist check apply, the callback data is a
  128-bit nonce, and a second tap is a no-op.
- The timer/callback race fix from round 2 holds, and `wait` never blocks.
- The dispatcher reap/enqueue race and `wg.Add`/`Wait` race: `d.closed` under `d.mu` closes it.
- `wrapOnDone` once-guard covers every terminal path.
- The cron minute de-dup across DST fall-back and the overlap CAS.
- flock semantics.
- Offset acknowledgement at shutdown: telego sets the offset before pushing an update, and the
  in-flight getUpdates carries it.
- The token never reaches logs.

## Unresolved questions

1. R1: is "only code renders" the intended Telegram look? If so, entities are strictly simpler. If
   real Markdown rendering is wanted, that is a separate feature.
2. B7: should `/new` refuse or queue while a turn is running?
3. B16: is losing queued messages on restart acceptable if the docs mention it, or should dropped chats
   get a notice?

Sources: [golang/go#24595](https://github.com/golang/go/issues/24595),
[huketo/herdr-cron#21](https://github.com/huketo/herdr-cron/issues/21).
