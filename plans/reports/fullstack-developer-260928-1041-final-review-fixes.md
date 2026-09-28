# Final review fixes: runtime and CLI/docs reports

Date: 2026-09-28. Branch `refactor/260928-full-review`, uncommitted tree.
Binding decisions: `plans/260928-1041-third-round-review-fixes/plan.md` plus the
controller's per-finding decisions for this pass. Every fix below has a
regression test that was verified to fail with the fix reverted (mutation
check), except the two items called out explicitly as accepted gaps.

## Runtime report (`code-reviewer-260928-1041-final-runtime-review.md`)

### High

- **H1 (Windows post-`Run` `taskkill` on a reused PID) - fixed.** Windows now
  assigns the child process to a Job Object
  (`JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`) right after `cmd.Start`, and kills by
  closing the job's handle - never by PID after the process handle has
  already been released. `golang.org/x/sys` moved from an indirect to a
  direct dependency (it was already resolved in go.sum); `go mod tidy -diff`
  is clean. unix behavior is unchanged (same pgid + SIGKILL). A small
  `trackedProcessTree` wrapper (mutex + `sync.Once`) makes the Go-level
  handoff between `cmd.Cancel`'s ctx-watcher goroutine and the main
  goroutine's post-Start assignment race-free under `-race`, and closes the
  narrow window where a cancellation lands in that gap by re-checking
  `runCtx.Err()` once the tree is assigned.
  Files: `internal/tools/exec.go`, `internal/tools/exec_unix.go`,
  `internal/tools/exec_windows.go`, `go.mod`, `go.sum`.
  Verified: `GOOS=windows go vet ./...` and a `GOOS=windows CGO_ENABLED=0 go
  build .` both succeed with the new import; existing exec tests (including
  the background-child and timeout-kills-whole-tree tests) still pass on
  linux with `-race`.

### Medium

- **M1 (`/new` racing a queued message) - fixed with a deterministic test.**
  `runWorker`'s loop now checks `w.control` first, non-blocking, before its
  main `select`, so a pending `/new` always wins over a message already
  sitting in the queue instead of Go picking between two simultaneously-ready
  cases at random. `runOnWorker` additionally drops (via
  `drainQueueOnDone(w, context.Canceled)`) anything left in the queue before
  running its own `fn`, closing the residual case where the queued message
  had already been dequeued microseconds earlier.
  Files: `internal/gateway/dispatch.go`.
  Test: `TestTelegramDeps_Reset_QueuedMessageDuringTurn_NeverRunsAgainstDeletedHistory`
  (`internal/gateway/gateway_test.go`) - fails 100% of the time with both
  parts of the fix reverted (verified).
- **M2 (unredacted/unescaped/unbounded classifier `Reason`) - fixed.** Added
  `sanitizeReason` (redact -> escape control/bidi -> cap at 300 bytes) in
  `internal/tools/approver.go`, called once in `execTool.ask` - the single
  place every `VerdictAsk` path's `Reason` turns into `Request.Reason` -
  rather than scattering the fix across each of policy.go's three call
  sites.
  Test: `TestExec_AutoModeApprovalReasonIsSanitizedBeforeReachingApprover`
  (`internal/tools/exec_test.go`) - verified to fail when reverted.
- **M3 (detached replies dropped at shutdown) - fixed with a bounded wait.**
  Added `dispatcher.replyWG` (separate from `wg`, which `drain` still uses)
  and `drainReplies` (its own `replyDrainDeadline`, never counted as a drain
  breach) in `internal/gateway/shutdown.go`, called from `Gateway.Run` right
  after `drain` returns.
  Test: `TestGatewayRun_WaitsForDetachedReplyBeforeReturning`
  (`internal/gateway/gateway_test.go`), plus two smaller
  `TestDrainReplies_*` unit tests in `shutdown_test.go` - all verified to
  fail when reverted.
