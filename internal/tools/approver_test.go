package tools

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDenyAllApprover_NeverBlocksAndAlwaysDenies(t *testing.T) {
	start := time.Now()
	approved, err := DenyAllApprover{}.Ask(context.Background(), Request{Tool: "exec", Command: "ls"})
	assert.Less(t, time.Since(start), 50*time.Millisecond, "DenyAllApprover must never block")
	assert.False(t, approved)
	assert.ErrorIs(t, err, ErrNoApprover)
}

func TestTerminalApprover_Approves(t *testing.T) {
	in := strings.NewReader("y\n")
	var out bytes.Buffer
	a := NewTerminalApprover(in, &out, time.Second)

	approved, err := a.Ask(context.Background(), Request{Tool: "exec", Command: "ls -la", Reason: "listing files"})
	require.NoError(t, err)
	assert.True(t, approved)
	assert.Contains(t, out.String(), "ls -la")
	assert.Contains(t, out.String(), "listing files")
}

func TestTerminalApprover_Denies(t *testing.T) {
	in := strings.NewReader("n\n")
	var out bytes.Buffer
	a := NewTerminalApprover(in, &out, time.Second)

	approved, err := a.Ask(context.Background(), Request{Tool: "exec", Command: "ls"})
	require.NoError(t, err)
	assert.False(t, approved)
}

func TestTerminalApprover_EmptyLineDenies(t *testing.T) {
	in := strings.NewReader("\n")
	var out bytes.Buffer
	a := NewTerminalApprover(in, &out, time.Second)

	approved, err := a.Ask(context.Background(), Request{Tool: "exec", Command: "ls"})
	require.NoError(t, err)
	assert.False(t, approved)
}

// blockingReader never produces a line, simulating a human who has not
// answered yet, so TerminalApprover's own Timeout is what ends the wait.
type blockingReader struct{ done chan struct{} }

func (b *blockingReader) Read(p []byte) (int, error) {
	<-b.done
	return 0, errors.New("blockingReader: closed")
}

func TestTerminalApprover_TimesOut(t *testing.T) {
	r := &blockingReader{done: make(chan struct{})}
	defer close(r.done)
	var out bytes.Buffer
	a := NewTerminalApprover(r, &out, 20*time.Millisecond)

	start := time.Now()
	approved, err := a.Ask(context.Background(), Request{Tool: "exec", Command: "ls"})
	elapsed := time.Since(start)

	assert.False(t, approved)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, elapsed, time.Second)
}

// TestTerminalApprover_OuterContextCanceledIsDistinguishable proves the
// exec tool's "turn was canceled" propagation path can tell the two
// fail-closed cases apart: canceling the caller's own ctx (not the
// approver's Timeout) must surface as context.Canceled specifically, not
// context.DeadlineExceeded, even though both derive from the same
// WithTimeout call internally.
func TestTerminalApprover_OuterContextCanceledIsDistinguishable(t *testing.T) {
	r := &blockingReader{done: make(chan struct{})}
	defer close(r.done)
	var out bytes.Buffer
	a := NewTerminalApprover(r, &out, time.Minute) // long enough that only ctx cancellation can end this

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	approved, err := a.Ask(ctx, Request{Tool: "exec", Command: "ls"})
	assert.False(t, approved)
	assert.ErrorIs(t, err, context.Canceled)
}

// --- RedactSecrets ------------------------------------------------------

func TestRedactSecrets_BearerToken(t *testing.T) {
	cmd := `curl -H "Authorization: Bearer sk-abc123" https://api.example.com`
	got := RedactSecrets(cmd)
	assert.NotContains(t, got, "sk-abc123")
	assert.NotContains(t, got, "Bearer sk-abc123")
}

func TestRedactSecrets_EnvAssignment(t *testing.T) {
	got := RedactSecrets("PASSWORD=hunter2 psql -c 'select 1'")
	assert.NotContains(t, got, "hunter2")
	assert.Contains(t, got, "PASSWORD=[REDACTED]")
}

