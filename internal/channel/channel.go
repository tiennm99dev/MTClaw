// Package channel defines the chat-surface boundary every concrete channel
// (Telegram today; others later) implements. It depends on nothing but the
// standard library so internal/agent and internal/tools never need to know
// which channels exist.
package channel

import (
	"context"
	"time"
)

// DeliverTarget overrides where a turn's reply is sent, distinct from the
// chat that triggered it. The zero value means "reply to the same chat the
// inbound message came from" - the only case phase 7 itself produces. Phase
// 8's cron scheduler is the first real user: a scheduled prompt has no
// triggering chat at all, only a configured delivery target.
type DeliverTarget struct {
	Channel  string
	ChatID   string
	ThreadID string
}

// Inbound is one accepted user message handed to whatever drives the agent
// loop (the gateway, in phase 7). Channel.Start only ever emits messages
// that already passed that channel's own access gating.
type Inbound struct {
	Channel   string
	ChatID    string
	ThreadID  string
	UserID    string
	Text      string
	MessageID string

	// DeliverTo overrides where the turn's reply is sent; a zero value
	// replies to ChatID/ThreadID. The dispatcher only ever reads this
	// field - it has no idea cron exists - which is the point: phase 8
	// populates it, phase 7 just has to honor it.
	DeliverTo DeliverTarget
	// Timeout, when non-zero, additionally bounds the turn on top of the
	// gateway's own shutdown context (phase 8's job.timeout).
	Timeout time.Duration
	// OnDone, when non-nil, is invoked with the turn's error (nil on
	// success) once the turn finishes, before the reply is sent. Phase 8
	// uses it to record cron_runs; phase 7 never sets it itself.
	OnDone func(error)
}

// Channel is one chat surface's boundary: it turns inbound platform updates
// into Inbound values and turns text back into platform sends. Nothing
// outside this package needs to know which wire protocol a channel speaks.
type Channel interface {
	// Name identifies the channel for session addressing (e.g. "telegram").
	Name() string
	// Start pumps accepted inbound messages into out until ctx is done, at
	// which point it returns (nil on a clean shutdown, ctx.Err() otherwise).
	Start(ctx context.Context, out chan<- Inbound) error
	// Send delivers text to chatID. threadID routes forum-topic replies
	// (Telegram's message_thread_id); "" targets the chat's general
	// timeline. replyTo, when non-empty, quotes the named message id;
	// normal turn replies pass "", approval prompts and other
	// message-specific sends pass the triggering message's id.
	Send(ctx context.Context, chatID, threadID, text, replyTo string) error
}
