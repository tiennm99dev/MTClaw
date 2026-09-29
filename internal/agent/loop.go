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

// toolResultElidedPlaceholder replaces a buffered tool-role message's
// content on the ErrContextLength retry, for every such message buffered up
// to the point the retry fired (see elideBufferedToolResults and Run's
// elideUpTo). history is not always what pushed a request over the limit: a
// tool-heavy turn's own in-progress buffer - several large tool results
// accumulated across earlier iterations of the same turn - can dominate it,
// and trimming history alone does nothing for that. Every tool message keeps
// its ToolCallID so the assistant/tool pairing invariant survives; only
// Content shrinks.
const toolResultElidedPlaceholder = "[tool output elided to fit the context window]"

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
// goes through finish, which persists what happened before returning:
// messages accumulate in memory and are flushed to the store exactly once,
// so a crash mid-turn can never leave an assistant tool_calls row without
// its matching tool rows.
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

	contextRetried := false
	elideUpTo := 0 // set once contextRetried fires; see elideBufferedToolResults
	var totalUsage provider.Usage
	iterations := 0
	maxIter := l.cfg.Agent.MaxIterations
	systemPrompt := Build(l.cfg, l.tools.Specs(), time.Now(), l.log)
	meta := Meta{SessionID: sessionID, Channel: sess.Channel, ChatID: sess.ChatID, ThreadID: sess.ThreadID, MessageID: messageID}

	for {
		if ctx.Err() != nil {
			return l.abortForCancellation(ctx, sessionID, buffer, totalUsage, iterations, ctx.Err())
		}

		iterations++
		onProgress(Event{Kind: EventIteration, Iteration: iterations})

		// The request sends an elided copy of buffer[:elideUpTo] once the
		// retry below has fired, but buffer itself is never mutated: flush
		// must still persist the real tool output, not the placeholder sent
		// to the provider. Results buffered after the retry (elideUpTo:)
		// are sent - and later flushed - in full: eliding them too would
		// mean the model never sees any tool output for the rest of the
		// turn, forcing it to re-run every tool call up to max_iterations.
		requestBuffer := buffer
		if contextRetried {
			requestBuffer = elideBufferedToolResults(buffer, elideUpTo)
		}
		req := provider.Request{
			Model:       l.cfg.Agent.Model,
			Messages:    l.assembleMessages(systemPrompt, history, requestBuffer),
			Tools:       l.tools.Specs(),
			Temperature: l.cfg.Agent.Temperature,
		}

		resp, err := l.prov.Complete(ctx, req)
		if err != nil {
			perr := provider.Classify(ctx, err)

			if ctx.Err() != nil {
				return l.abortForCancellation(ctx, sessionID, buffer, totalUsage, iterations, perr)
			}

			if perr.Kind == provider.ErrContextLength && !contextRetried {
				contextRetried = true
				elideUpTo = len(buffer)
				iterations-- // the retry is not a new agent step, just recovery
				history = HardTrim(history, contextLengthRetryTurns)
				continue
			}

			// Any other provider error - including a second
			// ErrContextLength - flushes the buffer and reports the
			// classified error, the same policy the cancellation and
			// tool-error exits below use: backfillAbortedToolCalls (run
			// after every tool call, not just a failing one) keeps buffer
			// pairing-consistent between iterations, so a provider error
			// arriving mid-turn is safe to persist along with whatever
			// tool activity already ran, rather than losing that activity
			// (and the usage it billed) from the transcript.
			return l.finish(ctx, sessionID, buffer, totalUsage, Result{Iterations: iterations, Usage: totalUsage, Err: perr})
		}

		totalUsage.Prompt += resp.Usage.Prompt
		totalUsage.Completion += resp.Usage.Completion

		// Dispatch on the payload, not the label: FinishReason is a
		// provider-supplied string, and an OpenAI-compatible endpoint
		// other than api.openai.com itself (base_url is a documented,
		// user-configurable knob) can return tool_calls under "stop" or
		// "" and vice versa. Trusting the label here either buffers an
		// assistant tool_calls row with no chance to answer it, or spins
		// through max_iterations answering zero calls.
		if len(resp.Message.ToolCalls) > 0 {
			buffer = append(buffer, resp.Message)

			for i, call := range resp.Message.ToolCalls {
				onProgress(Event{Kind: EventToolStarted, ToolName: call.Name, ToolCallID: call.ID})

				result, runErr := l.tools.Run(ctx, call, meta)
				if runErr != nil {
					// registry.Run's uniform rule (see its own doc comment)
					// turns a tool call that actually ran to completion into
					// a Go error whenever ctx had already ended by the time
					// it returned - a /stop, or the turn's own deadline,
					// landing right as an exec command or a write_file
					// finished. result in that case is the call's real,
					// successful output, not a failure description:
					// discarding it in favor of a generic "failed: context
					// canceled" line would both lose real information the
					// model could use next turn, and risk the model
					// re-running a side-effecting call (exec, write_file)
					// that already succeeded, because the history says it
					// failed. A tool that fails for its own reason (not ctx
					// ending) still gets the plain failure line - most such
					// failures report no output at all (result == "").
					content := fmt.Sprintf("tool %q failed: %v", call.Name, runErr)
					if ctx.Err() != nil && result != "" {
						content = fmt.Sprintf("%s\n\n[the turn was canceled as this tool call returned; the text above is what the tool reported]", result)
					}

					// Always buffer exactly one tool message per call id,
					// even on failure: a missing tool result is what
					// makes every subsequent request in this session an
					// API error.
					buffer = append(buffer, provider.Message{
						Role:       provider.RoleTool,
						Content:    content,
						ToolCallID: call.ID,
					})
					onProgress(Event{Kind: EventToolFinished, ToolName: call.Name, ToolCallID: call.ID, Err: runErr})

					// resp.Message may carry further calls after this one
					// that never got a turn to run (call i is answered
					// above; everything after it in the same batch is
					// not): backfill a synthetic result for each before
					// flushing, so the turn persisted below is internally
					// consistent (no orphaned tool_calls) no matter which
					// call in the batch aborted it.
					buffer = backfillAbortedToolCalls(buffer, resp.Message.ToolCalls[i+1:], runErr.Error())

					// ctx.Err() decides a turn cancellation, not runErr's
					// shape: a tool hitting its own internal deadline (an
					// exec timeout, say) is a tool failure, not a signal
					// that the caller gave up on the whole turn.
					if ctx.Err() != nil {
						return l.abortForCancellation(ctx, sessionID, buffer, totalUsage, iterations, runErr)
					}

					return l.finish(ctx, sessionID, buffer, totalUsage, Result{Iterations: iterations, Usage: totalUsage, Err: fmt.Errorf("agent: tool %q: %w", call.Name, runErr)})
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
				return l.finish(ctx, sessionID, buffer, totalUsage, Result{Text: capText, Iterations: iterations, Usage: totalUsage})
			}
			continue
		}

		// No ToolCalls payload ends the turn, regardless of FinishReason.
		// FinishReason only still drives the "length" and "content_filter"
		// notices below, since that is what it is actually reliable for.
		text := resp.Message.Content
		noReply := strings.TrimSpace(text) == noReplySentinel
		resultText := text
		switch resp.FinishReason {
		case "length":
			resultText += "\n\n[response truncated: the model's output hit the token limit]"
		case "content_filter":
			// Without a notice a filtered reply is an empty string, which the
			// channel layer sends as nothing at all.
			resultText += "\n\n[response withheld or cut short: the provider's content filter stopped it]"
		}

		// This branch is only reached when resp.Message.ToolCalls is
		// empty (the dispatch condition above), so the terminal row
		// appended here never carries ToolCalls.
		buffer = append(buffer, resp.Message)

		return l.finish(ctx, sessionID, buffer, totalUsage, Result{Text: resultText, NoReply: noReply, Iterations: iterations, Usage: totalUsage})
	}
}

