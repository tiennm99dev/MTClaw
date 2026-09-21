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

// TestStdioPrompter_ReadLine_EOFOnEmptyInputErrors pins the C1 fix: a
// closed/`/dev/null` stdin (EOF with nothing pending) must abort the
// prompt, not silently return an empty answer forever - that was how
// `mtclaw onboard`'s model prompt used to spin forever and fill the disk.
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
