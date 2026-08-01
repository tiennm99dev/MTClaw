---
phase: 3
title: OpenAI Provider — implementation report
date: 2026-08-01
status: completed
---

# Phase 3: OpenAI Provider — Implementation Report

## Status

Completed. `go build ./...`, `CGO_ENABLED=0 go build ./...`, `go vet ./...`,
`gofmt -l .`, and `go test ./...` (phases 1-2 plus this phase) all pass clean.
`go mod tidy` was run after adding the dependency (no drift produced).

## Files

Created:

- `internal/provider/provider.go` (86 lines) — `Role`, `ToolCall`, `Message`, `ToolSpec`, `Request`, `Usage`, `Response`, `Provider`
- `internal/provider/errors.go` (103 lines) — `ErrKind`, `Error`, generic `Classify` (ctx cancellation + `net.Error` timeout fallback)
- `internal/provider/openai/client.go` (55 lines) — `Client`, `New(config.OpenAIConfig)`
- `internal/provider/openai/chat.go` (174 lines) — `Complete`, `Probe`/`ProbeResult`, `toSDK`, `toSDKMessage`, `fromSDK`
- `internal/provider/openai/errors.go` (58 lines) — `classify` (SDK-specific: status + error code, falling back to `provider.Classify`)
- `internal/provider/openai/chat_test.go` (209 lines) — converter round trips, nested-schema tool spec, temperature, empty-choices, usage
- `internal/provider/openai/errors_test.go` (96 lines) — classification table + context-length-vs-other-400 + cancellation-checked-first
- `internal/provider/openai/client_test.go` (28 lines) — missing-API-key error path
- `internal/provider/openai/e2e_test.go` (57 lines) — network-gated (`MTCLAW_E2E=1`) real completion smoke test via `Probe`
- `internal/provider/mock/mock.go` (92 lines) — scripted `Provider` + request recorder
- `internal/provider/mock/mock_test.go` (104 lines) — two-step tool conversation, exhausted-steps error, scripted-error passthrough, canceled-context, default finish reason

Modified:

- `go.mod` / `go.sum` — added `github.com/openai/openai-go/v3 v3.49.0` (pinned exact version) and its transitive `tidwall/*` deps; bumped `golang.org/x/sys` to v0.47.0 via `go mod tidy`.

