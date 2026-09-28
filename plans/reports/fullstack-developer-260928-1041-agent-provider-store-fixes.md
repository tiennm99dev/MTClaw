# Agent / provider / store fix pass

Date: 2026-09-28. Branch `refactor/260928-full-review`. Scope: `code-reviewer-260928-0953-agent-provider-store-review.md`,
constrained by the decisions in `plans/260928-1041-third-round-review-fixes/plan.md`.

## Files touched

- `internal/agent/loop.go`, `internal/agent/history.go`, `internal/agent/prompt.go`
- `internal/agent/loop_test.go`, `internal/agent/history_test.go`
- `internal/provider/errors.go`, `internal/provider/errors_test.go` (new)
- `internal/provider/mock/mock.go`, `internal/provider/mock/mock_test.go`
- `internal/provider/openai/{client,chat,errors}.go`, `internal/provider/openai/errors_test.go`,
  `internal/provider/openai/complete_httptest_test.go` (new)
- `internal/store/store.go`, `internal/store/sqlite/sessions.go`
- `internal/store/sqlite/store_test.go`, `internal/store/sqlite/migrate_test.go`
- `internal/store/sqlite/migrations/001_init.sql` (comment-only edit, see M/L5 note below),
  `internal/store/sqlite/migrations/002_approvals_session_index.sql` (new migration)
- `internal/config/types.go`, `internal/config/defaults.go`, `internal/config/validate.go`,
  `internal/config/validate_test.go`
- `docs/configuration.md`

No mechanical caller edits were needed in `internal/cli` or `internal/gateway`: `agent.temperature`
had no reader outside `internal/agent`/`internal/config`, and `store.SessionStore.SetSummary` had no
caller anywhere in the repo.

## Bugs

**H1 (HTTP timeout reported as user cancel) - fixed.** `provider.Classify` and `openai`'s `classify`
now take `ctx` and decide `ErrCanceled` only from `ctx.Err() != nil`, never from an error's shape
(an `http.Client.Timeout` also produces a `context.DeadlineExceeded`-shaped error while `ctx` is
still healthy). The loop's two decision points (`Complete`'s error path, and a failed tool call) now
check `ctx.Err() != nil` directly instead of the classified `Kind` or `errors.Is(runErr, context.Canceled)`.
`abortForCancellation` now always forces `Kind: ErrCanceled` on its own, since by the time it is
called `ctx.Err() != nil` is already established - this avoids an already-classified `*provider.Error`
(e.g. a scripted `ErrAuth`) passing through `Classify`'s error-type shortcut with the wrong Kind.
Tests: `TestComplete_RealTimeoutIsTransientNotCanceled`, `TestComplete_RealCancelIsCanceled`
(httptest, real SDK round trip), `TestClassify_Table`'s new ctx-based rows, `TestClassify_LiveCtxFallsBackToTransient`,
`TestClassify_CtxDoneMeansCanceled` (`internal/provider`), `TestRun_ToolOwnDeadlineExceeded_IsAToolErrorNotATurnCancel`
(new), `TestRun_ToolCancelMidBatch_BackfillsUnrunCalls` (rewritten to actually cancel the shared ctx,
matching the real /stop-or-timeout shape instead of a tool merely returning a `context.Canceled`-shaped
error).

**H2 (elision leaks past the retry point) - fixed.** `Run` now records `elideUpTo := len(buffer)` when
the one `ErrContextLength` retry fires, and `elideBufferedToolResults(buffer, elideUpTo)` only touches
`buffer[:elideUpTo]` on every subsequent request; tool results produced in later iterations of the same
turn are sent in full. Test: `TestRun_ErrContextLengthRetry_ElidesOnlyUpToRetryPoint`.

**H3 (temperature default breaks reasoning models) - fixed, per the plan's binding decision ("optional,
sent only when set").** `config.AgentConfig.Temperature` is now `*float64` (`yaml:"temperature,omitempty"`),
`Default()` leaves it nil, `validate.go` only range-checks a set value, and the loop passes
`l.cfg.Agent.Temperature` straight through (already a pointer in `provider.Request`). `docs/configuration.md`
updated. Tests: `TestValidate_Agent_TemperatureUnsetOrInRangePasses`, updated `TestValidate_AgentFields`,
`TestComplete_OmitsTemperatureWhenUnset` (httptest, asserts the literal request body has no `temperature`
key when unset and the right value when set).

