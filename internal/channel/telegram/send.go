package telegram

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mymmrac/telego"
	ta "github.com/mymmrac/telego/telegoapi"
	tu "github.com/mymmrac/telego/telegoutil"
)

// interChunkDelay is the pause between successive chunks of one multi-part
// reply: cheap insurance against Telegram's per-chat rate limit (~1 msg/s),
// which a long, chunked answer can otherwise reach in a tight loop.
const interChunkDelay = 250 * time.Millisecond

// maxSendAttempts bounds the fallback/retry loop in sendOne: a 429 retry,
// one bounded 5xx retry, plus a parse-mode fallback retry is the realistic
// worst case, so this only exists to guarantee termination if Telegram
// keeps misbehaving.
const maxSendAttempts = 5

// transientRetryDelay is the single bounded backoff sendOne waits before
// retrying a 5xx response once.
const transientRetryDelay = 1 * time.Second

// escapedPart is one already-escaped piece ready to send, paired with the
// plain (unescaped) text sendOne falls back to on a parse-mode rejection.
type escapedPart struct {
	escaped string
	plain   string
}

// sendText chunks text (see Split), escapes each chunk for MarkdownV2, and
// sends the result serially, falling back to no parse_mode on an HTTP 400
// that names parsing (or the message being too long) as the problem. Only
// the first part carries replyTo, since a multi-part reply is one logical
// message split across several Telegram messages, not a chain of
// independent quotes. threadID, when non-empty, is applied to every part so
// a forum-topic reply stays entirely inside its topic.
func sendText(ctx context.Context, api botAPI, chatID, threadID, text, replyTo string) error {
	if text == "" {
		return nil
	}
	id, err := strconv.ParseInt(chatID, 10, 64)
	if err != nil {
		return fmt.Errorf("telegram: invalid chat id %q: %w", chatID, err)
	}
	tid := parseThreadID(threadID)

	var parts []escapedPart
	for _, chunk := range Split(text, DefaultChunkLimit) {
		parts = append(parts, escapeChunk(chunk, DefaultChunkLimit)...)
	}

	for i, part := range parts {
		params := tu.Message(tu.ID(id), part.escaped).WithParseMode(telego.ModeMarkdownV2)
		if tid != 0 {
			params = params.WithMessageThreadID(tid)
		}
		if i == 0 && replyTo != "" {
			if mid, err := strconv.Atoi(replyTo); err == nil {
				params = params.WithReplyParameters(&telego.ReplyParameters{MessageID: mid})
			}
		}

		if _, err := sendOne(ctx, api, params, part.plain); err != nil {
			return fmt.Errorf("telegram: send chunk %d/%d: %w", i+1, len(parts), err)
		}

		if i < len(parts)-1 {
			if !sleepCtx(ctx, interChunkDelay) {
				return ctx.Err()
			}
		}
	}
	return nil
}

// escapeChunk escapes chunk (a piece Split already bounded to limit bytes
// *before* escaping) for MarkdownV2 and, if the escaped result still
// exceeds limit, re-splits the plain chunk at a reduced limit and recurses -
// EscapeMarkdownV2 can insert a backslash before every character, so a
// chunk within Split's own bound can still cross the wire limit only after
// escaping. The reduced limit is sized to the inflation ratio actually
// observed, with a guaranteed-safe floor (limit/2) for a pathological chunk
// whose escaped length is close to double: no character escapes to more
// than two bytes, so halving the plain-text budget always fits.
//
// Termination is not automatic: Split(chunk, reduced) is only guaranteed to
// shrink the input for ordinary text and ordinary fences. A fence whose
// opening line's info string alone is longer than limit makes splitFence
// clamp its budget to 1 byte, so every piece it emits is
// overhead-dominated and, for a short enough inner body, comes back
// identical to chunk - recursing on that would never terminate. The
// len(pieces) == 1 && no-shorter guard below detects exactly that case and
// falls back to a raw hard cut instead of recursing again.
func escapeChunk(chunk string, limit int) []escapedPart {
	escaped := EscapeMarkdownV2(chunk)
	if len(escaped) <= limit {
		return []escapedPart{{escaped: escaped, plain: chunk}}
	}

	reduced := limit * len(chunk) / len(escaped)
	if reduced <= 0 || reduced >= limit {
		reduced = limit / 2
	}
	if reduced < 1 {
		reduced = 1
	}

	pieces := Split(chunk, reduced)
	if len(pieces) == 1 && len(pieces[0]) >= len(chunk) {
		return hardCutParts(chunk, limit)
	}

	var out []escapedPart
	for _, piece := range pieces {
		out = append(out, escapeChunk(piece, limit)...)
	}
	return out
}

