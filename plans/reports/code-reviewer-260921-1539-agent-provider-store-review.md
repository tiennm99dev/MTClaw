# Re-review: internal/agent, internal/provider, internal/store

Date: 2026-09-21 | Scope: `internal/agent/*`, `internal/provider/**`, `internal/store/**` (~4.1k LOC incl. tests) | Advisory only, no files changed.

## Gate results

- `go vet ./internal/agent/... ./internal/provider/... ./internal/store/...` — clean.
- `CGO_ENABLED=1 go test -race ./internal/agent/... ./internal/provider/... ./internal/store/...` — all green (race detector built fine on linux/arm64; no fallback needed). `internal/provider` and `internal/store` report "no test files".

## Prior-review fixes verified

- **H1 (mid-batch tool abort orphans `tool_calls`) — holds.** `backfillAbortedToolCalls` (`internal/agent/loop.go:285`) is called at `loop.go:192`, i.e. *before* both abort exits (`loop.go:195` cancellation, `loop.go:198` flush). Behavioural test at `internal/agent/loop_test.go:203`.
- **M1 (history window starts mid-turn) — holds.** `HardTrim` calls `dropLeadingNonUser` at `internal/agent/history.go:50`, *before* the `maxTurns <= 0` early return at `history.go:51`, so the boundary fix is not bypassable. Tests at `history_test.go:80` (generated invariants) and `history_test.go:109`.

Both fixes are real, minimal, and tested. No regression found in either.

---

## Critical

### C1. Turn dispatch keys on `FinishReason` alone; a `tool_calls` payload under any other finish reason is persisted as an orphan and poisons the session

`internal/agent/loop.go:166`
```go
if resp.FinishReason == "tool_calls" {
```
`internal/agent/loop.go:231-233`
```go
final := resp.Message
final.Content = text
buffer = append(buffer, final)
```

`final` is `resp.Message` **verbatim, including `ToolCalls`**. Nothing between line 221 and the flush at line 235 strips them. `store.FromProviderMessage` (`internal/store/types.go:77-82`) then JSON-encodes them into `messages.tool_calls`, and the row is committed with no matching tool rows.

Failure scenario (not hypothetical — `openai.base_url` is user-configurable to any OpenAI-compatible endpoint, `internal/config/validate.go:92`, `docs/configuration.md:50`; vLLM, llama.cpp, LiteLLM and several proxies return `finish_reason:"stop"` or `""` alongside a populated `tool_calls` array):

1. Model returns `tool_calls` with `finish_reason:"stop"`.
2. Loop takes the terminal branch, buffers the assistant message *with* `ToolCalls`, flushes.
3. Every subsequent turn's `loadHistory` replays that row; `toSDKMessage` (`internal/provider/openai/chat.go:116-132`) rebuilds a full assistant-with-tool-calls param; the API rejects it with 400 ("assistant message with `tool_calls` must be followed by tool messages").
4. `classify` maps it to `ErrBadRequest` (`internal/provider/openai/errors.go:48`), which the loop does **not** retry — it flushes only the user message (`loop.go:157`) and errors out.
5. The session is dead for `max_history_turns` (default 40) consecutive failed turns, until the poisoned turn ages out of `HardTrim`'s window. `HardTrim` cannot repair it: `dropLeadingNonUser` only fixes a *leading* prefix, never a mid-history orphan.

Mirror case, same root cause: `finish_reason == "tool_calls"` with an **empty** `ToolCalls` slice enters the tool branch, buffers an assistant message, runs zero tools, and loops — burning `max_iterations` (default 20) paid provider calls per turn before emitting the cap text.

Fix (smallest that closes both): dispatch on the payload, not the label.
```go
if len(resp.Message.ToolCalls) > 0 {   // replaces the FinishReason check at :166
```
and belt-and-braces at the terminal branch, so no orphan can ever reach the store:
```go
final := resp.Message
final.ToolCalls = nil
final.Content = text
```
Then `FinishReason` is used only for the `"length"` truncation notice, which is what it is actually reliable for.

