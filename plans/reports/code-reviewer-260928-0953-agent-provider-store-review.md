# Third-round review: internal/agent, internal/provider, internal/store

Date: 2026-09-28. Branch `refactor/260928-full-review` @ cd7806d. Read-only review; no source files edited.
Probes ran in a scratchpad copy of the repo, not in the working tree.

## Scope

- Files: `internal/agent/{loop,history,prompt,events}.go`, `internal/provider/{provider,errors}.go`,
  `internal/provider/openai/{client,chat,errors}.go`, `internal/provider/mock/mock.go`,
  `internal/store/{store,types}.go`, `internal/store/sqlite/*.go`, `migrations/001_init.sql`, plus tests.
- About 5,000 LOC (about 2,300 non-test).
- Skipped on purpose: items fixed in rounds 1 and 2 (orphan repair, WAL chmod, migration 002, 413/4xx mapping,
  prompt-file bounds, elision no longer persisted) and the accepted product decisions listed in the task.

## Gates

| Check | Result |
|---|---|
| `go vet` (slice) | clean |
| `go test -race -count=1 -cover` (slice) | all ok |
| Coverage | agent 92.4%, store 93.8%, store/sqlite 76.2%, provider/openai 59.8%, provider 0% (no tests), mock 100% |

## Verdict

The two earlier rounds closed the history and persistence bugs they went after. This round found five bugs that
reach production. They sit in two places the earlier rounds did not probe: how the provider layer behaves against
real HTTP, and what the loop does *after* its context-length retry. All five reproduce. Four matter for the default
setup. The fifth (M1) only affects the non-OpenAI backends that round 2 claimed to support.

---

## Bugs

### High

**H1. The OpenAI HTTP timeout is reported as a user cancel, so a slow or hung model turn gets no reply at all**
`internal/provider/openai/errors.go:36`, `internal/provider/errors.go:187`, consumed at `internal/gateway/dispatch.go:450`.

`openai.New` sets `http.Client{Timeout: openai.timeout}`. Go's client-timeout error satisfies
`errors.Is(err, context.DeadlineExceeded)`, so `classify` maps it to `ErrCanceled`. Probe: a server that sleeps
longer than the timeout gives `kind=canceled hits=4 elapsed=3.4s err=... context deadline exceeded (Client.Timeout
exceeded while awaiting headers)`. The SDK retried it 3 times and then reported it as a cancel.

Failure scenario: a long answer or a stalled backend runs past 120s. The loop sends it through
`abortForCancellation`. The gateway treats `ErrCanceled` as "whoever cancelled already told the user", so it sends
nothing and logs no error. The Telegram user waits up to 4 x 120s and then gets silence. A cron job gets the
misleading text "scheduled job timed out after <job.timeout>".

Fix: make `ctx` the only source of truth for cancellation. Change to `classify(ctx, err)`: return `ErrCanceled` only
when `ctx.Err() != nil`. Any other `DeadlineExceeded` / `net.Error` timeout becomes `ErrTransient`. Do the same in
the loop: the checks at `loop.go:157` and `loop.go:225` should test `ctx.Err() != nil`, not the error's kind. A
tool that hits its own internal deadline is not a turn cancel either. Add an `httptest` test with a slow handler.

**H2. After one context-length retry, the model never sees any tool output for the rest of the turn**
`internal/agent/loop.go:142-145`.

`contextRetried` stays true. `elideBufferedToolResults(buffer)` rebuilds every iteration and replaces *every*
tool-role message, including results produced after the retry. Probe: script = `ErrContextLength` -> one tool call
-> final answer. The request after the tool ran carried `"[tool output elided to fit the context window]"` instead
of `"THE REAL ANSWER"`.

Failure scenario: a session whose history hits the context limit (see M3; with default config this is routine
once a session is tool-heavy). The model calls a tool, sees a placeholder, and calls it again, up to
`max_iterations` (default 20). Every call really runs: exec side effects, approval prompts in Telegram, web
fetches. The turn then ends with the iteration-cap text.

Fix: record `elideUpTo := len(buffer)` when the retry fires, and elide only `buffer[:elideUpTo]`. Better still,
elide only entries above a size threshold. Add a loop test with the probe's shape.

**H3. The default `agent.temperature: 0.7` is sent on every request, and current OpenAI reasoning models reject it**
`internal/agent/loop.go:150` (always `&l.cfg.Agent.Temperature`), `internal/provider/openai/chat.go:72-73`,
default in `internal/config/defaults.go:18`.

