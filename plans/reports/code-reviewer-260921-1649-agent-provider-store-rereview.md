# Re-review: internal/agent, internal/provider, internal/store (post-fix-pass)

Scope: uncommitted diff over `7927b6d` in `internal/agent/**`, `internal/provider/**`,
`internal/store/**` (18 modified files, 2 new: `internal/store/types_test.go`,
`internal/store/sqlite/migrations/002_drop_redundant_message_index.sql`).
Advisory only; no source modified.

## Gates

- `gofmt -l internal/agent internal/provider internal/store` — empty.
- `go vet ./internal/agent/... ./internal/provider/... ./internal/store/...` — clean.
- `go build ./...` — clean (the `internal/tools` build break the implementer reported is gone).
- `go test -race -count=3 ./internal/agent/... ./internal/provider/... ./internal/store/...` — all green.
- Coverage: agent 92.0%, store 93.8%, store/sqlite 75.8%, provider/openai 58.3%, provider 0.0% (no test files).

Findings below were reproduced with throwaway probe tests run in a **copy** of the repo
(`$scratchpad/mtclaw-probe`), not in the working tree.

---

## High

### H1. Migration 002 makes every existing database unopenable read-only until a writer runs — `mtclaw sessions list` breaks on upgrade

`internal/store/sqlite/migrations/002_drop_redundant_message_index.sql` bumps the latest
migration to 2. `internal/store/sqlite/open.go:239` (pre-existing, previously unreachable):

```go
if !apply {
    return fmt.Errorf("database schema version %d is behind the latest migration %d; open it for writing (e.g. run the gateway) once to initialize it", current, latest)
}
```

Until this pass there was exactly one migration, so `current == latest` always held for any
existing file and the `!apply` branch never fired. Now every database written by the previous
binary sits at `user_version = 1`.

Reproduced (probe, applying only `001` + `PRAGMA user_version = 1`, then `Open(ctx, path, true)`):

```
READ-ONLY OPEN OF v1 DB FAILED: database schema version 1 is behind the latest migration 2;
open it for writing (e.g. run the gateway) once to initialize it
```

Affected commands, all of which open read-only (`internal/cli/sessions_cmd.go:34,70`,
`internal/cli/cron_cmd.go:48`, `internal/cli/approvals_cmd.go:39`): `sessions list`,
`sessions show`, `cron list`, `approvals list`. They fail on the first run of the new binary
and keep failing until a writer (gateway, `prompt`, `sessions rm`, `cron run`) opens the file.
`doctor` is unaffected — `internal/cli/doctor_checks.go:120-122` already falls back to a
write open.

Cost/benefit is poor: the migration is a pure micro-optimization. Its premise is correct —
I confirmed with `EXPLAIN QUERY PLAN` after the drop that both message queries use the
UNIQUE constraint's own index:

```
SELECT MAX(seq) FROM messages WHERE session_id = ?              => SEARCH messages USING COVERING INDEX sqlite_autoindex_messages_1 (session_id=?)
SELECT ... WHERE session_id = ? ORDER BY seq DESC LIMIT ?       => SEARCH messages USING COVERING INDEX sqlite_autoindex_messages_1 (session_id=?)
```

— but saving one B-tree write per insert does not pay for breaking the upgrade path.

Fix, cheapest first:
1. Drop migration 002 (revert to a single migration; the redundant index costs one index
   write per message row and nothing else), or
2. make the read-only path tolerant of a behind-schema database when reads do not depend on
   the pending migration, or
3. have `state.openStore` fall back to a write open like `checkDatabase` does.

Option 1 or 2 is a lead/owner decision, not mine — flagging the trade-off rather than picking.

### H2. The `chmod 0600` fix does not cover `-wal`/`-shm`, which hold the most recent conversation content in cleartext at 0644

`internal/store/sqlite/open.go:105`:

```go
_ = os.Chmod(path, 0o600)
```

Only the main database file. Reproduced (probe: open writer, `Ensure` a session, `Append`
a message whose content is `"secret payload"`, then stat + grep every file in the directory):

```
FILE p.db      mode=-rw-------  size=4096
FILE p.db-shm  mode=-rw-r--r--  size=32768
FILE p.db-wal  mode=-rw-r--r--  size=98912
DIR            mode=-rwxr-xr-x
p.db     contains secret payload: false
p.db-wal contains secret payload: true
```

