package mock

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tiennm99/MTClaw/internal/provider"
)

// TestScriptedTwoStepToolConversation drives a fake agent loop: send a user
// message, get back a tool-call step, append the tool result, send again,
// get back the final text step. This is the shape phases 4-8 depend on the
// mock provider for.
func TestScriptedTwoStepToolConversation(t *testing.T) {
	p := New(
		Step{
			ToolCalls: []provider.ToolCall{
				{ID: "call_1", Name: "get_weather", Args: json.RawMessage(`{"location":"Hanoi"}`)},
			},
			Usage: provider.Usage{Prompt: 10, Completion: 5},
		},
		Step{
			Content: "It's 30C and sunny in Hanoi.",
			Usage:   provider.Usage{Prompt: 20, Completion: 8},
		},
	)

	ctx := context.Background()
	messages := []provider.Message{
		{Role: provider.RoleUser, Content: "what's the weather in Hanoi?"},
	}

	resp1, err := p.Complete(ctx, provider.Request{Model: "gpt-4o", Messages: messages})
	require.NoError(t, err)
	assert.Equal(t, "tool_calls", resp1.FinishReason)
	require.Len(t, resp1.Message.ToolCalls, 1)
	assert.Equal(t, "get_weather", resp1.Message.ToolCalls[0].Name)

	messages = append(messages, resp1.Message)
	messages = append(messages, provider.Message{
		Role:       provider.RoleTool,
		Content:    `{"temp_c":30,"condition":"sunny"}`,
		ToolCallID: resp1.Message.ToolCalls[0].ID,
	})

	resp2, err := p.Complete(ctx, provider.Request{Model: "gpt-4o", Messages: messages})
	require.NoError(t, err)
	assert.Equal(t, "stop", resp2.FinishReason)
	assert.Equal(t, "It's 30C and sunny in Hanoi.", resp2.Message.Content)
	assert.Empty(t, resp2.Message.ToolCalls)

	requests := p.Requests()
	require.Len(t, requests, 2)
	assert.Len(t, requests[0].Messages, 1)
	// The second request must carry the tool result so a real assertion
	// can check the loop actually fed it back.
	require.Len(t, requests[1].Messages, 3)
	assert.Equal(t, provider.RoleTool, requests[1].Messages[2].Role)
	assert.Equal(t, `{"temp_c":30,"condition":"sunny"}`, requests[1].Messages[2].Content)
}

func TestComplete_ExhaustedStepsReturnsError(t *testing.T) {
	p := New(Step{Content: "only one step"})
	ctx := context.Background()

	_, err := p.Complete(ctx, provider.Request{Model: "gpt-4o"})
	require.NoError(t, err)

	_, err = p.Complete(ctx, provider.Request{Model: "gpt-4o"})
	require.Error(t, err)
}

func TestComplete_ScriptedError(t *testing.T) {
	wantErr := &provider.Error{Kind: provider.ErrRateLimit, Status: 429, Msg: "slow down"}
	p := New(Step{Err: wantErr})

	_, err := p.Complete(context.Background(), provider.Request{Model: "gpt-4o"})
	require.Error(t, err)
	assert.Same(t, wantErr, err)
}

func TestComplete_RespectsCanceledContext(t *testing.T) {
	p := New(Step{Content: "unreachable"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := p.Complete(ctx, provider.Request{Model: "gpt-4o"})
	require.Error(t, err)

	var provErr *provider.Error
	require.ErrorAs(t, err, &provErr)
	assert.Equal(t, provider.ErrCanceled, provErr.Kind)
}

func TestComplete_DefaultFinishReasonForTextStep(t *testing.T) {
	p := New(Step{Content: "hi"})
	resp, err := p.Complete(context.Background(), provider.Request{Model: "gpt-4o"})
	require.NoError(t, err)
	assert.Equal(t, "stop", resp.FinishReason)
}