gpt-5, o3 and o4-mini return 400 `Unsupported value: 'temperature' does not support 0.7 with this model. Only the
default (1) value is supported.` (documented widely; see Sources). The project's own original plan uses
`model: gpt-5`. With default config and one of these models, every turn is `ErrBadRequest`. The user sees the
generic turn-error reply, and nothing tells them which setting to change.

Fix: make temperature optional. Use `*float64` in config (or "unset = omit") with no default, and send it only when
the user set it. Loop: `Temperature: l.cfg.Agent.Temperature` (already a pointer). The config and docs change is
outside this slice, so the lead has to coordinate it. This changes a documented default, so it is a user decision
(options: omit by default / keep 0.7 and document the reasoning-model incompatibility / add a model-family
heuristic, which is not recommended).

### Medium

**M1. The round-2 "message fallback for non-OpenAI backends" never fires for vLLM or llama.cpp**
`internal/provider/openai/errors.go:91-102`.

These backends send a *numeric* `"code": 400`. openai-go decodes that into `Error.Code == "400"`, so
`apiErr.Code != ""` returns false before the message is checked. Probe against a real SDK round trip over
`httptest`:

```
vllm-wrapped: kind=bad_request code="400" msg="This model's maximum context length is 4096 tokens."
llamacpp:     kind=bad_request code="400" msg="the request exceeds the available context size, try increasing it"
vllm (unwrapped body): kind=bad_request code="" msg="" raw=""   (SDK parses nothing)
```

The unit test `"400 context length with no code"` builds a hand-made `openaisdk.Error{Code: ""}` that no real
backend produces, so it passes while production fails.

Fix: treat `Code` as authoritative only when it equals `context_length_exceeded`. Otherwise always check the
message. Add `"context size"` and `"context window"`. `"maximum context length"` is already covered by
`"context length"`, so drop it. Replace the synthetic table rows with `httptest`-served real bodies for OpenAI,
vLLM and llama.cpp.

**M2. A turn that completed can be lost, because three of the four flush sites use the caller's cancellable ctx**
`internal/agent/loop.go:229, 246, 269` vs `:352`.

Only `abortForCancellation` flushes on `context.WithoutCancel`. If ctx is cancelled after `Complete` returns but
before or during `Append`, the final flush fails with `context canceled`. That can be a shutdown drain, `/stop`,
or a cron `job.timeout` expiring. The user still gets the reply (the gateway sends `Text` when `Err` is not a
`provider.Error`), but the turn is gone from history. The next turn then has no record of an answer the user saw,
or of tool side effects that ran.

Fix, which also simplifies: have `flush` itself always use
`context.WithTimeout(context.WithoutCancel(ctx), flushTimeout)`, and delete the special-casing in
`abortForCancellation`.

**M3. The context-length retry is not remembered, so every turn in a large session costs a failed request first**
`internal/agent/loop.go:279-294`, `:161-171`.

`loadHistory` rebuilds 40 turns (up to 320 rows, no byte budget) on every turn. Once a tool-heavy session is over
the model's window, *every* turn sends an oversized request, gets a 400, and retries with 4 turns. Cost: double
latency, and on backends that bill prompt tokens for rejected requests, double cost. It also feeds H2 on every
turn.

Fix: add a byte budget for history in `HardTrim`/`loadHistory` (for example, drop whole oldest turns until total
content length is under a configurable `agent.max_history_bytes`). That does not need to know the model's window,
and it makes the retry rare instead of routine.

**M4. Backfill after an aborted tool uses a turn-wide id set, so a reused call id is persisted as an orphan**
`internal/agent/loop.go:319-338`.

`answered` collects tool ids from the *whole* buffer. Probe: iteration 1 = `[call_0]` answered; iteration 2 =
`[call_1, call_0]` where `call_1` returns a Go error. The persisted turn has
`assistant[call_1, call_0]`, `tool(call_1)` and no tool row for the second `call_0`. The read-time repair hides it
on the next load, but the stored transcript is inconsistent, which is the exact state the backfill exists to
prevent. Round 2 fixed this same bug class in `history.go` and missed it here.

Fix (and simplification): backfill by position. `for i, call := range resp.Message.ToolCalls { ... on error:
backfill resp.Message.ToolCalls[i+1:] }`. No map, and no scan of the buffer.

**M5. Provider-error exit drops tool activity that already ran, and records zero usage**
`internal/agent/loop.go:173-184`.

The comment says "deliberately not persisted, per the ... spec", but the other two abort paths (tool Go error and
cancellation) persist the whole buffer. After backfill the buffer is always pairing-consistent between iterations,
so persisting it is safe. Scenario: iteration 1 runs `exec git push` (approved), then iteration 2 gets a 5xx after
retries. The store records only the user message. The next turn's model does not know the push happened. Session
token totals also undercount what was billed (`provider.Usage{}` is passed while `Result.Usage` reports the real
figure).

