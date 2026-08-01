package telegram

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestEscapeMarkdownV2_EscapesSpecialCharsOutsideCode(t *testing.T) {
	in := "Done. Step 1 - ok! (see notes)"
	got := EscapeMarkdownV2(in)
	assert.Equal(t, `Done\. Step 1 \- ok\! \(see notes\)`, got)
}

func TestEscapeMarkdownV2_LeavesInlineCodeUntouched(t *testing.T) {
	in := "run `ls -la .` now."
	got := EscapeMarkdownV2(in)
	assert.Equal(t, "run `ls -la .` now\\.", got)
}

func TestEscapeMarkdownV2_LeavesFencedCodeUntouched(t *testing.T) {
	in := "before.\n```go\nfmt.Println(\"a.b-c!\")\n```\nafter."
	got := EscapeMarkdownV2(in)
	assert.Equal(t, "before\\.\n```go\nfmt.Println(\"a.b-c!\")\n```\nafter\\.", got)
}

func TestEscapeMarkdownV2_DoesNotDoubleEscape(t *testing.T) {
	// A single pass over raw input escapes each special character exactly
	// once; it must never re-scan its own output and escape the
	// backslashes it just inserted.
	in := "a.b!c-d"
	got := EscapeMarkdownV2(in)
	assert.Equal(t, `a\.b\!c\-d`, got)
	assert.Equal(t, 3, countBackslashes(got))
}

func TestEscapeMarkdownV2_UnmatchedBacktickIsEscaped(t *testing.T) {
	in := "oops ` unmatched"
	got := EscapeMarkdownV2(in)
	assert.Equal(t, "oops \\` unmatched", got)
}

func TestEscapeMarkdownV2_Empty(t *testing.T) {
	assert.Equal(t, "", EscapeMarkdownV2(""))
}

func countBackslashes(s string) int {
	n := 0
	for _, r := range s {
		if r == '\\' {
			n++
		}
	}
	return n
}
