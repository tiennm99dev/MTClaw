// Package config defines MTClaw's configuration schema and the pure
// load/validate pipeline every other package depends on.
package config

import (
	"fmt"
	"time"

	yaml "github.com/goccy/go-yaml"
)

// Duration wraps time.Duration so config values like "120s" or "5m" decode
// from a plain YAML string instead of relying on goccy's built-in numeric
// nanosecond handling, and round-trip back to the same string form for
// `config show`.
type Duration time.Duration

// Std returns the underlying time.Duration for use with the standard library.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// UnmarshalYAML implements yaml.BytesUnmarshaler. It first decodes the node
// as a plain YAML string (stripping quotes/newlines the encoder may have
// added) and then parses it with time.ParseDuration.
func (d *Duration) UnmarshalYAML(data []byte) error {
	var s string
	if err := yaml.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("duration: %w", err)
	}
	if s == "" {
		*d = 0
		return nil
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// MarshalYAML implements yaml.BytesMarshaler, emitting the duration as a
// quoted string (e.g. "2m0s") so it re-parses unambiguously.
func (d Duration) MarshalYAML() ([]byte, error) {
	return []byte(fmt.Sprintf("%q", time.Duration(d).String())), nil
}

// Config is the root of the MTClaw YAML schema. See
// plans/260731-2219-mtclaw-core-system/phase-01-foundation-and-config.md for
// the authoritative annotated example.
type Config struct {
	Version  int            `yaml:"version"`
	Agent    AgentConfig    `yaml:"agent"`
	OpenAI   OpenAIConfig   `yaml:"openai"`
	Channels ChannelsConfig `yaml:"channels"`
	Tools    ToolsConfig    `yaml:"tools"`
	Cron     CronConfig     `yaml:"cron"`
	Storage  StorageConfig  `yaml:"storage"`
	Log      LogConfig      `yaml:"log"`
}

// AgentConfig controls the agent loop's identity and turn limits.
type AgentConfig struct {
	Name              string   `yaml:"name"`
	Model             string   `yaml:"model"`
	Temperature       float64  `yaml:"temperature"`
	MaxIterations     int      `yaml:"max_iterations"`
	MaxHistoryTurns   int      `yaml:"max_history_turns"`
	Workspace         string   `yaml:"workspace"`
	SystemPromptFiles []string `yaml:"system_prompt_files"`
}

// OpenAIConfig configures the OpenAI provider. The API key itself is never a
// config value: api_key_env/api_key_file are indirections resolved at load
// time into the unexported apiKey field, reachable only via APIKey().
//
// APIKeyInline exists solely so a literal `api_key:` in the YAML is a known
// field (and therefore a validate.go error naming api_key_env) instead of an
// opaque "unknown field" decode failure. It must always be empty after a
// successful Load; config show reuses it transiently to render a redacted
// placeholder such as "<set:env:OPENAI_API_KEY>".
type OpenAIConfig struct {
	APIKeyEnv    string   `yaml:"api_key_env"`
	APIKeyFile   string   `yaml:"api_key_file"`
	APIKeyInline string   `yaml:"api_key,omitempty"`
	BaseURL      string   `yaml:"base_url"`
	Timeout      Duration `yaml:"timeout"`
	MaxRetries   int      `yaml:"max_retries"`

	apiKey       string
	apiKeySource string // "env:NAME" | "file:PATH" | "unset"
}

// APIKey returns the resolved secret value, or "" if unset.
func (o *OpenAIConfig) APIKey() string { return o.apiKey }

// APIKeySource reports where the resolved key came from: "env:NAME",
// "file:PATH", or "unset".
func (o *OpenAIConfig) APIKeySource() string { return o.apiKeySource }

func (o *OpenAIConfig) setSecret(value, source string) {
	o.apiKey = value
	o.apiKeySource = source
}

// ChannelsConfig groups all chat channel configuration. Telegram is the only
// channel in v1.
type ChannelsConfig struct {
	Telegram TelegramConfig `yaml:"telegram"`
}

// TelegramConfig configures the Telegram long-polling channel. See
// TokenInline's sibling comment on OpenAIConfig.APIKeyInline for why the
// `token` key exists as a known field.
type TelegramConfig struct {
	Enabled     bool                           `yaml:"enabled"`
	TokenEnv    string                         `yaml:"token_env"`
	TokenFile   string                         `yaml:"token_file"`
	TokenInline string                         `yaml:"token,omitempty"`
	AllowFrom   []int64                        `yaml:"allow_from"`
	Groups      map[string]TelegramGroupConfig `yaml:"groups"`

	token       string
	tokenSource string
}

// Token returns the resolved bot token, or "" if unset.
func (t *TelegramConfig) Token() string { return t.token }

// TokenSource reports where the resolved token came from: "env:NAME",
// "file:PATH", or "unset".
func (t *TelegramConfig) TokenSource() string { return t.tokenSource }

func (t *TelegramConfig) setSecret(value, source string) {
	t.token = value
	t.tokenSource = source
}

// TelegramGroupConfig configures mention gating and allowlisting for one
// group chat. The key "*" in TelegramConfig.Groups is a default applied to
// any group not otherwise listed; its AllowFrom, when empty, inherits the
// channel-level allowlist rather than granting access on its own.
type TelegramGroupConfig struct {
	RequireMention bool    `yaml:"require_mention"`
	AllowFrom      []int64 `yaml:"allow_from"`
}

// ToolsConfig groups every built-in tool's configuration.
type ToolsConfig struct {
	Filesystem FilesystemConfig `yaml:"filesystem"`
	WebFetch   WebFetchConfig   `yaml:"web_fetch"`
	Exec       ExecConfig       `yaml:"exec"`
}

// FilesystemConfig confines the fs tool to a set of root directories.
type FilesystemConfig struct {
	Enabled       bool     `yaml:"enabled"`
	Roots         []string `yaml:"roots"`
	MaxReadBytes  int      `yaml:"max_read_bytes"`
	MaxWriteBytes int      `yaml:"max_write_bytes"`
}

// WebFetchConfig bounds the web_fetch tool.
type WebFetchConfig struct {
	Enabled  bool     `yaml:"enabled"`
	Timeout  Duration `yaml:"timeout"`
	MaxBytes int      `yaml:"max_bytes"`
}

// ExecConfig configures the shell exec tool and its layered policy engine.
type ExecConfig struct {
	Enabled         bool           `yaml:"enabled"`
	Mode            string         `yaml:"mode"` // approval | auto | off
	Shell           []string       `yaml:"shell"`
	CWD             string         `yaml:"cwd"`
	Timeout         Duration       `yaml:"timeout"`
	MaxOutputBytes  int            `yaml:"max_output_bytes"`
	ApprovalTimeout Duration       `yaml:"approval_timeout"`
	Deny            []string       `yaml:"deny"`
	Allow           []string       `yaml:"allow"`
	Auto            ExecAutoConfig `yaml:"auto"`
}

// ExecAutoConfig configures the beta "auto" exec mode's LLM classifier.
type ExecAutoConfig struct {
	Model     string   `yaml:"model"` // empty = agent.model
	ConfirmOn []string `yaml:"confirm_on"`
}

// CronConfig declares scheduled prompt jobs. Expression validity is not
// checked in phase 1 (gronx is not yet a dependency); only structural rules
// (unique names, non-empty prompt/deliver_to, a loadable timezone) apply.
type CronConfig struct {
	Enabled  bool      `yaml:"enabled"`
	Timezone string    `yaml:"timezone"` // IANA name or "Local"
	Jobs     []CronJob `yaml:"jobs"`
}

// CronJob is one scheduled prompt.
type CronJob struct {
	Name      string        `yaml:"name"`
	Schedule  string        `yaml:"schedule"` // 5-field cron expression, validated in phase 8
	Prompt    string        `yaml:"prompt"`
	Enabled   bool          `yaml:"enabled"`
	Session   string        `yaml:"session"` // persistent | ephemeral
	Timeout   Duration      `yaml:"timeout"`
	DeliverTo CronDeliverTo `yaml:"deliver_to"`
}

// CronDeliverTo names where a cron job's result is sent.
type CronDeliverTo struct {
	Channel string `yaml:"channel"`
	ChatID  string `yaml:"chat_id"`
}

// StorageConfig points at the SQLite database file.
type StorageConfig struct {
	Path string `yaml:"path"`
}

// LogConfig configures the slog logger built in internal/logging.
type LogConfig struct {
	Level  string `yaml:"level"`  // debug | info | warn | error
	Format string `yaml:"format"` // text | json
	File   string `yaml:"file"`   // empty = stderr
}
