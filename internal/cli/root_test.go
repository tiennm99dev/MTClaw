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
)

// TestNewRootCmd_RegistersExpectedCommands builds the cobra tree the same
// way Execute does and walks it, asserting the commands other tests and
// docs assume exist actually got wired into root.go.
func TestNewRootCmd_RegistersExpectedCommands(t *testing.T) {
	root := newRootCmd(&state{})

	prompt, _, err := root.Find([]string{"prompt"})
	require.NoError(t, err)
	assert.Equal(t, "prompt <text>", prompt.Use)

	cronRun, _, err := root.Find([]string{"cron", "run"})
	require.NoError(t, err)
	assert.Equal(t, "run <name>", cronRun.Use)

	for _, name := range []string{"version", "config", "sessions", "approvals", "send", "gateway", "cron", "onboard", "doctor"} {
		cmd, _, err := root.Find([]string{name})
		require.NoErrorf(t, err, "command %q must be registered", name)
		assert.NotNil(t, cmd)
	}
}

// TestNewRootCmd_ExecutesVersion drives the built command tree exactly as a
// real invocation would (parse flags, run PersistentPreRunE, run RunE) and
// asserts `mtclaw version` succeeds and prints the build-stamped string -
// it is exempt from config loading (skipsConfigLoad), so it must work with
// no config file present at all.
func TestNewRootCmd_ExecutesVersion(t *testing.T) {
	useFakeHome(t) // version resolves a config path via os.UserHomeDir even though it never loads it

	root := newRootCmd(&state{})
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"version"})

	require.NoError(t, root.ExecuteContext(context.Background()))
	assert.Contains(t, out.String(), "mtclaw")
}

// TestOpenStore_WriteModeCreatesStorageDir_ReadOnlyDoesNot pins the M1 fix:
// config.Validate/config.LoadFile never create storage.path's parent
// directory (see internal/config/validate.go's ensureDirCreatable), so a
// read-only command that only loads the config must leave the filesystem
// untouched, while a write-mode store open is the one place that creates it.
func TestOpenStore_WriteModeCreatesStorageDir_ReadOnlyDoesNot(t *testing.T) {
	root := t.TempDir()
	storageDir := filepath.Join(root, "nested", "state")
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
`, root, root, root, filepath.Join(storageDir, "mtclaw.db"))
	require.NoError(t, os.WriteFile(configPath, []byte(doc), 0o600))

	// `config validate` only loads the config (PersistentPreRunE); it never
	// calls openStore, so the storage directory must not appear.
	validateCmd := newRootCmd(&state{})
	var out bytes.Buffer
	validateCmd.SetOut(&out)
	validateCmd.SetErr(&out)
	validateCmd.SetArgs([]string{"--config", configPath, "config", "validate"})
	require.NoError(t, validateCmd.ExecuteContext(context.Background()))

	_, err := os.Stat(storageDir)
	assert.True(t, os.IsNotExist(err), "config validate must not create the storage directory")

	// `sessions list` opens the store read-only; it must not create the
	// directory either - there is nothing to list yet.
	s := &state{}
	listCmd := newRootCmd(s)
	listCmd.SetOut(&out)
	listCmd.SetErr(&out)
	listCmd.SetArgs([]string{"--config", configPath, "sessions", "list"})
	// A read-only open against a nonexistent database is expected to fail;
	// what matters here is that it does not create the directory as a
	// side effect while doing so.
	_ = listCmd.ExecuteContext(context.Background())
	_ = s.closeStore()
	_, err = os.Stat(storageDir)
	assert.True(t, os.IsNotExist(err), "a read-only store open must not create the storage directory")

	// `cron run` (via openStore(ctx, false)) is a write open and must
	// create the directory. sessions list above didn't need one; prompt
	// exercises the same openStore(ctx, false) path more directly.
	s2 := &state{}
	writeCmd := newRootCmd(s2)
	writeCmd.SetOut(&out)
	writeCmd.SetErr(&out)
	writeCmd.SetArgs([]string{"--config", configPath, "sessions", "rm", "does-not-exist"})
	_ = writeCmd.ExecuteContext(context.Background())
	_ = s2.closeStore()
	info, err := os.Stat(storageDir)
	require.NoError(t, err, "a write-mode store open must create the storage directory")
	assert.True(t, info.IsDir())
}

// TestPrepare_ReadOnlyCommandsDoNotCreateLogFile pins the M1 fix: pointing
// --config at a config whose log.file lives under a directory that does
// not exist yet must not create that directory (or the log file) just to
// run a read-only inspection command like `config validate`.
func TestPrepare_ReadOnlyCommandsDoNotCreateLogFile(t *testing.T) {
	root := t.TempDir()
	logDir := filepath.Join(root, "nested", "logs")
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
log:
  file: %q
`, root, root, root, filepath.Join(root, "mtclaw.db"), filepath.Join(logDir, "mtclaw.log"))
	require.NoError(t, os.WriteFile(configPath, []byte(doc), 0o600))

	cmd := newRootCmd(&state{})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--config", configPath, "config", "validate"})
	require.NoError(t, cmd.ExecuteContext(context.Background()))

	_, err := os.Stat(logDir)
	assert.True(t, os.IsNotExist(err), "config validate must not create the log directory")
}