Under WAL the freshest messages live in `-wal` until a checkpoint, so the file that is
actually world-readable is the one holding today's conversation and exec output. The
`MkdirAll(..., 0o700)` change at `open.go:62,70` does not help: `MkdirAll` never chmods an
*existing* directory, and the storage directory normally already exists (created by a prior
version, by the packaging, or by `config`'s `ensureDirCreatable`).

`internal/store/sqlite/migrate_test.go:81` (`TestOpen_WriterCreatesFileWithOwnerOnlyPermissions`)
asserts only `path`, so it passes while the leak stands.

Fix: chmod `path+"-wal"` and `path+"-shm"` on the same best-effort line (they are created by
the first write, so do it lazily or accept a window), and/or chmod the storage directory to
`0700` when we own it. Extend the permission test to assert all three files after one
`Append`.

### H3. `repairOrphanedToolCalls` keys on a global `answered` set, so it is not order-aware and does not repair the two shapes that matter most

`internal/agent/history.go:116,128`:

```go
if answered[tc.ID] {        // keeps an assistant tool_call ...
if answered[m.ToolCallID] { // ... and a tool row ...
```

`answered` is a set of ids, populated in a first pass over the whole slice. The second pass
consults it without any positional information, contradicting the doc comment's claim that
"both checks are order-aware". Two shapes survive the repair (reproduced with probes; each
output was fed to the same invariant checker `assertNoOrphans` uses):

**(a) Re-used call id** — several OpenAI-compatible backends (llama.cpp, some vLLM/ollama
builds) emit non-unique ids such as `call_1`. The fix pass itself justifies other changes by
"base_url is a documented, user-configurable knob", so this backend class is in scope:

```
input:  user, assistant[call_1], tool(call_1), assistant"a1", user, assistant[call_1], assistant"a2"
HardTrim(msgs, 0) output: unchanged
=> INVALID: unanswered tool_calls remain: map[call_1:true]
HardTrim(msgs, 1) output: user"u2", assistant[call_1], assistant"a2"
=> INVALID: unanswered tool_calls remain: map[call_1:true]
```

Turn 1 answered `call_1`, so turn 2's orphan is kept. The session stays permanently
400-poisoned — exactly the state the function claims to self-heal.

**(b) Tool row preceding its declaration**, with an id that is answered later:

```
input: user, tool(call_1)"stray", assistant[call_1], tool(call_1)"r1", assistant"a1"
output: unchanged
=> INVALID: tool message at 1 id="call_1" has no preceding declaration
```

The property test does not catch either: `genHistory` (`history_test.go:133`) builds globally
unique ids `t%d-r%d-c%d`, and `poisonHistory` (`history_test.go:183`) injects stray ids as
`fmt.Sprintf("orphan-%d", rng.Int())` — never a duplicate of a real one.

Fix: make the second pass genuinely positional. One walk, carrying a map of ids declared by
an assistant message *seen so far* and a per-id count of outstanding answers (decrement on a
matching tool row), plus a pre-pass that records, per assistant-message index, which of its
calls are answered by a tool row **after that index and before the next declaration of the
same id**. Then add a `poisonHistory` shape that re-uses an existing call id and one that
moves a legitimate tool row before its assistant message.

Note this is an incomplete fix, not a regression: pre-pass behaviour was equally broken.

---

## Medium

### M1. The repair runs *before* the turn trim, so the trim can re-orphan what the repair just cleaned

`internal/agent/history.go:50-51`:

```go
msgs = dropLeadingNonUser(msgs)
msgs = repairOrphanedToolCalls(msgs)
```

`SegmentTurns` cuts at **every** `RoleUser` message, so a user message sitting between an
assistant `tool_calls` row and its tool results splits the pair, and nothing re-checks the
result. Probe:

```
input: user"u1", assistant[c1], user"interrupting", tool(c1), assistant"a1"
HardTrim(msgs, 1) => user"interrupting", tool(c1), assistant"a1"
=> INVALID: tool message at 1 id="c1" has no preceding declaration
```

Not reachable from today's writers (a turn is flushed as one atomic `Append`, and
`backfillAbortedToolCalls` keeps partial turns paired), so the ordering is safe only because
of an invariant enforced elsewhere. Since the whole point of this function is to survive a
history that already violates invariants, that is the wrong dependency.

Fix: run the repair on the trimmed output —
`msgs = dropLeadingNonUser(msgs)` → trim → `repairOrphanedToolCalls` → `dropLeadingNonUser`
again. Cheaper too (repairs only the messages actually kept).

### M2. The context-length retry permanently destroys the turn's real tool output, in the store as well as in the request

`internal/agent/loop.go:157` mutates the buffer that is later flushed:

```go
buffer = elideBufferedToolResults(buffer)
```

Consequences a real session hits that it did not before:

