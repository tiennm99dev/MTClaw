// End-to-end coverage for `mtclaw prompt`: the real command tree
// (newRootCmd), the real config-load pipeline, the real agent loop and
// tool registry, driven against fakeapi.OpenAI instead of a live OpenAI
// key. `package cli` (not `cli_test`) only because it is this package's
// existing convention (onboard_test.go, doctor_test.go are both in-package
// too) - nothing here reaches an unexported symbol a `cli_test` package
// could not also see via newRootCmd/state.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	yaml "github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tiennm99/MTClaw/internal/config"
	"github.com/tiennm99/MTClaw/internal/testsupport"
	"github.com/tiennm99/MTClaw/internal/testsupport/fakeapi"
)

// writeE2EConfigFile renders a Config value (agent.model, openai.base_url
// pointed at oa, telegram disabled, a temp workspace/sqlite file) and writes
// it to disk as real YAML - via the same yaml package internal/config
// itself uses to marshal a Config, rather than hand-templated YAML text, so
// there is no Windows-backslash-in-a-quoted-path escaping to work around.
// Returns the config file's path and the workspace directory it points at.
func writeE2EConfigFile(t *testing.T, oa *fakeapi.OpenAI) (configPath, workspace string) {
	t.Helper()

	dir := t.TempDir()
	workspace = filepath.Join(dir, "workspace")
	require.NoError(t, os.MkdirAll(workspace, 0o755))

	cfg := config.Default()
	cfg.Agent.Model = "fake-model"
	cfg.Agent.Workspace = workspace
	cfg.OpenAI.BaseURL = oa.BaseURL()
	cfg.Channels.Telegram.Enabled = false
	cfg.Tools.Filesystem.Roots = []string{workspace}
	cfg.Tools.Exec = config.ExecConfig{Enabled: false, Mode: "off"}
	cfg.Storage.Path = filepath.Join(dir, "mtclaw.db")

	raw, err := yaml.Marshal(cfg)
	require.NoError(t, err)

	configPath = filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(configPath, raw, 0o600))
	return configPath, workspace
}

// runPromptCmd builds a fresh root command (a fresh *state, exactly as a
// new `mtclaw` process invocation would have) and runs `prompt text
// --config configPath`, returning stdout/stderr separately - stdout must
// stay script-clean (the final answer only), stderr carries the
// "-> running <tool>" progress line (prompt_cmd.go's progressPrinter).
//
// It closes the state's store handle itself before returning: production's
// Execute() (root.go) does the same after cmd.Execute() returns, but calling
// newRootCmd/ExecuteContext directly - the only way to get separate
// stdout/stderr buffers and a fresh *state per call - bypasses Execute
// entirely, and a leaked sqlite handle onto a WAL-mode database blocks
// t.TempDir()'s own cleanup on Windows (file deletion of an open handle).
func runPromptCmd(t *testing.T, configPath, text string) (stdout, stderr string) {
	t.Helper()

	s := &state{}
	var outBuf, errBuf bytes.Buffer
	cmd := newRootCmd(s)
	cmd.SetArgs([]string{"prompt", text, "--config", configPath})
	cmd.SetOut(&outBuf)
	cmd.SetErr(&errBuf)

	err := cmd.ExecuteContext(context.Background())
	closeErr := s.closeStore()
	require.NoError(t, err)
	require.NoError(t, closeErr)
	return outBuf.String(), errBuf.String()
}

// TestE2E_PromptCompletesToolUsingTurn runs `mtclaw prompt` twice against
// the same config (and therefore the same sqlite session) through two
// entirely separate root commands - each newRootCmd call is a fresh *state,
// the same isolation a fresh process restart would give. The first
// assertion closes v1's "mtclaw prompt completes a tool-using turn"
// criterion; the second - the third fakeapi.OpenAI request already
// containing the first turn's messages - closes "sessions and history
// survive a restart" for the CLI path, since nothing but the on-disk sqlite
// file carries that state between the two commands.
func TestE2E_PromptCompletesToolUsingTurn(t *testing.T) {
	testsupport.FakeHome(t)

	oa := fakeapi.NewOpenAI(
		fakeapi.Step{ToolCalls: []fakeapi.ToolCall{{ID: "call_1", Name: "read_file", Args: `{"path":"notes.txt"}`}}},
		fakeapi.Step{Content: "your notes say: buy oat milk"},
		fakeapi.Step{Content: "you previously asked me to read your notes"},
	)
	t.Cleanup(oa.Close)

	configPath, workspace := writeE2EConfigFile(t, oa)
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "notes.txt"), []byte("buy oat milk"), 0o644))
	t.Setenv("OPENAI_API_KEY", "sk-fake")

	stdout, stderr := runPromptCmd(t, configPath, "read my notes")
	assert.Contains(t, stdout, "buy oat milk", "stdout must carry the completed turn's final text")
	assert.Contains(t, stderr, "-> running read_file", "stderr must carry the tool-call progress line")

	require.Len(t, oa.Requests(), 2, "the tool call must trigger exactly one follow-up completion request")

	stdout2, _ := runPromptCmd(t, configPath, "what did I just ask you to do?")
	assert.Contains(t, stdout2, "read your notes")

	reqs := oa.Requests()
	require.Len(t, reqs, 3, "the second invocation must add exactly one more request")
	third, err := json.Marshal(reqs[2])
	require.NoError(t, err)
	assert.Contains(t, string(third), "read my notes",
		"the second process's first request must already replay the first turn's history - proving sessions persist across a restart")
}