**M1 (numeric error code hides context-length overflow) - fixed.** `isContextLengthError` no longer
returns `false` the moment `apiErr.Code` is non-empty; it now treats `Code` as authoritative only when
it equals `context_length_exceeded`, and otherwise always checks the message (added `"context size"`
and `"context window"`; dropped the now-redundant `"maximum context length"` substring check, since it
is already covered by `"context length"`). Tests: `TestComplete_ContextLength_OpenAIStyle`,
`TestComplete_ContextLength_NumericCodeStyle` (vLLM- and llama.cpp-shaped bodies over a real SDK round
trip), plus new rows in `TestClassify_Table`.

**M2 (final flush can be lost to the caller's own cancellation) - fixed.** `flush` itself now always
runs on `context.WithTimeout(context.WithoutCancel(ctx), flushTimeout)` (5s, matching the sqlite DSN's
`busy_timeout`); the special-casing that used to live only in `abortForCancellation` is gone, so every
exit path gets the same detached-and-bounded write. Covered indirectly by every existing `Loop.Run`
test that asserts persistence (all of them now go through the same `flush`); no new test isolates the
exact race window between `Complete` returning and `flush` starting, since reproducing "ctx cancels
between Complete and Append" deterministically would need a fake store with an injectable delay, which
felt like more machinery than this fix warrants - the fix itself is a straight code-level guarantee
(flush no longer takes ctx's cancellation as an input at all), not a race that needs empirical
reproduction.

**M3 (no byte budget on loaded history) - fixed with a fixed internal constant, per the plan's binding
decision (no new config key).** Added `TrimToByteBudget` in `history.go` (`agent.history`'s
`maxHistoryBytes = 256 * 1024`, applied in `loadHistory` after `HardTrim`): drops whole oldest turns
until total message size (`Content` plus every tool call's own `Args`) is at or under the budget, or
only the last turn remains. Tests:
`TestTrimToByteBudget_DropsOldestWholeTurnsUntilUnderBudget`,
`TestTrimToByteBudget_KeepsAtLeastTheLastTurnEvenIfOversize`,
`TestTrimToByteBudget_NoOpWhenUnderBudgetOrDisabled`, and a property test
(`TestTrimToByteBudget_NeverOrphansGeneratedHistories`) mirroring `HardTrim`'s existing invariant tests.

**M4 (backfill uses a turn-wide id set) - fixed via the proposed positional refactor.**
`backfillAbortedToolCalls` now takes `resp.Message.ToolCalls[i+1:]` (the calls after the one that just
aborted, found by position) instead of scanning the whole buffer for "already answered" ids. Covered by
`TestRun_ToolCancelMidBatch_BackfillsUnrunCalls` and the property-style `assertNoOrphans`/`assertSequentialPairing`
checks already run against `HardTrim` output built from a persisted transcript.

**M5 (provider error mid-turn drops tool activity) - fixed per the plan's controller decision: persist
like the other two abort paths.** The "any other provider error" branch now flushes the full `buffer`
and `totalUsage` through the same `finish` helper the cancellation and tool-error exits use, instead of
flushing only the user message with zero usage. Test:
`TestRun_ProviderErrorMidTurn_PersistsToolActivityAlreadyRun` (asserts the tool row, its `ToolName`, and
the billed usage all survive). `TestRun_OtherProviderError_PersistsWhateverWasBuffered` (renamed from
`..._FlushesOnlyUserMessage`) and `TestRun_ErrContextLength_SecondFailureReturnsError`'s comment were
updated to describe the real invariant ("whatever was buffered", which happens to be just the user
message in those two scripts) instead of overclaiming a hardcoded "user message only" policy.

**L1 (reused call id gets the wrong tool_name) - fixed via R1.** `flush` now resolves each tool row's
name from the nearest preceding assistant `tool_calls` declaration while it walks `buffer`, instead of
a turn-wide map. Test: `TestRun_ReusedCallID_RecordsTheToolNameActuallyDeclared`.

**L2 (doctor probe sends `max_tokens`) - fixed in `provider/openai`, since that is where `Probe` lives.**
`Probe` now sends `MaxCompletionTokens` instead of the deprecated `MaxTokens`; the stale "wiring it up is
deliberately out of scope" doc comment on `ProbeResult` was also removed (`doctor` already calls it).
Test: `TestProbe_SendsMaxCompletionTokensNotMaxTokens`.

**L3 (no tie-breaker in session list order) - fixed.** `ORDER BY updated_at DESC, id DESC` (ids embed a
creation-order millisecond prefix, see `newSessionID`). The existing 2ms-sleep-based test was rewritten
to plant `updated_at` explicitly (the way `TestMessages_Append_BumpsSessionUpdatedAt` next to it already
does), and a new `TestSessions_List_TiesBrokenByIDDesc` pins the same-millisecond tie-break directly.

**L4 (no index on `approvals.session_id`) - fixed with a new migration**, not an edit to `001_init.sql`'s
schema: `internal/store/sqlite/migrations/002_approvals_session_index.sql` adds
`CREATE INDEX idx_approvals_session ON approvals(session_id)`. Test: `TestOpen_ApprovalsSessionIDIndexExists`.

**L5 (stale schema comment) - fixed.** `001_init.sql`'s `cron_runs.status` column comment now reads
`started|ok|error|skipped|interrupted`. This is a comment-only edit inside an already-applied migration
file, not a schema change - the review itself calls this out as harmless since migrations are keyed by
version, not checksum, and it is the one exception to "never edit an applied migration" I'm flagging
explicitly. The stale note in `store.go`'s `CronRunStore` doc comment pointing at this discrepancy was
also deleted.

**L6 (dynamic message string breaks log filtering) - fixed.** `writePromptFiles` now logs
`"system prompt file skipped"` as the fixed message with `path`/`error` as structured attributes,
instead of putting `err.Error()` in the message slot.

**L7 (finding-code/phase references in comments and test names) - fixed** across every file in
`internal/agent`, `internal/provider` (incl. `openai`, `mock`), and `internal/store` (incl. `sqlite`):
verified with a repo-wide grep for `phase N` / `H\d` / `M\d` / `L\d` / `R\d` patterns inside those
package trees - zero remaining.

## Refactors

- **R1 (delete `toolNames`, resolve at flush time) - done.** Removed the map from `Run`,
  `backfillAbortedToolCalls`, and `abortForCancellation`; `flush` now tracks the last assistant
  declaration's call names while it walks `buffer`.
- **R2 (positional backfill) - done**, folded into the M4 fix above; `backfillAbortedToolCalls` is now
  ~10 lines with no map and no buffer scan.
- **R3 (one exit helper) - done.** Added `Loop.finish`; all four flush-and-return blocks in `Run` (and
  `abortForCancellation`) now share one flush-failure policy: always log, and only overwrite `res.Err`
  when the turn did not already fail for a more specific reason.
- **R4 (remove dead summary plumbing) - done.** Deleted `SessionStore.SetSummary` (interface + sqlite
  impl, no callers anywhere in the repo), the loop's `summary` variable, and `assembleMessages`'s
  `summary` parameter. Kept the `sessions.summary` column and `Session.Summary` field, per the
  instruction. Left `Session.Title`/`Model` untouched - the review explicitly flags dropping those as
  "the owner's call", not something this pass decides.
- **R5 (replace `Turn`/`SegmentTurns` with one backward scan) - skipped.** `SegmentTurns` (and `Turn`)
  is now used by *two* trims, not one: `HardTrim`'s turn-count trim and the new `TrimToByteBudget` for
  M3. Collapsing both into ad hoc backward scans would mean re-deriving the same turn-boundary logic
  twice instead of once, and rewriting two heavily property-tested functions in the same pass as
  everything else above felt like more risk than the ~35-line savings justify right now. Left as a
  clearly reversible deferral, not a correctness gap - flagging for the final review pass.
- **R6 (simplify `classify`) - done.** Collapsed 400/404/422 into one `ErrBadRequest` case and folded
  408/409/425 into the `default` `ErrTransient` case; the duplicate ctx-cancellation check is gone (only
  the ctx-based one remains, per H1). Also took the "give the taxonomy a consumer" option:
  `provider.Error.Error()` now folds `Kind` (and `Status` when known) into the rendered message, so a log
  line built from it shows the classification even though `agent`/`gateway` still only branch on `Kind`
  directly. Tests: `TestError_ErrorStringFoldsKindAndStatusIn`, `TestErrKind_StringCoversEveryKind`.
- **R7 (one client constructor) - done.** `New` and `NewWithAPIKey` both call a shared
  `newClient(apiKey, baseURL, timeout, maxRetries)`.
- **R8 (tighten loop.go's comments) - partially done.** Consolidated the elision rationale (previously
  repeated at three call sites) down to one explanation each in `Run`'s request-building comment,
  `elideBufferedToolResults`'s doc comment, and `flush`'s doc comment - each now states a distinct part
  of the invariant instead of the same sentence three times. I did not do a full line-by-line audit of
  every remaining comment in the file; given how much of `loop.go` already changed for H1/H2/M2-M5/R1-R4,
  a further comment-trimming pass felt like a nice-to-have rather than something this slice should risk
  more churn on.

## Test coverage (`go test -count=1 -cover`, non-race)

| Package | Before | After |
|---|---|---|
| `internal/agent` | 92.4% | 93.5% |
| `internal/provider` | 0% | 100% |
| `internal/provider/mock` | 100% | 100% |
| `internal/provider/openai` | 59.8% | 89.7% |
| `internal/store` | 93.8% | 93.8% |
| `internal/store/sqlite` | 76.2% | 78.1% |

## Validation

- `gofmt -l` on every touched/added file in `internal/agent`, `internal/provider` (incl. `openai`,
  `mock`), `internal/store` (incl. `sqlite`), and the touched `internal/config` files: clean.
- `go build ./...`: clean.
- `go vet ./...` (whole repo, not just my packages): clean at the time of this report. An earlier
  `go vet ./...` run mid-session showed failures in `internal/tools` and `internal/cli` (undefined
  `resolveShell`, undefined `TerminalApprover.Timeout`) - those are outside my ownership (other agents'
  concurrent work) and had already been resolved by the time of the final check; not something I touched
  or needed to fix.
- `go test -race -count=1 ./internal/agent/... ./internal/provider/... ./internal/store/... ./internal/config/...`:
  all packages pass.

### One test-writing note worth flagging

My first draft of the real-cancellation httptest (`TestComplete_RealCancelIsCanceled`) had the server
handler block on `<-r.Context().Done()`, assuming the client's context cancellation would promptly
cancel the server's per-request context. It does not, reliably, for a plain HTTP/1.1 handler with
nothing left to read from the connection - the test hung for 3 minutes until `go test`'s own timeout
killed it (confirmed via `-timeout=180s` and a goroutine dump showing the handler still parked on that
channel receive). Fixed by having the handler sleep a fixed, bounded duration instead, so
`httptest.Server.Close()` can never block on a stuck handler goroutine regardless of what the client
side does. Also had to update `TestRun_ToolCancelMidBatch_BackfillsUnrunCalls`, which pre-dates this
pass: it scripted a tool returning a bare `context.Canceled` value without ever cancelling the turn's
own `ctx` (`context.Background()`), which is exactly the shape H1's fix says must *not* be treated as a
turn cancellation anymore. Rewrote it to actually cancel the shared `ctx`, matching the real /stop-or-
cron-timeout scenario the test's own comment describes, and added a new test
(`TestRun_ToolOwnDeadlineExceeded_IsAToolErrorNotATurnCancel`) that pins the case the old test used to
(mis)cover: a tool's own internal deadline, with the turn's ctx genuinely healthy, is an ordinary tool
error, not a cancellation.

## Unresolved / left for the final review pass

1. R5 deferral (see above) - `Turn`/`SegmentTurns` kept, now shared by two trims.
2. `Session.Title`/`Model` (never written anywhere) - left as is; the review calls dropping them "the
   owner's call", not part of this slice's decided scope.
3. `maxHistoryBytes = 256 * 1024` is a judgment call, not a value handed down by the plan (which only
   fixed *that* it must be an internal constant, not a config key). The final review flagged this as a
   discrepancy against an earlier, inaccurate `32 * 1024` mention in this same report (a documentation
   error, not a code change - the code was always `256 * 1024`); the controller decided 256 KiB stays.
