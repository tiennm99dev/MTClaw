# Telegram/gateway/cron fixes: third-round review

Date: 2026-09-28. Branch `refactor/260928-full-review`.

Source: `plans/reports/code-reviewer-260928-0953-telegram-gateway-cron-review.md`,
binding decisions in `plans/260928-1041-third-round-review-fixes/plan.md`.

## Headline decision: Telegram output switched to HTML

Replaced `EscapeMarkdownV2`/`escapeChunk`/`hardCutParts` (deleted
`format.go`/`format_test.go`) with a hand-written Markdown-to-Telegram-HTML
converter (`internal/channel/telegram/render.go`): bold, italic, inline
code, fenced code (`<pre><code class="language-x">`), links, and headings
folded to bold. No new dependency.

- Chunking splits the source Markdown at safe boundaries first
  (`Split`, unchanged in spirit, still in `chunk.go`), then renders each
  chunk to HTML and verifies the *rendered* result still fits 4096 bytes,
  re-splitting (and, as a last resort, hard-cutting with a verify-and-shrink
  loop) when tag/entity overhead or escape inflation pushes it over -
  `send.go`'s `renderChunks`/`fitHTML`/`hardCutHTMLParts`. Every chunk is
  parsed independently, so an inline marker split across a chunk boundary
  falls back to literal text instead of emitting invalid HTML.
- `sendOne` falls back to plain text (no parse_mode) on an HTTP 400 naming
  parsing or length as the problem - unchanged mechanism, now applied to
  HTML.
- Approval prompts (`formatApprovalPrompt`) are built as HTML with the
  command in an html-escaped `<pre><code>` span - fixes B1 by construction
  (escaping guarantees the command can never close the tag or forge a
  "reason:" line).
- Table tests in `render_test.go` and `send_test.go`: nested/unbalanced
  markdown, fences with long info strings, backticks inside code (including
  the different-length-delimiter case), HTML-looking text, 4096-boundary
  cases, multibyte runes (CJK + emoji), escape-inflation edge cases.

## Findings

Legend: fixed (test), skipped (reason).

- **B1** (approval prompt fence-breaking) - fixed by construction (HTML
  escaping). `TestFormatApprovalPrompt_CommandCannotBreakOutOfCodeSpan`.
- **B2** (command for another bot) - fixed: `parseCommand` now returns the
  `@target` suffix; `handleMessage` drops the command silently unless the
  target matches `c.username`. `TestHandleMessage_CommandAddressedToAnotherBot_DroppedSilently`,
  `TestHandleMessage_CommandAddressedToThisBot_StillRuns`.
- **B3** (`AllowSendingWithoutReply`) - fixed in both `sendText`'s reply
  params and the approver's prompt. `TestSendText_ReplyAllowsSendingWithoutReply`,
  `TestApprover_MessageIDQuoteAllowsSendingWithoutReply`.
- **B4** (cron wall-clock vs monotonic) - fixed:
  `tickWithCatchUp` now compares `now.Round(0).Sub(lastTick.Round(0))`.
  `TestScheduler_TickWithCatchUp_MonotonicReadingsStillDetectTheGap` - see
  that test's own comment for why a hermetic unit test cannot reproduce an
  actual host-suspend monotonic freeze (Go's time API exposes no public way
  to construct a Time with mismatched wall/monotonic deltas); this is a
  coverage guard for the fix's ordinary-input behavior, relying on
  `Time.Sub`/`Round(0)`'s documented semantics for the suspend case itself.
- **B5** (synchronous, per-message busy reply) - fixed: `worker.busyNotified`
  (set/cleared under `d.mu`) limits the notice to once per overflow burst;
  `replySessionBusyAsync` sends on its own goroutine, off the pump.
  `TestDispatch_SessionQueueFull_RepeatedOverflow_OneReplyPerBurst`.
- **B6** (long fence info string -> UTF-8-invalid message storm) - fixed:
  `fenceOpenLang` caps the info string at `maxFenceLang` (64 bytes; a longer
  line is not treated as a fence at all), and `safeRuneCut` now scans
  forward from the start of the string instead of backward from `limit`
  (the old backward scan could fall back to a bare `limit=1` cut that still
  landed mid-rune). `TestFenceOpenLang_CapsInfoStringLength`,
  `TestRenderChunks_LongFenceInfoString_TerminatesQuickly`,
  `TestSafeRuneCut_NeverSplitsMultibyteRune`,
  `TestSplit_HardCutWithSpacesReassemblesLosslessly` (this last one also
  caught and fixed a real, separate bug: `splitPlain` was unconditionally
  trimming leading whitespace after *every* cut, including a hard cut that
  consumed no separator - silently dropping a content space at chunk
  boundaries. `bestPlainCut` now reports whether the cut was a hard cut and
  `splitPlain` only trims for the three designed-separator cases).
