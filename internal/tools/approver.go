package tools

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Request is one approval ask. ThreadID routes the prompt to the right
// Telegram forum topic; TerminalApprover ignores it.
type Request struct {
	SessionID string
	Channel   string
	ChatID    string
	ThreadID  string
	Tool      string
	Command   string // already RedactSecrets'd by the caller
	Reason    string
	// MessageID is the id of the message that triggered this turn, when the
	// channel supplies one, so an interactive approver can quote it instead
	// of posting an unanchored prompt. TerminalApprover ignores it.
	MessageID string
}

// Approver decides one Request. An approval prompt leaves the host: the
// command is rendered into whatever surface the approver uses (a terminal,
// a Telegram message that persists on Telegram's servers indefinitely), so
// every caller must pass an already-redacted Request.Command - see
// RedactSecrets.
type Approver interface {
	// Ask returns approved=true only on an explicit affirmative decision.
	// A non-nil error means no decision was reached (the wait timed out, or
	// no interactive approver exists at all); callers must treat that the
	// same as a denial and must not run the command, but should record it
	// under a distinct audit label ("expired") rather than "denied_user" so
	// the audit trail distinguishes a human saying no from nobody being
	// asked. context.Canceled specifically (as opposed to
	// context.DeadlineExceeded from the approver's own timeout) means the
	// caller's ctx ended first - typically the turn itself being canceled -
	// and callers should propagate that instead of treating it as a
	// same-as-denial outcome.
	Ask(ctx context.Context, req Request) (approved bool, err error)
}

// ErrNoApprover is DenyAllApprover's error: no interactive approver is
// wired for this turn (used for cron turns, which have no interactive
// approver at all, and as registry.New's fallback when a caller forgets to
// supply one).
var ErrNoApprover = errors.New("tools: no interactive approver is available for this turn")

// DenyAllApprover always denies, without blocking. It is the safe default
// when no interactive approver exists: a cron job has no terminal and no
// chat to prompt, so "cannot ask" must mean "refuse", not "run
// unattended".
type DenyAllApprover struct{}

var _ Approver = DenyAllApprover{}

func (DenyAllApprover) Ask(_ context.Context, _ Request) (bool, error) {
	return false, ErrNoApprover
}

// approvalPromptFooter is appended to the terminal prompt so a human
// running `mtclaw prompt` knows why the process appears to hang.
const approvalPromptFooter = "approve? [y/N]: "

// TerminalApprover asks y/N on a terminal, used by `mtclaw prompt`. It owns a
// single bufio.Reader over in and a single long-lived goroutine that reads
// it line by line for the approver's entire lifetime - not one goroutine per
// Ask - so a prompt that times out cannot leave a second reader racing a
// later Ask for the same buffered stdin bytes. Consent is per-prompt: a line
// typed before a prompt was shown (e.g. an answer to a prompt that already
// timed out) is stale and is discarded at the top of the next Ask rather
// than being applied to it - see the drain loop in Ask.
type TerminalApprover struct {
	in      io.Reader
	out     io.Writer
	timeout time.Duration

	// lines is buffered so readLines is never parked mid-send holding a
	// line no Ask has consumed yet; a parked send could not be drained
	// non-blockingly by Ask's stale-input guard below.
	lines chan string

	mu      sync.Mutex
	readErr error // set once readLines hits EOF/error, then lines is closed
	// unanswered is set when the previous Ask ended without consuming a
	// line (timeout or cancellation). Anything typed after that point and
	// before the next prompt was an answer to the dead prompt, so the next
	// Ask discards it. Lines queued while no prompt ever went unanswered
	// are legitimate pipeline input (`printf 'y\n' | mtclaw prompt ...`)
	// and are consumed in order.
	unanswered bool
}

var _ Approver = (*TerminalApprover)(nil)

// NewTerminalApprover builds a TerminalApprover reading from in and writing
// prompts to out, waiting at most timeout for a response. It starts the
// single reader goroutine immediately so the first Ask does not race it.
func NewTerminalApprover(in io.Reader, out io.Writer, timeout time.Duration) *TerminalApprover {
	t := &TerminalApprover{
		in:      in,
		out:     out,
		timeout: timeout,
		lines:   make(chan string, 8),
	}
	go t.readLines()
	return t
}

// readLines is the sole reader of t.in for this TerminalApprover's whole
// lifetime. It runs until in returns an error (EOF, closed pipe), at which
// point it records that error under t.mu and closes t.lines so every Ask
// from then on - not just the next one - observes the closed channel and
// returns the same terminal error immediately instead of blocking for the
// full approval timeout.
func (t *TerminalApprover) readLines() {
	r := bufio.NewReader(t.in)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.mu.Lock()
			t.readErr = err
			t.mu.Unlock()
			close(t.lines)
			return
		}
		t.lines <- line
	}
}

