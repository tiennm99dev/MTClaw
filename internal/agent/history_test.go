package agent

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tiennm99/MTClaw/internal/provider"
)

func TestSegmentTurns_SplitsOnlyAtUserBoundaries(t *testing.T) {
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: "hi"},
		{Role: provider.RoleAssistant, Content: "", ToolCalls: []provider.ToolCall{{ID: "c1", Name: "x"}}},
		{Role: provider.RoleTool, Content: "result", ToolCallID: "c1"},
		{Role: provider.RoleAssistant, Content: "done"},
		{Role: provider.RoleUser, Content: "again"},
		{Role: provider.RoleAssistant, Content: "ok"},
	}

	turns := SegmentTurns(msgs)
	require.Len(t, turns, 2)
	assert.Len(t, turns[0].Messages, 4)
	assert.Len(t, turns[1].Messages, 2)
	assert.Equal(t, provider.RoleUser, turns[0].Messages[0].Role)
	assert.Equal(t, provider.RoleUser, turns[1].Messages[0].Role)
}

func TestSegmentTurns_Empty(t *testing.T) {
	assert.Empty(t, SegmentTurns(nil))
}

func TestHardTrim_KeepsWholeTrailingTurnsOnly(t *testing.T) {
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: "1"},
		{Role: provider.RoleAssistant, Content: "1a"},
		{Role: provider.RoleUser, Content: "2"},
		{Role: provider.RoleAssistant, Content: "2a"},
		{Role: provider.RoleUser, Content: "3"},
		{Role: provider.RoleAssistant, Content: "", ToolCalls: []provider.ToolCall{{ID: "c1", Name: "t"}}},
		{Role: provider.RoleTool, Content: "r", ToolCallID: "c1"},
		{Role: provider.RoleAssistant, Content: "3a"},
	}

	out := HardTrim(msgs, 2)
	require.Len(t, out, 6)
	assert.Equal(t, "2", out[0].Content)
	assert.Equal(t, "3", out[2].Content)
	assertNoOrphans(t, out)
}

func TestHardTrim_NoOpWhenUnderLimit(t *testing.T) {
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: "1"},
		{Role: provider.RoleAssistant, Content: "1a"},
	}
	out := HardTrim(msgs, 10)
	assert.Equal(t, msgs, out)
}

func TestHardTrim_ZeroOrNegativeDisablesTrimming(t *testing.T) {
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: "1"},
		{Role: provider.RoleUser, Content: "2"},
		{Role: provider.RoleUser, Content: "3"},
	}
	assert.Equal(t, msgs, HardTrim(msgs, 0))
	assert.Equal(t, msgs, HardTrim(msgs, -1))
}

// TestHardTrim_InvariantsHoldOverGeneratedHistories is the property-style
// test the phase 4 plan calls for: trimming a randomly generated, but
// structurally valid, history to any maxTurns must never start the result
// with a tool message and must never leave an assistant tool_calls id
// without its matching tool row.
func TestHardTrim_InvariantsHoldOverGeneratedHistories(t *testing.T) {
	rng := rand.New(rand.NewSource(1))

	for i := 0; i < 200; i++ {
		turnCount := rng.Intn(15) + 1
		history := genHistory(rng, turnCount)

		maxTurns := rng.Intn(turnCount + 2) // includes 0 (disables trimming) and values above turnCount
		out := HardTrim(history, maxTurns)

		assertNoOrphans(t, out)

		wantTurns := turnCount
		if maxTurns > 0 && maxTurns < turnCount {
			wantTurns = maxTurns
		}
		gotTurns := SegmentTurns(out)
		assert.Len(t, gotTurns, wantTurns, "case %d: maxTurns=%d turnCount=%d", i, maxTurns, turnCount)
	}
}

// TestHardTrim_MidTurnWindow_NeverOrphansOrStartsWithNonUser covers the
// sibling case to the invariant test above: a raw window that starts
// mid-turn, exactly what Loop.loadHistory's Recent(maxTurns*8) fetch can
// hand HardTrim when a single turn spans more messages than the window
// covers. Cutting a structurally valid history at an arbitrary message
// index (not a turn boundary) and trimming the result must still never
// orphan a tool_calls id and must never start with anything but a user
// message.
func TestHardTrim_MidTurnWindow_NeverOrphansOrStartsWithNonUser(t *testing.T) {
	rng := rand.New(rand.NewSource(2))

	for i := 0; i < 200; i++ {
		turnCount := rng.Intn(15) + 2 // at least 2 turns so a mid-turn cut has somewhere to land
		history := genHistory(rng, turnCount)

		cut := rng.Intn(len(history))
		raw := history[cut:]

		maxTurns := rng.Intn(turnCount + 2)
		out := HardTrim(raw, maxTurns)

		assertNoOrphans(t, out)
		if len(out) > 0 {
			assert.Equal(t, provider.RoleUser, out[0].Role, "case %d: cut=%d maxTurns=%d: trimmed history must start at a user message", i, cut, maxTurns)
		}
	}
}

// genHistory builds a structurally valid random history of turnCount
// turns: each turn is a user message, zero or more rounds of an assistant
// tool_calls message (1-3 calls) followed by exactly one matching tool
// message per call, then a final assistant text message.
func genHistory(rng *rand.Rand, turnCount int) []provider.Message {
	var out []provider.Message
	for t := 0; t < turnCount; t++ {
		out = append(out, provider.Message{Role: provider.RoleUser, Content: fmt.Sprintf("turn %d", t)})

		rounds := rng.Intn(3)
		for r := 0; r < rounds; r++ {
			callCount := rng.Intn(3) + 1
			calls := make([]provider.ToolCall, callCount)
			for c := 0; c < callCount; c++ {
				calls[c] = provider.ToolCall{
					ID:   fmt.Sprintf("t%d-r%d-c%d", t, r, c),
					Name: "tool",
					Args: json.RawMessage(`{}`),
				}
			}
			out = append(out, provider.Message{Role: provider.RoleAssistant, ToolCalls: calls})
			for _, call := range calls {
				out = append(out, provider.Message{Role: provider.RoleTool, Content: "ok", ToolCallID: call.ID})
			}
		}

		out = append(out, provider.Message{Role: provider.RoleAssistant, Content: fmt.Sprintf("reply %d", t)})
	}
	return out
}

// assertNoOrphans checks the two invariants trimming must never violate:
// the output never starts with a tool message, and every assistant
// tool_calls id has a matching tool message (and vice versa).
func assertNoOrphans(t *testing.T, msgs []provider.Message) {
	t.Helper()

	if len(msgs) > 0 {
		require.NotEqual(t, provider.RoleTool, msgs[0].Role, "history must never start with a tool message")
	}

	pending := map[string]bool{}
	for _, m := range msgs {
		switch m.Role {
		case provider.RoleAssistant:
			for _, tc := range m.ToolCalls {
				pending[tc.ID] = true
			}
		case provider.RoleTool:
			require.True(t, pending[m.ToolCallID], "tool message %q has no preceding tool_calls entry", m.ToolCallID)
			delete(pending, m.ToolCallID)
		}
	}
	require.Empty(t, pending, "orphaned tool_calls with no matching tool message: %v", pending)
}
