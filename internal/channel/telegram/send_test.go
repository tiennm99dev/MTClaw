package telegram

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mymmrac/telego"
	ta "github.com/mymmrac/telego/telegoapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- send fallback ---------------------------------------------------------

func TestSendOne_FallsBackToPlainTextOnParseError(t *testing.T) {
	calls := 0
	api := &fakeBotAPI{
		sendFunc: func(params *telego.SendMessageParams) (*telego.Message, error) {
			calls++
			if calls == 1 {
				return nil, &ta.Error{ErrorCode: 400, Description: "Bad Request: can't parse entities"}
			}
			assert.Empty(t, params.ParseMode, "the retry must drop parse_mode entirely")
			return &telego.Message{MessageID: 1}, nil
		},
	}

	params := &telego.SendMessageParams{ChatID: telego.ChatID{ID: 100}, Text: "<b>bad html", ParseMode: telego.ModeHTML}
	msg, err := sendOne(context.Background(), api, params, "bad html")
	require.NoError(t, err)
	require.NotNil(t, msg)
	assert.Equal(t, 2, calls, "exactly one retry: the message is delivered exactly once")
}

// TestSendOne_FallsBackToPlainTextOnTooLongError covers the other 400
// wording Telegram uses when a chunk somehow still lands over 4096 chars:
// "message is too long" names no parsing problem, but must still degrade
// to plain text instead of dropping the reply.
func TestSendOne_FallsBackToPlainTextOnTooLongError(t *testing.T) {
	calls := 0
	api := &fakeBotAPI{
		sendFunc: func(params *telego.SendMessageParams) (*telego.Message, error) {
			calls++
			if calls == 1 {
				return nil, &ta.Error{ErrorCode: 400, Description: "Bad Request: message is too long"}
			}
			assert.Empty(t, params.ParseMode, "the retry must drop parse_mode entirely")
			return &telego.Message{MessageID: 1}, nil
		},
	}

	params := &telego.SendMessageParams{ChatID: telego.ChatID{ID: 100}, Text: "rendered html", ParseMode: telego.ModeHTML}
	msg, err := sendOne(context.Background(), api, params, "plain text")
	require.NoError(t, err)
	require.NotNil(t, msg)
	assert.Equal(t, 2, calls)
}

// TestSendOne_FallsBackToPlainTextOnAny400WhileParseModeSet proves the
// fallback no longer depends on Telegram's exact wording naming "parse" or
// "too long": a 400 like "message text is empty" - which names neither -
// must still degrade to plain text (which, for a genuinely non-empty plain
// source, no longer reproduces the same empty-text rejection) instead of
// permanently failing the whole chunk and, in sendText, every chunk after
// it in the same reply.
func TestSendOne_FallsBackToPlainTextOnAny400WhileParseModeSet(t *testing.T) {
	calls := 0
	api := &fakeBotAPI{
		sendFunc: func(params *telego.SendMessageParams) (*telego.Message, error) {
			calls++
			if calls == 1 {
				return nil, &ta.Error{ErrorCode: 400, Description: "Bad Request: message text is empty"}
			}
			assert.Empty(t, params.ParseMode, "the retry must drop parse_mode entirely")
			return &telego.Message{MessageID: 1}, nil
		},
	}

	params := &telego.SendMessageParams{ChatID: telego.ChatID{ID: 100}, Text: "<pre><code></code></pre>", ParseMode: telego.ModeHTML}
	msg, err := sendOne(context.Background(), api, params, "not actually empty")
	require.NoError(t, err)
	require.NotNil(t, msg)
	assert.Equal(t, 2, calls)
}