---

## High

### H1. `ErrContextLength` retry trims history but never the in-turn buffer, so a tool-heavy turn fails deterministically on every attempt

`internal/agent/loop.go:144-150`
```go
if perr.Kind == provider.ErrContextLength && !contextRetried {
    contextRetried = true
    iterations--
    history = HardTrim(history, contextLengthRetryTurns)
    summary = ""
    continue
}
```
`buffer` — which by iteration N holds N assistant turns plus every tool result — is not touched, and neither is `systemPrompt`. Tool output is capped per call (`internal/tools/exec.go:222-224`) but not per turn, so with `max_iterations: 20` the buffer alone can carry ~20× `MaxOutputBytes`.

Scenario: user asks for something that requires several large `exec` outputs. Iteration 7 overflows. Retry trims history to 4 turns — the dominant term (the buffer) is unchanged — and overflows again. Second `ErrContextLength` falls through to `loop.go:157`: only the user message is persisted, the turn and all its tool work are discarded, user gets `turnErrorReply`. The user retries; the model takes the same path; overflows identically. Permanent wedge for that request, invisible in the transcript (the store shows a lone user message).

Fix: on the retry, also shrink the buffer — keep the user message and the last complete assistant/tool pair, replacing older tool results with a short elision marker (preserving one tool message per `tool_call_id` so the pairing invariant survives). Minimum viable alternative: truncate each buffered tool-role `Content` to a few KB on retry.

### H2. Cancellation flush runs on a context with no deadline and a 1-connection pool — a wedged writer blocks the worker past the drain deadline

`internal/agent/loop.go:310-313`
```go
bg := context.WithoutCancel(ctx)
if err := l.flush(bg, sessionID, buffer, toolNames, usage); err != nil {
```
`context.WithoutCancel` strips the deadline as well as the cancellation. The writer handle is capped at one physical connection (`internal/store/sqlite/open.go:84`, `db.SetMaxOpenConns(1)`), and `database/sql`'s pool wait is bounded **only** by the context. `BeginTx` in `messageStore.Append` (`internal/store/sqlite/messages.go:31`) therefore waits forever if the single connection is held by a stuck write.