- **M4 (`cron run` missing `DisableEnvironRead`) - fixed by moving the call
  into `state.newLoop`.** `newLoop` (root.go) is the one function both
  `prompt` and `cron run` go through; it now calls
  `tools.DisableEnvironRead` once, and the separate call in `prompt_cmd.go`
  was removed (unused `log/slog` import dropped with it). `gateway_cmd.go`
  keeps its own explicit call since the gateway does not go through
  `newLoop` - no double call. `docs/security.md`'s sentence naming only
  "gateway and prompt" is corrected to name every exec-capable command and
  explain how `cron run` reaches the same call.
  Test (Linux-only, real prctl check): `TestCronRunCmd_DisablesEnvironRead`
  (`internal/cli/cron_cmd_linux_test.go`) - reads `PR_GET_DUMPABLE` directly
  after running `cron run` end to end; verified to fail when reverted, and
  resets `PR_SET_DUMPABLE` in cleanup so it cannot leak into later tests in
  the same binary.
- **M5 (intraword `_`/`*` misread as emphasis) - fixed with CommonMark's
  actual left/right-flanking rules (rules 2/4/6/8), applied to `_`/`__` only;
  `*`/`**` are intentionally untouched, matching real CommonMark (which does
  not restrict intraword asterisk emphasis).** Added
  `leftFlanking`/`rightFlanking`/`underscoreCanOpen`/`underscoreCanClose` in
  `internal/channel/telegram/render.go`, wired into `renderEmphasis` (for
  `__`) and `renderSingleDelimEmphasis` (for `_`) via a new
  `underscoreFlanking bool` parameter.
  Tests added to `TestRenderHTML_Table`: `MAX_READ_BYTES and
  file_name_here`, `snake_case_name`, and `2*3*4` (still emphasis, per
  CommonMark) all verified to fail with the flanking checks reverted.
  **Deviation, verified against the CommonMark spec, not a bug:**
  `__init__.py` still renders "init" bold
  (`<b>init</b>.py`). I re-derived CommonMark emphasis rules 6/8 by hand for
  this exact string: the opening `__` is at the very start of the string
  (which the spec treats the same as being preceded by whitespace, so it is
  not right-flanking and can open), and the closing `__` is followed by `.`
  (punctuation), which makes the closing run not left-flanking - and rule 8
  permits closing whenever a run is right-flanking and *not* left-flanking,
  regardless of what follows it. This is a well-documented real-world
  CommonMark/GFM quirk (the same thing GitHub's own Markdown does with
  `__init__.py` in a comment or README), not something this fix should
  suppress with an ad-hoc rule not grounded in the spec. The test asserts the
  verified-correct output and documents this reasoning inline.

### Low

