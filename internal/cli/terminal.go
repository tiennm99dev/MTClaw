package cli

import (
	"fmt"
	"strings"
)

// sanitizeForTerminal rewrites s so every C0 control character other than
// "\n" and "\t", DEL, every C1 control character, and every Unicode
// bidirectional-override/isolate character (U+202A-U+202E, U+2066-U+2069)
// renders as a visible "\xNN" or "\uNNNN" escape instead of being
// interpreted by the terminal that prints it. Stored message content and
// tool output can contain a web_fetch page's body or other model-visible
// text nobody has reviewed: a "\r" plus an ANSI erase-line sequence can
// redraw what the terminal shows underneath it, an OSC sequence can rename
// the terminal title or (on a terminal that allows it) write the clipboard,
// and a bidi override can reorder how a line reads - all without the
// operator ever seeing the raw bytes that did it. `mtclaw sessions show` and
// `mtclaw approvals list` are exactly the audit-review path operators are
// told to trust after a crash (see docs/security.md), so every
// previously-untrusted field they print goes through this first.
func sanitizeForTerminal(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f):
			fmt.Fprintf(&b, `\x%02x`, r)
		case isBidiControlRune(r):
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// isBidiControlRune reports whether r is one of the Unicode bidirectional
// override or isolate control characters (U+202A-U+202E, U+2066-U+2069)
// that can change the visual order glyphs render in, independent of the
// underlying byte order.
func isBidiControlRune(r rune) bool {
	return (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069)
}

// sanitizeForTable is sanitizeForTerminal plus escaping "\n" and "\t"
// themselves: a tabwriter-rendered row (`mtclaw approvals list`) is a
// single logical line per record, and an embedded real newline or tab in a
// stored command would otherwise splice in extra rows/columns that look
// like separate, legitimate table entries.
func sanitizeForTable(s string) string {
	s = sanitizeForTerminal(s)
	s = strings.ReplaceAll(s, "\n", `\n`)
	s = strings.ReplaceAll(s, "\t", `\t`)
	return s
}
