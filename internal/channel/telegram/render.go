package telegram

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// renderHTML converts one already-chunked piece of the model's Markdown
// output into Telegram-flavored HTML: bold, italic, inline code, fenced
// code blocks (with an optional language tag rendered as a "language-x"
// class), links, and headings folded into bold lines. Plain lists and
// ordinary punctuation pass through unchanged (escaped) text, since
// Telegram's HTML mode supports no list or heading tags at all.
//
// chunk is parsed independently of any sibling chunk split produced it
// alongside: an inline marker left unbalanced within chunk (a split that
// landed inside a bold span, for instance) falls back to literal text
// instead of emitting an unclosed tag, so every chunk renders to
// independently valid HTML no matter where the source was cut.
func renderHTML(chunk string) string {
	var b strings.Builder
	for i, seg := range parseSegments(chunk) {
		if i > 0 {
			// parseSegments partitions chunk's lines (split on "\n")
			// contiguously across segments, so exactly one newline
			// separated the last line of the previous segment from the
			// first line of this one in the original text.
			b.WriteByte('\n')
		}
		if seg.isCode {
			renderCodeSegment(&b, seg)
		} else {
			renderTextSegment(&b, seg.lines)
		}
	}
	return b.String()
}

// renderCodeSegment renders one fenced code block as <pre><code
// class="language-X">...</code></pre> (the class omitted when the fence
// carried no language tag), with its content escaped but never
// markdown-parsed - code must render byte-for-byte.
func renderCodeSegment(b *strings.Builder, seg segment) {
	inner := ""
	if len(seg.lines) > 2 {
		inner = strings.Join(seg.lines[1:len(seg.lines)-1], "\n")
	}
	b.WriteString("<pre><code")
	if seg.lang != "" {
		b.WriteString(` class="language-`)
		b.WriteString(escapeHTMLAttr(seg.lang))
		b.WriteString(`"`)
	}
	b.WriteString(">")
	b.WriteString(escapeHTMLText(inner))
	b.WriteString("</code></pre>")
}

// renderTextSegment renders one plain-text segment line by line, so a "#"
// heading line can be folded into bold independently of its neighbors.
func renderTextSegment(b *strings.Builder, lines []string) {
	for i, line := range lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		renderLine(b, line)
	}
}

func renderLine(b *strings.Builder, line string) {
	if heading, ok := stripHeadingMarker(line); ok {
		b.WriteString("<b>")
		b.WriteString(renderInline(heading))
		b.WriteString("</b>")
		return
	}
	b.WriteString(renderInline(line))
}

// stripHeadingMarker reports whether line is an ATX heading ("#" through
// "######" followed by a space), returning its text with the marker
// removed. Telegram HTML has no heading tags, so a heading is rendered as a
// bold line instead - readable, not a literal wall of "#" characters.
func stripHeadingMarker(line string) (text string, ok bool) {
	trimmed := strings.TrimLeft(line, " ")
	i := 0
	for i < len(trimmed) && i < 6 && trimmed[i] == '#' {
		i++
	}
	if i == 0 || i >= len(trimmed) || trimmed[i] != ' ' {
		return "", false
	}
	return strings.TrimSpace(trimmed[i:]), true
}

// renderInline converts inline Markdown - “ `code` “, **bold**/__bold__,
// *italic*/_italic_, and [text](url) links - to HTML, recursing into a
// bold or italic span's own content so the two can nest. Any opening
// marker with no matching close anywhere in s is emitted as literal text
// instead of being consumed, which is what keeps an unbalanced or
// mid-span-truncated chunk from ever producing invalid HTML.
func renderInline(s string) string {
	var b strings.Builder
	i := 0
	for i < len(s) {
		switch {
		case s[i] == '`':
			if consumed := renderInlineCode(&b, s, i); consumed > 0 {
				i += consumed
				continue
			}

		case strings.HasPrefix(s[i:], "**"):
			if consumed := renderEmphasis(&b, s, i, "**", "b", false); consumed > 0 {
				i += consumed
				continue
			}
		case strings.HasPrefix(s[i:], "__"):
			if consumed := renderEmphasis(&b, s, i, "__", "b", true); consumed > 0 {
				i += consumed
				continue
			}

		case s[i] == '*':
			if consumed := renderSingleDelimEmphasis(&b, s, i, '*', "i", false); consumed > 0 {
				i += consumed
				continue
			}
		case s[i] == '_':
			if consumed := renderSingleDelimEmphasis(&b, s, i, '_', "i", true); consumed > 0 {
				i += consumed
				continue
			}

		case s[i] == '[':
			if consumed := renderLink(&b, s, i); consumed > 0 {
				i += consumed
				continue
			}
		}

		r, size := utf8.DecodeRuneInString(s[i:])
		writeHTMLRune(&b, r)
		i += size
	}
	return b.String()
}