// maxHistoryBytes bounds the total Content size of the history loadHistory
// hands to assembleMessages, independent of max_history_turns: a tool-heavy
// session can blow past the model's real context window on turn count alone
// long before max_history_turns says to stop, and once it does, every turn
// pays for a failed request first (see the ErrContextLength retry above). It
// does not need to know the model's actual context window - it only needs to
// be comfortably under what any real deployment target supports, so the
// retry stays the rare exception instead of the routine case. Fixed rather
// than configurable: it is an internal implementation budget, not a user-
// facing tuning knob. 256 KiB is roughly 64k tokens, under the 128k window
// of current OpenAI chat models.
const maxHistoryBytes = 256 * 1024

// historyFetchMaxDoublings bounds how many times loadHistory widens its raw
// window (each time doubling it) when one turn is larger than the window.
const historyFetchMaxDoublings = 5

// loadHistory fetches a generous raw window (max_history_turns*8 messages,
// a heuristic covering typical turn sizes), hard-trims it to whole turns,
// then trims again to maxHistoryBytes. A window that came back full but
// holds fewer than max_history_turns whole turns (the oldest turn in it is
// cut off mid-way, or a single huge turn fills it) is widened by doubling,
// a bounded number of times, so one very long turn cannot make the next
// request forget everything before it: the cut-off prefix is unusable on
// its own and HardTrim would otherwise leave nothing at all.
func (l *Loop) loadHistory(ctx context.Context, sessionID string) ([]provider.Message, error) {
	maxTurns := l.cfg.Agent.MaxHistoryTurns
	limit := maxTurns * 8
	for widen := 0; ; widen++ {
		raw, err := l.store.Messages().Recent(ctx, sessionID, limit)
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

		full := limit > 0 && len(raw) >= limit
		short := maxTurns > 0 && len(SegmentTurns(dropLeadingNonUser(msgs))) < maxTurns
		if full && short && widen < historyFetchMaxDoublings {
			limit *= 2
			continue
		}
		return TrimToByteBudget(HardTrim(msgs, maxTurns), maxHistoryBytes), nil
	}
}