- The persisted transcript keeps `[tool output elided to fit the context window]` instead of
  what the tool actually returned — asserted as intended by
  `loop_test.go:531` ("the persisted tool row must be the elided version"), so this is a
  deliberate choice; flagging the cost, not reversing it.
- The model is asked to finish the turn from placeholders. Realistic outcome is that it
  re-issues the same tool call; the new (large) result appends to the buffer, overflows
  again, and because `contextRetried` is already true the second `ErrContextLength` takes the
  error path at `loop.go:173`, which persists only the user message. Net effect: the tool ran
  twice, its output is gone, and nothing but the user's text is stored.
- Elision is unconditional: a 200-byte tool result is elided even when the overflow came
  entirely from history.

Fix options (pick one, owner's call):
1. Elide only for the request: build a separate slice in `assembleMessages`, keep `buffer`
   intact for the flush.
2. Elide only entries above a threshold (e.g. > 4 KiB), largest first, stopping once enough
   has been shed.
3. Keep as is but truncate rather than replace (head+tail of the original), so the model and
   the transcript retain something usable.

### M3. The system-prompt size cap does not bound a non-regular file

`internal/agent/prompt.go:90-100`:

```go
info, err := os.Stat(p)
...
if info.Size() > maxSystemPromptFileSize {
...
content, err := os.ReadFile(p)
```

`Stat` reports size 0 for character devices, FIFOs and `/proc` entries, so the cap passes and
`ReadFile` proceeds: `system_prompt_files: ["/dev/urandom"]` reads until OOM, a FIFO blocks
the turn forever, `/dev/stdin` blocks. The cap's own comment says its purpose is to stop
"a user pointing the setting at a large generated file" from making the session
"permanently un-completable with no diagnostic" — the unbounded-read case is the worse
version of exactly that and is not covered. There is also a benign TOCTOU between `Stat` and
`ReadFile`.

Fix: `if !info.Mode().IsRegular() { warn; continue }`, and read through
`io.LimitReader(f, maxSystemPromptFileSize+1)` on an already-open handle so the check and the
read see the same file.

### M4. Blanket 4xx→`ErrBadRequest` also captures 413/409/425, and 413 is how several proxies report a context overflow

`internal/provider/openai/errors.go:49-56`:

```go
case status == 400 && isContextLengthError(apiErr):
    return &provider.Error{Kind: provider.ErrContextLength, ...}
case status == 408:
    ...ErrTransient...
case status >= 400 && status < 500:
    ...ErrBadRequest...
```

Before the pass these fell through `default` to `ErrTransient`. The reclassification is right
for 404/422 but sweeps in 413 Payload Too Large, 409 Conflict and 425 Too Early. 413 is the
status nginx/`llama.cpp`-style front ends commonly return when the prompt exceeds the
configured limit — and `isContextLengthError` is consulted **only** under `status == 400`, so
that case now lands as a permanent `ErrBadRequest` and the loop's trim-and-retry recovery
never fires. That undercuts M3's own stated goal (make the retry work against non-OpenAI
backends).

Fix: check `isContextLengthError` for 413 as well as 400, and keep 409/425 transient:

```go
case (status == 400 || status == 413) && isContextLengthError(apiErr):
case status == 408 || status == 409 || status == 425:
```

Also stale after this change: `internal/provider/errors.go:26` still documents
`ErrBadRequest` as "any other 400", and `errors.go:22` documents `ErrContextLength` as "a 400
whose error code is context_length_exceeded" — the message fallback is now a second path in.

### M5. `final.ToolCalls = nil` is unreachable, and its comment describes a state the new dispatch makes impossible

`internal/agent/loop.go:259`:

```go
final.ToolCalls = nil
```

It sits inside the branch reached only when `len(resp.Message.ToolCalls) > 0` is false
(`loop.go:186`), so `final.ToolCalls` is already empty. The eight-line comment above it
explains a scenario ("if the model attached them under a non-`tool_calls` FinishReason") that
the payload-based dispatch on line 186 already routes elsewhere. A future reader will believe
there is a defended failure mode here.

`loop_test.go:435` (`assert.Empty(t, msgs[3].ToolCalls, "the terminal assistant row must never
carry tool_calls")`) passes with this line deleted — it is asserting the dispatch change, not
this one.

Fix: delete the assignment and reduce the comment to one line on the dispatch condition, or
keep it and say plainly that it is belt-and-braces against a future change to line 186.

---

## Low

- **L1.** `internal/store/sqlite/migrations/002_drop_redundant_message_index.sql:5` —
  `DROP INDEX idx_messages_session_seq;` with no `IF EXISTS`. Any database where the index
  was already dropped by hand becomes permanently unopenable (`applyMigration` rolls back and
  `Open` returns the error). One word fixes it.