- **B7** (`/new` during a running turn) - fixed per the binding decision
  (cancel, then reset): `dispatcher.runOnWorker` cancels the session's
  in-flight turn and runs a func on that worker's own goroutine, guaranteed
  to run only after the canceled turn's own end-of-turn work has returned;
  `telegramDeps.Reset` uses it.
  `TestTelegramDeps_Reset_CancelsRunningTurnThenResets`,
  `TestTelegramDeps_Reset_NoRunningTurn_StillResets`.
- **B8** (`/stop` while queued behind the semaphore) - fixed: `runTurn` now
  creates the turn context and registers `w.cancel` *before* waiting on the
  global semaphore, and the semaphore wait itself selects on
  `turnCtx.Done()`. `TestDispatch_CancelSession_WhileQueuedBehindGlobalSemaphore`.
- **B9** (lock error tells the operator to delete a live flock file) - fixed
  text only, in `lock_unix.go` (the file itself, plus its relocation, is a
  later phase's ownership): now says "stop pid N first"; the no-pid
  fallback says "find and stop it first" instead of naming `remove`.
- **B10** (reply budget vs drain deadline) - fixed: `runTurn` now spawns
  `d.reply(...)` on its own goroutine, detached from `d.wg` (which drain
  waits on) - the send touches no store, so a slow multi-chunk delivery no
  longer makes a clean shutdown look like a breach.
  `TestDispatch_ReplySendRunsDetachedFromWorkerGroup` (verified to fail
  without the fix, by temporarily reverting the `go` in front of the call).
- **B11** (startup sweep misses not-yet-expired rows) - partially fixed
  within file ownership: `Approver.ExpirePending` now passes a
  far-future horizon instead of `time.Now()`, so every pending row is swept
  regardless of remaining time-to-expiry. The underlying
  `sqlite/approvals.go` query has no channel filter at all (I do not own
  that file); today this is safe because the Telegram channel is the only
  writer of `approvals` rows, but if a future caller writes a
  differently-scoped pending row, the store-owning agent should add a
  channel filter to `ExpirePending`'s query - flagged, not fixed, since it
  is out of my file ownership.
- **B12** (unbounded DB calls off the turn path) - fixed: `commands.go`'s
  `handleCommand` wraps `/new` and `/status` in a 10s `commandDBTimeout`;
  cron's `fire`/`onDone`/`recordRun` now use a bounded `cronStoreTimeout`
  instead of `context.Background()`.
- **B13** (`CaptureSenders` counts pre-window updates) - fixed: a
  `windowStart` timestamp is captured before polling, and any update whose
  `Date` predates it is skipped.
- **B14** (escape inflation) - resolved by construction: the HTML rewrite's
  render-then-verify-fit approach (`fitHTML`) checks actual rendered size
  before sending, so there is no separate "escape inflation" class of bug
  left for `replyTimeout` to ignore.
- **B15** (shutdown logs ERROR for normal drain-time drops) - fixed:
  `runWorker`'s message-dequeue branch checks `d.rootCtx.Err()` before
  calling `runTurn`, and if already canceled, calls `OnDone` with that
  error directly (no `Ensure` attempt, no log line) and keeps draining.
  `TestDispatch_ShutdownRace_QueuedMessageGetsNoErrorLogNoise`.
- **B16** (queued messages lost on restart) - documented per the binding
  decision (no persistence): new section in `docs/telegram-setup.md`.

## Refactors

- **R1** - superseded by the headline HTML decision (see above); not a
  smaller "entities only" version.
- **R2** (remove the global queue/`relayInbound`) - done: `Gateway.Run` now
  wires `Channel.Start` directly into the one channel `dispatcher.pump`
  reads. `queue.go` keeps only `trySend`/`sessionQueueSize`/`globalQueueSize`;
  `relayInbound`/`notifyGlobalQueueFull`/`errGlobalQueueFull` and their
  tests are removed.
- **R3** (build the Telegram channel before the registry) - done:
  `gateway.New` now builds `telegram.New` first and passes
  `newApproverMux(tgChannel.Approver())` (an immutable field, no mutex, no
  `setTelegram`) into `tools.New`.
- **R4** (one dispatcher lifecycle entry point) - done, with one deliberate
  deviation: added `dispatcher.start(ctx)` (uses `context.AfterFunc`) and
  `Gateway.Run` calls it instead of hand-writing the shutdown goroutine.
  I kept the `d.closed || d.rootCtx.Err() != nil` check inside `dispatch`'s
  locked section (not a separate *unsynchronized* pre-lock check, which is
  what the review objected to) rather than dropping it to `d.closed` alone:
  `context.AfterFunc`'s callback runs in its own goroutine, so there is a
  real (if narrow) window where `d.closed` can still read `false`
  immediately after `rootCtx` ends; `TestDispatch_AfterRootCtxCanceled_OnDoneFiresImmediately`
  depends on this and would flake without it.
- **R5** (split `approver_test.go`) - done: send/escaping tests moved into
  `send_test.go` (`sendOne`, `sendText`, chunk-fitting tests); approver
  tests stay in `approver_test.go`.
- **R6** (small DRY/dead-code items) - done: `commands.go`'s
  `commandNames` map + two arrays replaced with one ordered `commands`
  slice; `SendOnce`/`GetMe`/`CaptureSenders` share `oneShotBot` (which also
  gives `CaptureSenders` the empty-token check it was missing);
  `handleCommand`'s nil-`Deps` branches removed (only production caller
  always passes deps; `NewApprover`/`Split`/`Decide` unexported
  (`EscapeMarkdownV2` no longer exists); `recordRun` takes one `at
  time.Time` instead of `started, finished` always passed the same value;
  cron's `running map[string]*atomic.Bool` replaced with `map[string]bool`
  under the existing `s.mu`; `waitForShutdown` removed, `Gateway.Run` calls
  `drain(..., drainDeadline)` directly.
- **R7** (remove plan/phase/finding-code references) - done across all
  files I own: replaced with plain descriptions of the invariant. Verified
  with a repo-wide grep restricted to my packages (excluding
  `lock*.go`/`lock*_test.go`, which I don't own) - clean.

## Tests requested explicitly

- **Gateway.Run shutdown order**: new `gateway_test.go` builds a `Gateway`
  directly (bypassing `New`'s OpenAI/tools/lock wiring) with a
  `fakeGatewayChannel` (implements the new `gatewayChannel` interface -
  `Gateway.channel`'s field type changed from concrete `*telegram.Channel`
  to this interface specifically to make this testable) and a real temp
  store: `TestGatewayRun_CleanShutdown_ClosesStoreAndReleasesLock`,
  `TestGatewayRun_ChannelFailure_ReturnsNonNilAndStillShutsDown`,
  `TestGatewayRun_InboundMessageReachesDispatcher`. The drain-deadline-breach
  path is not re-tested at the `Run` level (it would require waiting out
  the real 30s `drainDeadline`); it is already covered at the `drain`
  level by the pre-existing `TestDrain_ReturnsAtDeadlineWhenWorkersOutlastIt`.
- **Repeated overflow**: `TestDispatch_SessionQueueFull_RepeatedOverflow_OneReplyPerBurst`
  (see B5). Debugging note: an early version of this test was itself flaky
  for reasons unrelated to the fix under test - `Sessions().Ensure` against
  the real temp store (plus semaphore acquisition) takes real, variable
  time between a message leaving the queue and the runner actually being
  called, so "queue is empty" is not proof the occupying turn has reached
  its blocking point. Fixed by having the test's fake runner explicitly
  signal when each occupier call reaches its blocking point, and having the
  test wait on that signal instead.
- **Other-bot command**: `TestHandleMessage_CommandAddressedToAnotherBot_DroppedSilently`
  (see B2).
- **Deleted reply target**: covered by the `AllowSendingWithoutReply`
  tests (see B3) - Telegram's failure mode for a deleted reply target *is*
  the 400 that flag suppresses.

## Also added

- `telegramDeps` (Status/Reset/Cancel) had no tests at all; added
  `TestTelegramDeps_Status_ReportsSessionFields`,
  `TestTelegramDeps_Cancel_DelegatesToDispatcher`, plus the B7 tests above.

## Mechanical caller updates outside owned packages

None needed - `internal/cli` and `main.go` call `gateway.New`/`gateway.Run`
by their existing signatures, which are unchanged.

## Files modified

`internal/channel/channel.go`, `internal/channel/telegram/{approver,capture,
channel,chunk,commands,gating,poll,send}.go` (+new `render.go`),
`internal/channel/telegram/*_test.go` (+new `render_test.go`,
`send_test.go`), `internal/cron/{job,scheduler}.go` +
`scheduler_test.go`, `internal/gateway/{approver,dispatch,gateway,queue,
shutdown}.go` (+new `gateway_test.go`) + their `*_test.go`,
`internal/gateway/lock_unix.go` (message text only, per instructions),
`docs/architecture.md`, `docs/telegram-setup.md`.

Not touched (other agents' concurrent work, observed via `git status` only):
`docs/configuration.md`, `docs/security.md`, `internal/cli/prompt_cmd_test.go`,
`internal/tools/**`, `internal/store/sqlite/approvals.go`.

## Validation

- `gofmt -l .`: clean.
- `go vet ./...`: clean.
- `go test -race ./internal/channel/... ./internal/gateway/... ./internal/cron/...`:
  pass (also re-ran with `-count=2` to `-count=40` on the previously-flaky
  test while chasing it down).
- `go test -race ./...` (whole repo, including other agents' packages at
  time of this run): pass.
- `CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ./...`: clean.
- `CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build ./...`: clean.
- `go mod tidy -diff`: clean (no new dependency - the HTML converter is
  hand-written against the standard library only).

## Unresolved questions

1. B11: the store-owning agent should consider whether `ExpirePending`'s
   query needs a `channel = 'telegram'` filter now or as future-proofing;
   I mitigated at the call site (a far-future horizon) since I don't own
   `internal/store/sqlite/approvals.go`.
2. B4's regression test is a coverage guard, not a bug reproduction - see
   its own doc comment and the write-up above. If a more faithful
   reproduction is wanted, it would need OS-level suspend or unsafe
   manipulation of `time.Time` internals, which I judged out of proportion
   for this fix.
