# Agent / Provider / Store review fixes

Scope: `internal/agent/*.go`, `internal/provider/{provider,errors}.go`, `internal/provider/openai/*.go`, `internal/provider/mock/*.go`, `internal/store/types.go`, `internal/store/sqlite/{approvals,messages,sessions,convert,open,id}.go`, migrations, and new test files.

## Files changed

- `internal/agent/loop.go` - C1 dispatch fix, H1 buffer elision, H2 bounded cancellation flush, M4 truncation-notice fix, provider-error Result.Usage fix.
- `internal/agent/loop_test.go` - 5 new tests (dispatch-on-payload x2, length-notice, context-length buffer elision).
- `internal/agent/history.go` - `repairOrphanedToolCalls`, wired into `HardTrim`.
- `internal/agent/history_test.go` - poisoned-history invariant test + `poisonHistory` generator.
- `internal/agent/prompt.go` - M5 per-file size cap (256 KiB) with warn-and-skip.
- `internal/agent/prompt_test.go` - oversize-file test.
- `internal/provider/provider.go` - M1 JSON tags on `ToolCall`.
- `internal/provider/errors.go` - removed dead `net.Error` branch (Low).
- `internal/provider/openai/errors.go` - M2 (collapsed dead `>=500` branch, 4xx-except-429/408 now `ErrBadRequest`), M3 (`isContextLengthError` message fallback when `Code==""`).
- `internal/provider/openai/errors_test.go` - 404/422/context-length-no-code table cases.
- `internal/provider/openai/client.go` - `NewWithAPIKey` now sets `option.WithMaxRetries` (Low).
- `internal/store/types_test.go` - new file: round-trip tests (M8) + legacy-uppercase-field decode test (M1).
- `internal/store/sqlite/approvals.go` - M6 `Create` rejects zero `ExpiresAt`.
- `internal/store/sqlite/store_test.go` - M6 test, `Append` updated_at-bump test (Low).
- `internal/store/sqlite/messages.go` - `Append` bumps `sessions.updated_at` in the same tx (Low).
- `internal/store/sqlite/open.go` - M7 invariant comment on `SetMaxOpenConns(1)`, DB file `chmod 0600` best-effort after open.
- `internal/store/sqlite/migrate_test.go` - permission test (unix-only, skips on Windows).
- `internal/store/sqlite/migrations/002_drop_redundant_message_index.sql` - new migration, drops `idx_messages_session_seq` (Low).

## Tasks and verification

