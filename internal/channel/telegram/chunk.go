package telegram

import (
	"strings"
	"unicode/utf8"
)

// DefaultChunkLimit is Telegram's message length ceiling (4096 characters).
// Split treats it as a byte-length ceiling: since every UTF-8 rune is at
// least one byte, a chunk within this many bytes is always within it in
// characters too, which is the safe direction to round on a boundary
// Telegram itself enforces server-side.
const DefaultChunkLimit = 4096

// Split breaks text into chunks no longer than limit, in order, preferring
// to break at a paragraph boundary (a blank line), then a line boundary,
// then a sentence end, then a hard cut - but never inside a fenced code
// block: a fence that would otherwise straddle a chunk boundary is closed
// at the end of one chunk and reopened with the same language tag at the
// start of the next. limit <= 0 uses DefaultChunkLimit. An empty text
// yields no chunks at all.
func Split(text string, limit int) []string {
	if text == "" {
		return nil
	}
	if limit <= 0 {
		limit = DefaultChunkLimit
	}

	var out []string
	var pending string
	havePending := false

	flushPending := func() {
		if havePending {
			out = append(out, pending)
			pending = ""
			havePending = false
		}
	}

	for _, seg := range parseSegments(text) {
		var pieces []string
		if seg.isCode {
			pieces = splitFence(seg.lines, seg.lang, limit)
		} else {
			pieces = splitPlain(strings.Join(seg.lines, "\n"), limit)
		}

		if len(pieces) != 1 {
			// This segment needed splitting on its own: flush whatever
			// small segments were pending (so they are not glued onto a
			// fence's first or last piece), then emit its pieces verbatim.
			flushPending()
			out = append(out, pieces...)
			continue
		}

		// A segment that fits in one piece may still be small enough to
		// share a chunk with its neighbors - e.g. a short paragraph
		// followed by a short fenced snippet - so pack them together
		// (rejoined with the "\n" that separated them in the original
		// text) instead of forcing a chunk boundary at every segment edge.
		candidate := pieces[0]
		switch {
		case !havePending:
			pending = candidate
			havePending = true
		case len(pending)+1+len(candidate) <= limit:
			pending = pending + "\n" + candidate
		default:
			flushPending()
			pending = candidate
			havePending = true
		}
	}
	flushPending()
	return out
}

// segment is one contiguous run of either plain text or one fenced code
// block (opening and closing fence lines included in lines).
type segment struct {
	isCode bool
	lang   string
	lines  []string
}

// parseSegments splits text into alternating plain-text and fenced-code
// segments by scanning line-by-line for ``` delimiters. A fence left open
// at end of input is defensively closed with a synthetic "```" line so
// downstream splitting never has to reason about an unbalanced fence.
func parseSegments(text string) []segment {
	lines := strings.Split(text, "\n")
	var segs []segment
	var cur []string
	inFence := false
	lang := ""

	flushText := func() {
		if len(cur) > 0 {
			segs = append(segs, segment{lines: cur})
			cur = nil
		}
	}

	for _, line := range lines {
		if !inFence {
			if l, ok := fenceOpenLang(line); ok {
				flushText()
				inFence = true
				lang = l
				cur = []string{line}
				continue
			}
			cur = append(cur, line)
			continue
		}

		cur = append(cur, line)
		if isFenceClose(line) {
			segs = append(segs, segment{isCode: true, lang: lang, lines: cur})
			cur = nil
			inFence = false
			lang = ""
		}
	}

	if len(cur) > 0 {
		if inFence {
			cur = append(cur, "```")
		}
		segs = append(segs, segment{isCode: inFence, lang: lang, lines: cur})
	}
	return segs
}

// fenceOpenLang reports whether line opens a fenced code block (a line
// consisting of ``` followed by an optional, space-free language tag) and,
// if so, returns that tag.
func fenceOpenLang(line string) (lang string, ok bool) {
	trimmed := strings.TrimRight(line, "\r")
	if !strings.HasPrefix(trimmed, "```") {
		return "", false
	}
	rest := trimmed[3:]
	if strings.ContainsAny(rest, " \t") {
		return "", false
	}
	return rest, true
}