- **L2. Stale contracts.** `internal/store/types.go:128` still says
  `Status string // ok | error | skipped` though `started` (scheduler) and now `interrupted`
  (`cron_runs.go:57`) are both written. `internal/store/store.go:100` says `Finish` "moves a
  'started' row to a terminal status" but the SQL is `WHERE id = ?` with no status guard, so
  it will happily move an `interrupted` row back to `ok`.
- **L3.** `approvalStore.Create`'s new precondition (`internal/store/sqlite/approvals.go:20-25`,
  `"create approval: ExpiresAt must be set"`) is a contract change documented only in the
  implementation. Put it on `ApprovalStore.Create` in `internal/store/store.go` so a second
  implementation inherits it. (Verified the only production caller,
  `internal/channel/telegram/approver.go:117`, always sets `ExpiresAt` — no break.)
- **L4.** `internal/agent/loop.go:345`: the 5s budget covers *both* store calls inside `flush`
  (`Append` **and** `AddUsage`), and the comment's justification ("matches the DSN's
  busy_timeout(5000)") treats pool-wait and busy-timeout as the same budget when they are
  additive — a flush that waits 4s for the single connection then hits a busy writer will
  exceed 5s and lose the turn with only a log line. Consider 10s, or applying the budget per
  store call.
- **L5.** With the payload dispatch, a response carrying `FinishReason: "length"` *and* a
  partially generated `tool_calls` payload is now dispatched instead of ending the turn
  (`loop.go:186`), and the `[response truncated ...]` notice is never surfaced on that path.
  Truncated argument JSON will not parse, so the tool fails safe — but the user sees a tool
  error instead of "the model's output hit the token limit".
- **L6.** `internal/store/sqlite/store_test.go:80-96` orders sessions using two 2 ms sleeps
  against millisecond-resolution timestamps. Mildly flaky on a loaded CI box; comparing
  `UpdatedAt` values directly would be deterministic.
- **L7. Leftover.** `internal/tools/zzprobe_test.go` (104 lines, untracked) is a scratch probe
  file from an earlier session — outside this slice but it will be swept into the commit.

---

## Test quality of the new tests

Checked each new test by reasoning about what fails if the corresponding fix is reverted.

| Test | Proves behaviour? | Notes |
|---|---|---|
| `TestRun_ToolCallsDispatchOnPayloadRegardlessOfFinishReason` | yes | Fails on revert (`tools.calls` would be 0). |
| `TestRun_EmptyToolCallsWithToolCallsFinishReason_EndsTurnImmediately` | yes | Fails on revert (iteration count). |
| `TestRun_LengthTruncation_NoticeNotPersistedAsModelContent` | yes | Fails on revert (persisted content). |
| `TestRun_ErrContextLength_RetryAlsoElidesBufferedToolResults` | yes | Fails on revert (`len(m.Content) < 100`). |
| `TestHardTrim_RepairsOrphanedToolCallsInPoisonedHistory` | partly | Real property test, but `poisonHistory` only ever injects **fresh unique** ids, so it cannot reach H3(a)/(b). It certifies an invariant the implementation does not actually hold. |
| `TestBuild_OversizePromptFileWarnsAndSkips` | yes | But no non-regular-file case (M3). |
| `TestOpen_WriterCreatesFileWithOwnerOnlyPermissions` | partly | Asserts only the main db; `-wal` at 0644 passes (H2). |
| `TestApprovals_Create_RejectsZeroExpiresAt` | yes | |
| `TestMessages_Append_BumpsSessionUpdatedAt` | yes | Timing-sensitive (L6). |
| `TestCronRuns_ExpireStarted_*` (3) | yes | Good boundary coverage (terminal rows, cutoff). |
| `internal/store/types_test.go` (5) | yes | The legacy-uppercase decode test is the right test for the JSON-tag change. |
| `errors_test.go` 404 / 422 / empty-code cases | yes | |
| `errors_test.go` `{"net timeout", fakeTimeoutErr{}, ErrTransient}` | **no — now a phantom** | After the `net.Error` branch was deleted from `provider/errors.go`, this case passes for *any* non-context error. It no longer discriminates; either delete it or assert via a table of unrelated error types that all map to `ErrTransient`. |

Missing: no test opens a behind-schema database read-only (that gap is how H1 shipped).

---

## Prior fixes verified / not verified

