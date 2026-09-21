# Round-2 review fixes: internal/agent, internal/provider, internal/store slice

Source: `plans/reports/code-reviewer-260921-1649-agent-provider-store-rereview.md`.
Scope owned: `internal/agent/**`, `internal/provider/**`, `internal/store/types.go`,
`internal/store/types_test.go`, `internal/store/sqlite/{approvals,messages,sessions,convert,id,store_test}.go`.
H1 and H2 were already fixed by the controller before this pass; not touched here.

## H3 + M1 - order-aware orphan repair, run after the trim

`internal/agent/history.go`:

- `repairOrphanedToolCalls` rewritten as a single strictly-sequential pass
  (`closeToolCallRun` helper) instead of two global-set passes. An assistant
  `tool_calls` message opens a "run"; only the tool rows immediately
  following it (before the next assistant/user message) can answer those
  calls; an id reused by an unrelated declaration elsewhere can never
  cross-pair with the wrong run's tool row; a tool row with no open run
  (unknown id, already answered, or arriving before any declaration) is
  dropped. Fixes both H3 shapes: reused call ids across turns, and a tool
  row preceding its declaration.
- `HardTrim` reordered: `dropLeadingNonUser` -> turn trim -> repair ->
  `dropLeadingNonUser` again, so the repair sees (and can heal) whatever the
  trim itself just broke, per the report's proposed fix.

Test changes (`internal/agent/history_test.go`):

- `genHistory`'s call ids now come from a small fixed pool (`call_0..2`)
  reused across every round and turn instead of being globally unique, so
  every existing property test now exercises id reuse as a baseline.
