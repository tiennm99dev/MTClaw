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

// htmlPart is one Telegram HTML string guaranteed to fit within the caller's
// byte limit, paired with the plain-text source it was rendered from -
// sendOne falls back to plain on an HTML parse-mode rejection instead of
// dropping the reply.
type htmlPart struct {
	html  string
	plain string
}

// sendText chunks text (see split), renders each chunk to Telegram HTML
// (see renderHTML), and sends the result serially, falling back to no
// parse_mode on an HTTP 400 that names parsing (or the message being too
// long) as the problem. Only the first part carries replyTo, since a
// multi-part reply is one logical message split across several Telegram
// messages, not a chain of independent quotes. threadID, when non-empty, is
// applied to every part so a forum-topic reply stays entirely inside its
// topic.
func sendText(ctx context.Context, api botAPI, chatID, threadID, text, replyTo string) error {
	if text == "" {
		return nil
	}
	id, err := strconv.ParseInt(chatID, 10, 64)
	if err != nil {
		return fmt.Errorf("telegram: invalid chat id %q: %w", chatID, err)
	}
	tid := parseThreadID(threadID)

	parts := renderChunks(text, DefaultChunkLimit)

	for i, part := range parts {
		params := tu.Message(tu.ID(id), part.html).WithParseMode(telego.ModeHTML)
		if tid != 0 {
			params = params.WithMessageThreadID(tid)
		}
		if i == 0 && replyTo != "" {
			if mid, err := strconv.Atoi(replyTo); err == nil {
				params = params.WithReplyParameters(&telego.ReplyParameters{
					MessageID:                mid,
					AllowSendingWithoutReply: true,
				})
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

// renderChunks splits text into markdown-source chunks (split) and renders
// each to HTML, verifying every rendered chunk actually fits limit bytes -
// HTML tag overhead (especially a fenced code block's <pre><code
// class="language-x">) can inflate a chunk that fit as raw Markdown past
// the limit, so split's boundary is a starting point, not a guarantee. A
// part with no visible content once its tags are stripped (an empty fence
// "```\n```", or a hard-cut piece that landed on whitespace) is dropped
// instead of sent: Telegram rejects it with a 400 "message text is empty",
// whose description names neither "parse" nor "too long", so nothing in
// sendOne would otherwise recover from it, and every chunk after it in the
// reply would be silently dropped along with it.
func renderChunks(text string, limit int) []htmlPart {
	var out []htmlPart
	for _, chunk := range split(text, limit) {
		for _, part := range fitHTML(chunk, limit) {
			if isVisiblyEmpty(part.html) {
				continue
			}
			out = append(out, part)
		}
	}
	return out
}

// stripHTMLTags removes every "<...>" span from html, leaving only its text
// content. This is safe specifically for this package's own output (not a
// general HTML sanitizer): renderHTML always escapes a literal '<' or '>'
// found in real content to "&lt;"/"&gt;" before it ever reaches here, so
// every unescaped '<' or '>' still present is one of the tags - <b>, <i>,
// <code>, <pre>, <a href="...">, and their closes - this renderer itself
// emitted.
func stripHTMLTags(html string) string {
	var b strings.Builder
	inTag := false
	for _, r := range html {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			inTag = false
		case !inTag:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// isVisiblyEmpty reports whether html has no visible content once its tags
// are stripped - see renderChunks for why such a part must never be sent.
func isVisiblyEmpty(html string) bool {
	return strings.TrimSpace(stripHTMLTags(html)) == ""
}

// fitHTML renders chunk and, if the result exceeds limit, re-splits chunk
// (at a budget scaled to the inflation ratio just observed) and recurses -
// preferring split's markdown-aware boundaries for as long as they keep
// shrinking the input, and falling back to hardCutHTMLParts only once they
// stop making progress (a single token whose rendered form cannot be
// shortened by cutting elsewhere in the surrounding text).
func fitHTML(chunk string, limit int) []htmlPart {
	html := renderHTML(chunk)
	if len(html) <= limit {
		return []htmlPart{{html: html, plain: chunk}}
	}

	reduced := limit * len(chunk) / len(html)
	if reduced <= 0 || reduced >= limit {
		reduced = limit / 2
	}
	if reduced < 1 {
		reduced = 1
	}

	pieces := split(chunk, reduced)
	if !allPiecesShrink(pieces, chunk) {
		return hardCutHTMLParts(chunk, limit)
	}

	var out []htmlPart
	for _, piece := range pieces {
		out = append(out, fitHTML(piece, limit)...)
	}
	return out
}

// allPiecesShrink reports whether split actually made progress: every piece
// must be strictly shorter than chunk, or fitHTML's recursion above has no
// guarantee of terminating - a piece at least as long as chunk (split
// finding no boundary to cut at, or returning chunk unchanged alongside a
// spurious empty piece) would recurse on the same input forever, ending in
// a stack overflow no recover() can catch. len(pieces) == 0 can only happen
// for chunk == "" and reports false too, since fitHTML above already
// returns before ever reaching here in that case (rendering "" fits any
// positive limit).
func allPiecesShrink(pieces []string, chunk string) bool {
	if len(pieces) == 0 {
		return false
	}
	for _, p := range pieces {
		if len(p) >= len(chunk) {
			return false
		}
	}
	return true
}

// hardCutHTMLParts is fitHTML's last resort: it cuts chunk into rune-safe
// pieces with no markdown awareness at all, escaping each as plain text (no
// tags, so escaping is the only thing that can inflate it) and shrinking
// the cut point whenever escaping still pushes a piece over limit - e.g. a
// run of bare "&" characters, which each expand to "&amp;" (5 bytes) - so
// every emitted piece is verified to fit, not assumed to.
func hardCutHTMLParts(chunk string, limit int) []htmlPart {
	var out []htmlPart
	remaining := chunk
	for len(remaining) > 0 {
		cut := safeRuneCut(remaining, limit)
		for cut > 0 {
			piece := remaining[:cut]
			html := escapeHTMLText(piece)
			if len(html) <= limit {
				out = append(out, htmlPart{html: html, plain: piece})
				remaining = remaining[cut:]
				break
			}
			next := safeRuneCut(remaining, cut-1)
			if next >= cut {
				// A single rune's escaped form alone exceeds limit (an
				// unreasonably small limit): emit it anyway instead of
				// looping forever: Telegram will reject a limit this small
				// regardless of what this function does.
				out = append(out, htmlPart{html: html, plain: piece})
				remaining = remaining[cut:]
				break
			}
			cut = next
		}
	}
	return out
}

// sendOne sends one already-built SendMessageParams, retrying on a 429 by
// honoring retry_after, retrying once on a 5xx API error, and falling back
// to plain (unescaped, no parse_mode) text on any HTTP 400 while parse_mode
// is set - the model's output is not reliably valid HTML (an unbalanced or
// unsupported tag nesting, an href Telegram's parser happens to reject),
// and the message must still be delivered even when it is not. This used to
// require the error's description to mention "parse" or "too long", but
// Telegram's exact wording is not a documented, stable contract, and a 400
// that names neither (e.g. "message text is empty") would otherwise never
// retry and would drop every later chunk of the reply. Falling back
// unconditionally on 400 is safe: the retried attempt clears ParseMode, so
// this same branch cannot fire on it again, and a 400 caused by something
// parse_mode cannot fix (an invalid chat id) simply reproduces the same
// error with ParseMode already empty, returned to the caller as normal.
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
			if apiErr.ErrorCode == http.StatusBadRequest && params.ParseMode != "" {
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
// turn are responsible for calling this again every ~4s.
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