func (t *TerminalApprover) terminalError() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.readErr
}

func (t *TerminalApprover) Ask(ctx context.Context, req Request) (bool, error) {
	// Consent must be per-prompt: a human's late answer to a prompt that
	// already timed out can never be mistaken for the answer to this one.
	// Only that case drains, so answers piped in ahead of time still work.
	// EOF is not checked up front: a closed channel with answers still
	// buffered must hand them out first, and once empty the receive below
	// returns immediately with ok == false, which is the fast failure.
	t.mu.Lock()
	drainStale := t.unanswered
	t.unanswered = false
	t.mu.Unlock()
	if drainStale {
	drain:
		for {
			select {
			case _, ok := <-t.lines:
				if !ok {
					break drain
				}
			default:
				break drain
			}
		}
	}

	fmt.Fprintf(t.out, "\n[mtclaw] approval requested for tool %q\n  command: %s\n", req.Tool, req.Command)
	if req.Reason != "" {
		fmt.Fprintf(t.out, "  reason: %s\n", req.Reason)
	}
	fmt.Fprintf(t.out, "  (times out in %s) %s", t.timeout, approvalPromptFooter)

	// context.WithTimeout(ctx, ...) inherits ctx's own cancellation, so a
	// single derived context distinguishes both fail-closed cases by its
	// Err() once Done: context.Canceled means the caller's ctx ended first
	// (the turn itself was canceled), context.DeadlineExceeded means only
	// this wait's own timeout elapsed.
	waitCtx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()

	select {
	case <-waitCtx.Done():
		t.mu.Lock()
		t.unanswered = true
		t.mu.Unlock()
		fmt.Fprintln(t.out, "\n[mtclaw] no response in time; refusing")
		return false, waitCtx.Err()
	case line, ok := <-t.lines:
		if !ok {
			return false, fmt.Errorf("tools: read approval response: %w", t.terminalError())
		}
		answer := strings.ToLower(strings.TrimSpace(line))
		return answer == "y" || answer == "yes", nil
	}
}

// --- Secret redaction -------------------------------------------------

// maxDisplayCommandLen bounds how much of a redacted command an approval
// prompt may show a human. It sits comfortably below Telegram's
// 4096-character message cap so the command plus its reason and footer
// text still fit in one message. A command whose redacted form is longer
// than this is refused before ever reaching a prompt (see execTool.ask) -
// approving from a preview the human cannot fully see would defeat the
// point of asking.
const maxDisplayCommandLen = 3500

// maxAuditCommandLen bounds how much of a redacted command exec_audit
// stores. It is far more generous than maxDisplayCommandLen: exec_audit is
// the durable forensic record (see docs/security.md), so it keeps the
// whole command for any realistic input and only cuts a truly enormous
// one (a heredoc or base64 blob) rather than losing the tail the way a
// terminal-width truncation would.
const maxAuditCommandLen = 64 * 1024

// credentialValue is the character class every redactPatterns rule uses to
// capture the value half of a credential-shaped match: letters, digits, and
// the punctuation real key/token/URL-safe-base64 shapes use (dot,
// underscore, tilde, plus, slash, equals, colon, at, percent, hyphen). It
// deliberately excludes whitespace and every shell metacharacter (the
// semicolon, pipe, ampersand, dollar sign, backtick, parentheses, angle
// brackets, and quotes): a value that contains one of those is not a
// credential shape, so the match simply stops there instead of swallowing
// the rest of the command line into "[REDACTED]" - which would hide
// injected shell code from both the approval prompt and exec_audit.
const credentialValue = `[A-Za-z0-9._~+/=:@%-]+`

// RedactSecrets replaces credential-shaped values in cmd with
// "[REDACTED]" and is applied to every approval prompt and to
// exec_audit.command before either is written or displayed. It is
// best-effort pattern matching over plain text, not a security boundary:
// the real rule is "do not let the agent handle credentials as command
// arguments" (put them in an env file the command reads instead). The
// command that is actually executed is never altered by this function -
// only what is shown to a human and what is stored is. RedactSecrets never
// truncates: display length and audit storage length are separate
// concerns handled by displayCommand and capForAudit respectively.
func RedactSecrets(cmd string) string {
	s := cmd
	for _, re := range redactPatterns {
		s = re.re.ReplaceAllString(s, re.replacement)
	}
	return redactBase64Like(s)
}

