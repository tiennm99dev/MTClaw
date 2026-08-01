# Phase 4 Implementation Report - Agent Loop

Date: 2026-08-01
Plan: `plans/260731-2219-mtclaw-core-system/phase-04-agent-loop.md`

## Status: Completed

## Files Created

- `internal/agent/events.go` (54 lines) - `EventKind`, `Event`, `Progress`.
- `internal/agent/history.go` (52 lines) - `Turn`, `SegmentTurns`, `HardTrim`.
- `internal/agent/prompt.go` (99 lines) - `Build` (system prompt assembly).
- `internal/agent/loop.go` (283 lines) - `ToolRunner`, `Meta`, `Result`, `Loop`, `New`, `Run`.
- `internal/agent/history_test.go`, `prompt_test.go`, `loop_test.go` - unit + property tests.
- `internal/cli/prompt_cmd.go` (117 lines) - `mtclaw prompt "<text>"`.

## Files Modified

- `internal/store/types.go` - added `Message.ToProviderMessage()` and `FromProviderMessage()`, JSON-encoding `ToolCalls` losslessly via `json.Marshal`/`Unmarshal` of `[]provider.ToolCall` directly (no intermediate mirror type). `ToolName` is intentionally left for the caller to set (provider.Message carries no tool name; the loop has the originating `ToolCall.Name` in hand and fills it in after conversion).
- `internal/cli/root.go` - one-line `root.AddCommand(newPromptCmd(s))`. Not in the phase's file-ownership list but required for the command to exist at all; scoped to a single line.

## Key Design Decisions

- `Loop.cfg` is `config.Config` (not just `AgentConfig`, as the interface sketch implied): system prompt assembly needs `Tools.Filesystem.Roots` and `Tools.Exec.Mode` too, and `config` has no cross-package dependents that would make this a layering violation.
- `Meta.Channel/ChatID/ThreadID` come from `store.Session` (loaded once at the top of `Run`), so `Loop.Run`'s signature stays exactly `(ctx, sessionID, userText, onProgress)` per spec, with no separate channel/chatID params.
- Tool-name propagation for `tool`-role rows: a local `map[callID]name` populated while iterating `resp.Message.ToolCalls`, consulted only inside `flush`. Keeps `store.FromProviderMessage` a pure, generic converter.
- Error-path buffering, exactly as specified: provider errors other than `ErrContextLength`/`ErrCanceled` flush **only** the user message (discarding any tool progress already buffered earlier in the same turn); cancellation and tool-Go-error abort both flush the **whole** buffer (it is always internally consistent - exactly one tool row per call id is appended before any abort path returns) so a turn that got partway through tool execution is not silently lost.
- A tool's Go error that wraps `context.Canceled`/`DeadlineExceeded` is routed through the same cancellation path (background-context flush, `ErrCanceled` result) rather than the generic tool-abort path, since it is the same failure mode by another route.
- `ErrContextLength` retry does not consume an iteration-cap slot (decremented back before `continue`): it is recovery, not a new agent step.
- `mtclaw prompt --new` forces a fresh session by `Ensure`-ing a synthetic time-unique thread id, since `SessionStore` has no "always create" method and adding one was out of this phase's file ownership.

## Tasks Completed