1. **C1** - `loop.go` now dispatches on `len(resp.Message.ToolCalls) > 0` instead of `resp.FinishReason == "tool_calls"`; terminal branch sets `final.ToolCalls = nil`. Tests: `TestRun_ToolCallsDispatchOnPayloadRegardlessOfFinishReason` (tool_calls under `FinishReason: "stop"` still runs tools, terminal row carries no ToolCalls), `TestRun_EmptyToolCallsWithToolCallsFinishReason_EndsTurnImmediately` (empty ToolCalls under `FinishReason: "tool_calls"` ends in 1 iteration).
2. **Q5** - `repairOrphanedToolCalls` in `history.go`, called from `HardTrim` right after `dropLeadingNonUser`. Order-aware: drops an assistant `ToolCalls` entry with no later matching tool row, drops a tool message whose id has no earlier declaring assistant entry; drops an assistant message entirely if it ends up with no ToolCalls and no Content. Test: `TestHardTrim_RepairsOrphanedToolCallsInPoisonedHistory` (200 generated histories, each poisoned with a dropped tool result or a stray tool message, `assertNoOrphans` on the output for every maxTurns value).
3. **H1** - `elideBufferedToolResults` replaces every buffered tool message's `Content` with a placeholder on the `ErrContextLength` retry, alongside the existing `HardTrim(history, ...)`. Test: `TestRun_ErrContextLength_RetryAlsoElidesBufferedToolResults` - tool call produces a 5000-byte result, provider returns `ErrContextLength`, retry request's tool content is `< 100` bytes and differs from the original; turn completes and the persisted tool row is the elided version.
4. **H2** - `abortForCancellation` now does `context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)`; comment explains the 5s ties to the DSN's `busy_timeout(5000)` and the single-connection writer pool. No new test (matches the existing `TestRun_CancelMidTurn_PersistsPartialTurn` behavior; the timeout only bounds an already-passing path, and simulating a genuinely wedged connection isn't practical against the real sqlite driver without a fake).
5. **M1** - `provider.ToolCall` now has `json:"id"`, `json:"name"`, `json:"args"` tags. Verified `store/types.go` uses `encoding/json` (both `json.Marshal`/`json.Unmarshal`), confirmed `sqlite/convert.go` has no JSON codec at all (only millis/null conversions). Test: `TestToProviderMessage_DecodesLegacyUppercaseFieldNames` decodes a literal `{"ID":...,"Name":...,"Args":...}` string and asserts the fields land correctly.
6. **M2/M3** - `openai/errors.go`: removed the duplicate `status >= 500` branch (folded into `default`); added `case status == 408` (transient, unchanged from before); `case status >= 400 && status < 500` now returns `ErrBadRequest` (covers 404, 422, etc.), checked after the context-length case so a context-length 400 is never misclassified; `isContextLengthError` keeps the code match as primary and falls back to a case-insensitive message substring match (`context_length`, `context length`, `maximum context length`) only when `Code == ""`. New table cases: 404 `model_not_found`, 422, and a 400 with empty `Code` and a context-length message.
7. **M4** - `loop.go`'s terminal branch now keeps `resultText` (with the truncation notice) separate from what's persisted; `final := resp.Message` is buffered with its original `Content` untouched (only `ToolCalls` is cleared). Test: `TestRun_LengthTruncation_NoticeNotPersistedAsModelContent`.
8. **M5** - `prompt.go` adds `maxSystemPromptFileSize = 256 * 1024`; `writePromptFiles` now `os.Stat`s first, warns+skips (`"too large"`) if oversized, otherwise reads as before. Test: `TestBuild_OversizePromptFileWarnsAndSkips`.
9. **M6** - `approvalStore.Create` returns an error immediately when `ap.ExpiresAt.IsZero()`, before any ID/CreatedAt/State defaulting or the INSERT. Test: `TestApprovals_Create_RejectsZeroExpiresAt`. Verified the one production caller (`internal/channel/telegram/approver.go:109`) always sets `ExpiresAt`, so this is not a behavior break.
10. **M7** - Extended the existing `SetMaxOpenConns(1)` comment in `open.go` with the explicit invariant text (no store method may nest a call, hold a tx, or hold open `*sql.Rows`, on this handle) and why it's dangerous (pool wait blocks on ctx, and some CLI paths use `context.Background()`).
11. **Low items**:
    - `internal/provider/errors.go`: deleted the dead `net.Error`/`Timeout()` branch and the unused `net` import; final fallback comment explains it now covers that case too. `internal/provider/openai/errors_test.go`'s `fakeTimeoutErr` case still passes (falls through to `provider.Classify`'s single `ErrTransient` fallback).
    - `client.go`: `NewWithAPIKey` now sets `option.WithMaxRetries(config.Default().OpenAI.MaxRetries)` (can't take a `config.OpenAIConfig` here without changing the signature onboard_cmd.go calls, which is out of ownership - used the same default value instead, with a comment explaining why).
    - `messages.go` `Append`: added `UPDATE sessions SET updated_at = ?` in the same tx as the message inserts. Test: `TestMessages_Append_BumpsSessionUpdatedAt`.
    - `loop.go`: the "any other provider error" `Result` now carries `Usage: totalUsage` (previously zero-valued); the flush call's persisted usage is unchanged (still `provider.Usage{}`, matching the documented "buffered activity not persisted" behavior - only the caller-facing Result was under-reporting).
    - New migration `002_drop_redundant_message_index.sql` drops `idx_messages_session_seq` (redundant with the `UNIQUE(session_id, seq)` constraint's own index). Existing migration tests (`migrate_test.go`) derive expectations from `loadMigrations()`, so they pass unchanged.
    - Did **not** touch `sessions.go`'s `Ensure` id-generation-before-conflict ordering or `open.go`'s per-call `Sessions()`/`Messages()`/... allocation caching - neither was in the explicit task list (task 11 named exactly: net.Error branch, `NewWithAPIKey` retries, `Append` updated_at bump, provider-error Result.Usage, migration 002), and `cron_runs.go`'s index suggestion is out of ownership entirely.
12. **DB file permissions** - `sqlite.Open` now does `_ = os.Chmod(path, 0o600)` (best-effort, ignored on error) right after migration succeeds, only on the writer path. Test: `TestOpen_WriterCreatesFileWithOwnerOnlyPermissions` (skips on Windows via `runtime.GOOS`).
13. **M8** - `internal/store/types_test.go` (new file): round-trip tests for `ToProviderMessage`/`FromProviderMessage` covering tool_calls, tool-result, and plain-text messages, plus the legacy-field-name decode test (M1) and a malformed-`tool_calls`-errors test.

## Tests added (full list)

- `internal/agent/loop_test.go`: `TestRun_ToolCallsDispatchOnPayloadRegardlessOfFinishReason`, `TestRun_EmptyToolCallsWithToolCallsFinishReason_EndsTurnImmediately`, `TestRun_LengthTruncation_NoticeNotPersistedAsModelContent`, `TestRun_ErrContextLength_RetryAlsoElidesBufferedToolResults`.
- `internal/agent/history_test.go`: `TestHardTrim_RepairsOrphanedToolCallsInPoisonedHistory` (+ `poisonHistory` helper).
- `internal/agent/prompt_test.go`: `TestBuild_OversizePromptFileWarnsAndSkips`.
- `internal/provider/openai/errors_test.go`: 3 new table cases.
- `internal/store/types_test.go` (new file): 5 tests.
- `internal/store/sqlite/store_test.go`: `TestApprovals_Create_RejectsZeroExpiresAt`, `TestMessages_Append_BumpsSessionUpdatedAt`.
- `internal/store/sqlite/migrate_test.go`: `TestOpen_WriterCreatesFileWithOwnerOnlyPermissions`.

## Skipped / deviations

- No dedicated test for H2's timeout value itself (nothing in the existing test setup can force the single sqlite connection to wedge without a fake driver, and the report flagged this as a hazard-prevention fix rather than a reproducible bug); the comment documents the reasoning and the existing cancellation test continues to pass.
- Left `internal/store/sqlite/sessions.go`'s `Ensure` id-before-conflict ordering and `open.go`'s per-call store-accessor allocation untouched - out of the explicit task list (Low items were scoped to a specific subset in the assignment, both call outs live in ownership but the assignment text did not include them among the numbered Low fixes).
- Did not touch `internal/store/sqlite/cron_runs.go` (out of ownership, another agent owns it) despite the review's index suggestion there.

## Gates

- `gofmt -l internal/agent internal/provider internal/store` - empty.
- `go vet ./internal/agent/... ./internal/provider/... ./internal/store/...` - clean.
- `go build ./internal/agent/... ./internal/provider/... ./internal/store/...` - clean.
- `go build ./...` - fails only in `internal/tools` (`policy.go:7` unused `strings` import, `policy.go:115` undefined `normalizeForDeny`) - pre-existing/another agent's mid-edit work, outside my ownership; confirmed `internal/cli`, `internal/gateway`, `internal/cron`, `internal/channel` (all consumers of my packages) build clean.
- `go test -race ./internal/agent/... ./internal/provider/... ./internal/store/...` - all green:
  - `internal/agent` ok
  - `internal/provider` no test files (unchanged, only `errors.go`'s dead branch removed)
  - `internal/provider/mock` ok
  - `internal/provider/openai` ok
  - `internal/store` ok
  - `internal/store/sqlite` ok

## Unresolved questions

None - all numbered tasks and their explicitly-listed Low sub-items were completed; the two Low items outside the assignment's explicit list were left alone per file-ownership/scope discipline rather than left ambiguous.