// TestSendOne_400WithoutParseModeIsNotRetried proves the broadened fallback
// only fires while parse_mode is actually set: a 400 reached with no
// parse_mode at all (already a plain-text send, or the retry attempt this
// same function just made) has nothing left to fall back to, and must
// surface as a normal error instead of looping.
func TestSendOne_400WithoutParseModeIsNotRetried(t *testing.T) {
	calls := 0
	api := &fakeBotAPI{
		sendFunc: func(params *telego.SendMessageParams) (*telego.Message, error) {
			calls++
			return nil, &ta.Error{ErrorCode: 400, Description: "Bad Request: chat not found"}
		},
	}

	params := &telego.SendMessageParams{ChatID: telego.ChatID{ID: 100}, Text: "hi"}
	_, err := sendOne(context.Background(), api, params, "hi")
	require.Error(t, err)
	assert.Equal(t, 1, calls, "a 400 with no parse_mode to drop must not be retried at all")
}

// TestSendOne_NetworkErrorIsNotRetried proves a raw network error (unlike a
// 5xx, which Telegram never accepted) is not retried: Telegram may have
// already delivered the message before the error surfaced, and Telegram has
// no idempotency key, so retrying risks a user-visible duplicate send (or a
// second, unresolved approval prompt).
func TestSendOne_NetworkErrorIsNotRetried(t *testing.T) {
	calls := 0
	api := &fakeBotAPI{
		sendFunc: func(params *telego.SendMessageParams) (*telego.Message, error) {
			calls++
			return nil, errors.New("connection reset by peer")
		},
	}

	params := &telego.SendMessageParams{ChatID: telego.ChatID{ID: 100}, Text: "hi"}
	_, err := sendOne(context.Background(), api, params, "hi")
	require.Error(t, err)
	assert.Equal(t, 1, calls, "a non-API network error must not be retried - Telegram may have already delivered the message")
}

// --- Channel.Send thread routing / reply-without-target -------------------

func TestSendText_CarriesMessageThreadID(t *testing.T) {
	api := &fakeBotAPI{}
	err := sendText(context.Background(), api, discardLog(), "100", "42", "hello", "")
	require.NoError(t, err)

	params := api.lastSent()
	require.NotNil(t, params)
	assert.Equal(t, 42, params.MessageThreadID)
}

func TestSendText_EmptyThreadIDTargetsGeneralTimeline(t *testing.T) {
	api := &fakeBotAPI{}
	err := sendText(context.Background(), api, discardLog(), "100", "", "hello", "")
	require.NoError(t, err)

	params := api.lastSent()
	require.NotNil(t, params)
	assert.Equal(t, 0, params.MessageThreadID)
}

// TestSendText_UsesHTMLParseMode proves Channel.Send now sends parse_mode
// HTML (the headline decision), not MarkdownV2.
func TestSendText_UsesHTMLParseMode(t *testing.T) {
	api := &fakeBotAPI{}
	require.NoError(t, sendText(context.Background(), api, discardLog(), "100", "", "**bold**", ""))

	params := api.lastSent()
	require.NotNil(t, params)
	assert.Equal(t, telego.ModeHTML, params.ParseMode)
	assert.Equal(t, "<b>bold</b>", params.Text)
}

// TestSendText_ReplyAllowsSendingWithoutReply proves a reply to a message
// that may have been deleted while the model was thinking is still sent,
// not silently refused by Telegram's 400 "message to be replied not
// found".
func TestSendText_ReplyAllowsSendingWithoutReply(t *testing.T) {
	api := &fakeBotAPI{}
	require.NoError(t, sendText(context.Background(), api, discardLog(), "100", "", "hello", "555"))

	params := api.lastSent()
	require.NotNil(t, params)
	require.NotNil(t, params.ReplyParameters)
	assert.Equal(t, 555, params.ReplyParameters.MessageID)
	assert.True(t, params.ReplyParameters.AllowSendingWithoutReply)
}

// --- chunking / rendering fit -------------------------------------------

