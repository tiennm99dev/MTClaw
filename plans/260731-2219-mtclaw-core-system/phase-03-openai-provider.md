---
phase: 3
title: "OpenAI Provider"
status: completed
priority: P1
dependencies: [1]
effort: ""
---

# Phase 3: OpenAI Provider

## Overview

Wrap `github.com/openai/openai-go/v3` behind a narrow internal interface so the
agent loop never imports the SDK directly. Handles tool-call round trips, usage
accounting, retries, and error classification.

## Requirements

**Functional**
- One completion call taking our message/tool types and returning our result type.
- Tool calling with JSON-schema parameters.
- Usage (prompt/completion tokens) returned for every call.
- Retry on transient failures; never retry a 400-class request error.
- Errors classified into: auth, rate limit, context length, bad request, transient, canceled.

**Non-functional**
- `internal/agent` must compile without the OpenAI SDK in its import graph.
- Streaming is deliberately **out of scope for v1** — see Architecture.

## Architecture

The SDK's live API surface, verified against
[`examples/chat-completion-tool-calling`](https://github.com/openai/openai-go/blob/main/examples/chat-completion-tool-calling/main.go):

```go
params := openai.ChatCompletionNewParams{
    Messages: []openai.ChatCompletionMessageParamUnion{ openai.UserMessage(q) },
    Tools: []openai.ChatCompletionToolUnionParam{
        openai.ChatCompletionFunctionTool(openai.FunctionDefinitionParam{
            Name:        "get_weather",
            Description: openai.String("…"),
            Parameters:  openai.FunctionParameters{ /* raw JSON schema map */ },
        }),
    },
    Model: openai.ChatModelGPT4o,
}
completion, err := client.Chat.Completions.New(ctx, params)
toolCalls := completion.Choices[0].Message.ToolCalls
// feed the assistant turn back verbatim:
params.Messages = append(params.Messages, completion.Choices[0].Message.ToParam())
params.Messages = append(params.Messages, openai.ToolMessage(result, toolCall.ID))
```

Two facts that shape our types: the assistant turn is appended back via
`.ToParam()` (so we must be able to reconstruct it from our stored representation),
and tool arguments arrive as a **JSON string** in `toolCall.Function.Arguments`.

### Internal interface

```go
package provider

type Role string // "system" | "user" | "assistant" | "tool"

type ToolCall struct {
    ID   string
    Name string
    Args json.RawMessage // raw; the registry validates and unmarshals
}

type Message struct {
    Role       Role
    Content    string
    ToolCalls  []ToolCall // assistant only
    ToolCallID string     // tool only
}

type ToolSpec struct {
    Name        string
    Description string
    Schema      map[string]any // JSON Schema object
}

type Request struct {
    Model       string
    Messages    []Message
    Tools       []ToolSpec
    Temperature *float64
}

type Usage struct{ Prompt, Completion int }

type Response struct {
    Message      Message // assistant turn, possibly with ToolCalls
    Usage        Usage
    FinishReason string // "stop" | "tool_calls" | "length" | …
}

type Provider interface {
    Complete(ctx context.Context, req Request) (*Response, error)
}
```

Our `Message` is the single representation used by the store, the loop, and the
provider. `openai/chat.go` converts in both directions; nothing else knows about
the SDK's union types.

**Why no streaming in v1.** Streaming only pays off if the channel renders partial
output. Telegram has no streaming primitive — OpenClaw fakes it by repeatedly
calling `editMessageText`, which burns rate limit and produces visible flicker.
For a tool-using assistant the useful progress signal is *which tool is running*,
which the loop already emits without streaming. Decision: send a typing action
while working (phase 6), no token streaming. Revisit only if turns routinely
exceed ~30s of pure text generation.

### Error classification

```go
type ErrKind int
const (
    ErrTransient ErrKind = iota // 5xx, 429 with retry-after, connection reset, timeout
    ErrAuth                     // 401, 403
    ErrRateLimit                // 429 after retries exhausted
    ErrContextLength            // 400 with context_length_exceeded
    ErrBadRequest               // other 400 — a bug in our request
    ErrCanceled                 // ctx canceled/deadline
)
type Error struct { Kind ErrKind; Status int; Msg string; Err error }
```

`ErrContextLength` is called out separately because the agent loop reacts to it
specifically: trim history and retry once (phase 4).

Retries: the SDK has built-in retry with backoff; set `option.WithMaxRetries(n)`
from `openai.max_retries` and do **not** add a second retry layer around it.
Classification happens after the SDK gives up.

## Related Code Files

- Create: `internal/provider/provider.go` — the interface and types above
- Create: `internal/provider/errors.go` — `ErrKind`, `Error`, `Classify(error)`
- Create: `internal/provider/openai/client.go` — construction from `config.OpenAI`
- Create: `internal/provider/openai/chat.go` — `Complete` plus both converters
- Create: `internal/provider/openai/chat_test.go` — converter round trips
- Create: `internal/provider/openai/errors_test.go` — classification table
- Create: `internal/provider/mock/mock.go` — scripted provider for phases 4-9 tests
- Modify: `internal/cli/doctor_cmd.go` — add the reachability probe (command itself lands in phase 9)

## Implementation Steps

1. Add `github.com/openai/openai-go/v3`. This is what forces `go 1.25` in `go.mod`
   (SDK v3.45+ requires it) — confirm the module actually builds on the toolchain in
   use before proceeding.
2. `client.go`: `New(cfg config.OpenAI) (*Client, error)`. Options:
   `option.WithAPIKey(cfg.APIKey())`, `option.WithBaseURL(cfg.BaseURL)`,
   `option.WithMaxRetries(cfg.MaxRetries)`, `option.WithHTTPClient(&http.Client{Timeout: cfg.Timeout})`.
   Error out on an empty key with a message naming the configured env var.
3. `chat.go` → `toSDK(req Request)`:
   - `system` → `openai.SystemMessage`, `user` → `openai.UserMessage`
   - `assistant` with no tool calls → `openai.AssistantMessage`
   - `assistant` with tool calls → reconstruct the assistant param including
     `ToolCalls` (id, name, arguments string). This is the fiddly one; cover it with a
     round-trip test rather than trusting it by inspection.
   - `tool` → `openai.ToolMessage(content, toolCallID)`
   - `Tools` → `openai.ChatCompletionFunctionTool(openai.FunctionDefinitionParam{…})`,
     `Schema` passed straight through as `openai.FunctionParameters`
4. `fromSDK(completion)`: read `Choices[0]`; return empty-choices as `ErrBadRequest`
   rather than panicking on index 0. Map `ToolCalls` to ours, keeping `Arguments` as
   raw bytes. Copy `Usage.PromptTokens` / `CompletionTokens` / `FinishReason`.
5. `errors.go`: `Classify` inspects `*openai.Error` for status and body code; check
   `errors.Is(err, context.Canceled)` and `context.DeadlineExceeded` first; match
   `context_length_exceeded` on the error code, not on English message text.
6. `mock/mock.go`: `New(steps ...Step)` where each `Step` is either a tool-call
   response or a final text response, plus a recorder of received requests. This is
   what makes phases 4-8 testable without network access — build it now, not later.
7. Add the doctor probe: one minimal completion against the configured model with
   `max_tokens` low, reporting resolved base URL, model, and latency.

## Tests / Validation

- Converter round trip: `Message` → SDK param → back, for all four roles including
  an assistant turn carrying two tool calls.
- A tool spec with a nested-object JSON schema survives conversion unchanged.
- `Classify` table: 401, 403, 429, 500, 400 with `context_length_exceeded`, 400
  other, `context.Canceled`, `context.DeadlineExceeded`, raw `net.Error` timeout.
- Empty `Choices` yields `ErrBadRequest`, not a panic.
- Mock provider: scripted two-step (tool call, then final) drives a fake loop and
  records exactly two requests with the second containing the tool result.
- Optional, network-gated by `MTCLAW_E2E=1`: one real completion against the API.

## Success Criteria

- [x] `internal/agent` and `internal/tools` compile with no `openai-go` import (verified by `go list -deps`) — neither package exists yet; verified instead that no package outside `internal/provider/openai` imports it, via `go list -deps` over every package in the module
- [x] An assistant message with tool calls round trips through both converters unchanged
- [x] Every error class in the table maps to the right `ErrKind`
- [x] `ErrContextLength` is distinguishable from other 400s
- [x] Usage tokens are populated on every successful response
- [x] Mock provider can script a multi-turn tool-calling conversation
- [x] Doctor probe reports success/failure against a real key without leaking the key in output — implemented as `(*openai.Client).Probe`, exported and unit-testable, but not wired to a CLI command (the `doctor` command itself is phase 9's `internal/cli/doctor_cmd.go`, which does not exist yet)

## Risk Assessment

- **SDK churn.** `openai-go` is at v3.49 and moves fast; the tool-call union types
  have changed shape across minor versions. The `Provider` interface exists exactly
  so upgrades stay contained to two files. Pin an exact version in `go.mod`.
- **Go 1.25 floor** comes from this dependency alone. If that toolchain is
  unavailable, the fallback is hand-rolling the four HTTP calls we need over
  `net/http` (which is what goclaw does) — a day of work, not a redesign.
- **`base_url` invites scope creep.** It exists to support proxies. The moment
  someone points it at a non-OpenAI backend, tool-call semantics may differ. Document
  as unsupported.
- **No streaming is a real UX tradeoff.** A 60-second silent turn feels broken. The
  typing action plus tool-progress messages in phase 6 are the mitigation and must
  not be dropped as "polish".