// isFenceClose reports whether line closes a fence: an info-string-free
// "```" line, per CommonMark's closing-fence rule.
func isFenceClose(line string) bool {
	return strings.TrimRight(line, "\r") == "```"
}

// splitFence packs a fenced code block's inner lines into one or more
// chunks, each independently a well-formed fence reopened with lang.
func splitFence(lines []string, lang string, limit int) []string {
	open := "```" + lang
	const closeLine = "```"
	full := strings.Join(lines, "\n")
	if len(full) <= limit {
		return []string{full}
	}

	inner := ""
	if len(lines) > 2 {
		inner = strings.Join(lines[1:len(lines)-1], "\n")
	}
	overhead := len(open) + 1 /* \n */ + 1 /* \n */ + len(closeLine)
	budget := limit - overhead
	if budget < 1 {
		budget = 1
	}

	pieces := splitByLineThenHardCut(inner, budget)
	out := make([]string, 0, len(pieces))
	for _, p := range pieces {
		out = append(out, open+"\n"+p+"\n"+closeLine)
	}
	return out
}

// splitPlain packs plain (non-code) text into chunks, preferring a
// paragraph break, then a line break, then a sentence end, then a hard cut.
func splitPlain(text string, limit int) []string {
	if text == "" {
		return nil
	}
	var out []string
	remaining := text
	for len(remaining) > limit {
		cut := bestPlainCut(remaining, limit)
		out = append(out, remaining[:cut])
		// The separator itself (blank line, newline, or the space after a
		// sentence end) is dropped rather than kept on either side, so
		// neither piece carries the boundary's whitespace as noise.
		remaining = strings.TrimLeft(remaining[cut:], " \t\n")
	}
	if remaining != "" {
		out = append(out, remaining)
	}
	return out
}

// bestPlainCut picks where to end a chunk within the first limit bytes of
// s (len(s) > limit is assumed), preferring - in order - the last
// paragraph break, the last line break, the last sentence end, then a hard
// cut at a valid rune boundary. The returned index excludes the separator
// itself (splitPlain trims it off the next piece instead).
func bestPlainCut(s string, limit int) int {
	window := s[:limit]

	if i := strings.LastIndex(window, "\n\n"); i > 0 {
		return i
	}
	if i := strings.LastIndex(window, "\n"); i > 0 {
		return i
	}
	if i := lastSentenceEnd(window); i > 0 {
		return i
	}
	return safeRuneCut(s, limit)
}

// lastSentenceEnd returns the index just after the last ". ", "! ", or "? "
// in s, or -1 if none is found.
func lastSentenceEnd(s string) int {
	best := -1
	for _, sep := range [...]string{". ", "! ", "? "} {
		if i := strings.LastIndex(s, sep); i >= 0 {
			end := i + 1 // keep the punctuation, drop the trailing space
			if end > best {
				best = end
			}
		}
	}
	return best
}

// splitByLineThenHardCut packs s into chunks of at most limit bytes,
// preferring line boundaries (used for code, where paragraph/sentence
// splitting would corrupt meaning) and falling back to a hard cut for a
// single line longer than limit.
func splitByLineThenHardCut(s string, limit int) []string {
	if s == "" {
		return []string{""}
	}
	var out []string
	remaining := s
	for len(remaining) > limit {
		window := remaining[:limit]
		cut := strings.LastIndex(window, "\n")
		if cut <= 0 {
			cut = safeRuneCut(remaining, limit)
		} else {
			cut++ // keep the newline with the first piece
		}
		out = append(out, strings.TrimRight(remaining[:cut], "\n"))
		remaining = remaining[cut:]
	}
	out = append(out, remaining)
	return out
}

// safeRuneCut returns the largest index <= limit that lands on a UTF-8 rune
// boundary in s, so a hard cut never splits a multi-byte character.
func safeRuneCut(s string, limit int) int {
	if limit >= len(s) {
		return len(s)
	}
	for limit > 0 && !utf8.RuneStart(s[limit]) {
		limit--
	}
	if limit == 0 {
		limit = 1
	}
	return limit
}