// TestRenderChunks_EveryChunkFitsAndReassembles is a broad boundary/rune
// table test: dense HTML-inflating text (every character escapes) must
// still produce only chunks that fit DefaultChunkLimit once rendered, and
// concatenating every chunk's plain source must reassemble the original
// text losslessly.
func TestRenderChunks_EveryChunkFitsAndReassembles(t *testing.T) {
	var b strings.Builder
	for b.Len() < 20_000 {
		b.WriteString("a & b < c > d & e < f > g ")
	}
	text := b.String()

	parts := renderChunks(text, DefaultChunkLimit)
	require.NotEmpty(t, parts)

	var reassembled strings.Builder
	for i, p := range parts {
		assert.LessOrEqualf(t, len(p.html), DefaultChunkLimit, "chunk %d rendered to %d bytes, over the limit", i, len(p.html))
		reassembled.WriteString(p.plain)
	}
	assert.Equal(t, text, reassembled.String())
}

// TestRenderChunks_MultibyteRunesNeverSplitMidCharacter drives a hard cut
// through dense multibyte content (CJK plus emoji, which in UTF-8 are 3 and
// 4 bytes respectively) and proves every chunk's plain source is valid
// UTF-8 - a hard cut landing mid-rune would corrupt one of the pieces.
func TestRenderChunks_MultibyteRunesNeverSplitMidCharacter(t *testing.T) {
	var b strings.Builder
	for b.Len() < 12_000 {
		b.WriteString("你好\U0001F600世界")
	}
	text := b.String()

	parts := renderChunks(text, DefaultChunkLimit)
	require.NotEmpty(t, parts)
	for i, p := range parts {
		assert.Truef(t, strings.ToValidUTF8(p.plain, "�") == p.plain, "chunk %d plain text is not valid UTF-8", i)
		assert.Truef(t, strings.ToValidUTF8(p.html, "�") == p.html, "chunk %d html is not valid UTF-8", i)
	}

	var reassembled strings.Builder
	for _, p := range parts {
		reassembled.WriteString(p.plain)
	}
	assert.Equal(t, text, reassembled.String())
}

func TestFitHTML_FitsWithoutSplittingWhenAlreadyUnderLimit(t *testing.T) {
	chunk := "just plain prose with no special characters at all"
	parts := fitHTML(chunk, DefaultChunkLimit)
	require.Len(t, parts, 1)
	assert.Equal(t, chunk, parts[0].plain)
	assert.Equal(t, chunk, parts[0].html)
}

// TestFitHTML_ReSplitsWhenRenderingInflatesPastLimit proves a chunk whose
// raw length is exactly at the limit, but whose HTML rendering inflates it
// past that (every character is "&", 5x inflation), comes back as more than
// one part, each independently fitting.
func TestFitHTML_ReSplitsWhenRenderingInflatesPastLimit(t *testing.T) {
	chunk := strings.Repeat("&", DefaultChunkLimit)
	parts := fitHTML(chunk, DefaultChunkLimit)

	require.Greater(t, len(parts), 1)
	var plainTotal strings.Builder
	for _, p := range parts {
		assert.LessOrEqual(t, len(p.html), DefaultChunkLimit)
		assert.Equal(t, renderHTML(p.plain), p.html)
		plainTotal.WriteString(p.plain)
	}
	assert.Equal(t, chunk, plainTotal.String())
}

// TestHardCutHTMLParts_EscapeInflationNeverCrossesLimit is a direct test of
// the last-resort path: every rune is "&" (5x inflation once escaped), so a
// naive rune-safe cut at exactly limit bytes would overflow once escaped.
// hardCutHTMLParts must shrink the cut until the escaped form actually
// fits.
// TestAllPiecesShrink_RejectsAPieceNotStrictlyShorterThanChunk is a direct
// unit test of fitHTML's recursion-progress guard: with any piece at least
// as long as chunk (split making no progress, or handing back the same
// content it was given), fitHTML must fall back to hardCutHTMLParts instead
// of recursing on an unchanged input forever.
func TestAllPiecesShrink_RejectsAPieceNotStrictlyShorterThanChunk(t *testing.T) {
	assert.False(t, allPiecesShrink(nil, "chunk"), "no pieces at all is not progress")
	assert.False(t, allPiecesShrink([]string{"chunk"}, "chunk"), "a single piece equal to chunk is not progress")
	assert.False(t, allPiecesShrink([]string{"short", ""}, "short"), "a trailing empty piece next to one equal in length to chunk is not progress")
	assert.True(t, allPiecesShrink([]string{"ab", "cd"}, "abcd"), "two pieces each strictly shorter than chunk is progress")
}