// hardCutParts splits chunk into limit-byte, rune-safe pieces without any
// regard for fence or paragraph structure, and returns each as an
// unescaped escapedPart (escaped == plain): escapeChunk's last resort when
// Split itself cannot make the input any shorter, so recursion is
// guaranteed to make progress instead of looping forever. Sending an
// unescaped piece under MarkdownV2 risks one extra parse-error round trip -
// sendOne already falls back to plain text on that - which is a small price
// for a chunk that is otherwise unsendable at all.
func hardCutParts(chunk string, limit int) []escapedPart {
	var out []escapedPart
	remaining := chunk
	for len(remaining) > limit {
		cut := safeRuneCut(remaining, limit)
		out = append(out, escapedPart{escaped: remaining[:cut], plain: remaining[:cut]})
		remaining = remaining[cut:]
	}
	if remaining != "" {
		out = append(out, escapedPart{escaped: remaining, plain: remaining})
	}
	return out
}

// sendOne sends one already-built SendMessageParams, retrying on a 429 by
// honoring retry_after, retrying once on a 5xx API error, and falling back
// to plain (unescaped, no parse_mode) text on an HTTP 400 whose description
// names parsing or message length as the problem - the model's output is
// not reliably valid (or short enough) MarkdownV2, and the message must
// still be delivered even when it is not.
//
// A non-API (network-level) error is deliberately not retried: unlike a
// 5xx, Telegram may have already accepted and delivered the message before
// the error surfaced (a response-read timeout, a connection reset after the
// request was written, an h2 GOAWAY mid-response), and Telegram has no
// idempotency key - retrying would risk sending the same message, or the
// same approval prompt, twice.
func sendOne(ctx context.Context, api botAPI, params *telego.SendMessageParams, plain string) (*telego.Message, error) {
	transientRetried := false
	for attempt := 0; attempt < maxSendAttempts; attempt++ {
		msg, err := api.SendMessage(ctx, params)
		if err == nil {
			return msg, nil
		}

		var apiErr *ta.Error
		if errors.As(err, &apiErr) {
			if apiErr.ErrorCode == http.StatusTooManyRequests && apiErr.Parameters != nil && apiErr.Parameters.RetryAfter > 0 {
				if !sleepCtx(ctx, time.Duration(apiErr.Parameters.RetryAfter)*time.Second) {
					return nil, ctx.Err()
				}
				continue
			}
			if apiErr.ErrorCode == http.StatusBadRequest && params.ParseMode != "" &&
				(strings.Contains(strings.ToLower(apiErr.Description), "parse") || strings.Contains(strings.ToLower(apiErr.Description), "too long")) {
				fallback := *params
				fallback.ParseMode = ""
				fallback.Text = plain
				params = &fallback
				continue
			}
			if apiErr.ErrorCode >= http.StatusInternalServerError && !transientRetried && ctx.Err() == nil {
				transientRetried = true
				if !sleepCtx(ctx, transientRetryDelay) {
					return nil, ctx.Err()
				}
				continue
			}
		}
		return nil, err
	}
	return nil, fmt.Errorf("telegram: exceeded %d send attempts", maxSendAttempts)
}

// sleepCtx waits for d or ctx cancellation, whichever comes first,
// reporting false if ctx ended the wait early.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// parseThreadID converts a session's string thread id back to the int
// Telegram's message_thread_id parameter wants; "" (or anything
// unparsable) means "no thread", i.e. the chat's general timeline.
func parseThreadID(threadID string) int {
	if threadID == "" {
		return 0
	}
	n, err := strconv.Atoi(threadID)
	if err != nil {
		return 0
	}
	return n
}

// sendTyping issues one sendChatAction: typing call. Telegram's typing
// indicator expires after ~5s; callers that want it to persist for a long
// turn are responsible for calling this again every ~4s (see the phase 6
// plan's send.go step) - this function only issues one occurrence.
func sendTyping(ctx context.Context, api botAPI, chatID, threadID string) error {
	id, err := strconv.ParseInt(chatID, 10, 64)
	if err != nil {
		return fmt.Errorf("telegram: invalid chat id %q: %w", chatID, err)
	}
	params := &telego.SendChatActionParams{ChatID: tu.ID(id), Action: telego.ChatActionTyping}
	if tid := parseThreadID(threadID); tid != 0 {
		params.MessageThreadID = tid
	}
	return api.SendChatAction(ctx, params)
}
