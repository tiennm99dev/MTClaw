package cli

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSanitizeForTerminal_EscapesControlCharsButKeepsNewlineAndTab(t *testing.T) {
	in := "line one\n\tindented\x1b[2Jclobbered\x07bell"
	got := sanitizeForTerminal(in)
	assert.Contains(t, got, "line one\n\tindented")
	assert.NotContains(t, got, "\x1b", "ESC must never reach the terminal raw")
	assert.NotContains(t, got, "\x07")
	assert.Contains(t, got, `\x1b`)
	assert.Contains(t, got, `\x07`)
}

func TestSanitizeForTerminal_EscapesBidiOverrides(t *testing.T) {
	// Built via fmt.Sprintf, not a literal escape sequence in source: a
	// literal "‮" written directly in a Go string (or backtick raw
	// string) here would embed the actual RTL-override rune into this test
	// file's own bytes, not the 6-character text "‮" the assertion
	// below needs to check for.
	bidiRune := rune(0x202E)
	in := "before" + string(bidiRune) + "after"
	got := sanitizeForTerminal(in)
	assert.NotContains(t, got, string(bidiRune))
	assert.Contains(t, got, fmt.Sprintf(`\u%04x`, bidiRune))
}

func TestSanitizeForTerminal_OrdinaryTextUnchanged(t *testing.T) {
	in := "hello, world! 100% safe."
	assert.Equal(t, in, sanitizeForTerminal(in))
}

func TestSanitizeForTable_EscapesEmbeddedNewlineAndTab(t *testing.T) {
	in := "line one\nline two\tcol2"
	got := sanitizeForTable(in)
	assert.Equal(t, `line one\nline two\tcol2`, got)
}
