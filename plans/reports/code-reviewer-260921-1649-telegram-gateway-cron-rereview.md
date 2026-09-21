# Re-review: telegram / gateway / cron fix pass (uncommitted on 7927b6d)

Slice: `internal/channel/**`, `internal/gateway/**`, `internal/cron/**`,
`internal/store/store.go`, `internal/store/sqlite/cron_runs.go`.

Checks run: `go build ./...` clean, `go vet ./internal/...` clean,
`go test -race -count=3 ./internal/channel/... ./internal/gateway/... ./internal/cron/...`
all pass (17s / 23s / 6s). Two extra probes were run in a throwaway copy of the
repo (not in the working tree): an `escapeChunk` fuzz/termination probe and a
`flock` cross-process probe.

---

## Critical

### C1. `escapeChunk` recurses forever on a fence with a long info string - the send path hangs

`internal/channel/telegram/send.go:94-113`

```go
reduced := limit * len(chunk) / len(escaped)
...
for _, piece := range Split(chunk, reduced) {
	out = append(out, escapeChunk(piece, limit)...)
}
```

Termination relies on `Split(chunk, reduced)` returning strictly shorter pieces.
That holds for plain text and for ordinary fences, but not when the fence's
opening line is itself longer than the limit. `splitFence` (chunk.go) computes
`overhead := len(open) + 2 + len(closeLine)` and clamps `budget` to 1 when the
overhead alone exceeds the limit, so every emitted piece is
`overhead + 1` bytes - larger than `reduced`, and for a one-character inner body
*identical to its own input*. `escapeChunk` then recurses on a value equal to its
argument, forever.

Reproduced (throwaway copy, not committed):

```go
lang := strings.Repeat("A", 5000)          // "```AAAA...\nxy\n```"
Split(text, DefaultChunkLimit)             // -> 2 chunks, len 5009 each
escapeChunk(chunk, DefaultChunkLimit)      // never returns; killed at 120s
```

`fenceOpenLang` accepts any space-free info string, so "a model emitted
` ``` ` immediately followed by a >4KB unbroken token (base64 blob, URL, minified
JSON)" is enough. Failure mode is not a dropped message: `sendText` is called from
`dispatcher.reply` on the worker goroutine *while holding the global concurrency
slot*, so one such reply permanently burns a slot and a session worker, makes the
shutdown drain breach, and grows memory until the process is OOM-killed.

Fix: make the recursion provably shrinking. Cheapest correct form:

```go
pieces := Split(chunk, reduced)
if len(pieces) == 1 && len(pieces[0]) >= len(chunk) {
	// Cannot shrink further (e.g. a fence whose info line alone exceeds
	// the limit): send it as-is and let sendOne's "too long" fallback deal
	// with it rather than recursing forever.
	return []escapedPart{{escaped: escaped, plain: chunk}}
}
```

A recursion-depth cap would also work, but the guard above is the one that
matches the actual invariant being relied on.

---

## High

### H1. The approval timeout/callback race is narrowed, not closed - DB says "approved", `Ask` returns false

`internal/channel/telegram/approver.go:173-186` and `:204-213`

The new non-blocking drain only covers the case where the callback has *already
pushed* to `wait`. The callback commits the verdict in `Decide` (line 273) and
pushes to `wait` ~20 lines later (line 299). A timer firing inside that gap:

1. `awaitDecision` drains `wait` - empty;
2. `finishExpired` -> `Decide(ctx, id, "expired", "")` -> `ErrAlreadyDecided`;
3. that branch now `return`s early (correctly skipping the contradictory edit);
4. `awaitDecision` still returns `false, context.DeadlineExceeded`.

The user sees "✅ approved by user N" (the callback's own edit lands), the
`approvals` row says `approved`, and the gated command never runs. The same hole
exists verbatim on the `ctx.Done()` arm (line 188-193), which has no drain at all.

`finishExpired` already *knows* it lost the race - it has `ErrAlreadyDecided` in
hand. Make it say so and consult the authoritative source:

```go
func (a *Approver) finishExpired(...) (alreadyDecided bool) { ... }

case <-timer.C:
	select { case v := <-wait: return v, nil; default: }
	if a.finishExpired(...) {
		if ap, err := a.approvals.Get(...); err == nil && ap.State == "approved" {
			return true, nil
		}
		return false, nil   // decided as denied: not a timeout either
	}
	return false, context.DeadlineExceeded