// TestFitHTML_TinyFenceBudgetTerminatesInsteadOfRecursingForever reproduces
// the fix report's exact repro: a fenced code block whose budget (once
// fence overhead is subtracted from a small limit) leaves splitFence
// nothing to cut a wide rune at, which used to make splitByLineThenHardCut
// emit a spurious empty trailing piece and fitHTML recurse on the
// unchanged chunk forever - a fatal, unrecoverable stack overflow, not a
// panic. This test reaching an assertion at all (rather than crashing the
// whole test binary) is itself the regression proof.
func TestFitHTML_TinyFenceBudgetTerminatesInsteadOfRecursingForever(t *testing.T) {
	cases := []struct {
		name  string
		chunk string
		limit int
	}{
		{"single wide rune, budget below one rune", "```go\n\U0001F600\n```", 40},
		{"long lang tag, budget below one rune", "```" + strings.Repeat("x", 64) + "\n\U0001F600\U0001F600\n```", 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Reaching this assertion at all - rather than the whole test
			// binary crashing with a stack overflow - is most of the proof;
			// re-fenced pieces of a split code block are not literal slices
			// of chunk (each carries its own reopened "```lang"/"```"
			// wrapper), so this checks the fitted-html contract instead of
			// byte-for-byte reassembly.
			parts := fitHTML(tc.chunk, tc.limit)
			require.NotEmpty(t, parts)
			for i, p := range parts {
				assert.LessOrEqualf(t, len(p.html), tc.limit, "part %d rendered to %d bytes, over the limit", i, len(p.html))
			}
		})
	}
}

// TestRenderChunks_DropsVisiblyEmptyParts proves an empty fence (rendering
// to "<pre><code></code></pre>", which has no visible text once tags are
// stripped) is dropped instead of reaching sendText as its own part - which
// would otherwise hand Telegram a message it rejects with a 400 "message
// text is empty", an error naming neither "parse" nor "too long". Two
// limit-sized filler paragraphs surround the empty fence so split keeps it
// as its own chunk instead of packing it alongside visible text (which
// would hide the very case this test exists to catch).
func TestRenderChunks_DropsVisiblyEmptyParts(t *testing.T) {
	filler := strings.Repeat("a", DefaultChunkLimit)
	text := filler + "\n```\n```\n" + filler

	parts := renderChunks(text, DefaultChunkLimit)

	require.Len(t, parts, 2, "the visibly-empty fence chunk must be dropped, leaving only the two filler paragraphs")
	for _, p := range parts {
		assert.NotEmpty(t, strings.TrimSpace(stripHTMLTags(p.html)), "no part may render to nothing visible")
		assert.Equal(t, filler, p.plain)
	}
}

// TestIsVisiblyEmpty_WhitespaceOnlyContentCountsAsEmpty proves a hard-cut
// piece that happens to land on nothing but whitespace - which renders to
// plain, tag-free text like " " rather than an empty <pre><code> - is
// caught by the same check, not just the fenced-code case.
func TestIsVisiblyEmpty_WhitespaceOnlyContentCountsAsEmpty(t *testing.T) {
	assert.True(t, isVisiblyEmpty("   \n\t "))
	assert.True(t, isVisiblyEmpty(""))
	assert.True(t, isVisiblyEmpty("<pre><code></code></pre>"))
	assert.False(t, isVisiblyEmpty("<b>hi</b>"))
	assert.False(t, isVisiblyEmpty("hi"))
}

