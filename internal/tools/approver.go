package tools

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
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

// TerminalApprover asks y/N on a terminal, used by `mtclaw prompt`. Reading
// stdin happens on its own goroutine so Ask can still return promptly on ctx
// cancellation or its own Timeout even though there is no portable way to
// cancel a blocking bufio.Reader.ReadString call.
type TerminalApprover struct {
	In      io.Reader
	Out     io.Writer
	Timeout time.Duration
}

var _ Approver = (*TerminalApprover)(nil)

// NewTerminalApprover builds a TerminalApprover reading from in and writing
// prompts to out, waiting at most timeout for a response.
func NewTerminalApprover(in io.Reader, out io.Writer, timeout time.Duration) *TerminalApprover {
	return &TerminalApprover{In: in, Out: out, Timeout: timeout}
}

func (t *TerminalApprover) Ask(ctx context.Context, req Request) (bool, error) {
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

	lineCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		line, err := bufio.NewReader(t.In).ReadString('\n')
		if err != nil {
			errCh <- err
			return
		}
		lineCh <- line
	}()

	select {
	case <-waitCtx.Done():
		fmt.Fprintln(t.Out, "\n[mtclaw] no response in time; refusing")
		return false, waitCtx.Err()
	case err := <-errCh:
		return false, fmt.Errorf("tools: read approval response: %w", err)
	case line := <-lineCh:
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
	if len(s) > maxDisplayCommandLen {
		s = s[:maxDisplayCommandLen] + "... [truncated; command is longer]"
	}
	return s
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
	// KEY=, TOKEN=, SECRET=, PASSWORD= environment-style assignments.
	{regexp.MustCompile(`(?i)\b(KEY|TOKEN|SECRET|PASSWORD)=(\S+)`), `${1}=[REDACTED]`},
	// Common credential shapes: OpenAI sk-..., GitHub ghp_..., AWS AKIA...,
	// and long base64/hex runs that are likely to be a key rather than
	// prose.
	{regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{8,}\b`), "[REDACTED]"},
	{regexp.MustCompile(`\bghp_[A-Za-z0-9]{20,}\b`), "[REDACTED]"},
	{regexp.MustCompile(`\bAKIA[0-9A-Z]{12,}\b`), "[REDACTED]"},
	{regexp.MustCompile(`\b[A-Za-z0-9+/]{32,}={0,2}\b`), "[REDACTED]"},
}
