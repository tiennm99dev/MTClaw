package telegram

import (
	"strings"
	"unicode/utf8"
)

// mdV2Special is every character Telegram's MarkdownV2 parser treats as
// special outside of code, per
// https://core.telegram.org/bots/api#markdownv2-style - plus the backslash
// itself, which is how any of them is escaped.
const mdV2Special = "_*[]()~`>#+-=|{}.!\\"

// EscapeMarkdownV2 escapes every MarkdownV2-special character in s that
// falls outside a fenced (```) or inline (`) code span, leaving code
// content byte-for-byte untouched. Model output is not valid MarkdownV2 on
// its own - this is the first half of the two-step send strategy documented
// in the phase 6 plan; the second half (falling back to no parse_mode on an
// HTTP 400) lives in send.go.
func EscapeMarkdownV2(s string) string {
	var b strings.Builder
	b.Grow(len(s))

	i := 0
	for i < len(s) {
		if strings.HasPrefix(s[i:], "```") {
			if end := strings.Index(s[i+3:], "```"); end >= 0 {
				inner := s[i+3 : i+3+end]
				b.WriteString("```")
				b.WriteString(escapeCodeContent(inner))
				b.WriteString("```")
				i += 3 + end + 3
				continue
			}
			// Unbalanced fence: everything from here to the end of the
			// string is code (best-effort; balanced fences are the caller's
			// job upstream in chunk.go), copy it verbatim.
			b.WriteString(s[i:])
			break
		}

		if s[i] == '`' {
			if end := strings.IndexByte(s[i+1:], '`'); end >= 0 {
				content := s[i+1 : i+1+end]
				b.WriteByte('`')
				b.WriteString(escapeBackslashAndBacktick(content))
				b.WriteByte('`')
				i += 1 + end + 1
				continue
			}
			// Unmatched backtick: treat as a literal character to escape.
			b.WriteString(escapeMDChar('`'))
			i++
			continue
		}

		r, size := utf8.DecodeRuneInString(s[i:])
		b.WriteString(escapeMDChar(r))
		i += size
	}

	return b.String()
}

func escapeMDChar(r rune) string {
	if strings.ContainsRune(mdV2Special, r) {
		return "\\" + string(r)
	}
	return string(r)
}

// escapeCodeContent escapes backslash and backtick within a fenced code
// block's inner text, per MarkdownV2's rule that both characters must be
// escaped inside a pre/code entity even though nothing else in it is. The
// block's first line (the optional language tag, e.g. "go" in "```go") is
// not part of the entity's text content, so it is left untouched - only
// what follows its trailing newline is escaped; a fence with no newline at
// all (no language line) is entirely content and is escaped in full.
func escapeCodeContent(inner string) string {
	if nl := strings.IndexByte(inner, '\n'); nl >= 0 {
		return inner[:nl+1] + escapeBackslashAndBacktick(inner[nl+1:])
	}
	return escapeBackslashAndBacktick(inner)
}

// escapeBackslashAndBacktick escapes the two characters MarkdownV2 still
// requires escaped inside code/pre entities, leaving every other character
// (which code must render byte-for-byte) untouched.
func escapeBackslashAndBacktick(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == '\\' || r == '`' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