Not modified: `internal/cli/doctor_cmd.go` does not exist yet (phase 9 owns the `doctor` command; it wasn't created in phases 1-2 either). See "Doctor probe" below.

## Tasks Completed

- [x] Added `github.com/openai/openai-go/v3` at v3.49.0, exact-pinned; confirmed it builds on the installed Go 1.26.4 toolchain (module floor is `go 1.25.0`, already set from phase 1)
- [x] `client.go`: `New(cfg config.OpenAIConfig) (*Client, error)` — `option.WithAPIKey`, `option.WithBaseURL` (only when non-empty), `option.WithMaxRetries(cfg.MaxRetries)`, `option.WithHTTPClient` when `cfg.Timeout.Std() > 0`. Errors on empty key naming the configured (or default `OPENAI_API_KEY`) env var
- [x] `chat.go` → `toSDK`/`toSDKMessage`: all four roles, including assistant-with-tool-calls reconstruction (`ChatCompletionAssistantMessageParam.ToolCalls` built directly from `provider.ToolCall`, not via the SDK's own `.ToParam()`, since our stored representation is `provider.Message`)
- [x] `fromSDK`: empty `Choices` → `provider.Error{Kind: ErrBadRequest}`, not a panic; `ToolCalls[].Function.Arguments` kept as `json.RawMessage`; `Usage`/`FinishReason` copied
- [x] `errors.go` (both provider and openai/errors.go): `errors.Is(context.Canceled)`/`DeadlineExceeded` checked before any `*openai.Error` inspection; `context_length_exceeded` matched on `apiErr.Code`, not message text
- [x] `mock/mock.go`: `New(steps ...Step)`, each `Step` either tool-call or text, plus `Requests()` recorder
- [x] Doctor probe: `(*openai.Client).Probe(ctx, model) ProbeResult` — one minimal completion with `max_tokens: 1`, reports `BaseURL`/`Model`/`Latency`/`Err`, never the key. **No CLI wiring** (see below)

## Tests Status

- Type check / build: **pass** (`go build ./...`, `CGO_ENABLED=0 go build ./...`, `go vet ./...`)
- gofmt: **clean** (`gofmt -l .` → no output)
- Unit tests: **pass**, `go test ./...`:
  - `internal/config` — ok (phase 1, unaffected)
  - `internal/store/sqlite` — ok (phase 2, unaffected)
  - `internal/provider/mock` — ok, 5 tests
  - `internal/provider/openai` — ok, 21 tests (1 E2E test skipped by default)
- `go list -deps` SDK-import check: for every package under `./...` except `internal/provider/openai` itself, `go list -deps <pkg> | grep openai-go` produced no matches. `internal/agent` and `internal/tools` don't exist yet, so the literal phase-file criterion ("compile with no openai-go import") was reinterpreted as the broader, already-stated constraint: **nothing outside `internal/provider/openai` imports the SDK**, verified module-wide.
- Optional E2E (`MTCLAW_E2E=1 OPENAI_API_KEY=... go test ./internal/provider/openai/... -run TestE2E`): not run (no key in this environment); skips cleanly by default.

## SDK API Divergences From the Plan Sketch

Verified against the installed `openai-go/v3@v3.49.0` source (`C:\Users\miti99\go\pkg\mod\github.com\openai\openai-go\v3@v3.49.0`), not just examples. All divergences below are additive/structural, not behavioral surprises:

1. **Assistant reconstruction is manual, not `.ToParam()`.** The plan sketch shows `completion.Choices[0].Message.ToParam()` for feeding an assistant turn back. That only works when you still hold the SDK's own `ChatCompletionMessage` response value. Since our `Message` is the stored representation (store, loop, provider all share it), `toSDKMessage` builds `ChatCompletionAssistantMessageParam.ToolCalls` field-by-field from `provider.ToolCall{ID, Name, Args}` instead. Confirmed correct by round-tripping through real `json.Marshal`/`Unmarshal`, not just by inspection.
2. **Tool call fields are populated directly on the response union**, not only reachable via `AsAny()`/type-switch. `ChatCompletionMessageToolCallUnion` carries `ID`, `Function` (name/arguments), and `Type` as top-level fields regardless of variant, because the SDK's JSON decoder populates all matching tags. `fromSDK` reads `tc.ID`/`tc.Function.Name`/`tc.Function.Arguments` directly — simpler than the union-switch the plan's raw SDK snippet implies, and correct because MTClaw only ever declares function-style tools (no custom tools), so `Type` is always `"function"`.
3. **`shared.ChatModel` is `= string`** (type alias, not a distinct named type with constants like `openai.ChatModelGPT4o` needing conversion) — `Request.Model string` assigns straight through with no cast.
4. **`shared.FunctionParameters` is `= map[string]any`** — `ToolSpec.Schema map[string]any` converts with a plain type conversion, no marshaling round-trip needed at the type level (still exercised through real JSON in the schema test for confidence).
5. **`openai.Error` is `= internal/apierror.Error`**, an exported alias with `Code`, `Message`, `Param`, `Type`, `StatusCode`, `Request`, `Response` fields directly (no need to unwrap a generic API-error envelope). Confirmed via SDK source (`internal/requestconfig/requestconfig.go:657-663`) that on any 4xx/5xx the SDK unmarshals the response body's nested `"error"` object into exactly these fields and returns `&aerr` — i.e., `errors.As(err, &apiErr)` reliably matches.
6. **Context cancellation is returned unwrapped, ahead of the retry loop and ahead of `apierror.Error` construction** (`requestconfig.go:597-598, 624`: `if ctx.Err() != nil { return ctx.Err() }`). This confirms the phase file's ordering requirement (check `errors.Is(context.Canceled)` before inspecting `*openai.Error`) is not just defensive — the SDK genuinely returns bare `context.Canceled`/`context.DeadlineExceeded` in that path, never wrapped in `*openai.Error`.

None of these required deviating from the phase file's *architecture* — only from its abbreviated code sketch, exactly as the phase file itself anticipated ("verified against examples... but trust the installed SDK source").

## Design Decision: Where Classification Lives

The phase file lists a single `internal/provider/errors.go` hosting `Classify(error)`, described as inspecting `*openai.Error`. Taken literally, that would import the SDK into `internal/provider` (the parent package), which contradicts the stated criterion that *nothing outside* `internal/provider/openai` imports the SDK. Resolved by splitting classification:

- `internal/provider/errors.go` — `ErrKind`, `Error`, and a generic `Classify(err error) *Error` that handles `context.Canceled`/`DeadlineExceeded` and `net.Error` timeouts. SDK-agnostic; reusable by any future provider.
- `internal/provider/openai/errors.go` (created; not explicitly named in the phase file's Related Code Files list, which only lists `errors_test.go` for this package — a minor omission, since a test needs a source file to test) — `classify(err error) *provider.Error`, which checks context cancellation first, then inspects `*openai.Error` for status/code, then falls back to `provider.Classify` for anything else (e.g. a raw `net.Error`).

This keeps the SDK import confined as required while still satisfying every line of the classification table test.

## Doctor Probe: CLI Wiring Deferred

Per your instruction, the probe is `(*openai.Client).Probe(ctx, model) ProbeResult` — exported, unit-testable (via the E2E test), returning `BaseURL`/`Model`/`Latency`/`Err` and never the API key. `internal/cli/doctor_cmd.go` does not exist in this repository (phase 1/2 didn't create it either — the `doctor` command itself is phase 9's deliverable per `plan.md`'s repository layout and phase table). No CLI file was created or modified this phase. Phase 9 should call `openai.Client.Probe` directly.

## Unresolved Questions

None blocking. One judgment call worth flagging: `mock.Provider.Complete` records the exact `provider.Request` value it receives (not a deep copy of `Messages`/`Tools` slice contents) — safe for the intended use (a test builds a request, calls `Complete`, then either discards or appends to a fresh slice per turn) but a caller that mutates a `Request.Messages` slice in place after the call would see the mutation reflected in `Requests()`. Not addressed since no current caller does this and doing so defensively would be speculative (YAGNI); flagging so phase 4's agent loop author is aware.

Status: DONE
Summary: OpenAI provider package implemented per spec (interface, converters, error classification, mock, doctor probe as exported helper); all builds/vet/gofmt/tests green including phases 1-2; SDK v3.49.0 pinned; go list -deps confirms SDK import confined to internal/provider/openai.
Concerns/Blockers: None. Doctor probe intentionally left unwired to any CLI command per your instruction (phase 9 dependency); errors.go split across provider/ and provider/openai/ to satisfy the import-confinement constraint, diverging slightly from the phase file's single-file sketch.
