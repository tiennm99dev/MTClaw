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
// Telegram forum topic in phase 6; TerminalApprover ignores it.
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
// wired for this turn (used for cron turns in phase 8, and as registry.New's
// fallback when a caller forgets to supply one).
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

// approvalTimeoutMessage is appended to the terminal prompt so a human
// running `mtclaw prompt` knows why the process appears to hang.
const approvalPromptFooter = "approve? [y/N]: "

// TerminalApprover asks y/N on a terminal, used by `mtclaw prompt`. It owns a
// single bufio.Reader over In and a single long-lived goroutine that reads
// it line by line for the approver's entire lifetime - not one goroutine per
// Ask - so a prompt that times out cannot leave a second reader racing a
// later Ask for the same buffered stdin bytes. Consent is per-prompt: a line
// typed before a prompt was shown (e.g. an answer to a prompt that already
// timed out) is stale and is discarded at the top of the next Ask rather
// than being applied to it - see the drain loop in Ask.
type TerminalApprover struct {
	In      io.Reader
	Out     io.Writer
	Timeout time.Duration

	// lines is buffered so readLines is never parked mid-send holding a
	// line no Ask has consumed yet; a parked send could not be drained
	// non-blockingly by Ask's stale-input guard below.
	lines chan string

	mu      sync.Mutex
	readErr error // set once readLines hits EOF/error, then lines is closed
}

var _ Approver = (*TerminalApprover)(nil)

// NewTerminalApprover builds a TerminalApprover reading from in and writing
// prompts to out, waiting at most timeout for a response. It starts the
// single reader goroutine immediately so the first Ask does not race it.
func NewTerminalApprover(in io.Reader, out io.Writer, timeout time.Duration) *TerminalApprover {
	t := &TerminalApprover{
		In:      in,
		Out:     out,
		Timeout: timeout,
		lines:   make(chan string, 8),
	}
	go t.readLines()
	return t
}

// readLines is the sole reader of t.In for this TerminalApprover's whole
// lifetime. It runs until In returns an error (EOF, closed pipe), at which
// point it records that error under t.mu and closes t.lines so every Ask
// from then on - not just the next one - observes the closed channel and
// returns the same terminal error immediately instead of blocking for the
// full approval timeout.
func (t *TerminalApprover) readLines() {
	r := bufio.NewReader(t.In)
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
	if err := t.terminalError(); err != nil {
		return false, fmt.Errorf("tools: read approval response: %w", err)
	}

	// Drain any line already queued before this prompt is shown, so a
	// human's answer to a previous prompt (in particular one that already
	// timed out) can never be mistaken for the answer to this one. This is
	// the real enforcement boundary alongside deny: consent must be
	// per-prompt.
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

	fmt.Fprintf(t.Out, "\n[mtclaw] approval requested for tool %q\n  command: %s\n", req.Tool, req.Command)
	if req.Reason != "" {
		fmt.Fprintf(t.Out, "  reason: %s\n", req.Reason)
	}
	fmt.Fprintf(t.Out, "  (times out in %s) %s", t.Timeout, approvalPromptFooter)

	// context.WithTimeout(ctx, ...) inherits ctx's own cancellation, so a
	// single derived context distinguishes both fail-closed cases by its
	// Err() once Done: context.Canceled means the caller's ctx ended first
	// (the turn itself was canceled), context.DeadlineExceeded means only
	// this wait's own Timeout elapsed.
	waitCtx, cancel := context.WithTimeout(ctx, t.Timeout)
	defer cancel()

	select {
	case <-waitCtx.Done():
		fmt.Fprintln(t.Out, "\n[mtclaw] no response in time; refusing")
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

// maxDisplayCommandLen bounds how much of a command RedactSecrets will
// show before truncating, so an enormous heredoc or base64 blob does not
// blow out a terminal or a Telegram message.
const maxDisplayCommandLen = 800

// RedactSecrets is applied to every approval prompt and to
// exec_audit.command before either is written or displayed. It is
// best-effort pattern matching over plain text, not a security boundary:
// the real rule is "do not let the agent handle credentials as command
// arguments" (put them in an env file the command reads instead). The
// command that is actually executed is never altered by this function -
// only what is shown to a human and what is stored is.
func RedactSecrets(cmd string) string {
	s := cmd
	for _, re := range redactPatterns {
		s = re.re.ReplaceAllString(s, re.replacement)
	}
	s = redactBase64Like(s)
	if len(s) > maxDisplayCommandLen {
		cut := runeSafeLen([]byte(s[:maxDisplayCommandLen]))
		s = s[:cut] + "... [truncated; command is longer]"
	}
	return s
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
	// "Bearer <token>", case-insensitive, stops at the next space or quote.
	{regexp.MustCompile(`(?i)(bearer\s+)([^\s"']+)`), `${1}[REDACTED]`},
	// "Authorization: <value>"
	{regexp.MustCompile(`(?i)(authorization:\s*)(\S+)`), `${1}[REDACTED]`},
	// --token, --password, --secret* flags, "=value" or " value" form
	{regexp.MustCompile(`(?i)(--token[= ])(\S+)`), `${1}[REDACTED]`},
	{regexp.MustCompile(`(?i)(--password[= ])(\S+)`), `${1}[REDACTED]`},
	{regexp.MustCompile(`(?i)(--secret\S*[= ])(\S+)`), `${1}[REDACTED]`},
	// "-p<value>" glued together (mysql/psql style), e.g. -pSecret123.
	// Deliberately over-broad: it also matches an unrelated "-pfoo" style
	// flag on some other tool. Over-redaction is the safe failure
	// direction for a display-only value; see the package-level note above.
	{regexp.MustCompile(`(\s-p)(\S+)`), `${1}[REDACTED]`},
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
	{regexp.MustCompile(`(?i)(^|[^A-Za-z0-9_])([A-Za-z0-9_]*(?:KEY|TOKEN|SECRET|PASSWD|PASSWORD))=(\S+)`), `${1}${2}=[REDACTED]`},
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
