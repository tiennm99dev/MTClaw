package telegram

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSplit_Empty(t *testing.T) {
	assert.Nil(t, Split("", DefaultChunkLimit))
}

func TestSplit_AtLimitBoundary(t *testing.T) {
	at4095 := strings.Repeat("a", 4095)
	chunks := Split(at4095, DefaultChunkLimit)
	require.Len(t, chunks, 1)
	assert.Equal(t, at4095, chunks[0])

	at4096 := strings.Repeat("a", 4096)
	chunks = Split(at4096, DefaultChunkLimit)
	require.Len(t, chunks, 1)
	assert.Equal(t, at4096, chunks[0])

	at4097 := strings.Repeat("a", 4097)
	chunks = Split(at4097, DefaultChunkLimit)
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

	chunks := Split(text, DefaultChunkLimit)
	require.Len(t, chunks, 2)
	assert.Equal(t, para1, chunks[0])
	assert.Equal(t, para2, chunks[1])
}

func TestSplit_PrefersLineBreakOverHardCut(t *testing.T) {
	line1 := strings.Repeat("a", 3000)
	line2 := strings.Repeat("b", 3000)
	text := line1 + "\n" + line2

	chunks := Split(text, DefaultChunkLimit)
	require.Len(t, chunks, 2)
	assert.Equal(t, line1, chunks[0])
	assert.Equal(t, line2, chunks[1])
}

func TestSplit_PrefersSentenceEnd(t *testing.T) {
	sentence1 := strings.Repeat("a", 4000) + ". "
	sentence2 := strings.Repeat("b", 200)
	text := sentence1 + sentence2

	chunks := Split(text, DefaultChunkLimit)
	require.Len(t, chunks, 2)
	assert.True(t, strings.HasSuffix(chunks[0], "."))
	assert.Equal(t, sentence2, chunks[1])
}

func TestSplit_SingleHugeWordHardCuts(t *testing.T) {
	word := strings.Repeat("x", 10000)
	chunks := Split(word, DefaultChunkLimit)
	require.Greater(t, len(chunks), 1)
	for _, c := range chunks {
		assert.LessOrEqual(t, len(c), DefaultChunkLimit)
		assert.NotEmpty(t, c)
	}
	assert.Equal(t, word, strings.Join(chunks, ""))
}

func TestSplit_NoWhitespaceAtAll(t *testing.T) {
	text := strings.Repeat("y", 9000)
	chunks := Split(text, DefaultChunkLimit)
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

	chunks := Split(text, DefaultChunkLimit)
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

func TestSplit_ShortTextWithFenceIsUntouched(t *testing.T) {
	text := "before\n```go\nfmt.Println(\"hi\")\n```\nafter"
	chunks := Split(text, DefaultChunkLimit)
	require.Len(t, chunks, 1)
	assert.Equal(t, text, chunks[0])
}

func truncateForAssert(s string) string {
	if len(s) > 80 {
		return s[:80] + "..."
	}
	return s
}
