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
		assertSequentialPairing(t, out)

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
		assertSequentialPairing(t, out)
		if len(out) > 0 {
			assert.Equal(t, provider.RoleUser, out[0].Role, "case %d: cut=%d maxTurns=%d: trimmed history must start at a user message", i, cut, maxTurns)
		}
	}
}

// genHistory builds a structurally valid random history of turnCount
// turns: each turn is a user message, zero or more rounds of an assistant
// tool_calls message (1-3 calls) followed by exactly one matching tool
// message per call, then a final assistant text message. Call ids are
// drawn from a small fixed pool ("call_0".."call_2") rather than made
// globally unique, so every generated history reuses ids across rounds and
// turns - the shape several OpenAI-compatible backends actually send, and
// exactly what repairOrphanedToolCalls's per-run matching must get right
// without cross-pairing an id to the wrong run.
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
					ID:   fmt.Sprintf("call_%d", c),
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

// TestHardTrim_RepairsOrphanedToolCallsInPoisonedHistory covers a session
// that is already poisoned - a mismatch between an assistant's declared
// tool_calls and its persisted tool-role results, the exact state a
// mislabeled FinishReason (or any other future provider mismatch) leaves
// behind. HardTrim must still return output that satisfies the no-orphan
// invariant and the stricter, order-aware sequential-pairing invariant no
// matter the maxTurns, since a poisoned row currently makes every later
// request in the session error out until it ages out of the window.
func TestHardTrim_RepairsOrphanedToolCallsInPoisonedHistory(t *testing.T) {
	rng := rand.New(rand.NewSource(3))

	for i := 0; i < 200; i++ {
		turnCount := rng.Intn(15) + 1
		history := genHistory(rng, turnCount)
		history = poisonHistory(rng, history)

		maxTurns := rng.Intn(turnCount + 2)
		out := HardTrim(history, maxTurns)

		assertNoOrphans(t, out)
		assertSequentialPairing(t, out)
	}
}

// poisonHistory injects one of four orphan shapes into a structurally valid
// history, at random: a declared tool_calls entry whose matching tool
// message is deleted (the "missing tool result" shape); an extra tool
// message whose call id no assistant message ever declared (the "stray
// tool message" shape); a second, unanswered declaration that reuses an id
// an earlier, already-answered run also used (the "reused call id" shape -
// several OpenAI-compatible backends emit non-unique ids); or a genuine
// tool row relocated to before the assistant message that declares it (the
// "tool row precedes its declaration" shape).
func poisonHistory(rng *rand.Rand, msgs []provider.Message) []provider.Message {
	switch rng.Intn(4) {
	case 0:
		return dropOneToolMessage(rng, msgs)
	case 1:
		return injectStrayToolMessage(rng, msgs)
	case 2:
		return reuseAnEarlierCallID(rng, msgs)
	default:
		return moveAToolRowBeforeItsDeclaration(rng, msgs)
	}
}

// dropOneToolMessage deletes a random tool-role message, orphaning the
// tool_calls entry it would have answered.
func dropOneToolMessage(rng *rand.Rand, msgs []provider.Message) []provider.Message {
	var toolIdx []int
	for i, m := range msgs {
		if m.Role == provider.RoleTool {
			toolIdx = append(toolIdx, i)
		}
	}
	if len(toolIdx) == 0 {
		return injectStrayToolMessage(rng, msgs)
	}

	out := make([]provider.Message, len(msgs))
	copy(out, msgs)
	drop := toolIdx[rng.Intn(len(toolIdx))]
	return append(out[:drop], out[drop+1:]...)
}

// injectStrayToolMessage inserts a tool-role message at a random position
// whose call id no assistant message ever declared.
func injectStrayToolMessage(rng *rand.Rand, msgs []provider.Message) []provider.Message {
	out := make([]provider.Message, len(msgs))
	copy(out, msgs)

	pos := rng.Intn(len(out) + 1)
	orphan := provider.Message{Role: provider.RoleTool, Content: "stray", ToolCallID: fmt.Sprintf("orphan-%d", rng.Int())}
	out = append(out, provider.Message{})
	copy(out[pos+1:], out[pos:])
	out[pos] = orphan
	return out
}

