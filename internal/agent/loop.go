// Package agent implements MTClaw's Think -> Act -> Observe cycle: it
// assembles a prompt from config and session history, calls the provider,
// executes any returned tool calls, feeds results back, and repeats until
// the model produces text or a bound is hit. It depends only on
// internal/provider's interfaces, internal/store's interfaces, and its own
// ToolRunner interface - no channel package, no concrete tool
// implementation, and no OpenAI SDK.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/tiennm99/MTClaw/internal/config"
	"github.com/tiennm99/MTClaw/internal/provider"
	"github.com/tiennm99/MTClaw/internal/store"
)

// noReplySentinel is the exact (trimmed) assistant text that suppresses an
// outbound reply. It is still persisted so the transcript stays honest
// about what the model actually produced.
const noReplySentinel = "NO_REPLY"

// contextLengthRetryTurns is how many trailing turns survive the one
// retry after provider.ErrContextLength: aggressive enough to make the
// retried request fit in virtually every real case, without discarding the
// whole conversation on a hard trim to max_history_turns alone.
const contextLengthRetryTurns = 4

// ToolRunner executes tool calls the model requests. Run never returns a
// Go error for tool-level failures: a failed tool produces a result string
// describing the failure so the model can react (apologize, try another
// approach). Errors are reserved for infrastructure faults - the tool
// registry crashing, a context deadline the tool itself could not honor -
// that should abort the turn instead of being shown to the model.
type ToolRunner interface {
	Specs() []provider.ToolSpec
	Run(ctx context.Context, call provider.ToolCall, meta Meta) (string, error)
}

// Meta carries the addressing context a ToolRunner needs to route
// approval prompts and replies, without internal/agent depending on any
// channel package.
type Meta struct {
	SessionID string
	Channel   string
	ChatID    string
	ThreadID  string // forum topic; routes approver prompts and replies
	// MessageID is the id of the inbound message that triggered this turn,
	// when the channel supplies one (Telegram does; `mtclaw prompt` and
	// cron turns do not). It lets an approval prompt quote the exact
	// message that caused it instead of landing unanchored in the chat.
	MessageID string
}

// Result is the outcome of one Loop.Run call.
type Result struct {
	Text       string
	NoReply    bool
	Iterations int
	Usage      provider.Usage
	Err        error
}

// Loop is the agent's Think -> Act -> Observe cycle over one session.
type Loop struct {
	cfg   config.Config
	prov  provider.Provider
	store store.Store
	tools ToolRunner
	log   *slog.Logger
}

// New builds a Loop. log may be nil, in which case slog.Default() is used.
func New(cfg config.Config, prov provider.Provider, st store.Store, tools ToolRunner, log *slog.Logger) *Loop {
	if log == nil {
		log = slog.Default()
	}
	return &Loop{cfg: cfg, prov: prov, store: st, tools: tools, log: log}
}