// renderInlineCode renders a “ `...` “ (or a longer same-length backtick
// run, for content that itself contains single backticks) span starting at
// s[start], returning the number of bytes consumed, or 0 if no matching
// close exists (the caller then treats s[start] as literal).
func renderInlineCode(b *strings.Builder, s string, start int) int {
	n := 0
	for start+n < len(s) && s[start+n] == '`' {
		n++
	}
	close, ok := findBacktickClose(s, start+n, n)
	if !ok {
		return 0
	}
	content := s[start+n : close]
	// CommonMark trims exactly one leading and trailing space when both are
	// present (so `` ` `x` ` `` can wrap in spaces without them appearing in
	// the rendered code); skip it if it is not present on both ends.
	if len(content) >= 2 && strings.HasPrefix(content, " ") && strings.HasSuffix(content, " ") &&
		strings.TrimSpace(content) != "" {
		content = content[1 : len(content)-1]
	}
	b.WriteString("<code>")
	b.WriteString(escapeHTMLText(content))
	b.WriteString("</code>")
	return close + n - start
}

// findBacktickClose finds the first run of exactly n backticks at or after
// start that is not itself part of a longer run, per CommonMark's rule that
// a code span's closing delimiter must match the opening one's length
// exactly.
func findBacktickClose(s string, start, n int) (int, bool) {
	delim := strings.Repeat("`", n)
	idx := start
	for idx <= len(s)-n {
		pos := strings.Index(s[idx:], delim)
		if pos < 0 {
			return 0, false
		}
		abs := idx + pos
		before := abs == 0 || s[abs-1] != '`'
		after := abs+n == len(s) || s[abs+n] != '`'
		if before && after {
			return abs, true
		}
		idx = abs + 1
	}
	return 0, false
}

// renderEmphasis renders a delim-delimited span (delim is "**" or "__")
// starting at s[start], recursively rendering its content so nested
// emphasis (bold containing italic, or vice versa) works, returning bytes
// consumed or 0 if delim never closes. underscoreFlanking must be true for
// "__" and false for "**": CommonMark's intraword restriction (see
// underscoreCanOpen/underscoreCanClose) applies to underscore delimiter
// runs of any length, never to asterisk ones, which is what keeps
// "__init__.py" from having "init" read as bold while leaving "**bold**"
// unaffected.
func renderEmphasis(b *strings.Builder, s string, start int, delim, tag string, underscoreFlanking bool) int {
	openEnd := start + len(delim)
	if underscoreFlanking && !underscoreCanOpen(s, start, openEnd) {
		return 0
	}

	searchFrom := openEnd
	for {
		rel := strings.Index(s[searchFrom:], delim)
		if rel < 0 {
			return 0
		}
		closeStart := searchFrom + rel
		if closeStart == openEnd {
			// Adjacent to the opening delimiter: an empty span ("****" or
			// "____"), never a valid one to render - and, since this can
			// only happen on the very first iteration (searchFrom starts
			// at openEnd), there is no earlier candidate a later loop
			// iteration could have already rejected instead.
			return 0
		}
		closeEnd := closeStart + len(delim)
		if !underscoreFlanking || underscoreCanClose(s, closeStart, closeEnd) {
			b.WriteString("<" + tag + ">")
			b.WriteString(renderInline(s[openEnd:closeStart]))
			b.WriteString("</" + tag + ">")
			return closeEnd - start
		}
		searchFrom = closeStart + 1
	}
}

