package cli

import (
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tiennm99/MTClaw/internal/config"
	"github.com/tiennm99/MTClaw/internal/tools"
)

// TestNewPromptApprover_WiresTerminalApprover pins the one behavioral
// invariant the newLoop refactor could silently break with no test
// catching it: `prompt` must always hand the agent loop an interactive
// TerminalApprover - reading from the command's own stdin, writing to its
// stderr, bounded by tools.exec.approval_timeout - never the
// deny-everything fallback `cron run` uses.
func TestNewPromptApprover_WiresTerminalApprover(t *testing.T) {
	cfg := config.Default()
	cfg.Tools.Exec.ApprovalTimeout = config.Duration(90 * time.Second)
	s := &state{cfg: cfg}
	cmd := &cobra.Command{}

	approver := newPromptApprover(cmd, s)

	require.NotNil(t, approver)
	var _ tools.Approver = approver // still satisfies what newLoop expects
	assert.Equal(t, 90*time.Second, approver.Timeout)
	assert.Equal(t, cmd.InOrStdin(), approver.In)
	assert.Equal(t, cmd.ErrOrStderr(), approver.Out)
}
