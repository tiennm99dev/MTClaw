//go:build unix

package cli

import (
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestNewRootContext_SIGINTCancelsContext proves the context Execute drives
// the whole command tree with is actually cancelled by a real SIGINT, which
// is the mechanism `agent.Loop`'s flush-on-cancel path (and the
// second-signal hard kill) depend on.
func TestNewRootContext_SIGINTCancelsContext(t *testing.T) {
	ctx, stop, lastSignal := newRootContext()
	defer stop()

	require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGINT))

	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("context was not cancelled after SIGINT")
	}
	require.Equal(t, int(syscall.SIGINT), lastSignal())
}

// TestNewRootContext_SIGTERMIsReportedAsSIGTERM proves the signal that
// cancelled the context is recorded, so a SIGTERM exits 143, not 130.
func TestNewRootContext_SIGTERMIsReportedAsSIGTERM(t *testing.T) {
	ctx, stop, lastSignal := newRootContext()
	defer stop()

	require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGTERM))

	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("context was not cancelled after SIGTERM")
	}
	require.Equal(t, int(syscall.SIGTERM), lastSignal())
	require.Equal(t, 143, ExitCode(&interruptedError{signal: lastSignal()}))
}
