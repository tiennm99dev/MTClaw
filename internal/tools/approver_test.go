package tools

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

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
