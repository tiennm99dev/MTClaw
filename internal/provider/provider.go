// Package provider defines MTClaw's narrow LLM-provider boundary. The agent
// loop (internal/agent) depends only on the types in this file; no concrete
// implementation package (e.g. internal/provider/openai) is imported outside
// its own construction site, so upgrading or replacing the backing SDK stays
// contained to that one package.
package provider

import (
	"context"
	"encoding/json"
)

// Role identifies the speaker of a Message.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ToolCall is one function call the model asked the agent loop to run.
// Args stays as raw JSON end to end: the provider layer never unmarshals
// it, and the tool registry is what validates and decodes it against the
// tool's schema.
type ToolCall struct {
	ID   string
	Name string
	Args json.RawMessage
}

// Message is MTClaw's single representation of one turn of a conversation.
// It is shared by the store, the agent loop, and every provider
// implementation; only openai/chat.go knows how to convert it to and from
// the SDK's own union types.
type Message struct {
	Role Role
	// Content is the message text. Empty for an assistant turn that is
	// pure tool calls.
	Content string
	// ToolCalls is set only on assistant messages that invoked one or
	// more tools.
	ToolCalls []ToolCall
	// ToolCallID identifies which ToolCall a tool-role message is
	// answering. Set only on tool messages.
	ToolCallID string
}

// ToolSpec describes one tool the model may call, in JSON-Schema terms.
type ToolSpec struct {
	Name        string
	Description string
	Schema      map[string]any // JSON Schema object
}

// Request is one completion call.
type Request struct {
	Model       string
	Messages    []Message
	Tools       []ToolSpec
	Temperature *float64
}

// Usage reports token accounting for one completion.
type Usage struct {
	Prompt     int
	Completion int
}

// Response is the result of one completion call.
type Response struct {
	// Message is the assistant turn, possibly carrying ToolCalls instead
	// of (or alongside) Content.
	Message Message
	Usage   Usage
	// FinishReason is one of "stop", "tool_calls", "length", or another
	// provider-defined reason string.
	FinishReason string
}

// Provider is the interface every LLM backend implements. The agent loop
// depends on this interface alone.
type Provider interface {
	Complete(ctx context.Context, req Request) (*Response, error)
}
