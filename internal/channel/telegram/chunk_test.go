package telegram

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSplit_Empty(t *testing.T) {
	assert.Nil(t, split("", DefaultChunkLimit))
}

func TestSplit_AtLimitBoundary(t *testing.T) {
	at4095 := strings.Repeat("a", 4095)
	chunks := split(at4095, DefaultChunkLimit)
	require.Len(t, chunks, 1)
	assert.Equal(t, at4095, chunks[0])

	at4096 := strings.Repeat("a", 4096)
	chunks = split(at4096, DefaultChunkLimit)
	require.Len(t, chunks, 1)
	assert.Equal(t, at4096, chunks[0])

	at4097 := strings.Repeat("a", 4097)
	chunks = split(at4097, DefaultChunkLimit)
	require.Len(t, chunks, 2)
	for _, c := range chunks {
		assert.LessOrEqual(t, len(c), DefaultChunkLimit)
	}
	assert.Equal(t, at4097, strings.Join(chunks, ""))
}

func TestSplit_PrefersParagraphBreak(t *testing.T) {
	para1 := strings.Repeat("a", 3000)
	para2 := strings.Repeat("b", 3000)
	text := para1 + "\n\n" + para2

	chunks := split(text, DefaultChunkLimit)
	require.Len(t, chunks, 2)
	assert.Equal(t, para1, chunks[0])
	assert.Equal(t, para2, chunks[1])
}

func TestSplit_PrefersLineBreakOverHardCut(t *testing.T) {
	line1 := strings.Repeat("a", 3000)
	line2 := strings.Repeat("b", 3000)
	text := line1 + "\n" + line2

	chunks := split(text, DefaultChunkLimit)
	require.Len(t, chunks, 2)
	assert.Equal(t, line1, chunks[0])
	assert.Equal(t, line2, chunks[1])
}

func TestSplit_PrefersSentenceEnd(t *testing.T) {
	sentence1 := strings.Repeat("a", 4000) + ". "
	sentence2 := strings.Repeat("b", 200)
	text := sentence1 + sentence2

	chunks := split(text, DefaultChunkLimit)
	require.Len(t, chunks, 2)
	assert.True(t, strings.HasSuffix(chunks[0], "."))
	assert.Equal(t, sentence2, chunks[1])
}

func TestSplit_SingleHugeWordHardCuts(t *testing.T) {
	word := strings.Repeat("x", 10000)
	chunks := split(word, DefaultChunkLimit)
	require.Greater(t, len(chunks), 1)
	for _, c := range chunks {
		assert.LessOrEqual(t, len(c), DefaultChunkLimit)
		assert.NotEmpty(t, c)
	}
	assert.Equal(t, word, strings.Join(chunks, ""))
}

func TestSplit_NoWhitespaceAtAll(t *testing.T) {
	text := strings.Repeat("y", 9000)
	chunks := split(text, DefaultChunkLimit)
	require.NotEmpty(t, chunks)
	for _, c := range chunks {
		assert.LessOrEqual(t, len(c), DefaultChunkLimit)
	}
	assert.Equal(t, text, strings.Join(chunks, ""))
}

func TestSplit_CodeFenceSpanningLimitStaysBalanced(t *testing.T) {
	var body strings.Builder
	for i := 0; i < 400; i++ {
		body.WriteString("some code line that repeats\n")
	}
	text := "intro text\n```go\n" + body.String() + "```\nafter text"

	chunks := split(text, DefaultChunkLimit)
	require.Greater(t, len(chunks), 1)

	for _, c := range chunks {
		assert.LessOrEqual(t, len(c), DefaultChunkLimit)
		opens := strings.Count(c, "```")
		assert.True(t, opens%2 == 0 || opens == 0, "every chunk must have balanced fences: %q has %d ``` markers", truncateForAssert(c), opens)
	}

	// Every fenced chunk (one containing "```go" or a bare "```" reopen)
	// must itself both open and close a fence.
	inCode := false
	for _, c := range chunks {
		lines := strings.Split(c, "\n")
		for _, l := range lines {
			if strings.HasPrefix(l, "```") {
				inCode = !inCode
			}
		}
	}
	assert.False(t, inCode, "fences must balance across the whole reassembled text")
}