// Run executes one turn: load history, think, act on any tool calls,
// observe the results, repeat until the model stops, NO_REPLY, the
// iteration cap is hit, or an unrecoverable error occurs. Every exit path
// persists what happened (or explicitly limits itself to the user's
// message) before returning, per the turn-buffering contract documented in
// the phase 4 plan: messages accumulate in memory and are flushed to the
// store exactly once, so a crash mid-turn can never leave an assistant
// tool_calls row without its matching tool rows.
func (l *Loop) Run(ctx context.Context, sessionID, userText, messageID string, onProgress Progress) Result {
	if onProgress == nil {
		onProgress = func(Event) {}
	}

	sess, err := l.store.Sessions().Get(ctx, sessionID)
	if err != nil {
		return Result{Err: fmt.Errorf("agent: load session %s: %w", sessionID, err)}
	}

	history, err := l.loadHistory(ctx, sessionID)
	if err != nil {
		return Result{Err: err}
	}

	userMsg := provider.Message{Role: provider.RoleUser, Content: userText}
	buffer := []provider.Message{userMsg}
	toolNames := map[string]string{}

	summary := sess.Summary
	contextRetried := false
	var totalUsage provider.Usage
	iterations := 0
	maxIter := l.cfg.Agent.MaxIterations
	systemPrompt := Build(l.cfg, l.tools.Specs(), time.Now(), l.log)
	meta := Meta{SessionID: sessionID, Channel: sess.Channel, ChatID: sess.ChatID, ThreadID: sess.ThreadID, MessageID: messageID}

	for {
		if err := ctx.Err(); err != nil {
			return l.abortForCancellation(ctx, sessionID, buffer, toolNames, totalUsage, iterations, err)
		}

		iterations++
		onProgress(Event{Kind: EventIteration, Iteration: iterations})

		req := provider.Request{
			Model:       l.cfg.Agent.Model,
			Messages:    l.assembleMessages(systemPrompt, summary, history, buffer),
			Tools:       l.tools.Specs(),
			Temperature: &l.cfg.Agent.Temperature,
		}

		resp, err := l.prov.Complete(ctx, req)
		if err != nil {
			perr := provider.Classify(err)

			if perr.Kind == provider.ErrCanceled {
				return l.abortForCancellation(ctx, sessionID, buffer, toolNames, totalUsage, iterations, perr)
			}

			if perr.Kind == provider.ErrContextLength && !contextRetried {
				contextRetried = true
				iterations-- // the retry is not a new agent step, just recovery
				history = HardTrim(history, contextLengthRetryTurns)
				summary = ""
				continue
			}

			// Any other provider error - including a second
			// ErrContextLength - flushes only the user's message and
			// returns the classified error; buffered tool activity from
			// earlier iterations in this turn is deliberately not
			// persisted, per the phase 4 spec.
			if flushErr := l.flush(ctx, sessionID, []provider.Message{userMsg}, nil, provider.Usage{}); flushErr != nil {
				l.log.Error("agent: flush user message after provider error failed", "session_id", sessionID, "error", flushErr)
			}
			return Result{Iterations: iterations, Err: perr}
		}

		totalUsage.Prompt += resp.Usage.Prompt
		totalUsage.Completion += resp.Usage.Completion

		if resp.FinishReason == "tool_calls" {
			buffer = append(buffer, resp.Message)

			for _, call := range resp.Message.ToolCalls {
				toolNames[call.ID] = call.Name
				onProgress(Event{Kind: EventToolStarted, ToolName: call.Name, ToolCallID: call.ID})

				result, runErr := l.tools.Run(ctx, call, meta)
				if runErr != nil {
					// Always buffer exactly one tool message per call id,
					// even on failure: a missing tool result is what
					// makes every subsequent request in this session an
					// API error.
					buffer = append(buffer, provider.Message{
						Role:       provider.RoleTool,
						Content:    fmt.Sprintf("tool %q failed: %v", call.Name, runErr),
						ToolCallID: call.ID,
					})
					onProgress(Event{Kind: EventToolFinished, ToolName: call.Name, ToolCallID: call.ID, Err: runErr})

					// resp.Message may carry further calls after this one
					// that never got a turn to run: backfill a synthetic
					// result for each before flushing, so the turn persisted
					// below is internally consistent (no orphaned
					// tool_calls) no matter which call in the batch aborted
					// it.
					buffer = backfillAbortedToolCalls(buffer, toolNames, resp.Message, runErr.Error())

					if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
						return l.abortForCancellation(ctx, sessionID, buffer, toolNames, totalUsage, iterations, runErr)
					}

					if flushErr := l.flush(ctx, sessionID, buffer, toolNames, totalUsage); flushErr != nil {
						l.log.Error("agent: flush aborted turn failed", "session_id", sessionID, "error", flushErr)
					}
					return Result{Iterations: iterations, Usage: totalUsage, Err: fmt.Errorf("agent: tool %q: %w", call.Name, runErr)}
				}

				onProgress(Event{Kind: EventToolFinished, ToolName: call.Name, ToolCallID: call.ID})
				buffer = append(buffer, provider.Message{
					Role:       provider.RoleTool,
					Content:    result,
					ToolCallID: call.ID,
				})
			}

			if iterations >= maxIter {
				capText := fmt.Sprintf("Reached the maximum of %d iterations for this turn without a final answer. Ask again, or raise agent.max_iterations, to let it continue.", maxIter)
				buffer = append(buffer, provider.Message{Role: provider.RoleAssistant, Content: capText})
				if flushErr := l.flush(ctx, sessionID, buffer, toolNames, totalUsage); flushErr != nil {
					return Result{Text: capText, Iterations: iterations, Usage: totalUsage, Err: flushErr}
				}
				return Result{Text: capText, Iterations: iterations, Usage: totalUsage}
			}
			continue
		}

		// Any finish reason other than "tool_calls" ("stop", "length", or
		// an unrecognized provider-defined reason) ends the turn.
		text := resp.Message.Content
		noReply := strings.TrimSpace(text) == noReplySentinel
		if resp.FinishReason == "length" {
			text += "\n\n[response truncated: the model's output hit the token limit]"
		}

		final := resp.Message
		final.Content = text
		buffer = append(buffer, final)

		if flushErr := l.flush(ctx, sessionID, buffer, toolNames, totalUsage); flushErr != nil {
			return Result{Text: text, NoReply: noReply, Iterations: iterations, Usage: totalUsage, Err: flushErr}
		}
		return Result{Text: text, NoReply: noReply, Iterations: iterations, Usage: totalUsage}
	}
}