// assembleMessages builds one request's message list: system prompt, then
// trimmed history, then the turn buffered so far.
func (l *Loop) assembleMessages(systemPrompt string, history, buffer []provider.Message) []provider.Message {
	out := make([]provider.Message, 0, 1+len(history)+len(buffer))
	out = append(out, provider.Message{Role: provider.RoleSystem, Content: systemPrompt})
	out = append(out, history...)
	out = append(out, buffer...)
	return out
}

// backfillAbortedToolCalls appends a synthetic tool result for every call in
// unanswered: the calls after the one that just aborted the turn, in the
// same batch, found by position (the slice starting right after the
// aborting call) rather than by scanning buffer for which ids already have a
// tool row - a reused call id (several OpenAI-compatible backends emit
// non-unique ids) would otherwise look "already answered" from an earlier,
// unrelated batch and get silently skipped here. It is what keeps a
// multi-call batch internally consistent when one call aborts the turn
// partway through: without this, calls after the aborting one would be
// buffered (and then persisted) as part of an assistant tool_calls message
// with no matching tool row, which makes every later request in the session
// a 400 from the provider.
func backfillAbortedToolCalls(buffer []provider.Message, unanswered []provider.ToolCall, reason string) []provider.Message {
	for _, call := range unanswered {
		buffer = append(buffer, provider.Message{
			Role:       provider.RoleTool,
			Content:    fmt.Sprintf("tool execution aborted: %s", reason),
			ToolCallID: call.ID,
		})
	}
	return buffer
}

