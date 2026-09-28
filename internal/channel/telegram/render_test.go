package telegram

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRenderHTML_Table(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain text unchanged", "hello world", "hello world"},
		{"bold", "**bold**", "<b>bold</b>"},
		{"bold underscore", "__bold__", "<b>bold</b>"},
		{"italic star", "*italic*", "<i>italic</i>"},
		{"italic underscore", "_italic_", "<i>italic</i>"},
		{"inline code", "run `ls -la` now", "run <code>ls -la</code> now"},
		{"link", "[docs](https://example.com/a?b=1)", `<a href="https://example.com/a?b=1">docs</a>`},
		{"heading", "# Title", "<b>Title</b>"},
		{"heading level 2", "## Sub", "<b>Sub</b>"},
		{"not a heading - no space", "#nohash", "#nohash"},
		{"not a heading - hashtag mid text", "see #1 issue", "see #1 issue"},
		{"list bullet preserved literally", "- item one", "- item one"},
		{"numbered list preserved literally", "1. first", "1. first"},
		{"ampersand escaped", "a & b", "a &amp; b"},
		{"angle brackets escaped", "1 < 2 and 2 > 1", "1 &lt; 2 and 2 &gt; 1"},
		{"html-looking text is neutralized", "<script>alert(1)</script>", "&lt;script&gt;alert(1)&lt;/script&gt;"},
		{"html-looking text inside bold", "**<b>fake</b>**", "<b>&lt;b&gt;fake&lt;/b&gt;</b>"},

		// --- nested markdown ---
		{"bold containing italic", "**bold *and italic* end**", "<b>bold <i>and italic</i> end</b>"},
		{"italic containing bold", "*italic **and bold** end*", "<i>italic <b>and bold</b> end</i>"},
		{"bold containing code", "**see `x` here**", "<b>see <code>x</code> here</b>"},

		// --- unbalanced markdown falls back to literal ---
		{"unbalanced bold - no close", "**never closes", "**never closes"},
		{"unbalanced italic - no close", "*never closes", "*never closes"},
		{"unbalanced inline code - no close", "run `never closes", "run `never closes"},
		{"lone double-star is not empty bold", "****", "****"},
		{"single star followed by double star not misread", "**bold**not italic*", "<b>bold</b>not italic*"},

		// --- backticks inside code (different-length delimiter) ---
		{"double backtick wraps single backtick", "``code ` here``", "<code>code ` here</code>"},
		{"double backtick trims one surrounding space", "`` `x` ``", "<code>`x`</code>"},

		// --- CommonMark intraword emphasis: underscores never, asterisks
		// always (per spec, unlike underscores) ---
		{"intraword single underscores never emphasis", "MAX_READ_BYTES and file_name_here", "MAX_READ_BYTES and file_name_here"},
		// Per CommonMark emphasis rules 6/8 (the "__" analog of the
		// single-underscore rules 2/4 used above), the closing "__" here is
		// right-flanking and not left-flanking (followed by "." - end of
		// word - counts the same as end-of-string), so it can close
		// regardless of what precedes it; the opening "__" is at the very
		// start of the string, which counts as being preceded by
		// whitespace, so it is not right-flanking and can open freely. This
		// is not a bug in this renderer: real CommonMark (and GitHub's own
		// markdown, which is CommonMark-based) bolds "init" in "__init__.py"
		// the exact same way - a well-known dunder-method gotcha, not an
		// intraword case the spec actually rejects.
		{"double underscore around a trailing word boundary still emphasis, per CommonMark", "__init__.py", "<b>init</b>.py"},
		{"intraword underscore in an identifier never emphasis", "snake_case_name", "snake_case_name"},
		{"intraword asterisk still emphasis, per CommonMark", "2*3*4", "2<i>3</i>4"},
		{"underscore emphasis still works surrounded by whitespace", "some _emphasized_ text", "some <i>emphasized</i> text"},
		{"double underscore bold still works surrounded by whitespace", "some __bold__ text", "some <b>bold</b> text"},
		{"underscore emphasis at start and end of string still works", "_word_", "<i>word</i>"},
		{"underscore preceded by punctuation can still open mid-word", "say(_word_) now", "say(<i>word</i>) now"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, renderHTML(tc.in))
		})
	}
}

