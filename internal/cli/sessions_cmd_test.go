package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tiennm99/MTClaw/internal/store"
	"github.com/tiennm99/MTClaw/internal/store/sqlite"
)

// sessionsTestConfigPath writes a minimal, loadable, telegram-disabled
// config next to a fresh sqlite database (both under a temp dir) and
// returns the config path, ready for `newTestRootCmd(t).SetArgs(...)`.
func sessionsTestConfigPath(t *testing.T) (configPath, dbPath string) {
	t.Helper()
	root := t.TempDir()
	configPath = filepath.Join(root, "config.yaml")
	dbPath = filepath.Join(root, "mtclaw.db")
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
`, root, root, root, dbPath)
	require.NoError(t, os.WriteFile(configPath, []byte(doc), 0o600))
	return configPath, dbPath
}

// TestSessionsShowCmd_SanitizesUntrustedContent proves a stored message's
// Content/ToolCalls/ToolCallID/ToolName, which can carry untrusted text (a
// web_fetch page body, in the worst case), never reaches the operator's
// terminal as a raw ESC/OSC/bidi sequence via `sessions show`.
func TestSessionsShowCmd_SanitizesUntrustedContent(t *testing.T) {
	configPath, dbPath := sessionsTestConfigPath(t)

	db, dia, _, err := sqlite.Open(context.Background(), dbPath, false)
	require.NoError(t, err)
	st := store.New(db, dia)
	sess, err := st.Sessions().Ensure(context.Background(), "cli", "local", "")
	require.NoError(t, err)
	require.NoError(t, st.Messages().Append(context.Background(), sess.ID, []store.Message{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "reply\x1b[2Jclobbered", ToolCalls: `[{"evil":"\x1b[31m"}]`},
		{Role: "tool", Content: "result", ToolCallID: "call_1\x07", ToolName: "web_fetch"},
	}))
	require.NoError(t, st.Close())

	root := newTestRootCmd(t)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--config", configPath, "sessions", "show", sess.ID})
	require.NoError(t, root.ExecuteContext(context.Background()))

	rendered := out.String()
	assert.NotContains(t, rendered, "\x1b", "ESC must never reach the terminal raw")
	assert.NotContains(t, rendered, "\x07")
	assert.Contains(t, rendered, `\x1b`, "the escaped form must still be visible")
	assert.Contains(t, rendered, `\x07`)
	assert.Contains(t, rendered, "hello")
}

// TestSessionsListCmd_ListsSessionWithMessageCount exercises `sessions list`
// end to end against a real store, which had near-zero coverage.
func TestSessionsListCmd_ListsSessionWithMessageCount(t *testing.T) {
	configPath, dbPath := sessionsTestConfigPath(t)

	db, dia, _, err := sqlite.Open(context.Background(), dbPath, false)
	require.NoError(t, err)
	st := store.New(db, dia)
	sess, err := st.Sessions().Ensure(context.Background(), "cli", "local", "")
	require.NoError(t, err)
	require.NoError(t, st.Messages().Append(context.Background(), sess.ID, []store.Message{
		{Role: "user", Content: "hi"},
	}))
	require.NoError(t, st.Close())

	root := newTestRootCmd(t)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--config", configPath, "sessions", "list"})
	require.NoError(t, root.ExecuteContext(context.Background()))

	assert.Contains(t, out.String(), sess.ID)
	assert.Contains(t, out.String(), "cli")
}

// TestSessionsListCmd_LimitFlagBoundsResults proves --limit bounds both the
// listed sessions and the CountBySession query `sessions list` runs per
// session on top of the list itself, defaulting to 50 rather than no limit
// at all - otherwise a large session table costs one query per row on
// every invocation.
func TestSessionsListCmd_LimitFlagBoundsResults(t *testing.T) {
	configPath, dbPath := sessionsTestConfigPath(t)

	db, dia, _, err := sqlite.Open(context.Background(), dbPath, false)
	require.NoError(t, err)
	st := store.New(db, dia)
	for _, chatID := range []string{"a", "b", "c"} {
		_, err := st.Sessions().Ensure(context.Background(), "cli", chatID, "")
		require.NoError(t, err)
	}
	require.NoError(t, st.Close())

	root := newTestRootCmd(t)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--config", configPath, "sessions", "list", "--limit", "1"})
	require.NoError(t, root.ExecuteContext(context.Background()))

	lines := 0
	for _, r := range out.String() {
		if r == '\n' {
			lines++
		}
	}
	assert.Equal(t, 2, lines, "one header line plus exactly one session row")
}