// TestSplitByLineThenHardCut_NeverAppendsATrailingEmptyPiece proves content
// that divides evenly into limit-sized, newline-terminated pieces does not
// leave a spurious "" piece at the end - one layer up, fitHTML recurses on
// len(pieces) != 1 to keep shrinking a chunk, and a trailing "" piece next
// to a piece that is not itself shorter than the original chunk defeats
// that progress check, recursing on the same input forever.
func TestSplitByLineThenHardCut_NeverAppendsATrailingEmptyPiece(t *testing.T) {
	// A single 4-byte rune with limit 1: safeRuneCut cannot cut inside the
	// rune (splitting it would corrupt it), so its fallback emits the
	// rune whole, consuming every byte of remaining in one iteration - cut
	// == len(remaining), unlike every other case where cut is bounded by
	// limit and so always strictly less than len(remaining) going in. That
	// leaves remaining empty once the loop exits, the exact shape that used
	// to leave a spurious "" piece appended after it.
	pieces := splitByLineThenHardCut("😀", 1)
	for _, p := range pieces {
		assert.NotEqual(t, "", p, "no piece may be empty when the input itself is not")
	}
}

func TestSplit_ShortTextWithFenceIsUntouched(t *testing.T) {
	text := "before\n```go\nfmt.Println(\"hi\")\n```\nafter"
	chunks := split(text, DefaultChunkLimit)
	require.Len(t, chunks, 1)
	assert.Equal(t, text, chunks[0])
}

// TestSafeRuneCut_NeverSplitsMultibyteRune proves a limit landing anywhere
// inside a multi-byte rune rounds down to a full-rune boundary, never
// emitting an incomplete (invalid) rune prefix, however far back that
// requires scanning.
func TestSafeRuneCut_NeverSplitsMultibyteRune(t *testing.T) {
	s := strings.Repeat("é", 10) // 300 bytes of the 2-byte rune 'é'... in this case 20 bytes
	for limit := 0; limit <= len(s)+1; limit++ {
		cut := safeRuneCut(s, limit)
		require.True(t, utf8.RuneStart(byteOrZero(s, cut)) || cut == len(s), "limit=%d: cut %d does not land on a rune boundary", limit, cut)
		assert.True(t, utf8.ValidString(s[:cut]), "limit=%d: s[:%d] is not valid UTF-8", limit, cut)
	}
}

// TestSafeRuneCut_LimitZeroOrNegative_StillReturnsAFullRune proves an
// unreasonably small limit still emits one whole rune rather than a
// mid-rune (invalid UTF-8) prefix.
func TestSafeRuneCut_LimitZeroOrNegative_StillReturnsAFullRune(t *testing.T) {
	s := "éllo" // leading 2-byte rune
	cut := safeRuneCut(s, 0)
	assert.Equal(t, 2, cut)
	assert.True(t, utf8.ValidString(s[:cut]))
}

func byteOrZero(s string, i int) byte {
	if i >= len(s) {
		return 0
	}
	return s[i]
}

// TestSplit_HardCutWithSpacesReassemblesLosslessly is a boundary/multibyte
// regression test: a hard cut (no paragraph, line, or sentence boundary
// available near the limit) must not silently drop a content character
// that happens to sit right after the cut point, and must never split a
// multibyte rune.
func TestSplit_HardCutWithSpacesReassemblesLosslessly(t *testing.T) {
	var b strings.Builder
	for b.Len() < 9000 {
		b.WriteString("wordwordword éèê ")
	}
	text := b.String()

	chunks := split(text, DefaultChunkLimit)
	require.NotEmpty(t, chunks)
	for _, c := range chunks {
		assert.LessOrEqual(t, len(c), DefaultChunkLimit)
		assert.True(t, utf8.ValidString(c))
	}
	assert.Equal(t, text, strings.Join(chunks, ""))
}

func truncateForAssert(s string) string {
	if len(s) > 80 {
		return s[:80] + "..."
	}
	return s
}