Scenario: SIGTERM. All in-flight workers cancel simultaneously and every one calls `abortForCancellation`. If one `Append` is stuck (disk full, WAL on a hung NFS/loop mount, a long `busy_timeout` chain against a second process's writer), the remaining workers block indefinitely. `drain` (`internal/gateway/shutdown.go:42`, 30 s) gives up and logs "some workers may still be running" — the partial turns those workers were holding are lost *and* the goroutines leak until process exit.

Fix:
```go
bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
defer cancel()
```
5 s matches the DSN's `busy_timeout(5000)` (`open.go:109`), so the flush fails on the same budget the driver already uses rather than on an unbounded one.

---

## Medium

### M1. `provider.ToolCall` is a persisted wire format with no JSON tags

`internal/provider/provider.go:27-31` — `ID`, `Name`, `Args`, no struct tags. `store.FromProviderMessage` (`internal/store/types.go:78`) marshals it straight into `messages.tool_calls`, and `ToProviderMessage` (`types.go:58`) unmarshals it back. The on-disk column keys are therefore the Go field names. Renaming `Args` → `Arguments` (an obvious future tidy-up, especially if a second provider is added) silently decodes to a zero value for every historical row — `toSDKMessage` then sends `Arguments: ""`, which the API rejects, reproducing C1's poisoning symptom on old data. Fix: `json:"id"`, `json:"name"`, `json:"args"` tags, plus a round-trip test (see T1).

### M2. `openai.classify` has two identical branches and misclassifies 404/422 as transient

`internal/provider/openai/errors.go:50-53`
```go
case status >= 500:
    return &provider.Error{Kind: provider.ErrTransient, ...}
default:
    return &provider.Error{Kind: provider.ErrTransient, ...}
```
The `>= 500` case is dead — `default` is byte-identical. More importantly a 404 `model_not_found` (typo'd `agent.model`, or a model retired upstream mid-run) is reported as *transient*, so the operator is told "temporary problem, retry" for a permanent config fault. The test table (`errors_test.go:33-41`) covers 401/403/429/500/400×2 but no 4xx outside 400. Fix: collapse the two branches, add `case status == 404, status == 422: ErrBadRequest`.

### M3. `ErrContextLength` detection is OpenAI-vendor-specific, so the H1 retry path is dead against every other backend

`internal/provider/openai/errors.go:46`
```go
case status == 400 && apiErr.Code == codeContextLengthExceeded:
```
The comment at `errors.go:12-15` justifies matching on code rather than message text, and that reasoning is sound *for api.openai.com*. But `base_url` is a first-class, documented config knob pointing at arbitrary OpenAI-compatible endpoints (`docs/configuration.md:50`), and those overwhelmingly return 400 with `code: null` for context overflow. Against them the whole trim-and-retry recovery at `loop.go:144` never fires; the turn dies as a plain `ErrBadRequest`. I am not asking to reverse the verified decision — keep the code match as the primary signal and add a *fallback* only when `Code == ""`: a case-insensitive check for `maximum context length` / `context length` / `too many tokens` in `apiErr.Message`. Then non-OpenAI backends degrade to "usually recovers" instead of "never recovers".

### M4. Truncation notice is written into the transcript as the model's own words

`internal/agent/loop.go:227-233`
```go
if resp.FinishReason == "length" {
    text += "\n\n[response truncated: the model's output hit the token limit]"
}
final := resp.Message
final.Content = text
```
`final` is what gets flushed, so next turn the model reads our editorial note as something *it* said. This directly contradicts the `NO_REPLY` contract three lines of comment earlier (`loop.go:23-25`: "It is still persisted so the transcript stays honest about what the model actually produced"). Fix: keep the notice in `Result.Text` only; persist `resp.Message.Content` unmodified.

### M5. System prompt files are re-read from disk every turn, uncapped, and are never trimmable

`internal/agent/loop.go:118` calls `Build(...)` per `Run`; `writePromptFiles` (`internal/agent/prompt.go:80-89`) does `os.ReadFile` per configured path with no size limit. A user who points `agent.system_prompt_files` at a large generated file (or accidentally at a log) inflates *every* request. Because the system prompt sits outside `history` and `buffer`, neither `HardTrim` nor the H1 retry can shed it — the session becomes permanently un-completable with no diagnostic. Fix: cap each file (e.g. 64 KiB) with an explicit `[truncated]` marker and a `log.Warn`, matching how the fs tool already handles oversized reads (`internal/tools/fs.go:52`).

### M6. `ApprovalStore.Create` defaults every field except the one whose zero value is dangerous

`internal/store/sqlite/approvals.go:19-32` defaults `ID`, `CreatedAt`, `State` — but not `ExpiresAt`. `toMillis` maps a zero time to `0` (`internal/store/sqlite/convert.go:16-21`), and `ExpirePending` selects `expires_at < ?` (`approvals.go:102`), so an approval created with a zero `ExpiresAt` is expired by the next sweep, silently denying the user's pending command. Fix: return an error from `Create` when `ExpiresAt.IsZero()` — this is a caller bug, not a state to tolerate.

### M7. `MaxOpenConns(1)` makes any nested store call a self-deadlock, with nothing preventing one

`internal/store/sqlite/open.go:84`. No current path violates it (`Append` is the only tx and touches nothing else; `Decide` calls `Get` *after* its `Exec`, not inside a tx), so this is a latent hazard, not a live bug. But the failure mode is bad: a future call to any store method while a tx is open, or inside a `rows.Next()` loop, blocks until the context expires — and several CLI paths pass `context.Background()`. Fix (cheap, no behaviour change): document the invariant at `open.go:84` ("no store method may be called while a tx or open `*sql.Rows` is held on this handle"), and treat it as a review checklist item.

### M8. `internal/store` has zero tests, including for the persisted encoding

`internal/store/types.go:51-85` (`ToProviderMessage`/`FromProviderMessage`) is the encode/decode boundary for `messages.tool_calls` — the exact column C1 and M1 corrupt — and there is no `internal/store/*_test.go` at all (verified: `go test` reports "no test files"). See T1.

---

## Low

- `internal/provider/errors.go:97-102` — the `net.Error`/`Timeout()` branch and the final fallback both return `ErrTransient`. Dead branch; delete it or give timeouts a distinct kind.
- `internal/provider/openai/client.go:66-78` — `NewWithAPIKey` omits `option.WithMaxRetries`, which `New` sets at `client.go:44`. `onboard`'s key check thus retries on the SDK default (2) regardless of `openai.max_retries`. Inconsistent, harmless today.
- `internal/store/sqlite/migrations/001_init.sql:26-28` — `UNIQUE (session_id, seq)` already creates an index on exactly those columns; `idx_messages_session_seq` duplicates it. Two B-trees maintained per insert for one access path. Drop the explicit index (safe to edit 001 pre-release; otherwise a 002 `DROP INDEX`).
- `internal/store/sqlite/sessions.go:20-24` — `Ensure` generates a fresh session ID (a `crypto/rand` read) on *every* inbound message, discarding it on the conflict path. Move the ID generation behind the conflict, or accept it as negligible.
- `internal/store/sqlite/messages.go:26` — `Append` never bumps `sessions.updated_at`. A session whose turns record no usage (mock provider, cached/zero-usage responses) sinks in `sessions list`'s `ORDER BY updated_at DESC` (`sessions.go:53`) despite active traffic.
- `internal/store/sqlite/cron_runs.go:66` — with `jobName == ""` the query orders by `started_at` but `idx_cron_runs_job` is `(job_name, started_at)`, so it is a full scan + sort. Add `idx_cron_runs_started(started_at)` if history grows.
- `internal/agent/loop.go:157` — the provider-error flush passes `provider.Usage{}`, discarding `totalUsage` accumulated over earlier iterations. Tokens were spent and billed; `sessions.prompt_tokens` under-reports them. Pass `totalUsage`.
- `internal/store/sqlite/open.go:263-267` — `Sessions()`/`Messages()`/… allocate a new struct on every call, so idiomatic call sites (`st.Messages().Append(...)`) allocate per operation. Make them cached fields on `Store`.

---

## Test quality

Overall high: `internal/agent`'s loop tests run against a **real** SQLite store (`loop_test.go:22-28`), not a fake, so they actually exercise the `Append` atomicity the turn-buffering contract rests on. `history_test.go:80` is a generated-input invariant test. `store_test.go:74` and `:379` are genuine concurrency tests. No phantom tests found in this slice.

Gaps, in priority order:

- **T1 (blocks C1/M1).** No test asserts a `FinishReason`/`ToolCalls` mismatch. Add both directions: `mock.Step{Content:"done", ToolCalls:[...], FinishReason:"stop"}` must persist **no** `tool_calls` column; `mock.Step{FinishReason:"tool_calls"}` with no calls must not spin to `max_iterations`. Add a `store` package round-trip test: `FromProviderMessage` → `ToProviderMessage` preserves ID/Name/Args byte-for-byte, and a malformed `tool_calls` column errors rather than silently dropping (`types.go:58`).
- **T2 (blocks H1).** No test that the retried request is smaller *when the overflow is in the buffer*. `loop_test.go:277` seeds prior turns only — it proves history shrinks, not that a tool-heavy turn recovers.
- **T3.** No multi-process writer test. `store_test.go:379` opens one writer + one reader; the `mtclaw cron run` / `mtclaw prompt`-alongside-gateway case opens **two writers** on one file, where `MaxOpenConns(1)` provides no serialization and only `busy_timeout(5000)` stands between them.
- **T4.** `prompt_test.go` asserts tool lines and the missing-file warning but never the documented section **order** (`prompt.go:15-18` states a fixed order as a contract).
- **T5.** `classify` table has no 404/422 case (M2), and no case for `*openaisdk.Error` with `Code: ""` (M3).

---

## Keep / Refactor / Rewrite

| Package | Verdict | Reason |
|---|---|---|
| `internal/agent` | **Refactor (surgical, ~30 LOC)** | Shape is right — turn buffering with a single atomic flush, `HardTrim` as the one trimming primitive, `ToolRunner`/`Progress` as narrow local interfaces. No rewrite justified. Three targeted changes: C1 (dispatch on `len(ToolCalls)`), H1 (trim the buffer on context-length retry), H2 (bound the cancellation flush). M4/M5 are one-liners. `events.go` is 52 lines with no waste — keep as is. |
| `internal/provider` | **Keep** | 86 lines of types + 103 of taxonomy carrying the entire agent/backend boundary. Only edits: JSON tags on `ToolCall` (M1), delete the dead `net.Error` branch (Low). |
| `internal/provider/openai` | **Keep** | Genuinely thin adapter; the SDK import really is confined here (verified). `toSDKMessage`'s hand-rolled assistant reconstruction is necessary, not over-engineering — the SDK's `ToParam` cannot consume our stored type. Fix M2/M3 inside `errors.go`. |
| `internal/provider/mock` | **Keep** | 92 lines, mutex-correct, `Requests()` returns a copy, exhausted-steps is an error not a panic. Exactly right for its job. |
| `internal/store` (interfaces) | **Keep, tighten 2 contracts** | Interface-per-table is the right shape. Tighten: `ApprovalStore.Create` must reject a zero `ExpiresAt` (M6); document the no-nesting invariant (M7). Add tests (M8). |
| `internal/store/sqlite` | **Keep** | The hard parts are right and empirically verified: `_txlock=immediate` + `MAX(seq)`-then-insert in one tx, pragmas applied both via DSN and explicitly, migrations each in their own tx with a transactional `user_version` bump, `SQLITE_READONLY_RECOVERY` fallback. Only the duplicate index and the `updated_at` gap are worth touching. |

Nothing in this slice warrants a rewrite. The abstraction count is proportionate to the problem, there is no parallel reimplementation of an existing utility, no `any` widening, no catch-and-swallow, and the comments explain *why* rather than restating the code.

---

## Unresolved questions

1. **C1 breadth:** is `openai.base_url` actually expected to point at non-OpenAI backends in practice, or is that knob only for Azure/proxy passthrough? The answer changes C1's priority from Critical (third-party backends routinely mismatch `finish_reason`) to High (api.openai.com alone is well-behaved today). The fix is cheap either way.
2. **H1 elision policy:** when the buffer must shrink mid-turn, should an elided tool result keep its `tool_call_id` with placeholder content (preserves the pairing invariant, model sees "[elided]"), or should the whole assistant/tool group be dropped? The former is safer; the latter is closer to what the model would have seen on a fresh turn. Product call.
3. **M3:** does the message-substring fallback for `ErrContextLength` count as reversing the verified "never match on message text" decision, or as extending it to a case that decision did not cover (`Code == ""`)? I read it as the latter — the original rationale was about OpenAI *rewording* its message, which does not apply when there is no code to match at all — but it is the author's call.
4. **M7 / T3:** is `mtclaw prompt` supposed to refuse to run while the gateway holds the instance lock (`internal/gateway/gateway.go:58-62`)? `cron run` checks `Held`; I did not verify `prompt` does. If it does not, two write handles on one DB file is a supported configuration and deserves the T3 test.
5. Should there be a repair path for an already-poisoned history (drop any assistant `tool_calls` whose results are missing at `loadHistory` time), or is "it ages out of the window after N turns" an acceptable recovery? A ~15-line sanitizer in `history.go` would make C1 non-fatal even if a future provider reintroduces the mismatch.