func TestRedactSecrets_TokenAndSecretFlags(t *testing.T) {
	got := RedactSecrets("mytool --token abc123XYZ --secret-key deadbeef1234")
	assert.NotContains(t, got, "abc123XYZ")
	assert.NotContains(t, got, "deadbeef1234")
}

func TestRedactSecrets_AWSAccessKey(t *testing.T) {
	got := RedactSecrets("aws configure set aws_access_key_id AKIAABCDEFGHIJKLMNOP")
	assert.NotContains(t, got, "AKIAABCDEFGHIJKLMNOP")
}

func TestRedactSecrets_GitHubToken(t *testing.T) {
	got := RedactSecrets("git remote set-url origin https://ghp_1234567890abcdefghijklmnopqrstuvwxyz@github.com/x/y")
	assert.NotContains(t, got, "ghp_1234567890abcdefghijklmnopqrstuvwxyz")
}

func TestRedactSecrets_TruncatesLongCommands(t *testing.T) {
	// Repeated short words, not a single long run: a long run of
	// alphanumerics would itself match the base64/hex key-shape pattern and
	// get collapsed to "[REDACTED]" before truncation is even relevant.
	long := strings.Repeat("echo hi; ", 200)
	got := RedactSecrets(long)
	assert.LessOrEqual(t, len(got), maxDisplayCommandLen+64)
	assert.Contains(t, got, "truncated")
}

func TestRedactSecrets_DoesNotAlterUnrelatedCommand(t *testing.T) {
	cmd := "ls -la /workspace"
	assert.Equal(t, cmd, RedactSecrets(cmd))
}

func TestRedactSecrets_APIKeyAssignment(t *testing.T) {
	got := RedactSecrets("export API_KEY=abc123")
	assert.NotContains(t, got, "abc123")
	assert.Contains(t, got, "API_KEY=[REDACTED]")
}

func TestRedactSecrets_SuffixedEnvAssignmentForms(t *testing.T) {
	cases := []string{
		"GITHUB_TOKEN=ghtoken123 gh auth login",
		"DB_PASSWORD=hunter2 psql",
		"AWS_SECRET_ACCESS_KEY=verysecretvalue aws s3 ls",
		"PASSWD=hunter2 chpasswd",
	}
	for _, cmd := range cases {
		got := RedactSecrets(cmd)
		assert.Contains(t, got, "[REDACTED]", "command: %q", cmd)
		assert.NotContains(t, got, "hunter2", "command: %q", cmd)
	}
}

func TestRedactSecrets_LongPathSurvivesRedaction(t *testing.T) {
	cmd := "ls /workspace/tiennm99dev/MTClaw/internal/tools"
	assert.Equal(t, cmd, RedactSecrets(cmd))
}

func TestRedactSecrets_URLQueryTokenAssignment(t *testing.T) {
	got := RedactSecrets(`curl "https://api.example.com/v1?token=SECRET123"`)
	assert.NotContains(t, got, "SECRET123")
	assert.Contains(t, got, "token=[REDACTED]")
}

func TestRedactSecrets_APIKeyFlagAssignment(t *testing.T) {
	got := RedactSecrets("mytool --api-key=abc123XYZsecret")
	assert.NotContains(t, got, "abc123XYZsecret")
	assert.Contains(t, got, "key=[REDACTED]")
}

func TestRedactSecrets_AWSStyleSecretWithSlashIsRedacted(t *testing.T) {
	got := RedactSecrets("aws configure set aws_secret_access_key wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY")
	assert.NotContains(t, got, "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY")
	assert.Contains(t, got, "[REDACTED]")
}

// TestRedactSecrets_LongPathSurvivesRedaction already covers the base
// "/"-preceded path case; this covers a path that itself begins with "/"
// as the very first character of the command.
func TestRedactSecrets_PathAtStartOfCommandSurvivesRedaction(t *testing.T) {
	cmd := "/workspace/tiennm99dev/MTClaw/internal/tools/exec.go"
	assert.Equal(t, cmd, RedactSecrets(cmd))
}