func TestHardCutHTMLParts_EscapeInflationNeverCrossesLimit(t *testing.T) {
	chunk := strings.Repeat("&", 500)
	parts := hardCutHTMLParts(chunk, 50)

	require.NotEmpty(t, parts)
	var plainTotal strings.Builder
	for _, p := range parts {
		assert.LessOrEqual(t, len(p.html), 50)
		plainTotal.WriteString(p.plain)
	}
	assert.Equal(t, chunk, plainTotal.String())
}

// --- real-transport 5xx retry ----------------------------------------------

// testBotToken satisfies telego's token format check; the httptest servers
// below accept any token.
const testBotToken = "123456789:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

const okMessageJSON = `{"ok":true,"result":{"message_id":7,"date":1,"chat":{"id":100,"type":"private"}}}`

// newTestBot builds a real *telego.Bot through newBot, pointed at an
// httptest server running handler, so the production transport (and its
// error typing) is what the test exercises.
func newTestBot(t *testing.T, handler http.HandlerFunc) *telego.Bot {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	bot, err := newBot(testBotToken, srv.URL, telego.WithDiscardLogger())
	require.NoError(t, err)
	return bot
}

// TestSendOne_RetriesOnceOn5xxThenSucceeds drives sendOne through a real
// bot against a server answering 502 then 200. A hand-built *ta.Error would
// pass even if the production transport never produced one.
func TestSendOne_RetriesOnceOn5xxThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	bot := newTestBot(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"ok":false,"error_code":502,"description":"Bad Gateway"}`))
			return
		}
		_, _ = w.Write([]byte(okMessageJSON))
	})

	params := &telego.SendMessageParams{ChatID: telego.ChatID{ID: 100}, Text: "hi"}
	start := time.Now()
	msg, err := sendOne(context.Background(), bot, params, "hi")
	require.NoError(t, err)
	require.NotNil(t, msg)
	assert.Equal(t, int32(2), calls.Load(), "a 5xx gets exactly one bounded retry")
	assert.GreaterOrEqual(t, time.Since(start), transientRetryDelay)
}

func TestSendOne_GivesUpAfterOneTransientRetry(t *testing.T) {
	var calls atomic.Int32
	bot := newTestBot(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	})

	params := &telego.SendMessageParams{ChatID: telego.ChatID{ID: 100}, Text: "hi"}
	_, err := sendOne(context.Background(), bot, params, "hi")
	require.Error(t, err)
	var apiErr *ta.Error
	require.ErrorAs(t, err, &apiErr, "a 5xx must surface as a typed API error")
	assert.Equal(t, http.StatusServiceUnavailable, apiErr.ErrorCode)
	assert.Equal(t, int32(2), calls.Load(), "exactly one retry, not a retry storm against a persistently failing backend")
}

// TestAPICaller_DecodesAPIErrorBodyOnNon5xx proves a 4xx keeps its decoded
// JSON error (retry_after included), which the 429 handling depends on.
func TestAPICaller_DecodesAPIErrorBodyOnNon5xx(t *testing.T) {
	bot := newTestBot(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":3}}`))
	})

	_, err := bot.SendMessage(context.Background(), &telego.SendMessageParams{ChatID: telego.ChatID{ID: 100}, Text: "hi"})
	var apiErr *ta.Error
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, http.StatusTooManyRequests, apiErr.ErrorCode)
	require.NotNil(t, apiErr.Parameters)
	assert.Equal(t, 3, apiErr.Parameters.RetryAfter)
}

// --- retry_after beyond the caller's deadline -------------------------------

