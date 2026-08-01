package config

import "time"

// Default returns a Config populated with MTClaw's built-in defaults. Load
// decodes the user's YAML onto this struct, so any key the user omits keeps
// its value here.
//
// agent.model is deliberately left empty: there is no hardcoded default
// model, so validate.go requires the user (or `onboard`, in a later phase)
// to set one explicitly rather than silently picking a model that may
// change or go away.
func Default() *Config {
	return &Config{
		Version: 1,
		Agent: AgentConfig{
			Name:            "MTClaw",
			Temperature:     0.7,
			MaxIterations:   20,
			MaxHistoryTurns: 40,
			Workspace:       "~/mtclaw-workspace",
		},
		OpenAI: OpenAIConfig{
			APIKeyEnv:  "OPENAI_API_KEY",
			BaseURL:    "https://api.openai.com/v1",
			Timeout:    Duration(120 * time.Second),
			MaxRetries: 3,
		},
		Channels: ChannelsConfig{
			Telegram: TelegramConfig{
				Enabled:  true,
				TokenEnv: "TELEGRAM_BOT_TOKEN",
				Groups: map[string]TelegramGroupConfig{
					"*": {RequireMention: true},
				},
			},
		},
		Tools: ToolsConfig{
			Filesystem: FilesystemConfig{
				Enabled:       true,
				Roots:         []string{"~/mtclaw-workspace"},
				MaxReadBytes:  262144,
				MaxWriteBytes: 1048576,
			},
			WebFetch: WebFetchConfig{
				Enabled:  true,
				Timeout:  Duration(30 * time.Second),
				MaxBytes: 1048576,
			},
			Exec: ExecConfig{
				Enabled:         true,
				Mode:            "approval",
				CWD:             "~/mtclaw-workspace",
				Timeout:         Duration(120 * time.Second),
				MaxOutputBytes:  65536,
				ApprovalTimeout: Duration(5 * time.Minute),
				Auto: ExecAutoConfig{
					ConfirmOn: []string{"destructive", "privileged", "network", "secret_access"},
				},
			},
		},
		Cron: CronConfig{
			Enabled:  false,
			Timezone: "Local",
		},
		Storage: StorageConfig{
			Path: "~/.mtclaw/mtclaw.db",
		},
		Log: LogConfig{
			Level:  "info",
			Format: "text",
		},
	}
}
