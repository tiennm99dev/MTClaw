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
				block := s[i : i+3+end+3]
				b.WriteString(block)
				i += len(block)
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
				span := s[i : i+1+end+1]
				b.WriteString(span)
				i += len(span)
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