// renderSingleDelimEmphasis renders a single-character-delimited span (*
// or _) starting at s[start]. The closing delimiter must be an isolated
// occurrence of c - not itself adjacent to another c - so "**bold**"
// is never misread as an empty italic span followed by literal
// asterisks. underscoreFlanking must be true for '_' and false for '*' -
// see renderEmphasis. Returns bytes consumed, or 0 if no valid close
// exists.
func renderSingleDelimEmphasis(b *strings.Builder, s string, start int, c byte, tag string, underscoreFlanking bool) int {
	contentStart := start + 1
	if underscoreFlanking && !underscoreCanOpen(s, start, contentStart) {
		return 0
	}
	for i := contentStart; i < len(s); i++ {
		if s[i] != c {
			continue
		}
		prevSame := i > 0 && s[i-1] == c
		nextSame := i+1 < len(s) && s[i+1] == c
		if prevSame || nextSame || i == contentStart {
			continue
		}
		if underscoreFlanking && !underscoreCanClose(s, i, i+1) {
			continue
		}
		b.WriteString("<" + tag + ">")
		b.WriteString(renderInline(s[contentStart:i]))
		b.WriteString("</" + tag + ">")
		return i + 1 - start
	}
	return 0
}

// --- CommonMark emphasis flanking (underscore intraword restriction) ------
//
// CommonMark disallows underscore emphasis from opening or closing in the
// middle of a word (MAX_READ_BYTES, __init__.py, snake_case_name) while
// still allowing it for asterisks (a*b*c, 2*3*4) - the two delimiters are
// not interchangeable. What follows is a direct, single-delimiter-run
// implementation of the relevant part of that spec (the "left-flanking" /
// "right-flanking" definitions and emphasis rules 2 and 4;
// https://spec.commonmark.org/0.31.2/#emphasis-and-strong-emphasis): a
// simplification the hand-written recursive-descent parser above can
// afford because it only ever asks these questions about one delimiter run
// (start, end) at a time, never maintaining a full delimiter stack.

// runeBefore returns the rune immediately preceding byte position pos in s
// and true, or ok=false if pos is at the very start of s. CommonMark treats
// the start (and end) of the string the same as whitespace for flanking
// purposes, so callers pass the !ok case into isFlankWhitespace, not treat
// it as "no rune".
func runeBefore(s string, pos int) (r rune, ok bool) {
	if pos <= 0 {
		return 0, false
	}
	r, _ = utf8.DecodeLastRuneInString(s[:pos])
	return r, true
}

// runeAfter returns the rune immediately following byte position pos in s
// and true, or ok=false if pos is at the very end of s.
func runeAfter(s string, pos int) (r rune, ok bool) {
	if pos >= len(s) {
		return 0, false
	}
	r, _ = utf8.DecodeRuneInString(s[pos:])
	return r, true
}

// isFlankWhitespace reports whether (r, ok) - as returned by runeBefore or
// runeAfter - counts as whitespace for flanking purposes: either r actually
// is Unicode whitespace, or ok is false (the start/end of the string,
// which CommonMark defines as behaving like whitespace here).
func isFlankWhitespace(r rune, ok bool) bool {
	return !ok || unicode.IsSpace(r)
}

// isFlankPunct reports whether (r, ok) is a Unicode punctuation character
// for flanking purposes - CommonMark's definition covers both the P
// (punctuation) and S (symbol) general categories.
func isFlankPunct(r rune, ok bool) bool {
	return ok && (unicode.IsPunct(r) || unicode.IsSymbol(r))
}

// leftFlanking reports whether the delimiter run s[start:end] is
// left-flanking: not followed by whitespace, and either not followed by
// punctuation, or followed by punctuation that is itself preceded by
// whitespace or punctuation.
func leftFlanking(s string, start, end int) bool {
	after, afterOK := runeAfter(s, end)
	if isFlankWhitespace(after, afterOK) {
		return false
	}
	if !isFlankPunct(after, afterOK) {
		return true
	}
	before, beforeOK := runeBefore(s, start)
	return isFlankWhitespace(before, beforeOK) || isFlankPunct(before, beforeOK)
}