// abortForCancellation ends the turn because ctx is done - a caller cancel,
// a shutdown drain, or a deadline (job.timeout, /stop) expiring - and always
// reports it as provider.ErrCanceled: by the time a caller reaches this
// function, ctx.Err() != nil already established that this is a genuine
// cancellation, regardless of cause's own shape (an already-classified
// *provider.Error would otherwise pass through Classify's error-type
// shortcut with whatever Kind it already carried). flush (via finish) still
// records whatever was buffered so far, on a context detached from ctx's own
// cancellation.
func (l *Loop) abortForCancellation(ctx context.Context, sessionID string, buffer []provider.Message, usage provider.Usage, iterations int, cause error) Result {
	return l.finish(ctx, sessionID, buffer, usage, Result{
		Iterations: iterations,
		Usage:      usage,
		Err:        &provider.Error{Kind: provider.ErrCanceled, Msg: cause.Error(), Err: cause},
	})
}

// elideBufferedToolResults replaces every tool-role message's Content within
// buffer[:upTo] with a short placeholder, keeping every message (and
// therefore the assistant/tool pairing invariant) intact; messages at or
// after upTo are returned unchanged. It is what the ErrContextLength retry
// uses to shrink the in-turn buffer itself, not just history: by the time a
// tool-heavy turn overflows, the buffer accumulated across this turn's own
// earlier iterations can dominate the request, and trimming history alone
// does nothing for that case. upTo is fixed at the point the retry fires
// (Run's elideUpTo): eliding the whole buffer on every later iteration too
// would replace real tool output produced *after* the retry with the same
// placeholder, so the model would never see any tool result for the rest of
// the turn and would keep re-running tools up to max_iterations.
func elideBufferedToolResults(buffer []provider.Message, upTo int) []provider.Message {
	out := make([]provider.Message, len(buffer))
	copy(out, buffer)
	for i := 0; i < upTo && i < len(out); i++ {
		if out[i].Role == provider.RoleTool {
			m := out[i]
			m.Content = toolResultElidedPlaceholder
			out[i] = m
		}
	}
	return out
}

// flushTimeout bounds a detached flush (see flush): it matches the DSN's
// busy_timeout(5000) (sqlite's dsn helper), and the writer handle is capped
// at one physical connection (sqlite.Open's db.SetMaxOpenConns(1)), so an
// unbounded wait would let Append block forever behind a wedged writer.
const flushTimeout = 5 * time.Second

// flush converts the buffered turn to store rows and appends them in one
// call, then records accumulated usage in one AddUsage call. The tool name
// for each buffered tool-role message is worked out here, from the nearest
// preceding assistant tool_calls declaration in buffer, rather than threaded
// through Run as a separate map: a call id reused across two different runs
// in the same turn (several OpenAI-compatible backends emit non-unique ids)
// then always resolves to whichever run actually preceded it, instead of
// whichever run happened to declare that id first.
//
// flush always runs on a context detached from ctx's own cancellation
// (context.WithoutCancel), bounded by flushTimeout: a turn that finished (or
// aborted) after ctx itself was done - a shutdown drain, /stop, or a cron
// job.timeout expiring - must still be able to record what happened, instead
// of losing it to "context canceled" on the write that follows.
func (l *Loop) flush(ctx context.Context, sessionID string, buffer []provider.Message, usage provider.Usage) error {
	if len(buffer) == 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), flushTimeout)
	defer cancel()

	toolNames := map[string]string{}
	storeMsgs := make([]store.Message, 0, len(buffer))
	for _, m := range buffer {
		if m.Role == provider.RoleAssistant {
			for _, tc := range m.ToolCalls {
				toolNames[tc.ID] = tc.Name
			}
		}
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

// finish flushes the buffered turn and applies the one flush-failure policy
// every exit path shares: a flush error is always logged, and becomes res.Err
// only when the turn itself did not already fail for a more specific reason
// - that existing Err (a cancellation, a tool error, a provider error)
// describes something the caller needs to see more than a storage failure
// for an already-decided outcome does.
func (l *Loop) finish(ctx context.Context, sessionID string, buffer []provider.Message, usage provider.Usage, res Result) Result {
	if err := l.flush(ctx, sessionID, buffer, usage); err != nil {
		l.log.Error("agent: flush failed", "session_id", sessionID, "error", err)
		if res.Err == nil {
			res.Err = err
		}
	}
	return res
}