func TestSendOne_HonorsRetryAfterBeyondCallerDeadline(t *testing.T) {
	calls := 0
	api := &fakeBotAPI{
		sendFunc: func(*telego.SendMessageParams) (*telego.Message, error) {
			calls++
			if calls == 1 {
				return nil, &ta.Error{ErrorCode: 429, Parameters: &ta.ResponseParameters{RetryAfter: 1}}
			}
			return &telego.Message{MessageID: 1}, nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	params := &telego.SendMessageParams{ChatID: telego.ChatID{ID: 100}, Text: "hi"}
	msg, err := sendOne(ctx, api, params, "hi")
	require.NoError(t, err, "a flood wait longer than the reply budget must extend the budget, not drop the reply")
	require.NotNil(t, msg)
	assert.Equal(t, 2, calls)
}

func TestSendOne_RetryAfterOverCapIsNotWaited(t *testing.T) {
	calls := 0
	api := &fakeBotAPI{
		sendFunc: func(*telego.SendMessageParams) (*telego.Message, error) {
			calls++
			return nil, &ta.Error{ErrorCode: 429, Parameters: &ta.ResponseParameters{RetryAfter: int(maxRetryAfter/time.Second) + 1}}
		},
	}
	params := &telego.SendMessageParams{ChatID: telego.ChatID{ID: 100}, Text: "hi"}
	start := time.Now()
	_, err := sendOne(context.Background(), api, params, "hi")
	require.Error(t, err)
	assert.Equal(t, 1, calls)
	assert.Less(t, time.Since(start), time.Second)
}

func TestSendOne_CancelDuringRetryAfterStopsWaiting(t *testing.T) {
	api := &fakeBotAPI{
		sendFunc: func(*telego.SendMessageParams) (*telego.Message, error) {
			return nil, &ta.Error{ErrorCode: 429, Parameters: &ta.ResponseParameters{RetryAfter: 30}}
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)

	params := &telego.SendMessageParams{ChatID: telego.ChatID{ID: 100}, Text: "hi"}
	start := time.Now()
	_, err := sendOne(ctx, api, params, "hi")
	require.ErrorIs(t, err, context.Canceled)
	assert.Less(t, time.Since(start), 2*time.Second)
}

// TestSendText_LaterChunksSurviveAnExtendedFloodWait proves the extended
// deadline carries over to the chunks after the one that was rate limited.
func TestSendText_LaterChunksSurviveAnExtendedFloodWait(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	api := &fakeBotAPI{
		sendFunc: func(*telego.SendMessageParams) (*telego.Message, error) {
			mu.Lock()
			defer mu.Unlock()
			calls++
			if calls == 1 {
				return nil, &ta.Error{ErrorCode: 429, Parameters: &ta.ResponseParameters{RetryAfter: 1}}
			}
			return &telego.Message{MessageID: calls}, nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	text := strings.Repeat("word ", 1500) // two chunks
	require.NoError(t, sendText(ctx, api, discardLog(), "100", "", text, ""))
	assert.Equal(t, 3, calls, "429 retry plus two chunks")
}

// --- incomplete reply notice ------------------------------------------------

func TestSendText_MidReplyFailureSendsIncompleteNotice(t *testing.T) {
	calls := 0
	api := &fakeBotAPI{
		sendFunc: func(*telego.SendMessageParams) (*telego.Message, error) {
			calls++
			if calls == 2 {
				return nil, &ta.Error{ErrorCode: 403, Description: "Forbidden"}
			}
			return &telego.Message{MessageID: calls}, nil
		},
	}
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))

	text := strings.Repeat("word ", 1500) // two chunks
	err := sendText(context.Background(), api, log, "100", "42", text, "")
	require.Error(t, err)

	last := api.lastSent()
	require.NotNil(t, last)
	assert.Equal(t, incompleteReplyNotice, last.Text)
	assert.Equal(t, 42, last.MessageThreadID, "the notice must stay in the reply's topic")
	assert.Contains(t, buf.String(), "reply incomplete")
}

func TestSendText_FirstChunkFailureSendsNoNotice(t *testing.T) {
	api := &fakeBotAPI{
		sendFunc: func(*telego.SendMessageParams) (*telego.Message, error) {
			return nil, &ta.Error{ErrorCode: 403, Description: "Forbidden"}
		},
	}
	require.Error(t, sendText(context.Background(), api, discardLog(), "100", "", "hello", ""))
	assert.Len(t, api.sent, 1, "nothing was delivered, so there is nothing to mark incomplete")
}