// TestRuneSafeLen_BoundedBacktrackOnInvalidUTF8 proves runeSafeLen never
// walks all the way back to an empty prefix on input that is not valid
// UTF-8 at all: it backtracks at most utf8.UTFMax-1 bytes, so a long run of
// invalid bytes still yields a non-empty, if imperfectly cut, prefix.
func TestRuneSafeLen_BoundedBacktrackOnInvalidUTF8(t *testing.T) {
	b := bytes.Repeat([]byte{0xff}, 900)
	n := runeSafeLen(b)
	assert.NotZero(t, n, "runeSafeLen must not erase the entire buffer on invalid UTF-8")
	assert.GreaterOrEqual(t, n, len(b)-3, "runeSafeLen must backtrack at most 3 bytes")
}

func TestRedactSecrets_TruncationIsRuneSafe(t *testing.T) {
	long := strings.Repeat("あ", 400) // multi-byte content, well past maxDisplayCommandLen
	got := RedactSecrets(long)
	body := strings.TrimSuffix(got, "... [truncated; command is longer]")
	assert.True(t, utf8.ValidString(body), "truncated command must not split a multi-byte rune")
}

// TestTerminalApprover_TimedOutAskDoesNotStealTheNextAnswer proves consent
// is per-prompt: a "y" written for a prompt that already timed out must
// never be silently applied to a later, different prompt the human never
// saw - that would let the human's consent to one command approve a
// different one. The single long-lived reader (not one goroutine per Ask)
// is still correct; what changed is that Ask now discards stale input
// queued before its own prompt was shown instead of consuming it.
func TestTerminalApprover_TimedOutAskDoesNotStealTheNextAnswer(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	var out bytes.Buffer
	a := NewTerminalApprover(pr, &out, 20*time.Millisecond)

	approved, err := a.Ask(context.Background(), Request{Tool: "exec", Command: "first"})
	assert.False(t, approved)
	assert.ErrorIs(t, err, context.DeadlineExceeded)

	// Written only after the first Ask already timed out, and confirmed
	// queued in the reader's own buffer (not merely handed to the pipe)
	// before the second prompt is asked at all: this must not be treated
	// as an answer to the second prompt.
	go func() {
		_, _ = pw.Write([]byte("y\n"))
	}()
	require.Eventually(t, func() bool { return len(a.lines) == 1 }, time.Second, time.Millisecond,
		"stale line never reached the reader's buffer")

	approved, err = a.Ask(context.Background(), Request{Tool: "exec", Command: "second"})
	assert.False(t, approved, "a 'y' typed before the second prompt was shown must not approve it")
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

// TestTerminalApprover_AnswerTypedAfterPromptShownApproves is the other
// half of the per-prompt consent contract: once a prompt has actually been
// shown, an answer typed for it must still be honored normally.
func TestTerminalApprover_AnswerTypedAfterPromptShownApproves(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	var out bytes.Buffer
	a := NewTerminalApprover(pr, &out, time.Second)

	go func() {
		_, _ = pw.Write([]byte("y\n"))
	}()

	approved, err := a.Ask(context.Background(), Request{Tool: "exec", Command: "ls"})
	require.NoError(t, err)
	assert.True(t, approved, "an answer typed after the prompt is shown must approve it")
}

// TestTerminalApprover_EOFFailsFastOnEveryAsk proves H2: once the reader
// hits EOF, every later Ask returns the terminal error immediately instead
// of blocking for the full approval timeout, not just the first one.
func TestTerminalApprover_EOFFailsFastOnEveryAsk(t *testing.T) {
	in := strings.NewReader("") // ReadString returns io.EOF immediately
	var out bytes.Buffer
	a := NewTerminalApprover(in, &out, time.Minute) // long enough that only EOF, not the timeout, can end these fast

	for i := 0; i < 2; i++ {
		start := time.Now()
		approved, err := a.Ask(context.Background(), Request{Tool: "exec", Command: "ls"})
		elapsed := time.Since(start)

		assert.False(t, approved)
		assert.Error(t, err)
		assert.Less(t, elapsed, time.Second, "Ask #%d after EOF must fail fast, not wait out the approval timeout", i+1)
	}
}