- **L1 (latent infinite recursion / stack overflow at small chunk limits) -
  fixed, two parts.** `splitByLineThenHardCut` no longer appends a spurious
  trailing `""` piece when a cut consumes every remaining byte (`chunk.go`).
  `fitHTML`'s progress guard was generalized from "single piece, not
  shorter" to "any piece not strictly shorter than chunk" via a new
  `allPiecesShrink` helper (`send.go`), so a multi-piece non-shrinking case
  also falls back to `hardCutHTMLParts` instead of recursing forever.
  Tests: `TestSplitByLineThenHardCut_NeverAppendsATrailingEmptyPiece`,
  `TestAllPiecesShrink_RejectsAPieceNotStrictlyShorterThanChunk`,
  `TestFitHTML_TinyFenceBudgetTerminatesInsteadOfRecursingForever` (the
  report's exact repro strings at limits 40 and 100). I additionally
  reverted both halves of the fix together in a throwaway local run (not
  committed) and confirmed the process really does crash with `fatal error:
  stack overflow` after ~7s, then restored the fix - the two unit-level
  tests above are the permanent, safe-to-run regression coverage; the
  integration test at the reported limits also passes.
- **L2 (empty chunks sent; narrow 400 fallback) - fixed, two parts.**
  `renderChunks` now drops any part with no visible content once its own
  tags are stripped (`isVisiblyEmpty`/`stripHTMLTags` in `send.go`) -
  covers an empty fence and a whitespace-only hard-cut piece alike.
  `sendOne`'s plain-text fallback no longer requires the 400's description
  to mention "parse" or "too long"; it fires on any 400 while
  `ParseMode != ""` (safe: the retried attempt clears `ParseMode`, so the
  same branch cannot loop).
  Tests: `TestRenderChunks_DropsVisiblyEmptyParts`,
  `TestIsVisiblyEmpty_WhitespaceOnlyContentCountsAsEmpty`,
  `TestSendOne_FallsBackToPlainTextOnAny400WhileParseModeSet`,
  `TestSendOne_400WithoutParseModeIsNotRetried`.
- **L3 (backtick in fence info string) - fixed.** `fenceOpenLang` now
  rejects a `rest` containing a backtick, per CommonMark's own rule.
  Tests: `TestFenceOpenLang_RejectsBacktickInInfoString`,
  `TestRenderHTML_BacktickFenceLookalikeDoesNotSwallowTheRestOfTheMessage`.
- **L4 (successful tool result replaced by "failed: context canceled") -
  fixed.** When `runErr != nil` but `ctx.Err() != nil` and `result != ""`
  (registry.Run's own `(out, nil)` -> `(out, ctx.Err())` wrapping), the
  buffered tool message now persists the real result with an appended note
  instead of the generic failure line - `write_file`/`exec` succeeding right
  as `/stop` (or a deadline) fires no longer looks like a failure the model
  might retry.
  File: `internal/agent/loop.go`.
  Test: `TestRun_ToolSucceedsButCtxEndsAtReturn_PersistsRealResultNotJustFailure`
  (`internal/agent/loop_test.go`).
- **L5 (phantom coverage for four claimed fixes) - three fixed with real
  regression tests, one documented as infeasible to test hermetically.**
  - Startup sweep horizon: `TestApprover_ExpirePending_SweepsARowNotYetPastItsOwnExpiresAt`
    plants a pending row with `ExpiresAt` still in the future and proves
    `Approver.ExpirePending` sweeps it anyway. Verified to fail when
    `expireAllPendingHorizon` is reverted to `time.Now()`.
  - `displayCommand` escape wiring: `TestExec_ApprovalMode_ControlAndBidiCharsInCommandReachApproverEscaped`
    drives the real `execTool.ask` path end to end and asserts
    `Request.Command` reaching the approver is escaped, not just
    `displayCommand` in isolation. Verified to fail when reverted.
  - `TrimToByteBudget` wiring: `TestRun_LoadHistory_DropsOldestTurnsPastByteBudget`
    seeds four ~90 KiB turns (well under `MaxHistoryTurns`, so only the byte
    budget can be responsible) and proves the oldest is dropped from the
    real request `Loop.Run` sends. Verified to fail when reverted.
  - **cron `Round(0)` - left as-is, with reasoning recorded here rather than
    a fake test.** I attempted several constructions to make
    `TestScheduler_TickWithCatchUp_MonotonicReadingsStillDetectTheGap`
    (or a new test) fail when `Round(0)` is removed. Every construction that
    stays within Go's public `time` API produces two `time.Time` values
    whose monotonic-clock delta and wall-clock delta are identical (`Add`
    always offsets both readings together, and any value built from
    `time.Date`/`time.Unix`/parsing carries no monotonic reading at all, so
    `Time.Sub` already falls back to wall-clock and `Round(0)` is a no-op
    either way). The one case `Round(0)` actually changes - two
    monotonic-bearing readings that have desynchronized, exactly what a real
    host suspend produces - has no public constructor in Go's `time`
    package (this is stated in Go's own monotonic-clock documentation, and
    was already the existing test's own stated limitation before this
    pass). The only way to fabricate it would be `unsafe`/`reflect` access
    to `time.Time`'s unexported fields, which I did not do: it would test
    against a state Go's own API guarantees can never occur, is brittle
    across Go versions, and is exactly the kind of shortcut the project's
    own rules ask me not to take to satisfy a check. The existing coverage
    guard and its comment stand unchanged.
- **L6 (finding-code/plan references in code) - scrubbed.** Fixed the three
  named spots (`internal/cli/sessions_cmd_test.go` "mitigates B23" ->
  states the invariant; `internal/channel/telegram/gating_test.go` "Phase 1
  validation" -> "config.Validate"; `internal/gateway/dispatch.go`'s "(not
  after, as a prior version did)" -> states the invariant directly) plus the
  two more from the CLI report (`internal/cron/scheduler_test.go` "the
  phase plan got wrong once already"; `internal/provider/openai/chat_test.go`
  "called out by the phase spec"). Re-grepped the whole repo afterward with
  the controller's regex; the only remaining hits are the ordinary English
  word "finding" in two unrelated comments (`send.go`, `path_guard.go`), not
  a finding-code reference.
- **L7 (history budget doc mismatch; tool-call args not counted) - fixed,
  both parts.** Corrected the stale `maxHistoryBytes = 32 * 1024` mentions in
  `plans/reports/fullstack-developer-260928-1041-agent-provider-store-fixes.md`
  to the actual `256 * 1024` (the controller's decision: 256 KiB stays).
  `TrimToByteBudget` now sums `Content` plus every tool call's own `Args`
  via a new `messageByteSize` helper, so a large `write_file` body or `exec`
  command sitting only in a tool call's arguments (assistant messages that
  invoke a tool typically have empty `Content`) counts toward the budget.
  Files: `internal/agent/history.go`.
  Tests: `TestTrimToByteBudget_CountsToolCallArgumentsTowardTheBudget`
  (verified to fail when reverted); the L5 `TestRun_LoadHistory_...` test
  above also covers the wiring end to end.
- **L8 (`CaptureSenders` window cutoff drops a same-second message) -
  fixed.** Extracted `captureWindowStart(now)` (truncates to whole seconds,
  then backdates by a new `captureWindowGrace` of 3s to absorb clock skew),
  used in place of a bare `time.Now()`.
  File: `internal/channel/telegram/capture.go`.
  Tests: `TestCaptureWindowStart_SameSecondMessageCounts`,
  `TestCaptureWindowStart_BackdatesByTheGraceWindow` (new
  `capture_test.go`) - verified to fail when reverted.

## CLI/docs report (`code-reviewer-260928-1041-final-cli-docs-review.md`)

### Medium

- **M1 (`log.level`/`log.format` validation stricter than `logging`'s own
  parsing) - fixed.** `config.validateLog` and `internal/cli.isValidLogLevel`
  now trim and lowercase before checking, and accept `warning` as an alias
  of `warn` - matching `internal/logging.parseLevel`, which already did
  this. Updated `docs/configuration.md`'s two rows and the stale
  `logging.New` doc comment (the old "typo'd flag degrades gracefully"
  wording no longer described what the code does).
  Files: `internal/config/validate.go`, `internal/cli/root.go`,
  `internal/logging/logger.go`, `docs/configuration.md`.
  Tests: `TestValidate_LogFields_AcceptsMixedCaseWhitespaceAndWarningAlias`,
  `TestPrepare_LogLevelFlagAcceptsMixedCaseAndWarningAlias` - both verified
  to fail when the normalization is reverted; the pre-existing
  `TestValidate_LogFields` negative-case assertion was updated to match the
  new error wording.
- **M2 - same as runtime M4 above.** Fixed once, covers both reports.

### Low

- **L1 (`cron run` prints unescaped model output) - fixed.** The print-only
  path (not `--deliver`, which goes to Telegram, not a terminal) now calls
  `sanitizeForTerminal`, matching `prompt`.
  Test: `TestCronRunCmd_PrintOnlyOutputIsSanitizedForTerminal` (constructs a
  raw ESC byte via a JSON `\u001b` escape so it survives a real JSON decode,
  unlike `%q`, which would emit an invalid-JSON `\x1b`) - verified to fail
  when reverted.
- **L2 - covered under runtime L6 above** (all four named spots plus the
  whole-repo regex sweep).
- **L3 (version package over-claims VCS stamping; phantom always-skip test;
  "built" should say "committed") - fixed, all three parts.** Corrected the
  package doc comment and `docs/architecture.md`'s matching claim to say
  `go build` run directly inside a git checkout, not `go run`/`go install`.
  `String()` now prints "committed %s" instead of "built %s" (Date is the
  commit time). Replaced the always-skipping test with
  `TestString_ResolvesFromBuildInfoInARealBuild`, which actually `go build`s
  a real binary and runs `<binary> version` against it - a `go test` binary
  is never VCS-stamped (confirmed: the old test skipped every single run),
  a real `go build` binary is.
  Files: `internal/version/version.go`, `internal/version/version_test.go`,
  `docs/architecture.md`.
- **L4 (release workflow hardening) - fixed, both parts.** `release.yml`'s
  build step now passes `github.ref_name` through `env: VERSION:` and runs
  `make release VERSION="$VERSION"` instead of interpolating the expression
  straight into `run:`. `persist-credentials: false` added to the checkout
  step in `release.yml` (the `contents: write` job) and, for the same
  least-privilege reason, to both checkout steps in `ci.yml` (whose token
  is already read-only, but nothing in those jobs needs git credentials
  persisted at all).
  Files: `.github/workflows/release.yml`, `.github/workflows/ci.yml`.
  Verified: both files parse as valid YAML (`python3 -c 'import yaml; ...'`,
  `actionlint` itself was not available in this sandbox).
- **L5 (doctor check name "DB opens and migrates" is inaccurate) - fixed.**
  Renamed to "DB opens at current schema" (the check deliberately never
  migrates); updated the one doc reference. No test referenced the old
  string.
  Files: `internal/cli/doctor_checks.go`, `docs/configuration.md`.
- **L6 (no upgrade note for the lock's new location) - fixed.** Added an
  "Upgrading" section to `README.md`: stop the old gateway before starting
  the new binary (the new binary only checks the lock next to the
  database, not the old fixed `~/.mtclaw/gateway.lock`), and the old lock
  file can be deleted.
- **L7 (secret prompt echo restore, narrow window) - fixed the code; no new
  automated test.** `Secret` now returns immediately if `p.ctx.Err() != nil`
  before starting the `term.ReadPassword` goroutine, closing the common case
  (ctx already done when `Secret` is entered). The report's own residual
  window (a signal landing in the few microseconds between that check and
  the goroutine actually turning echo off) is accepted, as the report itself
  proposed. I did not add an automated test for this: exercising the
  terminal-attached branch at all requires a real pty (`term.GetState`/
  `term.ReadPassword` are direct syscalls on a tty device), and this repo
  has no pty test dependency; the existing test file already documents this
  exact limitation for the sibling interrupt test. Adding a new third-party
  pty dependency for one narrow, already-accepted race window did not seem
  proportionate.
  File: `internal/cli/onboard_prompts.go`.
- **L8 (`config show` hides effective `require_mention` when omitted) -
  fixed.** `MarshalRedacted` now renders every group's effective
  `require_mention` (via `MentionRequired()`) on a copied map, rather than
  relying on the field's own `omitempty` YAML tag, which printed nothing at
  all for an omitted key - readable as `false` instead of the true
  effective default.
  File: `internal/config/load.go`.
  Test: `TestMarshalRedacted_ShowsEffectiveRequireMentionEvenWhenOmitted`
  (also asserts the input `cfg` is not mutated) - verified to fail when
  reverted.
- **L9 (stale "MarkdownV2" comment) - fixed.** `internal/channel/telegram/channel.go`
  now says "an HTML parse-mode 400", matching the actual `parse_mode`.
- **Pre-existing `agent.workspace` docs row - fixed.** Corrected to state
  that `tools.filesystem.roots`/`tools.exec.cwd` are independent literal
  defaults of the same path, not derived from `agent.workspace` - setting
  `agent.workspace` alone does not move them.
  File: `docs/configuration.md`.

## Validation (all green)

- `gofmt -l .` - clean.
- `go vet ./...`, `GOOS=windows go vet ./...`, `GOOS=darwin go vet ./...` -
  clean.
- `go test -race -count=1 ./...` - all 14 packages with tests pass.
- `CGO_ENABLED=0` builds for linux/amd64, linux/arm64 (native),
  windows/amd64, darwin/amd64, darwin/arm64 - all succeed (windows exercises
  the new `golang.org/x/sys/windows` import specifically).
- `go mod tidy -diff` - clean.
- No background processes left running.

## Files touched

`internal/tools/exec.go`, `exec_unix.go`, `exec_windows.go`, `approver.go`,
`exec_test.go`; `internal/gateway/dispatch.go`, `dispatch_test.go`,
`gateway.go`, `gateway_test.go`, `shutdown.go`, `shutdown_test.go`;
`internal/channel/telegram/render.go`, `render_test.go`, `chunk.go`,
`chunk_test.go`, `send.go`, `send_test.go`, `approver.go`, `approver_test.go`,
`capture.go`, `capture_test.go` (new), `channel.go`, `gating_test.go`;
`internal/agent/loop.go`, `loop_test.go`, `history.go`, `history_test.go`;
`internal/cli/root.go`, `root_test.go`, `prompt_cmd.go`, `cron_cmd.go`,
`cron_cmd_test.go`, `cron_cmd_linux_test.go` (new), `sessions_cmd_test.go`,
`onboard_prompts.go`, `doctor_checks.go`; `internal/config/validate.go`,
`validate_test.go`, `load.go`, `load_test.go`; `internal/logging/logger.go`;
`internal/version/version.go`, `version_test.go`; `internal/cron/scheduler_test.go`;
`internal/provider/openai/chat_test.go`; `go.mod`, `go.sum`;
`.github/workflows/ci.yml`, `release.yml`; `README.md`, `docs/configuration.md`,
`docs/security.md`, `docs/architecture.md`;
`plans/reports/fullstack-developer-260928-1041-agent-provider-store-fixes.md`.

## Unresolved questions

1. Runtime M5's `__init__.py` behavior (still bolds "init") is a verified,
   correct CommonMark/GFM outcome, not a bug - see the writeup above. If the
   product intent is "never touch dunder-style identifiers regardless of
   spec," that would be a deliberate, non-spec-compliant special case and is
   a product call, not something I changed unilaterally.
2. L5's cron `Round(0)` phantom test and L7's onboard echo-restore edge case
   are both left without new automated regression coverage, for the reasons
   detailed above (a genuine Go API limitation, and a missing pty test
   dependency respectively) rather than forced with something brittle or
   fake. Flagging in case the controller wants a different call on either
   (e.g., accepting an `unsafe`-based test for the former, or adding a pty
   dependency for the latter).

Status: DONE_WITH_CONCERNS
Summary: Every High/Medium finding in both reports is fixed with a verified
(revert-fails) regression test; all Low findings are fixed except two edge
cases (cron `Round(0)` phantom test, onboard echo-restore pty test) that are
documented as infeasible to test safely rather than forced. Full validation
gate (gofmt, vet on three GOOS, race tests, five-platform builds, mod tidy)
is green.
