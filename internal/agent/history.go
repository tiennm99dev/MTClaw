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
func HardTrim(msgs []provider.Message, maxTurns int) []provider.Message {
	msgs = dropLeadingNonUser(msgs)
	if maxTurns <= 0 {
		return msgs
	}
	turns := SegmentTurns(msgs)
	if len(turns) <= maxTurns {
		return msgs
	}

	keep := turns[len(turns)-maxTurns:]
	out := make([]provider.Message, 0, len(msgs))
	for _, t := range keep {
		out = append(out, t.Messages...)
	}
	return out
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
