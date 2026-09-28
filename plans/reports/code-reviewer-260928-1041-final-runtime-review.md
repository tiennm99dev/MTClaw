# Final runtime review: fix pass verification

Date: 2026-09-28. Branch `refactor/260928-full-review`, working tree vs `HEAD`.
Scope: `internal/agent`, `internal/provider`, `internal/store`, `internal/tools`,
`internal/channel/telegram`, `internal/gateway`, `internal/cron`, plus the
`internal/cli` seams that wire them. The plan's decisions are treated as binding.
Accepted items (fs gate, audit columns, `sh -c` bypass, poll fatality) are not
re-reported.

## Method

- Copied the working tree to the scratchpad and ran `gofmt -l .`, `go vet ./...`,
  and `go test -race -count=1 ./...` there. Everything was green.
- Ran an adversarial property probe on `renderChunks` in the scratchpad copy:
  3,000 random Markdown mixes (backticks, fences, `**`/`__`/`*`/`_`, links,
  `& < > "`, CJK and emoji, headings) plus edge inputs (20k `&`, 4097 `<`, 3000 emoji,
  an escape-heavy fence, a 64-byte lang tag, 2000 links with 900-`&` URLs). The
  validator checks every chunk for: HTML at most 4096 bytes, plain fallback at most
  4096 bytes, valid UTF-8, only `b/i/code/pre/a` tags, balanced tags, legal nesting,
  only `&lt;&gt;&amp;&quot;` entities, no raw `<`/`>`, and no loss of letters or digits.
- Ran 16 mutation checks. Each one reverted a claimed fix in the scratch copy and
  re-ran that package's tests, to find phantom tests.
- Wrote a scratch reproduction for the `/new` race.

## HTML renderer verdict (production limit 4096)

At 4096 there were no invalid-HTML chunks, no oversized chunks, no rune splits, and
no loss of letters or digits across all probe inputs. The approval prompt escapes
the command inside `<pre><code>` and cannot break out. Defects found are listed
below (M5, L1, L2, L3).

## Critical

None.

## High

### H1. Windows: the new unconditional post-`Run` tree kill targets a PID that may already belong to another process
`internal/tools/exec.go:282` together with `internal/tools/exec_windows.go:24-29`.

`killProcessTree(cmd)` now runs after every `cmd.Run()`. On Windows that means
`taskkill /F /T /PID <pid>`. By then Go's `Process.wait` has already called
`doRelease` and closed the process handle (confirmed in `$GOROOT/src/os/exec_windows.go`),
so Windows is free to hand that PID to a new process right away. Windows reuses PIDs
aggressively.