func TestRenderHTML_FencedCodeBlock(t *testing.T) {
	in := "before\n```go\nfmt.Println(\"a < b & c > d\")\n```\nafter"
	want := "before\n<pre><code class=\"language-go\">fmt.Println(\"a &lt; b &amp; c &gt; d\")</code></pre>\nafter"
	assert.Equal(t, want, renderHTML(in))
}

func TestRenderHTML_FencedCodeBlockNoLanguage(t *testing.T) {
	in := "```\nplain code\n```"
	want := "<pre><code>plain code</code></pre>"
	assert.Equal(t, want, renderHTML(in))
}

// TestRenderHTML_FenceContentNeverMarkdownParsed proves code content is
// escaped, never interpreted as bold/italic/links - code must render
// byte-for-byte (modulo the three HTML-special characters).
func TestRenderHTML_FenceContentNeverMarkdownParsed(t *testing.T) {
	in := "```\n**not bold** [not a link](x)\n```"
	got := renderHTML(in)
	assert.Contains(t, got, "**not bold** [not a link](x)")
	assert.NotContains(t, got, "<b>")
	assert.NotContains(t, got, "<a ")
}

// TestFenceOpenLang_CapsInfoStringLength proves a fence whose info string
// alone is far longer than any real language tag is not treated as a fence
// at all - splitFence's per-chunk budget would otherwise collapse to
// almost nothing.
func TestFenceOpenLang_CapsInfoStringLength(t *testing.T) {
	_, ok := fenceOpenLang("```" + strings.Repeat("x", maxFenceLang+1))
	assert.False(t, ok, "an info string over maxFenceLang must not open a fence")

	lang, ok := fenceOpenLang("```" + strings.Repeat("x", maxFenceLang))
	assert.True(t, ok)
	assert.Len(t, lang, maxFenceLang)
}

// TestFenceOpenLang_RejectsBacktickInInfoString proves a line like
// "```ls```" does not open a fence: CommonMark forbids a backtick anywhere
// in a backtick fence's info string. Before this fix, "```ls```" opened a
// fence with the literal lang "ls```" and read the rest of the message as
// fenced code.
func TestFenceOpenLang_RejectsBacktickInInfoString(t *testing.T) {
	_, ok := fenceOpenLang("```ls```")
	assert.False(t, ok, "a backtick in the info string must not open a fence")

	lang, ok := fenceOpenLang("```go")
	assert.True(t, ok, "an ordinary info string with no backtick must still open a fence")
	assert.Equal(t, "go", lang)
}

// TestRenderHTML_BacktickFenceLookalikeDoesNotSwallowTheRestOfTheMessage
// proves "```ls```" on its own line is rendered as ordinary text (with its
// backticks escaped/parsed as inline code, not as a fence), and the text
// that follows it is not misread as fenced code.
func TestRenderHTML_BacktickFenceLookalikeDoesNotSwallowTheRestOfTheMessage(t *testing.T) {
	got := renderHTML("```ls```\nnormal *italic* text")
	assert.Contains(t, got, "<i>italic</i>", "text after the fence lookalike must still be parsed as ordinary Markdown, not treated as fenced code")
}

// TestRenderChunks_LongFenceInfoString_TerminatesQuickly proves a fence
// whose opening line's info string alone is thousands of bytes does not
// turn one reply into hundreds of garbage-UTF-8 messages: fenceOpenLang
// rejects an info string this long, so the ``` line is treated as ordinary
// text instead of a fence
// opener, and rendering completes promptly with every chunk valid HTML.
func TestRenderChunks_LongFenceInfoString_TerminatesQuickly(t *testing.T) {
	body := strings.Repeat("é", 300) // 300 non-ASCII (2-byte) runes
	text := "```" + strings.Repeat("A", 5000) + "\n" + body + "\n```"

	done := make(chan []htmlPart, 1)
	go func() { done <- renderChunks(text, DefaultChunkLimit) }()

	select {
	case parts := <-done:
		require.NotEmpty(t, parts)
		require.Less(t, len(parts), 20, "an oversized info string must not explode into hundreds of chunks")
		for i, p := range parts {
			assert.LessOrEqualf(t, len(p.html), DefaultChunkLimit, "chunk %d is %d bytes, over the limit", i, len(p.html))
			assert.Truef(t, utf8Valid(p.html), "chunk %d is not valid UTF-8", i)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("renderChunks did not terminate on a fence whose info string alone exceeds the limit")
	}
}

func utf8Valid(s string) bool {
	return strings.ToValidUTF8(s, "�") == s
}