// rightFlanking reports whether the delimiter run s[start:end] is
// right-flanking: not preceded by whitespace, and either not preceded by
// punctuation, or preceded by punctuation that is itself followed by
// whitespace or punctuation.
func rightFlanking(s string, start, end int) bool {
	before, beforeOK := runeBefore(s, start)
	if isFlankWhitespace(before, beforeOK) {
		return false
	}
	if !isFlankPunct(before, beforeOK) {
		return true
	}
	after, afterOK := runeAfter(s, end)
	return isFlankWhitespace(after, afterOK) || isFlankPunct(after, afterOK)
}

// underscoreCanOpen implements CommonMark emphasis rule 2: an underscore
// delimiter run can open emphasis iff it is left-flanking, and either it is
// not also right-flanking, or it is right-flanking but preceded by
// punctuation. This is what rejects the opening "_" in "MAX_READ_BYTES"
// (preceded and followed by a letter, so left- and right-flanking at once,
// with no preceding punctuation to grant an exception) while still
// accepting it in "some _emphasis_ here" (preceded by whitespace, so not
// right-flanking).
func underscoreCanOpen(s string, start, end int) bool {
	if !leftFlanking(s, start, end) {
		return false
	}
	if !rightFlanking(s, start, end) {
		return true
	}
	before, beforeOK := runeBefore(s, start)
	return isFlankPunct(before, beforeOK)
}

// underscoreCanClose implements CommonMark emphasis rule 4: the mirror
// image of underscoreCanOpen, swapping left/right and preceded/followed.
func underscoreCanClose(s string, start, end int) bool {
	if !rightFlanking(s, start, end) {
		return false
	}
	if !leftFlanking(s, start, end) {
		return true
	}
	after, afterOK := runeAfter(s, end)
	return isFlankPunct(after, afterOK)
}

// renderLink renders a "[text](url)" span starting at s[start], returning
// bytes consumed or 0 if it is not a well-formed link (no nested brackets
// or parens support - a small hand-written converter, not a CommonMark
// implementation).
func renderLink(b *strings.Builder, s string, start int) int {
	textEnd := strings.IndexByte(s[start+1:], ']')
	if textEnd < 0 {
		return 0
	}
	textEnd += start + 1
	if textEnd+1 >= len(s) || s[textEnd+1] != '(' {
		return 0
	}
	urlEnd := strings.IndexByte(s[textEnd+2:], ')')
	if urlEnd < 0 {
		return 0
	}
	urlEnd += textEnd + 2

	text := s[start+1 : textEnd]
	url := s[textEnd+2 : urlEnd]
	b.WriteString(`<a href="`)
	b.WriteString(escapeHTMLAttr(url))
	b.WriteString(`">`)
	b.WriteString(renderInline(text))
	b.WriteString("</a>")
	return urlEnd + 1 - start
}

// writeHTMLRune writes r to b, escaping it if it is one of the three
// characters Telegram's HTML parser treats as special.
func writeHTMLRune(b *strings.Builder, r rune) {
	switch r {
	case '&':
		b.WriteString("&amp;")
	case '<':
		b.WriteString("&lt;")
	case '>':
		b.WriteString("&gt;")
	default:
		b.WriteRune(r)
	}
}

// escapeHTMLText escapes &, <, and > - the three characters Telegram's HTML
// parse mode requires escaped in text content (code and pre content
// included).
func escapeHTMLText(s string) string {
	if !strings.ContainsAny(s, "&<>") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		writeHTMLRune(&b, r)
	}
	return b.String()
}

// escapeHTMLAttr escapes &, <, >, and " for use inside a double-quoted HTML
// attribute value (an <a href="..."> URL, or a <code class="..."> language
// tag).
func escapeHTMLAttr(s string) string {
	if !strings.ContainsAny(s, `&<>"`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == '"' {
			b.WriteString("&quot;")
			continue
		}
		writeHTMLRune(&b, r)
	}
	return b.String()
}