// reuseAnEarlierCallID inserts a second, unanswered assistant tool_calls
// declaration that reuses an id an earlier run in msgs already declared and
// answered. A global (non-positional) repair would see the id as
// "answered" anywhere in the history and wrongly keep this new,
// never-answered declaration too.
func reuseAnEarlierCallID(rng *rand.Rand, msgs []provider.Message) []provider.Message {
	var earlierIDs []string
	for _, m := range msgs {
		if m.Role == provider.RoleAssistant {
			for _, tc := range m.ToolCalls {
				earlierIDs = append(earlierIDs, tc.ID)
			}
		}
	}
	if len(earlierIDs) == 0 {
		return injectStrayToolMessage(rng, msgs)
	}
	reused := earlierIDs[rng.Intn(len(earlierIDs))]

	out := make([]provider.Message, len(msgs))
	copy(out, msgs)

	redeclare := provider.Message{
		Role:      provider.RoleAssistant,
		ToolCalls: []provider.ToolCall{{ID: reused, Name: "tool", Args: json.RawMessage(`{}`)}},
	}
	pos := rng.Intn(len(out) + 1)
	out = append(out, provider.Message{})
	copy(out[pos+1:], out[pos:])
	out[pos] = redeclare
	return out
}

// moveAToolRowBeforeItsDeclaration relocates a genuine tool-role message to
// somewhere at or before the index of the assistant message that declares
// it, so the row now arrives with no preceding declaration in scope - the
// "tool row precedes its declaration" shape a mislabeled FinishReason
// ordering (or any other future provider mismatch) could produce.
func moveAToolRowBeforeItsDeclaration(rng *rand.Rand, msgs []provider.Message) []provider.Message {
	var toolIdx []int
	for i, m := range msgs {
		if m.Role == provider.RoleTool {
			toolIdx = append(toolIdx, i)
		}
	}
	if len(toolIdx) == 0 {
		return injectStrayToolMessage(rng, msgs)
	}
	j := toolIdx[rng.Intn(len(toolIdx))]
	tool := msgs[j]

	declIdx := -1
	for i := 0; i < j; i++ {
		if msgs[i].Role != provider.RoleAssistant {
			continue
		}
		for _, tc := range msgs[i].ToolCalls {
			if tc.ID == tool.ToolCallID {
				declIdx = i
			}
		}
	}
	if declIdx < 0 {
		return injectStrayToolMessage(rng, msgs)
	}

	without := make([]provider.Message, 0, len(msgs)-1)
	without = append(without, msgs[:j]...)
	without = append(without, msgs[j+1:]...)

	newPos := rng.Intn(declIdx + 1) // 0..declIdx: at or before the declaration
	out := make([]provider.Message, 0, len(msgs))
	out = append(out, without[:newPos]...)
	out = append(out, tool)
	out = append(out, without[newPos:]...)
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

// assertSequentialPairing is the stricter, order-aware companion to
// assertNoOrphans: it walks msgs exactly the way repairOrphanedToolCalls
// (and the provider API itself) pairs a tool_calls entry with the tool
// message that follows it - every call an assistant message declares must
// be answered by a tool row in the run that immediately follows, before
// the next assistant or user message, with no leftover unanswered calls at
// the end. Unlike assertNoOrphans's global id set, this catches a reused id
// or an out-of-position tool row cross-pairing with the wrong declaration.
func assertSequentialPairing(t *testing.T, msgs []provider.Message) {
	t.Helper()

	pending := map[string]bool{}
	for i, m := range msgs {
		switch m.Role {
		case provider.RoleAssistant:
			require.Empty(t, pending, "message %d: assistant declared new tool_calls while %v from the prior run was still unanswered", i, pending)
			pending = make(map[string]bool, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				pending[tc.ID] = true
			}
		case provider.RoleTool:
			require.True(t, pending[m.ToolCallID], "message %d: tool id %q does not answer the immediately preceding assistant tool_calls run", i, m.ToolCallID)
			delete(pending, m.ToolCallID)
		default:
			require.Empty(t, pending, "message %d: a %s message interrupted an unanswered tool_calls run %v", i, m.Role, pending)
		}
	}
	require.Empty(t, pending, "trailing unanswered tool_calls at end of history: %v", pending)
}