// capForAudit truncates a redacted command to maxAuditCommandLen on a rune
// boundary, marking the cut so a forensic read of exec_audit is never
// silently short. Real commands almost never approach this cap; it exists
// only to bound a pathological input (an enormous heredoc or base64 blob).
func capForAudit(redacted string) string {
	if len(redacted) <= maxAuditCommandLen {
		return redacted
	}
	cut := runeSafeLen([]byte(redacted[:maxAuditCommandLen]))
	return redacted[:cut] + "... [truncated; command is longer]"
}

// displayCommand prepares a redacted command for a human-facing approval
// prompt (terminal or Telegram): it escapes control and bidi characters
// that could redraw a terminal line or reorder how the text renders (a
// human approving something other than what they can see is the same
// class of problem RedactSecrets' tightened credentialValue class closes),
// then refuses - ok is false - when the escaped result is still longer
// than maxDisplayCommandLen. It never truncates the display copy: an
// approval from a preview the human cannot see in full is not a real
// approval.
func displayCommand(redacted string) (display string, ok bool) {
	escaped := escapeControlAndBidi(redacted)
	if len(escaped) > maxDisplayCommandLen {
		return "", false
	}
	return escaped, true
}

// maxDisplayReasonLen bounds how much of a classifier-produced Reason an
// approval prompt shows a human. Unlike maxDisplayCommandLen, going over
// this cap truncates rather than refusing the whole approval: a Reason is
// supplementary context an auto-mode classifier attaches, not the thing
// being approved, so losing its tail is acceptable where losing the
// command's own tail would not be - and refusing outright would fail an
// approval closed over nothing more than a verbose classifier.
const maxDisplayReasonLen = 300

// sanitizeReason prepares a Policy.Evaluate Reason for every surface
// Request.Reason reaches - a human-facing approval prompt (terminal or
// Telegram) and the approvals table row. In auto mode, Reason is free text
// an LLM classifier writes after seeing the raw, unredacted command (see
// evaluateAuto): without this, a classifier that quotes the command back
// verbatim would leak a secret RedactSecrets already stripped from
// Request.Command, and unescaped control/bidi characters or an unbounded
// length could reorder the prompt's text or push it past a channel's
// message-size limit - the same class of problem displayCommand exists to
// close for the command itself. execTool.ask, the one place that turns
// every VerdictAsk Decision into a Request, calls this so every path is
// covered without each policy.go call site needing to remember to.
func sanitizeReason(reason string) string {
	s := escapeControlAndBidi(RedactSecrets(reason))
	if len(s) <= maxDisplayReasonLen {
		return s
	}
	cut := runeSafeLen([]byte(s[:maxDisplayReasonLen]))
	return s[:cut] + "... [truncated]"
}

