//go:build unix

package cli

import (
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestNewRootContext_SIGINTCancelsContext pins the signal-handling half of
// the C1 fix: the context Execute drives the whole command tree with must
// actually be cancelled by a real SIGINT, which is the mechanism
// `agent.Loop`'s flush-on-cancel path (and the second-signal hard kill)
// depend on. Nothing previously exercised this - root_test.go only checked
// command registration and `version`, never a signal.
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
