package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNewRootCmd_RegistersExpectedCommands builds the cobra tree the same
// way Execute does and walks it, asserting the commands other tests and
// docs assume exist actually got wired into root.go.
func TestNewRootCmd_RegistersExpectedCommands(t *testing.T) {
	root := newTestRootCmd(t)

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

	root := newTestRootCmd(t)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"version"})

	require.NoError(t, root.ExecuteContext(context.Background()))
	assert.Contains(t, out.String(), "mtclaw")
}

// TestOpenStore_WriteModeCreatesStorageDir_ReadOnlyDoesNot proves
// config.Validate/config.LoadFile never create storage.path's parent
// directory (see internal/config/validate.go's ensureDirCreatable), so a
// read-only command that only loads the config leaves the filesystem
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
	validateCmd := newTestRootCmd(t)
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

// walkRunnable recurses through cmd's whole tree, calling fn on every
// command cobra considers Runnable (has its own RunE/Run) - a group command
// like `config` or `sessions` never executes on its own and does not need
// its own annotation.
func walkRunnable(cmd *cobra.Command, fn func(*cobra.Command)) {
	if cmd.Runnable() {
		fn(cmd)
	}
	for _, c := range cmd.Commands() {
		walkRunnable(c, fn)
	}
}

// TestEveryRunnableCommandDeclaresAConfigAnnotation walks the whole command
// tree and asserts every command newRootCmd registers explicitly declares
// its config requirement, rather than silently falling back to configLevel's
// default - a renamed or newly added command that forgets to annotate
// itself would otherwise load and validate the config (the safer default),
// but silently, with nothing here to catch the omission.
func TestEveryRunnableCommandDeclaresAConfigAnnotation(t *testing.T) {
	root := newTestRootCmd(t)
	walkRunnable(root, func(cmd *cobra.Command) {
		_, ok := cmd.Annotations[annotationConfig]
		assert.Truef(t, ok, "%q must declare Annotations[%q]", cmd.CommandPath(), annotationConfig)
	})
}

// TestHelpAndCompletionWorkWithoutConfig proves cobra's built-in `help` and
// `completion <shell>` commands, which inherit the root's
// PersistentPreRunE, can run against a fresh HOME with no config file at
// all - the first thing a new install's shell tab completion or `mtclaw
// help` does must not itself require onboarding first.
func TestHelpAndCompletionWorkWithoutConfig(t *testing.T) {
	useFakeHome(t)

	for _, args := range [][]string{
		{"help"},
		{"--help"},
		{"completion", "bash"},
		{"completion", "zsh"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			cmd := newTestRootCmd(t)
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetArgs(args)
			require.NoError(t, cmd.ExecuteContext(context.Background()))
		})
	}
}

// TestPrepare_InvalidLogLevelFlagIsRejected proves --log-level is validated
// the same as log.level in the config file: an invalid flag value must fail
// loudly instead of silently degrading to logging's own info fallback,
// which is exactly what a typo'd config value no longer does either (see
// config.validateLog).
func TestPrepare_InvalidLogLevelFlagIsRejected(t *testing.T) {
	useFakeHome(t)
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
	cmd.SetArgs([]string{"--config", configPath, "--log-level", "debgu", "config", "validate"})
	err := cmd.ExecuteContext(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--log-level")
}

// TestPrepare_LogLevelFlagAcceptsMixedCaseAndWarningAlias proves --log-level
// is not stricter than the config file's own log.level: "WARN" and
// "warning" always worked as a config value (see
// TestValidate_LogFields_AcceptsMixedCaseWhitespaceAndWarningAlias) and
// isValidLogLevel must accept the same spellings from the flag.
func TestPrepare_LogLevelFlagAcceptsMixedCaseAndWarningAlias(t *testing.T) {
	useFakeHome(t)
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

	for _, level := range []string{"WARN", "warning", " Info "} {
		cmd := newTestRootCmd(t)
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs([]string{"--config", configPath, "--log-level", level, "config", "validate"})
		assert.NoErrorf(t, cmd.ExecuteContext(context.Background()), "--log-level %q must be accepted", level)
	}
}

// TestFinalizeExecuteError proves main.go can tell "a command failed
// because the process was interrupted" apart from "a command failed on its
// own", so it can exit 130 instead of the generic 1 only for the former.
func TestFinalizeExecuteError(t *testing.T) {
	boom := fmt.Errorf("boom")
	interrupted := context.Canceled

	assert.ErrorIs(t, finalizeExecuteError(boom, interrupted), ErrInterrupted,
		"a command error while ctx had already ended must report as interrupted")
	assert.Equal(t, boom, finalizeExecuteError(boom, nil),
		"a command's own failure with a healthy ctx must be reported as-is")
	assert.NoError(t, finalizeExecuteError(nil, interrupted),
		"a command that still succeeded despite ctx ending must not be reported as an error at all")
	assert.NoError(t, finalizeExecuteError(nil, nil))
}

// TestPrepare_MissingConfigPointsAtOnboard proves a first run of any
// config-requiring command against a fresh HOME does not just report
// "config file not found" with no pointer to how to fix it.
func TestPrepare_MissingConfigPointsAtOnboard(t *testing.T) {
	useFakeHome(t)
	cmd := newTestRootCmd(t)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"config", "show"})

	err := cmd.ExecuteContext(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mtclaw onboard")
}

// TestPrepare_ReadOnlyCommandsDoNotCreateLogFile proves pointing --config
// at a config whose log.file lives under a directory that does not exist
// yet does not create that directory (or the log file) just to run a
// read-only inspection command like `config validate`.
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

	cmd := newTestRootCmd(t)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--config", configPath, "config", "validate"})
	require.NoError(t, cmd.ExecuteContext(context.Background()))

	_, err := os.Stat(logDir)
	assert.True(t, os.IsNotExist(err), "config validate must not create the log directory")
}