// escapeControlAndBidi rewrites s so every C0 control character other than
// "\n" and "\t", DEL, every C1 control character, and every Unicode
// bidirectional-override/isolate character (U+202A-U+202E,
// U+2066-U+2069) is shown as a visible "\xNN" or "\uNNNN" escape instead of
// being interpreted by the terminal or Telegram renderer that displays it.
// A "\r" plus an ANSI erase-line sequence can redraw what a terminal shows
// after the real command; a bidi override can reorder how a command reads
// in Telegram. Both let a human approve something other than what they are
// looking at, the same failure mode a value that swallows shell
// metacharacters during redaction would cause, so this runs on every
// command shown to a human, never on what is actually executed.
func escapeControlAndBidi(s string) string {
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

// runeSafeLen returns the largest n <= len(b) such that b[:n] does not end
// mid-rune, so a hard byte-length cap (here, and on exec's captured output)
// can never split a multi-byte UTF-8 character in two. It backtracks at
// most utf8.UTFMax-1 bytes - the most a single valid rune could still be
// waiting on - so input that is not valid UTF-8 at all (e.g. binary output
// wrongly treated as text) cannot walk all the way back to an empty prefix;
// the cut is kept at the raw byte boundary instead.
func runeSafeLen(b []byte) int {
	n := len(b)
	limit := n - utf8.UTFMax + 1
	if limit < 0 {
		limit = 0
	}
	for n > limit {
		if r, size := utf8.DecodeLastRune(b[:n]); r != utf8.RuneError || size > 1 {
			break
		}
		n--
	}
	return n
}

type redactRule struct {
	re          *regexp.Regexp
	replacement string
}

var redactPatterns = []redactRule{
	// "Bearer <token>", case-insensitive, stops at the first character
	// outside credentialValue (space, quote, or any shell metacharacter).
	{regexp.MustCompile(`(?i)(bearer\s+)(` + credentialValue + `)`), `${1}[REDACTED]`},
	// "Authorization: <value>"
	{regexp.MustCompile(`(?i)(authorization:\s*)(` + credentialValue + `)`), `${1}[REDACTED]`},
	// --token, --password, --secret* flags, "=value" or " value" form
	{regexp.MustCompile(`(?i)(--token[= ])(` + credentialValue + `)`), `${1}[REDACTED]`},
	{regexp.MustCompile(`(?i)(--password[= ])(` + credentialValue + `)`), `${1}[REDACTED]`},
	{regexp.MustCompile(`(?i)(--secret\S*[= ])(` + credentialValue + `)`), `${1}[REDACTED]`},
	// "-p<value>" glued together (mysql/psql style), e.g. -pSecret123.
	// Deliberately over-broad: it also matches an unrelated "-pfoo" style
	// flag on some other tool. Over-redaction is the safe failure
	// direction for a display-only value; see the package-level note above.
	{regexp.MustCompile(`(\s-p)(` + credentialValue + `)`), `${1}[REDACTED]`},
	// *_KEY=, *_TOKEN=, *_SECRET=, *_PASSWORD=, *_PASSWD= environment-style
	// assignments (API_KEY=, GITHUB_TOKEN=, DB_PASSWORD=,
	// AWS_SECRET_ACCESS_KEY=, PASSWD=, the bare PASSWORD= form, and the same
	// keyword glued onto a flag or URL query string: --api-key=,
	// ?token=..., &token=...): the keyword is matched by suffix, not \b,
	// because \b never fires between "_" and a following letter (both are
	// word characters), so a plain \bKEY\b would silently skip the API_KEY=
	// shape almost every real assignment uses. The character before the
	// keyword run only has to be a non-identifier character (or the start
	// of the string) - not one of a fixed punctuation set - so this also
	// catches the keyword right after a "-", "?", or "&" that a closed
	// boundary class would miss.
	{regexp.MustCompile(`(?i)(^|[^A-Za-z0-9_])([A-Za-z0-9_]*(?:KEY|TOKEN|SECRET|PASSWD|PASSWORD))=(` + credentialValue + `)`), `${1}${2}=[REDACTED]`},
	// Common credential shapes: OpenAI sk-..., GitHub ghp_..., AWS AKIA...
	{regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{8,}\b`), "[REDACTED]"},
	{regexp.MustCompile(`\bghp_[A-Za-z0-9]{20,}\b`), "[REDACTED]"},
	{regexp.MustCompile(`\bAKIA[0-9A-Z]{12,}\b`), "[REDACTED]"},
}

// base64Like matches a candidate base64-style secret: a run of at least 32
// characters from the base64 alphabet including "/" (real secrets - AWS
// secret access keys in particular - commonly contain "/"), with optional
// "=" padding, plus whatever single character immediately precedes it (or
// nothing, at the start of the string). "/" has to stay in the class for
// the secret case, which makes a long filesystem path (e.g. a repo
// checkout under a deep directory tree) exactly as long as a real secret
// and built from the same character set; redactBase64Like tells them apart
// instead of excluding "/" outright.
var base64Like = regexp.MustCompile(`(^|.)([A-Za-z0-9+/]{32,}={0,2})`)

// redactBase64Like replaces each base64Like candidate with [REDACTED],
// unless it looks path-shaped (it starts with "/", or is immediately
// preceded by "/" - either means it is a segment of a "/"-joined path, not
// a standalone secret) or it lacks the mix a real key almost always has: at
// least one uppercase letter, one lowercase letter, and one digit. A bare
// path segment is rarely all three at once.
func redactBase64Like(s string) string {
	locs := base64Like.FindAllStringSubmatchIndex(s, -1)
	if locs == nil {
		return s
	}
	var b strings.Builder
	last := 0
	for _, loc := range locs {
		wholeStart, wholeEnd := loc[0], loc[1]
		prefix := s[loc[2]:loc[3]]
		secret := s[loc[4]:loc[5]]
		b.WriteString(s[last:wholeStart])
		if prefix == "/" || strings.HasPrefix(secret, "/") || !hasUpperLowerDigit(secret) {
			b.WriteString(s[wholeStart:wholeEnd])
		} else {
			b.WriteString(prefix)
			b.WriteString("[REDACTED]")
		}
		last = wholeEnd
	}
	b.WriteString(s[last:])
	return b.String()
}

// hasUpperLowerDigit reports whether s contains at least one ASCII
// uppercase letter, one lowercase letter, and one digit.
func hasUpperLowerDigit(s string) bool {
	var upper, lower, digit bool
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			upper = true
		case r >= 'a' && r <= 'z':
			lower = true
		case r >= '0' && r <= '9':
			digit = true
		}
		if upper && lower && digit {
			return true
		}
	}
	return false
}
