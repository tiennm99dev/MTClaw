package cli

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStdioPrompter_ReadLine_EOFOnEmptyInputErrors proves a closed or
// `/dev/null` stdin (EOF with nothing pending) aborts the prompt with an
// error, rather than returning an empty answer that a required-answer loop
// would just re-prompt for forever.
func TestStdioPrompter_ReadLine_EOFOnEmptyInputErrors(t *testing.T) {
	p := newStdioPrompter(context.Background(), strings.NewReader(""), io.Discard, -1)
	_, err := p.readLine()
	require.Error(t, err)
	assert.ErrorIs(t, err, io.EOF)
}

// TestStdioPrompter_ReadLine_FinalUnterminatedLineIsStillRead ensures the
// EOF fix does not regress a legitimate last answer piped in without a
// trailing newline.
func TestStdioPrompter_ReadLine_FinalUnterminatedLineIsStillRead(t *testing.T) {
	p := newStdioPrompter(context.Background(), strings.NewReader("answer"), io.Discard, -1)
	line, err := p.readLine()
	require.NoError(t, err)
	assert.Equal(t, "answer", line)
}

func TestStdioPrompter_ReadLine_NormalLineIsRead(t *testing.T) {
	p := newStdioPrompter(context.Background(), strings.NewReader("hello\nworld\n"), io.Discard, -1)
	line, err := p.readLine()
	require.NoError(t, err)
	assert.Equal(t, "hello", line)
}

// A blocked prompt must end as soon as the command is interrupted, since a
// read on a real stdin cannot be woken any other way once the root command
// owns signal handling.
func TestStdioPrompter_InterruptAbortsBlockedRead(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	ctx, cancel := context.WithCancel(context.Background())
	p := newStdioPrompter(ctx, pr, io.Discard, -1)

	done := make(chan error, 1)
	go func() {
		_, err := p.Text("question", "")
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("prompt did not return after the context was canceled")
	}
}

// TestStdioPrompter_Secret_InterruptAbortsBlockedRead proves Secret aborts
// on interrupt too, not just Text/readLine: a first Ctrl-C at a Secret
// prompt must return immediately instead of being silently ignored while
// nothing observes it. stdinFd stays -1 (never a terminal in this hermetic
// test, so term.IsTerminal is false and Secret falls back to the same
// ctx-aware readLine path Text uses) - the terminal-attached branch that
// additionally restores echo via term.GetState/term.Restore needs a real
// pty to drive and is not reachable from a plain io.Pipe.
func TestStdioPrompter_Secret_InterruptAbortsBlockedRead(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	ctx, cancel := context.WithCancel(context.Background())
	p := newStdioPrompter(ctx, pr, io.Discard, -1)

	done := make(chan error, 1)
	go func() {
		_, err := p.Secret("secret")
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("Secret did not return after the context was canceled")
	}
}
