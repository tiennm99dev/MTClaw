package agent

import "github.com/tiennm99/MTClaw/internal/provider"

// Turn is one user message plus everything that followed it up to (but not
// including) the next user message: the assistant's reply, any tool calls
// and their results, and the final assistant text. A turn is always
// flushed to the store as one atomic Append (see Loop.Run), so trimming
// only ever cuts between turns, never inside one.
type Turn struct {
	Messages []provider.Message
}

// SegmentTurns splits a flat message history into turns, cutting only at
// role=="user" boundaries. Messages before the first user message (possible
// when the raw slice handed in is itself a window truncated mid-turn, e.g.
// Loop.loadHistory's Recent() fetch) form a leading turn of their own rather
// than being dropped silently here - HardTrim is what actually discards that
// leading segment, since it is the function callers rely on to produce a
// request-safe history.
func SegmentTurns(msgs []provider.Message) []Turn {
	var turns []Turn
	var cur []provider.Message
	for _, m := range msgs {
		if m.Role == provider.RoleUser && len(cur) > 0 {
			turns = append(turns, Turn{Messages: cur})
			cur = nil
		}
		cur = append(cur, m)
	}
	if len(cur) > 0 {
		turns = append(turns, Turn{Messages: cur})
	}
	return turns
}

// HardTrim keeps only the last maxTurns turns (see SegmentTurns), never
// splitting a turn apart, and always starts the result at a user message.
// This is what guarantees trimming can never orphan an assistant tool_calls
// message from its tool results, nor start the trimmed output with a stray
// tool or assistant message: every turn kept is either kept whole or
// dropped whole, and any leading messages that precede the first user
// message are discarded outright rather than kept as a turn of their own -
// a raw window fetched from the store (see Loop.loadHistory) can start
// mid-turn when a single turn spans more messages than the window covers,
// and that leftover prefix has no preceding tool_calls the model ever saw,
// which the provider API rejects. maxTurns <= 0 disables the turn-count
// trim but not this leading-boundary fix.
//
// repairOrphanedToolCalls runs last, on the already-trimmed output, not
// before it: SegmentTurns cuts at every user message, so a user message
// sitting between an assistant tool_calls row and its tool results (an
// interrupting message from a different turn boundary than the one the
// pair was written under) splits the pair apart, and only a repair pass
// that sees the trimmed result can catch what the trim itself just broke.
// dropLeadingNonUser runs once more afterward in case the repair dropped a
// message that had been the new leading boundary.
func HardTrim(msgs []provider.Message, maxTurns int) []provider.Message {
	msgs = dropLeadingNonUser(msgs)
	if maxTurns > 0 {
		if turns := SegmentTurns(msgs); len(turns) > maxTurns {
			keep := turns[len(turns)-maxTurns:]
			out := make([]provider.Message, 0, len(msgs))
			for _, t := range keep {
				out = append(out, t.Messages...)
			}
			msgs = out
		}
	}
	msgs = repairOrphanedToolCalls(msgs)
	return dropLeadingNonUser(msgs)
}

// dropLeadingNonUser discards any prefix of msgs before the first
// role=="user" message, returning nil if msgs holds no user message at all.
func dropLeadingNonUser(msgs []provider.Message) []provider.Message {
	for i, m := range msgs {
		if m.Role == provider.RoleUser {
			return msgs[i:]
		}
	}
	return nil
}

// repairOrphanedToolCalls makes a stored history tolerant of a session that
// is already poisoned: an assistant tool_calls id with no matching tool
// row, or a tool row whose call id no assistant message ever declared. A
// mismatch like that used to make every later request in the session fail
// with a 400 until the poisoned row aged out of the trim window - this
// makes it a one-time, self-healing repair instead.
//
// The pass is strictly sequential, mirroring how the provider API itself
// pairs a tool_calls entry with the tool message that follows it: an
// assistant message with tool_calls opens a run; only the tool rows that
// immediately follow it, before the next assistant or user message, can
// answer those calls; any call still unanswered when the run closes is
// dropped from that message, and a tool row with no open run to answer it
// - unknown id, already answered, or arriving before any declaration - is
// dropped too. Because matching is scoped to one run at a time, an id
// reused by an unrelated declaration elsewhere in the history (several
// OpenAI-compatible backends emit non-unique ids such as "call_1") can
// never cross-pair with the wrong run's tool row.
func repairOrphanedToolCalls(msgs []provider.Message) []provider.Message {
	out := make([]provider.Message, 0, len(msgs))
	pending := map[string]bool{} // call id -> answered yet, for the run currently open at out[pendingIdx]
	pendingIdx := -1

	for _, m := range msgs {
		switch m.Role {
		case provider.RoleAssistant:
			out, pendingIdx, pending = closeToolCallRun(out, pendingIdx, pending)
			out = append(out, m)
			if len(m.ToolCalls) > 0 {
				pendingIdx = len(out) - 1
				pending = make(map[string]bool, len(m.ToolCalls))
				for _, tc := range m.ToolCalls {
					pending[tc.ID] = false
				}
			}
		case provider.RoleTool:
			if pendingIdx < 0 {
				continue // no open run to answer: drop the stray row
			}
			if answered, declared := pending[m.ToolCallID]; declared && !answered {
				pending[m.ToolCallID] = true
				out = append(out, m)
			}
			// else: id not declared by the open run, or already answered
			// in it (a duplicate) - drop.
		default:
			out, pendingIdx, pending = closeToolCallRun(out, pendingIdx, pending)
			out = append(out, m)
		}
	}
	out, _, _ = closeToolCallRun(out, pendingIdx, pending)
	return out
}

// closeToolCallRun finalizes the run open at out[idx] (a no-op when idx <
// 0): every id in pending is kept on the message only if a tool row
// answered it during that run; a message left with no calls and no text is
// removed outright rather than kept as an empty husk. Always returns idx=-1
// and a nil pending map, ready for the next run.
func closeToolCallRun(out []provider.Message, idx int, pending map[string]bool) ([]provider.Message, int, map[string]bool) {
	if idx < 0 {
		return out, -1, nil
	}
	m := out[idx]
	kept := make([]provider.ToolCall, 0, len(m.ToolCalls))
	for _, tc := range m.ToolCalls {
		if pending[tc.ID] {
			kept = append(kept, tc)
		}
	}
	if len(kept) == 0 && m.Content == "" {
		out = append(out[:idx], out[idx+1:]...)
	} else {
		m.ToolCalls = kept
		out[idx] = m
	}
	return out, -1, nil
}
