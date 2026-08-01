package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validMinimalYAML is the smallest document that satisfies every phase 1
// validation rule against Default()'s built-in values: it sets the one
// field with no built-in default (agent.model) and disables telegram so
// the fail-closed allowlist rule does not trip.
const validMinimalYAML = `
version: 1
agent:
  model: gpt-5
channels:
  telegram:
    enabled: false
`

func TestLoad_TableDriven(t *testing.T) {
	cases := []struct {
		name       string
		yaml       string
		wantErr    bool
		wantSubstr string
	}{
		{
			name:    "valid minimal config",
			yaml:    validMinimalYAML,
			wantErr: false,
		},
		{
			name: "unknown top-level key",
			yaml: `
version: 1
agent:
  model: gpt-5
bogus_top_level: true
`,
			wantErr:    true,
			wantSubstr: `unknown field "bogus_top_level"`,
		},
		{
			name: "unknown nested key is rejected too (DisallowUnknownField must recurse)",
			yaml: `
version: 1
agent:
  model: gpt-5
tools:
  filesystem:
    enabled: true
    bogus_nested_field: true
`,
			wantErr:    true,
			wantSubstr: `unknown field "bogus_nested_field"`,
		},
		{
			name: "bad duration",
			yaml: `
version: 1
agent:
  model: gpt-5
channels:
  telegram:
    enabled: false
openai:
  timeout: "not-a-duration"
`,
			wantErr:    true,
			wantSubstr: "invalid duration",
		},
		{
			name: "inline secret is rejected, naming the *_env key",
			yaml: `
version: 1
agent:
  model: gpt-5
channels:
  telegram:
    enabled: false
openai:
  api_key: sk-hardcoded-in-the-file
`,
			wantErr:    true,
			wantSubstr: "openai.api_key_env",
		},
		{
			name: "telegram enabled with empty allowlist and only the default group",
			yaml: `
version: 1
agent:
  model: gpt-5
channels:
  telegram:
    enabled: true
`,
			wantErr:    true,
			wantSubstr: "would accept no one",
		},
		{
			name: "bad regex in exec.deny",
			yaml: `
version: 1
agent:
  model: gpt-5
channels:
  telegram:
    enabled: false
tools:
  exec:
    deny: ["(unterminated"]
`,
			wantErr:    true,
			wantSubstr: "invalid regex",
		},
		{
			name: "invalid cron expression is rejected, naming the job",
			yaml: `
version: 1
agent:
  model: gpt-5
channels:
  telegram:
    enabled: false
cron:
  jobs:
    - name: whatever
      schedule: "this is not a cron expression"
      prompt: "hi"
      session: persistent
      timeout: 1m
      deliver_to:
        channel: telegram
        chat_id: "1"
`,
			wantErr:    true,
			wantSubstr: `job "whatever": invalid cron expression`,
		},
		{
			name: "exec.cwd outside filesystem.roots",
			yaml: `
version: 1
agent:
  model: gpt-5
channels:
  telegram:
    enabled: false
tools:
  filesystem:
    roots: ["~/mtclaw-workspace"]
  exec:
    cwd: "~/somewhere-else-entirely"
`,
			wantErr:    true,
			wantSubstr: "must be inside one of tools.filesystem.roots",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			baseDir := t.TempDir()
			cfg, err := Load([]byte(tc.yaml), baseDir, map[string]string{})
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantSubstr)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, cfg)
		})
	}
}

func TestLoad_TildeExpansion(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)

	yaml := `
version: 1
agent:
  model: gpt-5
  workspace: "~/custom-workspace"
channels:
  telegram:
    enabled: false
`
	cfg, err := Load([]byte(yaml), t.TempDir(), map[string]string{})
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, "custom-workspace"), cfg.Agent.Workspace)
}

func TestLoad_ConfigRelativePathResolution(t *testing.T) {
	baseDir := t.TempDir()

	yaml := `
version: 1
agent:
  model: gpt-5
  workspace: "relative-workspace"
channels:
  telegram:
    enabled: false
`
	cfg, err := Load([]byte(yaml), baseDir, map[string]string{})
	require.NoError(t, err)
	// Resolved against the config file's own directory, not the process CWD.
	assert.Equal(t, filepath.Join(baseDir, "relative-workspace"), cfg.Agent.Workspace)
}

