package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
// deny-everything fallback `cron run` uses. It exercises this through Ask
// itself, not by reading back TerminalApprover's own fields, which are
// unexported (see internal/tools/approver.go).
func TestNewPromptApprover_WiresTerminalApprover(t *testing.T) {
	cfg := config.Default()
	cfg.Tools.Exec.ApprovalTimeout = config.Duration(90 * time.Second)
	s := &state{cfg: cfg}

	cmd := &cobra.Command{}
	cmd.SetIn(strings.NewReader("y\n"))
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)

	approver := newPromptApprover(cmd, s)
	require.NotNil(t, approver)
	var _ tools.Approver = approver // still satisfies what newLoop expects

	approved, err := approver.Ask(context.Background(), tools.Request{Tool: "exec", Command: "ls"})
	require.NoError(t, err)
	assert.True(t, approved, "must read the approval answer from the command's own stdin")
	assert.Contains(t, stderr.String(), "ls", "must write the approval prompt to the command's own stderr")
}

// TestPromptCmd_SessionAndNewAreMutuallyExclusive proves `--session X
// --new` refuses to parse, rather than silently ignoring --new
// (resolveCLISession's own logic always prefers --session, with no
// indication the user's --new was dropped). cobra validates flag groups
// after PersistentPreRunE, so this still needs a loadable config to reach
// that check at all - it must never reach a real turn, since the group
// validation fails first.
func TestPromptCmd_SessionAndNewAreMutuallyExclusive(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.yaml")
	doc := fmt.Sprintf(`version: 1
agent:
  model: gpt-4o-mini
  workspace: %q
channels:
  telegram:
    enabled: false
tools:
  filesystem:
    roots: [%q]
  exec:
    cwd: %q
cron:
  timezone: UTC
storage:
  path: %q
`, root, root, root, filepath.Join(root, "mtclaw.db"))
	require.NoError(t, os.WriteFile(configPath, []byte(doc), 0o600))

	cmd := newTestRootCmd(t)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--config", configPath, "prompt", "--session", "abc", "--new", "hello"})

	err := cmd.ExecuteContext(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "session")
	assert.Contains(t, err.Error(), "new")
}
