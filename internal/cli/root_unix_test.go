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
	ctx, stop := newRootContext()
	defer stop()

	require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGINT))

	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("context was not cancelled after SIGINT")
	}
}
