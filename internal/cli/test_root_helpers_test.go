package cli

import (
	"testing"

	"github.com/spf13/cobra"
)

// newTestRootCmd builds the command tree on a fresh state and closes the
// store and log handles that state opens when the test ends. Production's
// Execute does the same after the tree runs; tests that call
// ExecuteContext directly bypass it, and an open sqlite or log file blocks
// t.TempDir's cleanup on Windows. Call it after creating the temp dirs so
// its cleanup runs before theirs.
func newTestRootCmd(t *testing.T) *cobra.Command {
	t.Helper()
	s := &state{}
	t.Cleanup(func() {
		_ = s.closeStore()
		if s.closeLog != nil {
			_ = s.closeLog()
		}
	})
	return newRootCmd(s)
}