This is a spec decision, so options are listed, not a fix: (a) flush `buffer` and `totalUsage` like the other
aborts (recommended; it also merges three exit paths); (b) keep as is and write the trade-off down in
`docs/architecture.md`.

### Low

- **L1. A reused call id gives tool rows the wrong `tool_name`.** `loop.go:120, 201, 395`. `toolNames` is keyed by
  id for the whole turn. Probe: `call_0=exec`, then `call_0=fs_read`, persists both tool rows as `fs_read`. Fixed
  by refactor R1.
- **L2. The doctor probe fails for reasoning models and calls it "unreachable".** `openai/chat.go:52` sends
  `max_tokens: 1` (probe body verified: `{"messages":[...],"model":"gpt-5","max_tokens":1}`). gpt-5 and the
  o-series reject `max_tokens` (they require `max_completion_tokens`), so `doctor` prints "OpenAI endpoint
  unreachable" for a working setup. Fix: use `MaxCompletionTokens`, or drop the cap, or send a tools-free request
  and accept any 2xx. The doc comment at `chat.go:36-37` ("wiring it up is deliberately out of scope for this
  phase") is also stale: doctor calls it.
- **L3. Session list order has no tie-breaker.** `sqlite/sessions.go:53`: `ORDER BY updated_at DESC` with
  millisecond timestamps. Add `, id DESC` (ids sort by time). `TestSessions_List_OrderedByUpdatedAtDesc`
  (`store_test.go:63`) still depends on a 2 ms sleep. Plant `updated_at` the way the Append test next to it
  already does.
- **L4. `approvals.session_id` has no index.** It is an FK child with `ON DELETE CASCADE`, so `sessions rm` and
  cascades scan the whole approvals table, which is never pruned. `CREATE INDEX ... ON approvals(session_id)` in a
  new migration (read-only opens tolerate a behind schema only if you keep that path working; see the round-2
  lesson). Low: the tables are small in practice.
- **L5. Stale schema comment, plus a note about it.** `store/store.go:101-102` says the SQL comment "needs the
  same update", and `migrations/001_init.sql:65` still says `ok|error|skipped`. Editing a comment in an
  already-applied migration is harmless: migrations are keyed by version, not checksum. Fix the SQL comment and
  delete the note.
- **L6. `writePromptFiles` logs a dynamic message.** `prompt.go:93`: `log.Warn(err.Error(), "path", p)` puts the
  error string in the message slot, which breaks message-keyed log filtering. Use
  `log.Warn("system prompt file skipped", "path", p, "error", err)`.
- **L7. Plan and phase references in code comments violate the repo rule.** `loop.go:100, 177`, `prompt.go:17`,
  `openai/client.go:61, 88`, `openai/chat.go:36-37`, `mock/mock.go:2`, and in tests `loop_test.go:197, 460`,
  `store_test.go:146, 295, 423`, `history_test.go:76`, `mock_test.go:16` ("H1 regression test", "M4", "phase 4
  spec"). Replace each with the invariant it stands for.

---

## Refactors (each clearly reduces complexity)

**R1. Delete the `toolNames` map; work out the tool name at flush time.**
The map is threaded through `Run`, `backfillAbortedToolCalls`, `abortForCancellation` and `flush`. `flush` can
track "the last assistant message's calls" while it walks the buffer, and name each tool row from that run, the
same way `repairOrphanedToolCalls` scopes ids.
- Benefit: removes one parameter from 3 signatures and 5 map writes, and fixes L1.
- Risk: low. Covered by existing loop tests that assert `ToolName`.

**R2. Positional backfill** (fixes M4). `backfillAbortedToolCalls` shrinks to about 6 lines with no map. It could
be inlined.
- Risk: none beyond M4's new test.

**R3. One exit helper for the four flush-and-return blocks in `Run`.**
`Run` is 170 lines. Four places build a Result and flush, and each handles a flush failure differently: two log,
two return it as `Err`. Add `l.finish(ctx, sessionID, buffer, usage, res Result) Result`: flush on a detached,
bounded ctx (M2), log the flush error, and set `res.Err` only if it is nil.
- Benefit: `Run` loses about 25 lines, one flush-error policy, and M2 is fixed in one place.
- Risk: low. Existing tests cover every exit.

**R4. Remove the dead summary plumbing.**
`SessionStore.SetSummary` has no caller (`grep`: interface and impl only). `Session.Summary` is "written by nothing
in v1". The loop carries `summary` through `Run` (`:122, :169`) and `assembleMessages` (`:303`), and
`docs/architecture.md` lists conversation summarization under "deliberately did not build". Delete `SetSummary`
(interface and impl), the loop variable, and the `assembleMessages` parameter. Keep the column.
- Benefit: one fewer interface method for every fake to implement, and less loop state.
- Risk: none. `Session.Title` and `Session.Model` are also never written; `sessions list` prints an
  always-empty Title column. Either drop them from the struct and CLI, or leave them. That is the owner's call.

**R5. Replace `Turn` and `SegmentTurns` with one backward scan in `HardTrim`.**
Both are exported but only used by `HardTrim`, and `Turn` is a struct around one slice. Walk from the end,
counting user messages, and slice from the `maxTurns`-th one. That also makes the first `dropLeadingNonUser`
unnecessary. Then run the repair and `dropLeadingNonUser` as now.
- Benefit: about 35 fewer lines, no per-turn allocation, one pass.
- Risk: `history_test.go` has two `SegmentTurns` tests to rewrite as `HardTrim` cases. The property tests stay.

**R6. Simplify `classify`.**
Once H1 passes `ctx`: 404/422 and 400 both give `ErrBadRequest`, and 408/409/425 and `default` both give
`ErrTransient`. Collapse them to 4 cases. The inline ctx check duplicates `provider.Classify`; keep only the
ctx-based one. Also, no consumer reads `ErrAuth`, `ErrRateLimit`, `ErrBadRequest` or `ErrTransient`: the loop and
gateway only branch on `ErrCanceled` and `ErrContextLength`, and `Error()` returns just `Msg`, so the kind and
status never reach a log. Either include them in `Error()` (`"openai: auth (401): ..."`), which is cheap and gives
the taxonomy a consumer, or stop distinguishing them.
- Risk: none. The table tests still pin the kinds.

**R7. One client constructor.**
`New` and `NewWithAPIKey` duplicate the option-building code (`client.go:32-57` vs `66-85`). Move it into
`newClient(apiKey, baseURL string, timeout time.Duration, maxRetries int)` and have both call it.
- Benefit: the H1 and timeout fixes happen in one place.
- Risk: none.

**R8. Tighten the loop's comments.**
About 40% of `loop.go` is comments, and several repeat the same point (the elision rationale appears three times:
`:34-41`, `:138-141`/`:165-168`, `:360-366`). Keep one per invariant. This is not cosmetic: the duplicated comments
are why H2 passed review, because each copy says "buffer is never mutated" and none says "and later results are
elided too".

**Not recommended:** splitting `sqlite` or `agent` into more files or packages. The sizes are fine (largest non-test
file is 410 lines), and the store layout (one file per table) is already clear. `sqlite.DB.ReadOnly` is unused
outside tests, but it is a cheap diagnostic, so leave it.

---

## Test gaps on real behavior

1. `provider/openai`: no `httptest`-based test of `Complete` / `Probe` / `ListModels`. The classifier is tested
   only with hand-built `openaisdk.Error` values, which is how M1 and H1 shipped. Add a fake server covering
   status/body shapes (OpenAI, vLLM, llama.cpp), a slow handler (H1), and the request body sent (temperature
   omission, H3; probe token param, L2).
2. `agent`: no test of any iteration *after* the context-length retry (H2), of reused ids across iterations
   (M4, L1), or of cancellation landing between `Complete` and flush (M2).
3. `provider` package: 0% direct coverage. `Classify`'s `*Error` passthrough and `Error()` fallbacks are only
   covered indirectly.

## Plan follow-ups

No plan file was given. Suggested order: H1 -> H2 -> M2 (via R3) -> M4 (via R2) -> M1 -> H3 (needs a user
decision) -> M3 -> M5 (needs a user decision) -> R1, R4-R8 -> Lows.

## Unresolved questions

1. H3: omit `temperature` by default (a behavior change to a documented default), or keep 0.7 and document that
   reasoning models need `temperature: 1`?
2. M5: should a provider-error abort persist the tool activity that already ran, like the other two abort paths?
   Today's behavior cites the original spec.
3. M3: is a new config key (`agent.max_history_bytes` or similar) acceptable, or should the budget be a fixed
   internal constant?

## Sources

- [GPT-5 models - Temperature (OpenAI Developer Community)](https://community.openai.com/t/gpt-5-models-temperature/1337957)
- [litellm #13781: GPT-5 does not support temperature](https://github.com/BerriAI/litellm/issues/13781)
- [Why You Can't Set Temperature on GPT-5/o3](https://hippocampus-garden.com/llm_temperature/)