| ID (prior review) | Claim | Verdict |
|---|---|---|
| C1 dispatch on payload | `len(resp.Message.ToolCalls) > 0` at `loop.go:186` | **Holds** (see L5 for the `length` corner) |
| Q5 orphan repair | order-aware self-heal in `HardTrim` | **Not verified** — H3: not order-aware; re-used ids and pre-declaration tool rows survive. M1: runs before the trim |
| H1 buffer elision | retry shrinks the in-turn buffer | **Holds**, with the cost in M2 |
| H2 bounded cancellation flush | 5s deadline at `loop.go:345` | **Holds**; L4 on the budget's sizing/justification |
| M1 JSON tags on `ToolCall` | tags + legacy decode | **Holds** — round-trip and legacy-uppercase tests are correct; no other JSON consumer of `ToolCall` exists (`openai/chat.go` maps fields explicitly) |
| M2 4xx reclassification | 404/422 no longer transient | **Partly** — correct for 404/422, over-broad for 409/413/425 (M4) |
| M3 message fallback for context length | works against non-OpenAI backends | **Partly** — only under status 400, so the 413 case is still missed (M4) |
| M4 truncation notice not persisted | `resultText` vs `final.Content` | **Holds**, tested |
| M5 prompt file size cap | 256 KiB warn-and-skip | **Partly** — regular files only; non-regular files unbounded (M3) |
| M6 `Create` rejects zero `ExpiresAt` | | **Holds**; L3 on where the contract lives |
| M7 `MaxOpenConns(1)` invariant | comment only | **Holds** as documentation; nothing enforces it |
| M8 store tests | new `types_test.go` | **Holds**, store now 93.8% |
| DB file 0600 | `os.Chmod(path, 0o600)` | **Not verified** — `-wal`/`-shm` still 0644 and hold cleartext history (H2) |
| Migration 002 | "existing migration tests pass unchanged" | **True but insufficient** — read-only opens of v1 databases now fail (H1) |
| `Append` bumps `updated_at` | same tx | **Holds**, tested |
| `NewWithAPIKey` max retries | `config.Default().OpenAI.MaxRetries` | **Holds**; hardcoding the default (not the user's configured value) is acknowledged in the comment |
| provider-error `Result.Usage` | now reports `totalUsage` | **Holds**; note the store still records 0 usage for that turn, so `Result` and the DB now disagree by design |
| `net.Error` branch removal | behaviour-neutral | **Holds** (both paths returned `ErrTransient`); see the phantom test above |
| `ExpireStarted` (added by the cron slice, in these files) | startup sweep | **Holds** — no cross-process race: `mtclaw cron run` writes a terminal row in one `Append` (`cli/cron_cmd.go:204-214`), never a `started` row; L2 on the stale status docs |

---

## Recommended actions

1. Decide H1: drop migration 002, or make the read-only path tolerate a behind-schema database.
2. Fix H2: chmod `-wal`/`-shm` (and/or the storage directory); extend the permission test.
3. Fix H3: rewrite `repairOrphanedToolCalls` to be positional; extend `poisonHistory` with a
   re-used-id shape and a tool-before-declaration shape.
4. Move the repair after the trim (M1).
5. Decide M2's elision trade-off (request-only elision vs. size-thresholded elision).
6. `IsRegular()` + `io.LimitReader` in `writePromptFiles` (M3).
7. Narrow the 4xx sweep and extend `isContextLengthError` to 413 (M4).
8. Delete the dead `final.ToolCalls = nil` or downgrade its comment (M5).
9. Sweep the stale contract comments (L2) and remove `internal/tools/zzprobe_test.go` (L7).

## Metrics

- Type coverage: n/a (Go); `go vet` clean, no `interface{}`/`any` widening introduced.
- Test coverage: agent 92.0%, store 93.8%, store/sqlite 75.8%, provider/openai 58.3%.
- Lint: `gofmt`/`go vet` clean.
- Race: `-race -count=3` green across the slice.

## Unresolved questions

1. Are there deployed MTClaw databases in the wild? If yes, H1 is a shipped upgrade break and
   should block landing; if the project is still pre-release, it is a cheap fix but not urgent.
2. Is the persisted-placeholder behaviour in M2 an accepted product decision (a turn that
   overflowed loses its tool output from the transcript forever), or an implementation
   convenience? The test asserts it, so I treated it as intentional and did not propose
   reverting it unilaterally.
3. Which non-OpenAI backends are actually in scope? H3(a) and M4 only matter for endpoints
   that re-use tool-call ids or report overflow as 413. The fix pass repeatedly cites
   `base_url` as a supported knob, so I assumed they are supported; if `api.openai.com` is the
   only supported backend, both drop to Low and several of the fix pass's own justifications
   also fall away.
