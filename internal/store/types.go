package store

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/tiennm99/MTClaw/internal/provider"
)

// Session is one durable conversation, keyed by (channel, chat_id,
// thread_id) so a Telegram chat (or forum topic) maps to the same session
// across gateway restarts.
type Session struct {
	ID               string
	Channel          string // telegram | cli | cron
	ChatID           string
	ThreadID         string // forum topic id; "" when absent
	Title            string
	Model            string
	Summary          string // compaction target; written by nothing in v1
	PromptTokens     int
	CompletionTokens int
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// Message is one entry in a session's ordered history. ToolCalls carries
// the assistant's tool_calls payload as a JSON array, preserved losslessly
// for replay to the OpenAI API; it is populated on assistant rows only.
// ToolCallID and ToolName are populated on tool rows only.
type Message struct {
	ID         int64
	SessionID  string
	Seq        int64  // per-session monotonic, gapless
	Role       string // system | user | assistant | tool
	Content    string
	ToolCalls  string
	ToolCallID string
	ToolName   string
	CreatedAt  time.Time
}

// ToProviderMessage converts a stored row into internal/agent's and
// internal/provider's shared wire representation, decoding the
// JSON-encoded ToolCalls column (populated on assistant rows only) back
// into typed provider.ToolCall values. A malformed ToolCalls column - data
// this package did not write itself - is reported as an error rather than
// silently dropped, since discarding it would replay an incomplete
// tool_calls message to the provider.
func (m Message) ToProviderMessage() (provider.Message, error) {
	pm := provider.Message{
		Role:       provider.Role(m.Role),
		Content:    m.Content,
		ToolCallID: m.ToolCallID,
	}
	if m.ToolCalls != "" {
		if err := json.Unmarshal([]byte(m.ToolCalls), &pm.ToolCalls); err != nil {
			return provider.Message{}, fmt.Errorf("store: decode tool_calls for message %d: %w", m.ID, err)
		}
	}
	return pm, nil
}

// FromProviderMessage converts one provider.Message into a store row ready
// for MessageStore.Append, JSON-encoding ToolCalls losslessly. ToolName is
// left blank here: a provider.Message carries no tool name, only its
// caller (internal/agent's loop, which still has the originating
// provider.ToolCall in hand when it buffers a tool result) knows it, and
// sets Message.ToolName itself after calling this converter.
func FromProviderMessage(m provider.Message) (Message, error) {
	sm := Message{
		Role:       string(m.Role),
		Content:    m.Content,
		ToolCallID: m.ToolCallID,
	}
	if len(m.ToolCalls) > 0 {
		raw, err := json.Marshal(m.ToolCalls)
		if err != nil {
			return Message{}, fmt.Errorf("store: encode tool_calls: %w", err)
		}
		sm.ToolCalls = string(raw)
	}
	return sm, nil
}

// Approval is a request to run a tool, most commonly exec, that the policy
// engine routed to a human (or, in auto mode, that the classifier flagged
// as dangerous).
type Approval struct {
	ID        string // nonce, also the callback_data payload
	SessionID string
	Channel   string
	ChatID    string
	Tool      string
	Command   string
	Reason    string // classifier rationale in auto mode
	State     string // pending | approved | denied | expired
	MessageID string // telegram message holding the inline buttons
	CreatedAt time.Time
	ExpiresAt time.Time
	DecidedAt *time.Time
	DecidedBy string // telegram user id
}

// ExecAudit is one append-only record of a command that was decided,
// whether or not it ran. It intentionally has no reference back to a
// Session that survives the session's deletion: the audit trail must
// outlive the conversation it happened in.
type ExecAudit struct {
	ID         int64
	SessionID  string
	Command    string
	CWD        string
	Decision   string // denied_rule | allowed_rule | approved | denied_user | expired | auto_allowed
	Rule       string // the matching regex, when any
	ExitCode   *int
	DurationMS *int64
	Truncated  bool
	CreatedAt  time.Time
}

// CronRun is one execution record of a scheduled job.
type CronRun struct {
	ID         int64
	JobName    string
	SessionID  string
	Status     string // ok | error | skipped
	Error      string
	StartedAt  time.Time
	FinishedAt *time.Time
}
