package openai

import (
	"encoding/json"
	"testing"

	openaisdk "github.com/openai/openai-go/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tiennm99/MTClaw/internal/provider"
)

// roundTrip pushes m through toSDKMessage, marshals the resulting param to
// JSON (as a real request would), unmarshals that JSON into the SDK's
// response-shaped ChatCompletionMessage (the shape the API actually
// returns), and runs it through fromSDK. This exercises both converters and
// the real JSON encoding in between, rather than trusting either by
// inspection alone.
func roundTrip(t *testing.T, m provider.Message) provider.Message {
	t.Helper()

	sdkMsg, err := toSDKMessage(m)
	require.NoError(t, err)

	data, err := json.Marshal(sdkMsg)
	require.NoError(t, err)

	var respMsg openaisdk.ChatCompletionMessage
	require.NoError(t, json.Unmarshal(data, &respMsg))

	completion := &openaisdk.ChatCompletion{
		Choices: []openaisdk.ChatCompletionChoice{
			{Message: respMsg, FinishReason: "stop"},
		},
	}
	resp, err := fromSDK(completion)
	require.NoError(t, err)
	return resp.Message
}

func TestConverterRoundTrip_System(t *testing.T) {
	got := roundTrip(t, provider.Message{Role: provider.RoleSystem, Content: "be terse"})
	// System messages have no assistant-side equivalent to read back from a
	// response; the meaningful assertion is that toSDKMessage does not
	// error and produces the right union variant.
	sdkMsg, err := toSDKMessage(provider.Message{Role: provider.RoleSystem, Content: "be terse"})
	require.NoError(t, err)
	require.NotNil(t, sdkMsg.OfSystem)
	assert.Equal(t, "be terse", sdkMsg.OfSystem.Content.OfString.Value)
	_ = got
}

func TestConverterRoundTrip_User(t *testing.T) {
	sdkMsg, err := toSDKMessage(provider.Message{Role: provider.RoleUser, Content: "list files"})
	require.NoError(t, err)
	require.NotNil(t, sdkMsg.OfUser)
	assert.Equal(t, "list files", sdkMsg.OfUser.Content.OfString.Value)
}

func TestConverterRoundTrip_Tool(t *testing.T) {
	sdkMsg, err := toSDKMessage(provider.Message{Role: provider.RoleTool, Content: `{"ok":true}`, ToolCallID: "call_1"})
	require.NoError(t, err)
	require.NotNil(t, sdkMsg.OfTool)
	assert.Equal(t, "call_1", sdkMsg.OfTool.ToolCallID)
	assert.Equal(t, `{"ok":true}`, sdkMsg.OfTool.Content.OfString.Value)
}

func TestConverterRoundTrip_AssistantPlainText(t *testing.T) {
	original := provider.Message{Role: provider.RoleAssistant, Content: "the answer is 42"}
	got := roundTrip(t, original)
	assert.Equal(t, original.Content, got.Content)
	assert.Empty(t, got.ToolCalls)
}

// TestConverterRoundTrip_AssistantTwoToolCalls is the fiddly case called out
// by the phase spec: an assistant turn carrying two tool calls must survive
// toSDKMessage -> JSON -> fromSDK unchanged.
func TestConverterRoundTrip_AssistantTwoToolCalls(t *testing.T) {
	original := provider.Message{
		Role: provider.RoleAssistant,
		ToolCalls: []provider.ToolCall{
			{ID: "call_1", Name: "get_weather", Args: json.RawMessage(`{"location":"Hanoi"}`)},
			{ID: "call_2", Name: "get_time", Args: json.RawMessage(`{"tz":"Asia/Ho_Chi_Minh"}`)},
		},
	}

	got := roundTrip(t, original)

	require.Len(t, got.ToolCalls, 2)
	assert.Equal(t, "call_1", got.ToolCalls[0].ID)
	assert.Equal(t, "get_weather", got.ToolCalls[0].Name)
	assert.JSONEq(t, `{"location":"Hanoi"}`, string(got.ToolCalls[0].Args))

	assert.Equal(t, "call_2", got.ToolCalls[1].ID)
	assert.Equal(t, "get_time", got.ToolCalls[1].Name)
	assert.JSONEq(t, `{"tz":"Asia/Ho_Chi_Minh"}`, string(got.ToolCalls[1].Args))

	assert.Empty(t, got.Content)
}