func TestLoad_SecretEnvOverlay(t *testing.T) {
	baseDir := t.TempDir()

	t.Run("env var set wins", func(t *testing.T) {
		cfg, err := Load([]byte(validMinimalYAML), baseDir, map[string]string{
			"OPENAI_API_KEY": "secret-from-env",
		})
		require.NoError(t, err)
		assert.Equal(t, "secret-from-env", cfg.OpenAI.APIKey())
		assert.Equal(t, "env:OPENAI_API_KEY", cfg.OpenAI.APIKeySource())
	})

	t.Run("neither env nor file set resolves to unset", func(t *testing.T) {
		cfg, err := Load([]byte(validMinimalYAML), baseDir, map[string]string{})
		require.NoError(t, err)
		assert.Equal(t, "", cfg.OpenAI.APIKey())
		assert.Equal(t, "unset", cfg.OpenAI.APIKeySource())
	})

	t.Run("file set and env unset uses the file, trimmed of trailing newline", func(t *testing.T) {
		secretPath := filepath.Join(baseDir, "openai-key.secret")
		require.NoError(t, os.WriteFile(secretPath, []byte("secret-from-file\n"), 0o600))

		yaml := fmt.Sprintf(`
version: 1
agent:
  model: gpt-5
channels:
  telegram:
    enabled: false
openai:
  api_key_file: %q
`, secretPath)
		cfg, err := Load([]byte(yaml), baseDir, map[string]string{})
		require.NoError(t, err)
		assert.Equal(t, "secret-from-file", cfg.OpenAI.APIKey())
		assert.Equal(t, "file:"+secretPath, cfg.OpenAI.APIKeySource())
	})

	t.Run("file with CRLF trailing newline is trimmed too", func(t *testing.T) {
		secretPath := filepath.Join(baseDir, "openai-key-crlf.secret")
		require.NoError(t, os.WriteFile(secretPath, []byte("secret-crlf\r\n"), 0o600))

		yaml := fmt.Sprintf(`
version: 1
agent:
  model: gpt-5
channels:
  telegram:
    enabled: false
openai:
  api_key_file: %q
`, secretPath)
		cfg, err := Load([]byte(yaml), baseDir, map[string]string{})
		require.NoError(t, err)
		assert.Equal(t, "secret-crlf", cfg.OpenAI.APIKey())
	})

	t.Run("both env and file set: env wins", func(t *testing.T) {
		secretPath := filepath.Join(baseDir, "openai-key-both.secret")
		require.NoError(t, os.WriteFile(secretPath, []byte("secret-from-file\n"), 0o600))

		yaml := fmt.Sprintf(`
version: 1
agent:
  model: gpt-5
channels:
  telegram:
    enabled: false
openai:
  api_key_file: %q
`, secretPath)
		cfg, err := Load([]byte(yaml), baseDir, map[string]string{
			"OPENAI_API_KEY": "secret-from-env",
		})
		require.NoError(t, err)
		assert.Equal(t, "secret-from-env", cfg.OpenAI.APIKey())
		assert.Equal(t, "env:OPENAI_API_KEY", cfg.OpenAI.APIKeySource())
	})
}

func TestLoad_FileNotFound(t *testing.T) {
	_, err := LoadFile(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	require.Error(t, err)
	var notFound *FileNotFoundError
	assert.ErrorAs(t, err, &notFound)
}

// TestMarshalRedacted_NeverLeaksSecretMaterial is the config-show snapshot
// test: no matter which resolution path won, the rendered YAML must never
// contain the actual secret value, only a placeholder naming its source.
func TestMarshalRedacted_NeverLeaksSecretMaterial(t *testing.T) {
	baseDir := t.TempDir()
	tokenPath := filepath.Join(baseDir, "telegram.secret")
	require.NoError(t, os.WriteFile(tokenPath, []byte("tg-secret-token\n"), 0o600))

	yaml := fmt.Sprintf(`
version: 1
agent:
  model: gpt-5
channels:
  telegram:
    enabled: true
    allow_from: [111]
    token_file: %q
`, tokenPath)

	cfg, err := Load([]byte(yaml), baseDir, map[string]string{
		"OPENAI_API_KEY": "sk-super-secret-value",
	})
	require.NoError(t, err)
	require.Equal(t, "sk-super-secret-value", cfg.OpenAI.APIKey())
	require.Equal(t, "tg-secret-token", cfg.Channels.Telegram.Token())

	out, err := MarshalRedacted(cfg)
	require.NoError(t, err)
	rendered := string(out)

	assert.NotContains(t, rendered, "sk-super-secret-value")
	assert.NotContains(t, rendered, "tg-secret-token")
	assert.Contains(t, rendered, "<set:env:OPENAI_API_KEY>")
	// The YAML encoder may backslash-escape a Windows path inside a quoted
	// scalar, so match on the redaction marker and the file's base name
	// rather than the literal (possibly-escaped) path string.
	assert.Contains(t, rendered, "<set:file:")
	assert.Contains(t, rendered, filepath.Base(tokenPath))
}

func TestMarshalRedacted_UnsetSecretsShownAsUnset(t *testing.T) {
	cfg, err := Load([]byte(validMinimalYAML), t.TempDir(), map[string]string{})
	require.NoError(t, err)

	out, err := MarshalRedacted(cfg)
	require.NoError(t, err)
	rendered := string(out)

	assert.True(t, strings.Contains(rendered, "<unset>"))
}