- `poisonHistory` extended from 2 to 4 shapes, randomly selected: missing
  tool result, stray tool row (existing), plus two new ones -
  `reuseAnEarlierCallID` (an unanswered second declaration reusing an id an
  earlier, already-answered run used) and `moveAToolRowBeforeItsDeclaration`
  (relocates a genuine tool row to at-or-before its declaring assistant
  message's index).
- New `assertSequentialPairing` helper: a stricter, order-aware validator
  written directly against the run semantics above (global id sets like the
  old `assertNoOrphans` cannot see a reused-id or out-of-position
  cross-pairing). Added to all three property tests, not just the poisoned
  one, since it is a strict superset of `assertNoOrphans`.

## M2 - elision no longer destroys persisted tool output

`internal/agent/loop.go`: the `ErrContextLength` retry no longer reassigns
`buffer = elideBufferedToolResults(buffer)`. Instead, each iteration builds
`requestBuffer` (an elided copy, only once `contextRetried`) used solely for
`assembleMessages`; `buffer` itself stays the real data all the way to
`flush`. `TestRun_ErrContextLength_RetryAlsoElidesBufferedToolResults`
(loop_test.go) flipped: persisted tool row now asserts `Equal(bigResult, ...)`
(was `NotEqual`); the retried-request assertion (placeholder, `<100` bytes)
is unchanged.

## M3 - prompt file cap covers non-regular files, no TOCTOU-through-Stat

`internal/agent/prompt.go`: new `readPromptFile` does `os.Stat` on the
**path** first (never blocks, even on a FIFO with no writer) and rejects
anything that fails `Mode().IsRegular()` before ever calling `os.Open`. Only
a regular file gets opened and read through
`io.LimitReader(f, maxSystemPromptFileSize+1)`. This intentionally does not
Stat the open handle (the report's literal suggestion): `os.Open` on a FIFO
itself blocks until a writer connects, so stat-then-open on the *path* is
what actually prevents the hang the finding describes; verified with a real
FIFO test that this test suite runs and passes in milliseconds. The residual
Stat-then-Open TOCTOU is noted in the code comment as out of this feature's
threat model (local single-user config path, not an attacker racing a
symlink swap).

New tests, unix-only (`internal/agent/prompt_unix_test.go`, `//go:build unix`):
`TestBuild_FIFOPromptFile_SkippedWithoutBlocking` (real `syscall.Mkfifo`, no
writer - would hang the whole `go test` run if the fix regressed) and
`TestBuild_SymlinkToDeviceFile_SkippedWithoutUnboundedRead` (symlink to
`/dev/zero`, skips itself if that path is absent). No new dependency added;
build-tagged out of Windows entirely rather than skipped at runtime.

## M4 - narrower 4xx classification, 413 recognized as context overflow

`internal/provider/openai/errors.go`: added `status == 413 ->
ErrContextLength` (unconditional - there is no other reason an LLM
completion API returns 413, unlike a 400 which needs `isContextLengthError`
to disambiguate) and `408, 409, 425 -> ErrTransient`. 404/422 kept as an
explicit `ErrBadRequest` case, followed by a bare `400 -> ErrBadRequest`
fallback for any 400 that isn't a context-length hit.

**Deviation from the task's compact mapping list**, per the "Verified
Decisions" rule (an existing, tested behavior only reverses on new evidence):
kept `401/403 -> ErrAuth` and `429 -> ErrRateLimit` as their own cases,
*not* folded into `ErrBadRequest`/`ErrTransient` as the task's one-line
summary read. Both are asserted by the pre-existing `TestClassify_Table`
(`"401 unauthorized"`, `"403 forbidden"`, `"429 rate limit"` cases, all
still present and green) and by the source report M4, which only proposed
changing 413/408/409/425 - it never suggested touching 401/403/429. Folding
401/403 into `ErrBadRequest` would also erase a real semantic distinction
(`ErrAuth` vs `ErrBadRequest` are different failure classes for a caller
deciding whether to prompt for a new key). Flagging this rather than
guessing; happy to redo if the mapping really was intended as literally
stated.

`internal/provider/errors.go`: `ErrContextLength` and `ErrBadRequest` doc
comments updated to describe the message-fallback and 413 paths and the
404/422 inclusion; `ErrTransient`'s comment corrected (it previously
described a 429 that is actually classified `ErrRateLimit`, a pre-existing
stale line, now says 408/409/425 instead).

`internal/provider/openai/errors_test.go`: added `413 payload too large`
and `409 conflict` table cases. Removed the phantom `net.Error`-based `"net
timeout"` case (the `net.Error` branch it exercised was already deleted from
`provider.Classify`; it was passing for *any* unrecognized error type, not
discriminating anything) - replaced with `errors.New("connection reset by
peer")` asserting the real fallback behavior (unrecognized error type ->
`provider.Classify` -> `ErrTransient`). `net` import and the now-unused
`fakeTimeoutErr` type removed.

## M5 - dead `final.ToolCalls = nil` removed

`internal/agent/loop.go`: deleted the unreachable assignment and its
8-line comment; replaced with a one-line comment on the branch it actually
depends on (`resp.Message.ToolCalls` empty, per the dispatch condition
above it). `buffer = append(buffer, resp.Message)` used directly instead of
through the now-pointless `final` local.

## Low items

- `internal/store/types.go`: `CronRun.Status` comment now lists
  `started | ok | error | skipped | interrupted`.
- `internal/store/sqlite/store_test.go`:
  `TestMessages_Append_BumpsSessionUpdatedAt` rewritten to be deterministic -
  forces `updated_at` to a one-hour-old sentinel via a direct SQL update
  (using the existing `storeDB` test helper), then asserts the post-`Append`
  value is `After` that sentinel. No sleeps; ran `-count=5` locally, all
  green.
- L2 (stale `Finish` doc comment, no status guard in its SQL) and L3
  (`ApprovalStore.Create`'s zero-`ExpiresAt` precondition undocumented on
  the interface) both live in `internal/store/store.go`, which is outside
  my file ownership for this task - noting rather than fixing. The
  precondition is enforced correctly in `internal/store/sqlite/approvals.go`
  (already present, verified by `TestApprovals_Create_RejectsZeroExpiresAt`).

## Verification

```
gofmt -l internal/agent internal/provider internal/store     # empty
go vet ./internal/agent/... ./internal/provider/... ./internal/store/...   # clean
go build ./...                                                # clean
go test -race -count=2 ./internal/agent/... ./internal/provider/... ./internal/store/...  # all ok
git status --short internal/agent internal/provider internal/store        # only the files listed below + no probe files
```

All green; `internal/provider` itself still reports "no test files" (pre-existing,
not part of this task's scope).

## Files modified

- `internal/agent/history.go` - orphan repair rewrite, HardTrim reorder
- `internal/agent/history_test.go` - generator id reuse, 4-shape poisoning, strict validator
- `internal/agent/loop.go` - non-mutating elision, dead-code removal
- `internal/agent/loop_test.go` - flipped M2 assertion + comment cleanup
- `internal/agent/prompt.go` - Stat-before-Open non-regular-file guard + LimitReader
- `internal/agent/prompt_test.go` - import tidy only (no behavior change)
- `internal/agent/prompt_unix_test.go` - new, FIFO/device-symlink tests
- `internal/provider/errors.go` - doc-comment updates
- `internal/provider/openai/errors.go` - 413/408/409/425 reclassification
- `internal/provider/openai/errors_test.go` - new table cases, phantom case fixed
- `internal/store/types.go` - CronRun.Status comment
- `internal/store/sqlite/store_test.go` - deterministic updated_at test

## Tasks completed

1. H3 + M1 - done
2. M2 - done
3. M3 - done
4. M4 - done, with the noted 401/403/429 deviation
5. M5 - done
6. Low items - done except L2/L3 (store.go not owned; noted)
7. No leftover probe files under `internal/agent`, `internal/provider`, `internal/store`

Status: DONE_WITH_CONCERNS
Summary: All six numbered tasks implemented and green under gofmt/vet/build/race; the one open item is a deliberate deviation from the task's literal 401/403/429 mapping text (kept ErrAuth/ErrRateLimit as the existing, tested, report-unchallenged classes) documented above for a quick yes/no.
Concerns/Blockers: Confirm the 401/403/429 mapping deviation is acceptable, or say to fold them into ErrBadRequest/ErrTransient as literally written and I will redo that one hunk. L2/L3 remain undocumented since they live in internal/store/store.go, outside this task's file ownership.