func TestConverterRoundTrip_AssistantContentAndToolCalls(t *testing.T) {
	original := provider.Message{
		Role:    provider.RoleAssistant,
		Content: "checking the weather",
		ToolCalls: []provider.ToolCall{
			{ID: "call_1", Name: "get_weather", Args: json.RawMessage(`{"location":"Hanoi"}`)},
		},
	}

	got := roundTrip(t, original)
	assert.Equal(t, original.Content, got.Content)
	require.Len(t, got.ToolCalls, 1)
	assert.Equal(t, "call_1", got.ToolCalls[0].ID)
}

func TestToSDKMessage_UnknownRoleErrors(t *testing.T) {
	_, err := toSDKMessage(provider.Message{Role: provider.Role("bogus"), Content: "x"})
	assert.Error(t, err)
}

// TestToSDK_ToolSpecNestedSchemaSurvivesConversion covers a tool spec with a
// nested-object JSON schema: it must marshal through the request unchanged.
func TestToSDK_ToolSpecNestedSchemaSurvivesConversion(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"location": map[string]any{"type": "string"},
			"options": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"unit": map[string]any{
						"type": "string",
						"enum": []any{"c", "f"},
					},
				},
				"required": []any{"unit"},
			},
		},
		"required": []any{"location"},
	}

	req := provider.Request{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
		Tools: []provider.ToolSpec{
			{Name: "get_weather", Description: "get the weather", Schema: schema},
		},
	}

	params, err := toSDK(req)
	require.NoError(t, err)
	require.Len(t, params.Tools, 1)

	data, err := json.Marshal(params)
	require.NoError(t, err)

	var decoded struct {
		Tools []struct {
			Function struct {
				Name        string         `json:"name"`
				Description string         `json:"description"`
				Parameters  map[string]any `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
	}
	require.NoError(t, json.Unmarshal(data, &decoded))
	require.Len(t, decoded.Tools, 1)
	assert.Equal(t, "get_weather", decoded.Tools[0].Function.Name)
	assert.Equal(t, "get the weather", decoded.Tools[0].Function.Description)
	assert.Equal(t, schema, decoded.Tools[0].Function.Parameters)
}

func TestToSDK_Temperature(t *testing.T) {
	temp := 0.4
	req := provider.Request{
		Model:       "gpt-4o",
		Messages:    []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
		Temperature: &temp,
	}
	params, err := toSDK(req)
	require.NoError(t, err)
	require.True(t, params.Temperature.Valid())
	assert.InDelta(t, 0.4, params.Temperature.Value, 1e-9)
}

func TestFromSDK_EmptyChoicesIsBadRequest(t *testing.T) {
	completion := &openaisdk.ChatCompletion{Choices: nil}
	_, err := fromSDK(completion)
	require.Error(t, err)

	var provErr *provider.Error
	require.ErrorAs(t, err, &provErr)
	assert.Equal(t, provider.ErrBadRequest, provErr.Kind)
}

func TestFromSDK_UsagePopulated(t *testing.T) {
	completion := &openaisdk.ChatCompletion{
		Choices: []openaisdk.ChatCompletionChoice{
			{Message: openaisdk.ChatCompletionMessage{Content: "hi"}, FinishReason: "stop"},
		},
		Usage: openaisdk.CompletionUsage{PromptTokens: 12, CompletionTokens: 5},
	}
	resp, err := fromSDK(completion)
	require.NoError(t, err)
	assert.Equal(t, 12, resp.Usage.Prompt)
	assert.Equal(t, 5, resp.Usage.Completion)
	assert.Equal(t, "stop", resp.FinishReason)
}
