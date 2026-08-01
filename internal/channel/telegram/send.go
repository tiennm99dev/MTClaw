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

// maxSendAttempts bounds the fallback/retry loop in sendOne: a 429 retry
// plus a parse-mode fallback retry is the realistic worst case, so this
// only exists to guarantee termination if Telegram keeps misbehaving.
const maxSendAttempts = 5

// sendText chunks text (see Split) and sends each chunk serially with
// MarkdownV2, falling back to no parse_mode on an HTTP 400 that names
// parsing as the problem. Only the first chunk carries replyTo, since a
// multi-chunk reply is one logical message split across several Telegram
// messages, not a chain of independent quotes. threadID, when non-empty, is
// applied to every chunk so a forum-topic reply stays entirely inside its
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

	chunks := Split(text, DefaultChunkLimit)
	for i, chunk := range chunks {
		params := tu.Message(tu.ID(id), EscapeMarkdownV2(chunk)).WithParseMode(telego.ModeMarkdownV2)
		if tid != 0 {
			params = params.WithMessageThreadID(tid)
		}
		if i == 0 && replyTo != "" {
			if mid, err := strconv.Atoi(replyTo); err == nil {
				params = params.WithReplyParameters(&telego.ReplyParameters{MessageID: mid})
			}
		}

		if _, err := sendOne(ctx, api, params, chunk); err != nil {
			return fmt.Errorf("telegram: send chunk %d/%d: %w", i+1, len(chunks), err)
		}

		if i < len(chunks)-1 {
			if !sleepCtx(ctx, interChunkDelay) {
				return ctx.Err()
			}
		}
	}
	return nil
}

// sendOne sends one already-built SendMessageParams, retrying on a 429 by
// honoring retry_after, and falling back to plain (unescaped, no
// parse_mode) text on an HTTP 400 whose description names parsing as the
// problem - the model's output is not reliably valid MarkdownV2, and the
// message must still be delivered even when it is not.
func sendOne(ctx context.Context, api botAPI, params *telego.SendMessageParams, plain string) (*telego.Message, error) {
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
			if apiErr.ErrorCode == http.StatusBadRequest && params.ParseMode != "" && strings.Contains(strings.ToLower(apiErr.Description), "parse") {
				fallback := *params
				fallback.ParseMode = ""
				fallback.Text = plain
				params = &fallback
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
