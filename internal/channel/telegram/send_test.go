package telegram

import (
	"context"
	"errors"
	"strings"
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

func TestSendOne_RetriesOnceOn5xxThenSucceeds(t *testing.T) {
	calls := 0
	api := &fakeBotAPI{
		sendFunc: func(params *telego.SendMessageParams) (*telego.Message, error) {
			calls++
			if calls == 1 {
				return nil, &ta.Error{ErrorCode: 500, Description: "Internal Server Error"}
			}
			return &telego.Message{MessageID: 1}, nil
		},
	}

	params := &telego.SendMessageParams{ChatID: telego.ChatID{ID: 100}, Text: "hi"}
	start := time.Now()
	msg, err := sendOne(context.Background(), api, params, "hi")
	require.NoError(t, err)
	require.NotNil(t, msg)
	assert.Equal(t, 2, calls, "a 5xx gets exactly one bounded retry")
	assert.GreaterOrEqual(t, time.Since(start), transientRetryDelay)
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

func TestSendOne_GivesUpAfterOneTransientRetry(t *testing.T) {
	calls := 0
	api := &fakeBotAPI{
		sendFunc: func(params *telego.SendMessageParams) (*telego.Message, error) {
			calls++
			return nil, &ta.Error{ErrorCode: 503, Description: "Service Unavailable"}
		},
	}

	params := &telego.SendMessageParams{ChatID: telego.ChatID{ID: 100}, Text: "hi"}
	_, err := sendOne(context.Background(), api, params, "hi")
	require.Error(t, err)
	assert.Equal(t, 2, calls, "exactly one retry, not a retry storm against a persistently failing backend")
}

// --- Channel.Send thread routing / reply-without-target -------------------

func TestSendText_CarriesMessageThreadID(t *testing.T) {
	api := &fakeBotAPI{}
	err := sendText(context.Background(), api, "100", "42", "hello", "")
	require.NoError(t, err)

	params := api.lastSent()
	require.NotNil(t, params)
	assert.Equal(t, 42, params.MessageThreadID)
}

func TestSendText_EmptyThreadIDTargetsGeneralTimeline(t *testing.T) {
	api := &fakeBotAPI{}
	err := sendText(context.Background(), api, "100", "", "hello", "")
	require.NoError(t, err)

	params := api.lastSent()
	require.NotNil(t, params)
	assert.Equal(t, 0, params.MessageThreadID)
}

// TestSendText_UsesHTMLParseMode proves Channel.Send now sends parse_mode
// HTML (the headline decision), not MarkdownV2.
func TestSendText_UsesHTMLParseMode(t *testing.T) {
	api := &fakeBotAPI{}
	require.NoError(t, sendText(context.Background(), api, "100", "", "**bold**", ""))

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
	require.NoError(t, sendText(context.Background(), api, "100", "", "hello", "555"))

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