```

(Verified non-issues in the same area: `wait` is buffered-1 and every push is
`select/default`, so concurrent callbacks can never block on it; a second
callback for a decided id returns at the `ErrAlreadyDecided` toast before it can
overwrite the first edit; the `!ok` "no longer waiting" edit is only reachable
when `Decide` genuinely succeeded with no local waiter.)

### H2. New transient retry double-sends on an ambiguous network error

`internal/channel/telegram/send.go:154-162`

```go
} else if !transientRetried && ctx.Err() == nil {
	transientRetried = true
	...
	continue
```

A non-API error is retried blind. A response-read timeout, a connection reset
after the request was accepted, or an h2 GOAWAY mid-response all produce a
non-nil error for a message Telegram *did* deliver; the retry then posts it
again. Telegram has no idempotency key, so this is a user-visible duplicate for
every chunk of a long reply, and a duplicate *approval prompt* (`Ask` uses
`sendOne`), i.e. a second set of live buttons whose message is never edited with
the outcome.

The 5xx branch above it is safe (Telegram did not accept the update) - the
network branch is not. Either drop the network-error retry, or restrict it to
errors that provably happened before the request was written
(`errors.Is(err, syscall.ECONNREFUSED)`, DNS errors, `net.Error` with
`!wroteRequest`). At minimum document the duplicate risk where the retry is
chosen.

---

## Medium

### M1. `dispatch`'s new early return does not actually close the `wg.Add`/`wg.Wait` race the comment claims

`internal/gateway/dispatch.go:185-192` and `:284-294`

The runWorker comment asserts "a still-zero `d.wg` after every worker has taken
this path never races `spawnWorkerLocked`'s `wg.Add` against drain's `wg.Wait`".
That is not established: `dispatch` reads `d.rootCtx.Err()` *outside* `d.mu`, and
`pump` can be inside `dispatch` when the signal lands. Interleaving:

- `dispatch` sees `rootCtx.Err() == nil`;
- shutdown cancels; every existing worker exits; `wg` hits 0;
- `waitForShutdown`'s `wg.Wait()` returns;
- `dispatch` takes `d.mu` and calls `spawnWorkerLocked` -> `d.wg.Add(1)`.

`sync.WaitGroup` explicitly forbids a positive `Add` concurrent with a `Wait`
that is already unblocking ("sync: WaitGroup misuse" / silent under-wait). Fix:
gate spawning on a mutex-protected dispatcher flag rather than on `rootCtx.Err()`
- set `d.closed = true` under `d.mu` when rootCtx ends, and check it inside the
same critical section that spawns. Otherwise soften the comment; as written it
claims a guarantee the code does not provide.

### M2. Callback goroutines are unbounded and outlive the shutdown sequence

`internal/channel/telegram/poll.go:25`

```go
go c.approver.HandleCallback(ctx, update.CallbackQuery)
```

Nothing tracks these goroutines. `Gateway.Run` does `pumps.Wait()` (which only
waits for `pumpUpdates` to return) and then closes the store in its `defer`. A
callback goroutine spawned on the last polled batch can call
`approvals.Get`/`Decide` after `store.Close()` - `database/sql` answers with
"sql: database is closed", so the verdict is silently lost and logged as an
internal error, and the user's tap does nothing. Secondary: one goroutine per
callback with no cap, each doing a DB read plus up to two Telegram calls.

Fix: give `Channel` a `sync.WaitGroup` (or a small worker pool, e.g. 4 buffered
slots) that `pumpUpdates` waits on before returning, so the callbacks finish
inside the drain window that `pumps.Wait()` already provides.

### M3. Cron catch-up replays the *oldest* minutes of a long gap, not the recent ones

`internal/cron/scheduler.go:154-165`

```go
minute := lastTick.Truncate(time.Minute).Add(time.Minute)
for i := 0; minute.Before(now) && i < maxCatchUpMinutes; i++ {
```

After a 2-hour suspend the loop evaluates `lastTick+1m … lastTick+10m` - ten
minutes that are now two hours stale - and never reaches the minutes just before
`now`. An hourly job then fires with a two-hour-old intent while a job due three
minutes ago is skipped, which is the opposite of the stated goal ("does not
silently swallow a due job's only firing window"). For a `* * * * *` job the
overlap guard turns the burst into one fire plus up to nine `skipped` rows.

Fix: walk backwards from `now` - `start := max(lastTick+1m, now-maxCatchUpMinutes)`
- so the bounded window is the most recent skipped minutes.

Verified non-issues here: no double fire at the boundary (the last catch-up
minute and `now` share a wall-clock key, and `tryFire`'s `lastFired` guard
suppresses the second), and a ≤10-minute window cannot span a DST fall-back
repeat.

### M4. `telego` Errorf now emits ERROR for every API failure the send path recovers from

`internal/channel/telegram/channel.go:65` + `telego@v1.11.1/bot.go:170`

`performRequest` calls `log.Errorf("Execution error %s: %s", ...)` for *every*
failed call, not just polling. So the intended-and-handled cases -
the MarkdownV2 400 that triggers the plain-text fallback, a 429 that is retried,
`editMessageText`'s "message is not modified" - now each produce an
`ERROR telegram: Execution error sendMessage: ...` line for a request that
ultimately succeeded. Any alerting on ERROR will fire on normal operation.

Fix: route `Errorf` to `Warn` (the gateway's own code already logs the failures
it considers real at Error), or keep Error only for messages matching
`Getting updates`/`Retrying getting updates`.

The redaction itself checks out: the token-bearing URL is only logged through
`Debugf` (`bot.go:245`), which is a no-op here, and `Errorf`'s `ReplaceAll`
covers the exact-substring form telego would produce. No per-update allocation
is added by the adapter (telego evaluates `response.String()` for `Debugf`
regardless of the logger, unchanged from `WithDiscardLogger`).

### M5. `'interrupted'` is a new `cron_runs.status` value with no schema/doc anchor

`internal/store/sqlite/cron_runs.go:55`, `migrations/001_init.sql:58`

The column comment still reads `-- ok|error|skipped`. `mtclaw cron runs` prints
the raw status, so users will see a value documented nowhere. Add a line to the
new migration (or `docs/`) defining `interrupted`, rather than leaving the
schema comment lying.

---

## Low

- **L1 `relayInbound` drops without `OnDone`** (`internal/gateway/queue.go:55-59`).
  The drop path logs and notifies but never invokes `msg.OnDone`. Harmless today
  *only* because the one producer that sets `OnDone` (cron) calls
  `disp.dispatch` directly (`gateway.go:133`) and never traverses
  `raw -> relayInbound -> global`. That also makes the `msg.Channel == "cron"`
  guard in `notifyGlobalQueueFull` - and
  `TestRelayInbound_DropsCronInbound_NoReplyAttempted` - cover a path production
  cannot take. Either call `OnDone` on drop (two lines, removes the latent trap)
  or drop the unreachable cron special-case and its test.
- **L2 unbounded notify goroutines** (`queue.go:71-83`). One goroutine + one
  Telegram send per dropped message, with no per-chat dedup, spawned precisely
  when the process is already overloaded. Bound it (a single shared notifier, or
  a per-chat "already told them" set).
- **L3 `release()` on the drain-breach path** (`gateway.go:166`). With flock the
  lock is released by process exit anyway; explicitly releasing it while workers
  are still running (the definition of a breach) briefly re-opens the
  single-instance guarantee for a supervisor that restarts immediately. Skipping
  `release()` when `!drained` is strictly safer and matches the reasoning already
  applied to `store.Close()`.
- **L4 `ExpireStarted` uses `context.Background()`** (`gateway.go:80`) in the same
  pass that called out an unbounded `Ensure` as a defect (`dispatch.go:363`). A
  held write lock hangs startup with no timeout. (The sweep itself is safe: it
  runs after `Acquire`, and `mtclaw cron run` only ever writes an
  already-finished row - `cli/cron_cmd.go:210` - so it cannot clobber a
  legitimately running manual fire.)
- **L5 phantom test** (`internal/gateway/progress_test.go:47-69`).
  `TestProgressReporter_TimerFiresAfterCtxDone_SendsNothing` builds its own copy
  of the guarded closure instead of calling `onEvent`; it passes unchanged with
  the fix reverted. Make `slowToolNotice` a package `var` (or a reporter field)
  so the real closure can be exercised, or delete the test - the comment is
  honest but the test still proves nothing about `progress.go`.
- **L6 `replyTimeout` ignores escape inflation** (`dispatch.go:480`).
  `len(text)/4096+1` underestimates the part count whenever `escapeChunk`
  re-splits (escaping can nearly double a chunk), so a dense-in-specials reply can
  still be truncated by the budget it was added to protect. Using
  `len(text)/(replyChunkLimit/2)+1` costs nothing and covers the worst case.
- **L7 `Held` opens `O_RDWR`** (`lock_unix.go:52`). `mtclaw doctor` run by a user
  who cannot write the lock file reports FAIL ("cannot read instance lock")
  instead of "held". `O_RDONLY` is sufficient for `flock`.
- **L8 stale-PID content survives release** (by design, documented). `Held`
  returns the stale pid alongside `held=false`; both current callers
  (`doctor_checks.go:145`, `cron_cmd.go:194`) only print it when `held` is true,
  so this is fine today - worth a one-line note on `Held` so a future caller does
  not print an unheld pid.
- **L9 `escapeCodeContent` and newline-less fences** (`format.go:78-83`). For
  ` ```x``` ` (no newline anywhere) the whole body is escaped, including the part
  Telegram treats as the language token. Unreachable from `chunk.go` (which always
  emits `open + "\n"`), reachable from raw model output. Cosmetic.

---

## Prior fixes: verified / not verified

| ID | Claim | Verdict |
|----|-------|---------|
| H1 | escaped chunk never exceeds 4096 | **Partly.** Invariant holds (300-iteration fuzz, 9KB specials-dense inputs: no part >4096, none empty). But the re-split recurses forever on a long fence info string - **C1**. |
| H1b | 400 "too long" falls back to plain | Verified (`send.go:136-137`); `plain` is always ≤ escaped, so the fallback cannot itself be too long. |
| H2 | poll failures visible, token never logged | Verified. `long_polling.go:127,132` route through `Errorf`; the token-bearing URL is `Debugf`-only (`bot.go:245`) and `Debugf` is a no-op. Side effect: **M4**. |
| H3 | no worker leak / no enqueue into a dead queue | Verified for the queue path (`drainQueueOnDone` fires `OnDone` for everything left). The `wg.Add`/`wg.Wait` claim in the comment is **not** established - **M1**. |
| M1 | drain result propagated, store not closed on breach, non-nil `Run` error | Verified (`gateway.go:157-168,218-227`); no early return can leave `drained` false spuriously. See **L3**. |
| M2 | caption + caption entities used | Verified. `messageText` swaps both text and entities, and `utf16Slice` offsets are caption-relative, so mention detection in groups is correct (`gating.go:72-77,115-137`). |
| M3 | timer/callback race fixed | **Partly.** The drain closes the "already pushed" case (500-iteration test is a real regression test); the "Decide committed, push not yet reached" and `ctx.Done()` windows remain - **H1**. |
| M4 | bounded `Ensure` | Verified (`dispatch.go:363-365`), `ensureCancel()` called before use of `sess`. |
| M5 | callback no longer blocks the pump | Verified, and the test fails if reverted. New lifecycle problem - **M2**. |
| M6 | `ExpireStarted` sweep is safe | Verified. Runs after `Acquire`; only the in-gateway scheduler writes `status='started'`; `cron run` writes a single finished row and is refused outright for persistent jobs while the lock is held. Cannot mark a live manual run. |
| M7 | missed-tick catch-up | Implemented; no double fire (minute guard confirmed), but the replayed window is the wrong end of the gap - **M3**. |
| M8 | cron timeout notice, silent on shutdown/stop | Verified end to end: `provider.Error.Unwrap` (`provider/errors.go:75`) + `Classify(cause)` (`agent/loop.go:350`) make `errors.Is(result.Err, context.DeadlineExceeded)` true for a job timeout and false for a shutdown cancel; `deliver_to` is mandatory and validated, so the notice never targets the synthetic `job:<name>` chat. |
| L1 | flock rewrite | Verified empirically: while `Acquire` holds the lock, `Held`'s own open+trylock+`LOCK_UN` does **not** release it (independent open file descriptions), an external `flock -n` still fails, release-then-reacquire works, and nothing unlinks the file - so the unlink-after-flock "two gateways" race is genuinely gone. |
| L1w | Windows keeps the PID heuristic | Correct as written (same code as before, `writeLockPID` is safe on the freshly created `O_EXCL|O_WRONLY` handle); the scope note is honest. The TOCTOU and PID-reuse caveats it documents are real and remain. |
| L2 | one bounded transient retry | Implemented; introduces duplicate-send risk - **H2**. |
| L3 | scaled reply timeout | Verified; underestimates under escape inflation - **L6**. |
| L4 | `\` and `` ` `` escaped inside code | Verified for fenced and inline spans, language line preserved. Unbalanced-fence branch still copies verbatim (pre-existing; `chunk.go` guarantees balance upstream). |
| L5 | reused tool-call id / post-cancel notice | First half verified with a real test; second half's test is a phantom - **L5**. |
| L6 | global-queue-drop reply | Verified non-blocking (spawned goroutine, relay loop never stalls) - see **L1/L2** for what it misses. |

---

## Unresolved questions

1. C1: is a >4KB space-free fence info string worth handling with the shrink
   guard above, or should `escapeChunk` simply bail after N levels? The guard is
   3 lines and provable; the depth cap is vaguer but catches anything else
   `Split` might one day fail to shrink.
2. H1: when a verdict was committed just as the timer fired, should `Ask` honor
   it (run the command) or report a timeout? Honoring it matches the DB and the
   message the user already sees; reporting a timeout is safer if the command is
   destructive. This is a product decision, not a code one - the current code
   picks "report a timeout" silently while telling the user the opposite.
3. M2: is a `WaitGroup` over callback goroutines enough, or should callbacks be
   capped (a small pool) to bound the flood case as well?
4. H2's still-open consecutive-getUpdates-failure counter (the implementer's own
   note) remains a real gap: a revoked token now logs forever at ERROR instead of
   exiting non-zero. Worth its own decision rather than staying in a report.