// loadHistory fetches a generous raw window (max_history_turns*8 messages,
// a heuristic covering typical turn sizes) and hard-trims it to whole
// turns.
func (l *Loop) loadHistory(ctx context.Context, sessionID string) ([]provider.Message, error) {
	maxTurns := l.cfg.Agent.MaxHistoryTurns
	raw, err := l.store.Messages().Recent(ctx, sessionID, maxTurns*8)
	if err != nil {
		return nil, fmt.Errorf("agent: load history for session %s: %w", sessionID, err)
	}
	msgs := make([]provider.Message, 0, len(raw))
	for _, m := range raw {
		pm, err := m.ToProviderMessage()
		if err != nil {
			return nil, fmt.Errorf("agent: convert stored history for session %s: %w", sessionID, err)
		}
		msgs = append(msgs, pm)
	}
	return HardTrim(msgs, maxTurns), nil
}

// assembleMessages builds one request's message list: system prompt, then
// sessions.summary as a system message when non-empty (nothing writes it
// in v1, but the loop still injects it if a future compaction path does),
// then trimmed history, then the turn buffered so far.
func (l *Loop) assembleMessages(systemPrompt, summary string, history, buffer []provider.Message) []provider.Message {
	out := make([]provider.Message, 0, 2+len(history)+len(buffer))
	out = append(out, provider.Message{Role: provider.RoleSystem, Content: systemPrompt})
	if summary != "" {
		out = append(out, provider.Message{Role: provider.RoleSystem, Content: summary})
	}
	out = append(out, history...)
	out = append(out, buffer...)
	return out
}

// backfillAbortedToolCalls appends a synthetic tool result - and records its
// tool name in toolNames - for every call in msg.ToolCalls that has no
// corresponding tool message already in buffer. It is what keeps a
// multi-call batch internally consistent when one call aborts the turn
// partway through: without this, calls after the aborting one would be
// buffered (and then persisted) as part of an assistant tool_calls message
// with no matching tool row, which makes every later request in the session
// a 400 from the provider.
func backfillAbortedToolCalls(buffer []provider.Message, toolNames map[string]string, msg provider.Message, reason string) []provider.Message {
	answered := make(map[string]bool, len(msg.ToolCalls))
	for _, m := range buffer {
		if m.Role == provider.RoleTool {
			answered[m.ToolCallID] = true
		}
	}
	for _, call := range msg.ToolCalls {
		if answered[call.ID] {
			continue
		}
		toolNames[call.ID] = call.Name
		buffer = append(buffer, provider.Message{
			Role:       provider.RoleTool,
			Content:    fmt.Sprintf("tool execution aborted: %s", reason),
			ToolCallID: call.ID,
		})
	}
	return buffer
}

// abortForCancellation flushes whatever is buffered so far using a
// background context (context.WithoutCancel), so the partial turn is
// still recorded even though ctx itself is done, then returns a Result
// classified as provider.ErrCanceled.
func (l *Loop) abortForCancellation(ctx context.Context, sessionID string, buffer []provider.Message, toolNames map[string]string, usage provider.Usage, iterations int, cause error) Result {
	bg := context.WithoutCancel(ctx)
	if err := l.flush(bg, sessionID, buffer, toolNames, usage); err != nil {
		l.log.Error("agent: flush on cancellation failed", "session_id", sessionID, "error", err)
	}
	return Result{Iterations: iterations, Usage: usage, Err: provider.Classify(cause)}
}

// flush converts the buffered turn to store rows and appends them in one
// call, then records accumulated usage in one AddUsage call. toolNames
// supplies the tool name for each buffered tool-role message (keyed by
// ToolCallID), since provider.Message itself carries no tool name.
func (l *Loop) flush(ctx context.Context, sessionID string, buffer []provider.Message, toolNames map[string]string, usage provider.Usage) error {
	if len(buffer) == 0 {
		return nil
	}

	storeMsgs := make([]store.Message, 0, len(buffer))
	for _, m := range buffer {
		sm, err := store.FromProviderMessage(m)
		if err != nil {
			return fmt.Errorf("agent: encode message for session %s: %w", sessionID, err)
		}
		if m.Role == provider.RoleTool {
			sm.ToolName = toolNames[m.ToolCallID]
		}
		storeMsgs = append(storeMsgs, sm)
	}

	if err := l.store.Messages().Append(ctx, sessionID, storeMsgs); err != nil {
		return fmt.Errorf("agent: append messages for session %s: %w", sessionID, err)
	}

	if usage.Prompt != 0 || usage.Completion != 0 {
		if err := l.store.Sessions().AddUsage(ctx, sessionID, usage.Prompt, usage.Completion); err != nil {
			return fmt.Errorf("agent: add usage for session %s: %w", sessionID, err)
		}
	}
	return nil
}
