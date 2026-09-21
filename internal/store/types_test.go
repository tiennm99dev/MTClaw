package store

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tiennm99/MTClaw/internal/provider"
)

// TestFromProviderMessage_ToProviderMessage_ToolCallsRoundTrip is the
// encode/decode boundary for messages.tool_calls: it must preserve every
// field of every provider.ToolCall byte-for-byte.
func TestFromProviderMessage_ToProviderMessage_ToolCallsRoundTrip(t *testing.T) {
	msg := provider.Message{
		Role: provider.RoleAssistant,
		ToolCalls: []provider.ToolCall{
			{ID: "call_1", Name: "get_weather", Args: json.RawMessage(`{"city":"Hanoi"}`)},
			{ID: "call_2", Name: "get_time", Args: json.RawMessage(`{}`)},
		},
	}

	sm, err := FromProviderMessage(msg)
	require.NoError(t, err)
	assert.Equal(t, "assistant", sm.Role)
	assert.NotEmpty(t, sm.ToolCalls)

	got, err := sm.ToProviderMessage()
	require.NoError(t, err)
	assert.Equal(t, msg.Role, got.Role)
	assert.Equal(t, msg.ToolCalls, got.ToolCalls)
}

// TestFromProviderMessage_ToProviderMessage_ToolResultRoundTrip covers the
// tool-role side of the same boundary: Content and ToolCallID, no
// ToolCalls.
func TestFromProviderMessage_ToProviderMessage_ToolResultRoundTrip(t *testing.T) {
	msg := provider.Message{Role: provider.RoleTool, Content: "42 degrees", ToolCallID: "call_1"}

	sm, err := FromProviderMessage(msg)
	require.NoError(t, err)
	assert.Empty(t, sm.ToolCalls)

	got, err := sm.ToProviderMessage()
	require.NoError(t, err)
	assert.Equal(t, msg, got)
}

// TestFromProviderMessage_ToProviderMessage_PlainTextRoundTrip covers a
// message with neither ToolCalls nor ToolCallID.
func TestFromProviderMessage_ToProviderMessage_PlainTextRoundTrip(t *testing.T) {
	msg := provider.Message{Role: provider.RoleUser, Content: "hello"}

	sm, err := FromProviderMessage(msg)
	require.NoError(t, err)
	assert.Empty(t, sm.ToolCalls)

	got, err := sm.ToProviderMessage()
	require.NoError(t, err)
	assert.Equal(t, msg, got)
}

// TestToProviderMessage_DecodesLegacyUppercaseFieldNames proves
// encoding/json's case-insensitive field matching keeps a row written
// before provider.ToolCall carried explicit lowercase json tags decoding
// correctly: the on-disk column holds whatever field names were in effect
// when the row was written, and a future field rename must not silently
// zero out every historical row.
func TestToProviderMessage_DecodesLegacyUppercaseFieldNames(t *testing.T) {
	sm := Message{
		ID:        7,
		Role:      "assistant",
		ToolCalls: `[{"ID":"call_1","Name":"get_weather","Args":{"city":"Hanoi"}}]`,
	}

	got, err := sm.ToProviderMessage()
	require.NoError(t, err)
	require.Len(t, got.ToolCalls, 1)
	assert.Equal(t, "call_1", got.ToolCalls[0].ID)
	assert.Equal(t, "get_weather", got.ToolCalls[0].Name)
	assert.JSONEq(t, `{"city":"Hanoi"}`, string(got.ToolCalls[0].Args))
}

// TestToProviderMessage_MalformedToolCallsErrors proves a malformed
// tool_calls column - data this package did not write itself - is reported
// as an error rather than silently dropped.
func TestToProviderMessage_MalformedToolCallsErrors(t *testing.T) {
	sm := Message{ID: 9, Role: "assistant", ToolCalls: "not json"}
	_, err := sm.ToProviderMessage()
	require.Error(t, err)
}
