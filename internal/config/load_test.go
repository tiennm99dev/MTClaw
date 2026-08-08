package config

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tiennm99/MTClaw/internal/testsupport"
)

// captureStderr redirects os.Stderr to a pipe for the duration of fn,
// returning everything written to it. Load-time warnings (the storage.path
// deprecation notice, warnIfWorldReadable) go straight to os.Stderr rather
// than through a logger, precisely because the logger itself is built from
// the config Load is still in the middle of producing - so this is the
// only way to assert on them.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = orig })

	fn()

	require.NoError(t, w.Close())
	data, err := io.ReadAll(r)
	require.NoError(t, err)
	return string(data)
}

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

// TestConfigPath_DefaultAliasPrecedence covers the four cells of the
// config.yaml/config.yml alias table (see paths.go's defaultConfigPath):
// yaml-only, yml-only, both-present (yaml wins, one warning), and
// neither-present (yaml is still the name reported, so the eventual
// not-found error and `onboard`'s write agree on the canonical extension).
// Every case uses testsupport.FakeHome so no real home directory is ever
// touched.
func TestConfigPath_DefaultAliasPrecedence(t *testing.T) {
	cases := []struct {
		name      string
		writeYAML bool
		writeYML  bool
		wantExt   string
		wantWarn  bool
	}{
		{name: "yaml only", writeYAML: true, writeYML: false, wantExt: "yaml"},
		{name: "yml only", writeYAML: false, writeYML: true, wantExt: "yml"},
		{name: "both present: yaml wins, one warning naming the ignored yml", writeYAML: true, writeYML: true, wantExt: "yaml", wantWarn: true},
		{name: "neither present: yaml is still the reported name", writeYAML: false, writeYML: false, wantExt: "yaml"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := testsupport.FakeHome(t)
			dir := filepath.Join(home, ".mtclaw")
			require.NoError(t, os.MkdirAll(dir, 0o755))
			if tc.writeYAML {
				require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("version: 1\n"), 0o600))
			}
			if tc.writeYML {
				require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yml"), []byte("version: 1\n"), 0o600))
			}

			var path, source string
			var err error
			stderr := captureStderr(t, func() {
				path, source, err = ConfigPath("")
			})
			require.NoError(t, err)
			assert.Equal(t, "default", source, "the alias must never change the reported source")
			assert.Equal(t, filepath.Join(dir, "config."+tc.wantExt), path)
			if tc.wantWarn {
				assert.Contains(t, stderr, "config.yml")
				assert.Contains(t, stderr, "config.yaml")
			} else {
				assert.Empty(t, stderr, "no warning expected outside the both-present case")
			}
		})
	}
}

// TestConfigPath_ExplicitFlagNeverAliasResolved is requirement 2 of phase
// 4: an explicit --config value is used byte-for-byte, whether it ends in
// .yml (the alias exists) or names a file that does not exist on disk at
// all (ConfigPath never stats an explicit value - only the default branch
// does). Neither case touches the real home directory.
func TestConfigPath_ExplicitFlagNeverAliasResolved(t *testing.T) {
	testsupport.FakeHome(t)

	t.Run("explicit .yml path is returned verbatim", func(t *testing.T) {
		dir := t.TempDir()
		explicit := filepath.Join(dir, "foo.yml")
		require.NoError(t, os.WriteFile(explicit, []byte("version: 1\n"), 0o600))

		path, source, err := ConfigPath(explicit)
		require.NoError(t, err)
		assert.Equal(t, "flag", source)
		assert.Equal(t, explicit, path)
	})

	t.Run("explicit path with no matching file on disk is still returned verbatim", func(t *testing.T) {
		dir := t.TempDir()
		explicit := filepath.Join(dir, "does-not-exist.yml")

		path, source, err := ConfigPath(explicit)
		require.NoError(t, err, "ConfigPath itself never checks existence; LoadFile is what reports not-found")
		assert.Equal(t, "flag", source)
		assert.Equal(t, explicit, path)
	})
}

// TestLoad_StorageDSNOnly is the "new key" half of the storage.dsn/
// storage.path compatibility contract: a config that only ever mentions
// dsn loads normally, with EffectiveDSN() reflecting the expanded value
// and no deprecation warning.
func TestLoad_StorageDSNOnly(t *testing.T) {
	baseDir := t.TempDir()
	yamlDoc := validMinimalYAML + "storage:\n  dsn: \"mtclaw-custom.db\"\n"

	var cfg *Config
	stderr := captureStderr(t, func() {
		var err error
		cfg, err = Load([]byte(yamlDoc), baseDir, map[string]string{})
		require.NoError(t, err)
	})

	assert.Equal(t, filepath.Join(baseDir, "mtclaw-custom.db"), cfg.Storage.DSN)
	assert.Equal(t, "", cfg.Storage.Path)
	assert.Equal(t, cfg.Storage.DSN, cfg.Storage.EffectiveDSN())
	assert.NotContains(t, stderr, "storage.path")
}

// TestLoad_StoragePathAliasWarnsOnceAndNormalizes is the compatibility
// contract's other half: every config written before storage.dsn existed
// sets only storage.path, and must keep loading with no user action -
// with exactly one deprecation warning, and EffectiveDSN() (and, after
// Load's own normalization, DSN itself) equal to that same path.
func TestLoad_StoragePathAliasWarnsOnceAndNormalizes(t *testing.T) {
	baseDir := t.TempDir()
	yamlDoc := validMinimalYAML + "storage:\n  path: \"mtclaw-legacy.db\"\n"

	var cfg *Config
	stderr := captureStderr(t, func() {
		var err error
		cfg, err = Load([]byte(yamlDoc), baseDir, map[string]string{})
		require.NoError(t, err)
	})

	wantDSN := filepath.Join(baseDir, "mtclaw-legacy.db")
	assert.Equal(t, wantDSN, cfg.Storage.EffectiveDSN())
	// Load normalizes path into dsn and clears path, so `config show`
	// renders the canonical key.
	assert.Equal(t, wantDSN, cfg.Storage.DSN)
	assert.Equal(t, "", cfg.Storage.Path)

	assert.Equal(t, 1, strings.Count(stderr, "storage.path is deprecated"), "must warn exactly once: %q", stderr)
}

// TestLoad_StorageBothDSNAndPathSetFails proves the full Load pipeline
// surfaces validateStorage's both-set error, naming both keys, rather than
// silently preferring one - the scenario expandStorage's default-value
// heuristic must NOT swallow, since both values here are explicit and
// distinct from storage.dsn's own default.
func TestLoad_StorageBothDSNAndPathSetFails(t *testing.T) {
	baseDir := t.TempDir()
	yamlDoc := validMinimalYAML + "storage:\n  dsn: \"mtclaw-custom.db\"\n  path: \"mtclaw-legacy.db\"\n"

	_, err := Load([]byte(yamlDoc), baseDir, map[string]string{})
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, "storage.dsn")
	assert.Contains(t, msg, "storage.path")
	assert.Contains(t, msg, "not both")
}

// TestLoad_StorageUnsupportedDriverFails proves the whole-pipeline
// rejection of a driver this binary never registers - see
// internal/store/factory_test.go for the matching runtime-side assertion
// against store.Open itself.
func TestLoad_StorageUnsupportedDriverFails(t *testing.T) {
	baseDir := t.TempDir()
	yamlDoc := validMinimalYAML + "storage:\n  driver: postgres\n"

	_, err := Load([]byte(yamlDoc), baseDir, map[string]string{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `storage.driver: must be one of: sqlite, got "postgres"`)
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