Failure scenario: an exec finishes, the PID is reused by an unrelated process (the
user's editor, another concurrent exec), and `taskkill /F /T` force-kills that
process and its whole tree.

It also fails its own goal. `taskkill /T` on a dead root reports "not found", so a
backgrounded child is not killed on Windows. The plan decision ("background children
do not outlive the call") is not met there.

Before this pass, `killProcessTree` ran only from `cmd.Cancel`, while the handle was
still held, so this is a regression. On unix the matching `kill(-pgid)` after reap
has only a theoretical pgid-reuse risk.

Fix: on Windows, put the child in a Job Object with `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`
at start and close the job after `Run`. Or skip the post-`Run` kill on Windows
(build-tag it) and document the gap. Keep the unix behaviour.

## Medium

### M1. `/new` during a turn with a queued message stalls the Telegram update loop and fails half the time
`internal/gateway/dispatch.go:298` and `:325` (the worker `select`), `:417-452`
(`runOnWorker`), `internal/gateway/gateway.go:295-309`, and
`internal/channel/telegram/poll.go:80`, where `handleCommand` runs synchronously on
the update pump.

`runOnWorker` cancels only the in-flight turn. When that turn returns, the worker's
`select` chooses at random between the queued message and the control func. If the
queued message wins, a new, uncancelled turn runs to completion first. That can
include a full approval wait.

Meanwhile `/new` blocks the Telegram update pump for up to `commandDBTimeout` (10s).
That stalls every chat and stops approval callbacks from being received. `/new` then
fails with a timeout, and the queued turn has already run on the old history.

Scratch reproduction: the Reset timed out in **10 of 20** trials, which matches the
50% `select` odds. The existing `TestTelegramDeps_Reset_CancelsRunningTurnThenResets`
has no queued message, so it never hits this.

Fix:
1. Give control priority. At the top of the `runWorker` loop, do a non-blocking
   `select { case fn := <-w.control: fn(); continue; default: }`.
2. Have `/new` drop the session's queued messages (`callOnDone(in, context.Canceled)`),
   or cancel them.
3. Optionally run `Reset` off the pump goroutine.

### M2. The classifier's `reason` reaches approval surfaces unredacted, unescaped, and unbounded
`internal/tools/policy.go:211-215`, `internal/tools/exec.go:197`,
`internal/channel/telegram/approver.go:444-445`, and `internal/tools/approver.go:173`.

The fix pass cleaned `Request.Command`: it is redacted, control/bidi-escaped, and
capped at 3500. `Request.Reason` gets none of that. In auto mode, `Reason` is free
text from the LLM classifier, and the classifier sees the **raw** command
(`Policy.Evaluate(ctx, rawCmd)`).

Failure scenarios:
- The classifier quotes the command, secret included, in its reason. The secret then
  shows up in a Telegram group prompt and is stored in the `approvals.reason` column.
- Bidi or control characters in the reason (steered through the command text)
  reorder the Telegram text or inject terminal escapes into `mtclaw prompt`.
- A long reason pushes the prompt past 4096 characters. Both HTML and plain sends
  fail, and the approval fails closed as "expired" with a confusing error.

Fix: pass the reason through `RedactSecrets`, then `escapeControlAndBidi`, then a
small cap (for example 300 bytes) where the `VerdictAsk` decision is built.

### M3. Detached reply goroutines are dropped at process exit
`internal/gateway/dispatch.go:526` and `internal/gateway/gateway.go` (`Run`).

The reply-drain fix moved `d.reply` off `d.wg`. `Gateway.Run` now returns as soon as
the workers drain, and the process exits, killing any in-flight multi-chunk reply
(250ms between chunks, plus any 429 wait).

Failure scenario: a turn finishes just before SIGTERM. Its reply is cut off or never
sent. That contradicts `reply`'s own comment ("a reply produced right at shutdown
gets a real chance to go out"). Before this pass, drain waited for it.

Fix: track replies in a second `WaitGroup` and wait on it after `drain` with its own
bounded deadline. Do not count that wait as a drain breach.

### M4. `mtclaw cron run` runs the exec tool without `DisableEnvironRead`
`internal/cli/cron_cmd.go:114-135`. Only `gateway_cmd.go:41` and `prompt_cmd.go:37`
call it.

`cron run` builds the same registry (`s.newLoop`) and runs auto-allowed or allowlisted
commands with `OPENAI_API_KEY` in the parent environment. A same-uid child can read
`/proc/<ppid>/environ`. The docs name only gateway and prompt, so they are accurate,
but the protection has a hole.

Fix: call it in `newLoop` (the one place every tool-running command goes through), or
in `cron run` as well.

### M5. Intraword `_` and `*` are taken as emphasis, which corrupts identifiers outside backticks
`internal/channel/telegram/render.go:128-137` and `:209-228`.

Verified outputs:
- `MAX_READ_BYTES and file_name_here` renders as `MAX<i>READ</i>BYTES and file<i>name</i>here`.
- `__init__.py` renders as `<b>init</b>.py`.
- `2*3*4` renders as `2<i>3</i>4`.

The underscores and asterisks disappear from the visible text. Models often write env
var names, snake_case identifiers, and file names without backticks, so users will
copy wrong names.

Fix: follow CommonMark flanking rules, at minimum for `_`/`__`. An opening delimiter
must not be preceded by an alphanumeric, and a closing one must not be followed by
one. Apply the same check to `*` when both sides are alphanumeric.

## Low

### L1. Latent unbounded recursion in `fitHTML` causes a fatal stack overflow (not recoverable)
`internal/channel/telegram/chunk.go:284` and `internal/channel/telegram/send.go:121-129`.

When the fence budget is smaller than one rune, `splitByLineThenHardCut` appends a
trailing `""`. `split` then returns `[chunk, "```go\n\n```"]`. `len(pieces) != 1`, so
`fitHTML` recurses on the identical chunk forever. Reproduced at limit 40
(`"```go\n😀\n```"`) and at limit 100 with long lang tags.

By analysis this cannot happen at 4096: `reduced` is at least about 800 and the fence
overhead is at most 72 bytes. Still, a runtime `fatal error: stack overflow` kills the
whole gateway.

Fix:
- Do not append an empty `remaining` in `splitByLineThenHardCut`.
- In `fitHTML`, fall back to `hardCutHTMLParts` whenever any piece is not strictly
  shorter than `chunk`.

### L2. Empty-text chunks are not covered by the plain-text fallback
`internal/channel/telegram/send.go:198`.

A chunk made only of an empty fence (`"```"` or `"```\n```"`) renders as
`<pre><code></code></pre>`, and a whitespace-only hard-cut piece renders as `"   "`.
Both are empty after entity parsing. Telegram rejects them with 400 "message text is
empty". That message contains neither "parse" nor "too long", so there is no
fallback, and `sendText` drops every later chunk. The probe produced 21 such chunks
out of 3,000 random inputs at 4096. Real model output hits this rarely.

Fix: skip chunks whose text is empty after stripping tags. Consider falling back to
plain on any 400 while `ParseMode != ""`. It is cheap, and it also covers Telegram
rejecting an `href` (see the unresolved questions).

### L3. The fence opener accepts backticks in the info string
`internal/channel/telegram/chunk.go:150-159`.

A line like "```ls```" opens a fence with lang `ls````, and the rest of the message
becomes code. CommonMark forbids a backtick in a backtick fence's info string.

Fix: also reject a `rest` that contains a backtick.

### L4. A successful tool result is replaced by "failed: context canceled"
`internal/tools/registry.go:92-94` and `internal/agent/loop.go:190-196`.

The centralised `(out, nil)` to `(out, ctx.Err())` rule now also covers write_file and
a successful exec that finished just as `/stop` fired. The loop persists
`tool "x" failed: context canceled` and throws away `out`, although the side effect
happened and exec_audit records success. On the next turn the model may run it again.

Fix: when `runErr` is a ctx error and `result != ""`, persist the result with a
"turn canceled after this completed" note.

### L5. Phantom or missing regression coverage for claimed fixes (mutation survivors)
With each fix reverted, the tests still passed for:
- The startup sweep horizon (`telegram/approver.go:84`, `time.Now().Add(expireAllPendingHorizon)` changed back to `time.Now()`). No test plants a not-yet-expired pending row.
- The `displayCommand` control/bidi escape wiring (`tools/approver.go:272`). The only `displayCommand` tests check the length cap. The escape itself is unit-tested but not proven to reach `Request.Command`.
- The `TrimToByteBudget` wiring in `loadHistory` (`agent/loop.go:298`). Only unit tests exist.
- The cron `Round(0)` change (acknowledged as a coverage guard in the fix report).

These were killed correctly, so their tests are real: ctx-based Classify, `elideUpTo`,
the ExpirePending channel filter, `AllowSendingWithoutReply`, `busyNotified`, the
detached reply, the unconditional kill (unix), `filterEnv`, and the other-bot command
drop.

### L6. Finding-code and plan references still in code
- `internal/cli/sessions_cmd_test.go:107`: "mitigates B23".
- `internal/channel/telegram/gating_test.go:279`: "Phase 1 validation is responsible...".
- `internal/gateway/dispatch.go` `runTurn` comment: "(not after, as a prior version did)". This is historical narration; state the invariant instead.

### L7. The fix report does not match the code on the history budget; tool-call args are not counted
- The fix report says `maxHistoryBytes = 32 * 1024`. The code has `256 * 1024` (`internal/agent/loop.go:278`). 256 KiB is about 64k tokens. With 8k to 32k self-hosted `base_url` backends, every long session will hit the ErrContextLength retry first.
- `TrimToByteBudget` sums only `Content` and ignores `ToolCalls[].Arguments`. A large write_file body in the history is not counted.

### L8. `CaptureSenders` window cutoff compares second-resolution `Date` with a sub-second `windowStart`
`internal/channel/telegram/capture.go:39` and `:57`.

A message sent within the first second, or within the local-to-Telegram clock skew,
is dropped. Fix: `windowStart.Truncate(time.Second)`, optionally with a few seconds of
grace.

## Integration seams verified OK

- `provider.Classify(ctx, err)`: all four callers (loop, openai `classify`, `Probe`, mock) pass the call's own ctx. The loop decides cancellation from `ctx.Err()`, and `abortForCancellation` forces `ErrCanceled` while wrapping the cause, so the cron-timeout reply's `errors.Is(DeadlineExceeded)` check still works.
- ExpirePending: the query filters `channel = ?`, and the telegram approver passes `"telegram"`. The mutation test proves it.
- The lock next to the database: `LockPath` gives `storage.path + ".lock"`. It is shared by gateway `New`, `cron run`, and doctor. `storage.path` is made absolute during load. Symlinked aliases of the same db are not deduplicated (acceptable).
- The approval display string into Telegram `<pre>`: `displayCommand` escapes, then caps at 3500 bytes (bytes are at least UTF-16 units, so it stays under Telegram's post-parse 4096). The HTML escaping cannot break out of the block. `refused_too_long` is audited through `capForAudit`.
- `/stop` while queued behind the semaphore: the cancel is registered before the wait, and the wait selects on `turnCtx`.
- `DisableEnvironRead` is wired into gateway and prompt (see M4 for cron run).

## Recommended actions (priority order)

1. H1: stop the post-`Run` `taskkill` on Windows (Job Object, or build-tag it out).
2. M1: give control priority in `runWorker`, and have `/new` drop queued messages.
3. M2: redact, escape, and cap the classifier `Reason`.
4. M3: bounded wait for detached replies at shutdown.
5. M4: `DisableEnvironRead` in `newLoop`.
6. M5: flanking rules for `_` and `*`.
7. L1 and L2: progress guard, no empty trailing piece, skip empty chunks, broaden the 400 fallback.
8. L5: add the missing regression tests. L6: scrub the three references.

## Unresolved questions

1. Does Telegram reject `<a href="relative/path">` or `javascript:` hrefs with a 400 whose description lacks "parse"? Models often emit relative file links. This could not be verified without a bot token. The L2 broad-fallback fix covers it either way.
2. Does Telegram accept `<a><code>…</code></a>` and `<b><code>…</code></b>`? Its nesting rules ("all other entities can't contain each other") are ambiguous. The renderer emits both, and the plain fallback exists only if the rejection says "parse".
3. Does 256 KiB versus the reported 32 KiB reflect a deliberate change by the phase-2 agent? It needs a one-line decision.

Status: DONE_WITH_CONCERNS
Summary: Gate green. HTML chunking at 4096 is valid, fits, and loses no content under adversarial probing. One High issue (a Windows post-reap `taskkill` that can hit a reused PID) and five Medium issues remain; three of the Medium issues are regressions or gaps between slices (a `/new` race reproduced at 50%, dropped shutdown replies, and `cron run` without the dumpable guard).
