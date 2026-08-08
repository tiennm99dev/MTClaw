package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/tiennm99/MTClaw/internal/channel/telegram"
)

// prompter is onboard's seam for interactive input. Production code drives
// stdioPrompter against a real terminal; onboard_test.go drives a scripted
// fake instead of a TTY, so onboard's whole sequence - including the
// Telegram ID capture - is covered by ordinary, hermetic unit tests.
type prompter interface {
	// Text asks a free-text question. def is shown as the default and
	// returned verbatim when the answer is an empty line.
	Text(question, def string) (string, error)
	// Secret asks for a value that must never be written to disk or logged.
	// The real implementation also avoids echoing it to the terminal when
	// one is attached; it is used only in memory for the remainder of
	// onboard's run.
	Secret(question string) (string, error)
	// Confirm asks a yes/no question, returning defaultYes on an empty
	// answer.
	Confirm(question string, defaultYes bool) (bool, error)
	// Printf writes an informational line: no question, no response read.
	Printf(format string, args ...any)
}

// stdioPrompter is prompter's real, TTY-driven implementation.
type stdioPrompter struct {
	in      *bufio.Reader
	out     io.Writer
	stdinFd int // used only to detect a real terminal for hidden Secret input
}

var _ prompter = (*stdioPrompter)(nil)

// newStdioPrompter builds a stdioPrompter reading from in and writing to
// out. stdinFd is the OS file descriptor backing in, used solely to detect
// whether Secret can hide its input; pass a negative value (or any fd that
// is not a terminal) to always fall back to a plain, visible line read.
func newStdioPrompter(in io.Reader, out io.Writer, stdinFd int) *stdioPrompter {
	return &stdioPrompter{in: bufio.NewReader(in), out: out, stdinFd: stdinFd}
}

func (p *stdioPrompter) Printf(format string, args ...any) {
	fmt.Fprintf(p.out, format, args...)
}

func (p *stdioPrompter) Text(question, def string) (string, error) {
	if def != "" {
		fmt.Fprintf(p.out, "%s [%s]: ", question, def)
	} else {
		fmt.Fprintf(p.out, "%s: ", question)
	}
	line, err := p.readLine()
	if err != nil {
		return "", err
	}
	if line == "" {
		return def, nil
	}
	return line, nil
}

func (p *stdioPrompter) Secret(question string) (string, error) {
	fmt.Fprintf(p.out, "%s: ", question)
	if p.stdinFd >= 0 && term.IsTerminal(p.stdinFd) {
		data, err := term.ReadPassword(p.stdinFd)
		fmt.Fprintln(p.out)
		if err != nil {
			return "", fmt.Errorf("read secret input: %w", err)
		}
		return strings.TrimSpace(string(data)), nil
	}
	return p.readLine()
}

func (p *stdioPrompter) Confirm(question string, defaultYes bool) (bool, error) {
	suffix := "[y/N]"
	if defaultYes {
		suffix = "[Y/n]"
	}
	fmt.Fprintf(p.out, "%s %s: ", question, suffix)
	line, err := p.readLine()
	if err != nil {
		return false, err
	}
	line = strings.ToLower(line)
	if line == "" {
		return defaultYes, nil
	}
	return line == "y" || line == "yes", nil
}

func (p *stdioPrompter) readLine() (string, error) {
	line, err := p.in.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", fmt.Errorf("read input: %w", err)
	}
	return strings.TrimSpace(line), nil
}

// telegramCapturer is onboard's seam for every Telegram network call it
// makes (getMe, and the ID-capture long poll), so tests can drive the whole
// Telegram half of onboard - including the capture window - with a fake
// instead of a live bot and a real 60s wait.
type telegramCapturer interface {
	// GetMe confirms token is a working bot token, returning its username.
	GetMe(ctx context.Context, token string) (username string, err error)
	// Capture blocks for the full window (or until ctx ends), returning
	// every distinct sender seen, in first-seen order. It must not return
	// early on the first message - a second, later sender is exactly the
	// case onboard needs to catch.
	Capture(ctx context.Context, token string, window time.Duration) ([]telegram.Sender, error)
}

// realTelegramCapturer is telegramCapturer's production implementation,
// delegating to internal/channel/telegram.
type realTelegramCapturer struct{}

var _ telegramCapturer = realTelegramCapturer{}

func (realTelegramCapturer) GetMe(ctx context.Context, token string) (string, error) {
	// "" for apiBaseURL: onboard runs before any config file exists, so
	// there is nothing to read channels.telegram.api_base_url from - it
	// always talks to the real api.telegram.org.
	return telegram.GetMe(ctx, token, "")
}

func (realTelegramCapturer) Capture(ctx context.Context, token string, window time.Duration) ([]telegram.Sender, error) {
	return telegram.CaptureSenders(ctx, token, "", window)
}