- [x] History: `SegmentTurns`, `HardTrim`, cutting only at `role=="user"` boundaries.
- [x] Prompt: `Build` in the specified order (identity, workspace, tool guidance from `Specs()`, exec posture, `system_prompt_files` under source-path headers with missing-file warn-and-skip, conventions incl. `NO_REPLY`).
- [x] Loop skeleton: session load, `Recent(maxHistoryTurns*8)` raw fetch, convert, `HardTrim`.
- [x] Iteration cycle: request assembly (system + summary-if-nonempty + trimmed history + buffer), `Complete`, `tool_calls`/`stop`/`length` handling, per-call tool execution with one buffered tool message per call id always.
- [x] Iteration cap: buffered final assistant message, single flush, cap text returned.
- [x] `NO_REPLY`: exact trimmed match sets `Result.NoReply`; still persisted verbatim.
- [x] Usage: accumulated across iterations, one `AddUsage` at flush.
- [x] Cancellation: `context.WithoutCancel(ctx)` flush, classified `ErrCanceled` returned.
- [x] `prompt_cmd.go`: `channel="cli" chat="local"` session via `Ensure`, `--session`/`--new` flags, progress to stderr, final text to stdout, `NO_REPLY` suppresses stdout output.
- [x] `ToolRunner` wired as zero-spec `noopToolRunner` until phase 5.

## Tests Status

- Type check / build: `CGO_ENABLED=0 go build ./...` - pass.
- `gofmt -l .` - no output (clean).
- `go vet ./...` - pass.
- `go test ./...` - all packages pass, including phases 1-3 (`internal/config`, `internal/store/sqlite`, `internal/provider/mock`, `internal/provider/openai`).
- `internal/agent` tests (19 cases) cover: single-shot no-tool turn; two-tool-call turn with matching ids and second-request tool-result verification; tool error-string keeps loop alive; tool Go-error aborts turn while still persisting one paired tool row; iteration cap enforced at exactly `max_iterations` provider calls with cap notice; `ErrContextLength` one retry with a measurably smaller retried request; second `ErrContextLength` failure returns the error with only the user message persisted; other provider errors (e.g. `ErrAuth`) flush only the user message; cancellation mid-turn (tool completes, then the loop notices `ctx.Done()` before the next Think call) persists the paired partial turn and returns classified `ErrCanceled`; `NO_REPLY` sets the flag and still persists the raw text; unknown session id errors cleanly.
- `history_test.go` includes a property-style test (200 randomized histories, 1-15 turns each with random tool-call rounds) asserting `HardTrim` never starts with a `tool` message and never orphans a `tool_calls` id, for every `maxTurns` in `[0, turnCount+1]`.
- `prompt_test.go` verifies tool lines match registry specs, the no-tools case, and that a missing prompt file logs a warning (captured via a buffered `slog.Logger`) and is omitted from the assembled prompt while a present one is included.
- `go list -deps ./internal/agent` confirms the only intra-module imports are `internal/config`, `internal/provider`, `internal/store` - no channel package, no tool implementation, no `internal/provider/openai`.
- `go mod tidy` produced no diff: no dependency changes.

## Issues / Deviations

- Success criteria "`mtclaw prompt "hello"` completes a turn" and "restarting retains prior context" are implemented and wired (verified the command tree, flags, and session-resolution logic run correctly) but **not exercised against a live OpenAI endpoint** - there is no network access or API key in this environment. Everything downstream of the provider boundary is covered against `provider/mock` instead, per the phase's own test-scoping ("All against `provider/mock`, no network"). Left both boxes unchecked in the phase file with a note; the other six success criteria are checked.
- `internal/cli/root.go` required a one-line edit (`root.AddCommand(newPromptCmd(s))`) to actually expose the new command - not listed in the phase's file-ownership section but unavoidable; no other line in that file was touched.

## Unresolved Questions

None blocking. Worth flagging for phase 5: `ToolRunner.Specs()` is called once per `Loop.Run` invocation (cached into `systemPrompt` and reused per-request `Tools` field) rather than per-iteration, on the assumption the tool registry doesn't change mid-turn; this matches "generated from the registry, never hand-maintained" and should still hold once phase 5 wires a real registry.

Status: DONE
Summary: Phase 4 agent loop implemented per spec (loop, history trim, prompt assembly, events, store<->provider conversions, `mtclaw prompt` CLI); full test suite green including a new property-style trim-invariant test.
Concerns/Blockers: None. Two success-criteria checkboxes left unchecked pending a live OpenAI run (no network in this environment), documented above.
