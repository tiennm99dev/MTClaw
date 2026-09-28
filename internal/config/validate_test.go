package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validConfig returns a Config that satisfies every validation rule, with
// all path fields already "post-expansion" absolute paths rooted under
// t.TempDir() - Validate assumes ExpandPath already ran, so tests that call
// it directly (bypassing Load) must hand it already-expanded paths.
func validConfig(t *testing.T) *Config {
	t.Helper()
	root := t.TempDir()

	cfg := Default()
	cfg.Agent.Model = "gpt-5"
	cfg.Agent.Workspace = root
	cfg.Channels.Telegram.Enabled = false
	cfg.Tools.Filesystem.Roots = []string{root}
	cfg.Tools.Exec.CWD = root
	cfg.Storage.DSN = filepath.Join(root, "mtclaw.db")
	cfg.Cron.Timezone = "UTC"
	return cfg
}

func TestValidate_ValidConfigPasses(t *testing.T) {
	err := Validate(validConfig(t))
	assert.NoError(t, err)
}

// TestValidate_Agent_TemperatureUnsetOrInRangePasses proves the range check
// only fires once the user actually sets agent.temperature: Default()
// leaves it nil, and Validate must not treat that as 0 (an in-range value
// that would also pass, but for the wrong reason).
func TestValidate_Agent_TemperatureUnsetOrInRangePasses(t *testing.T) {
	cfg := validConfig(t)
	assert.Nil(t, cfg.Agent.Temperature, "Default() must leave temperature unset")
	assert.NoError(t, Validate(cfg))

	inRange := 1.2
	cfg.Agent.Temperature = &inRange
	assert.NoError(t, Validate(cfg))
}

func TestValidate_Version(t *testing.T) {
	cfg := validConfig(t)
	cfg.Version = 2
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "version: unsupported schema version 2")
}

func TestValidate_AgentFields(t *testing.T) {
	cfg := validConfig(t)
	cfg.Agent.Model = ""
	cfg.Agent.MaxIterations = 0
	cfg.Agent.MaxHistoryTurns = 1
	badTemp := 3.0
	cfg.Agent.Temperature = &badTemp

	err := Validate(cfg)
	require.Error(t, err)

	ve, ok := err.(*ValidationErrors)
	require.True(t, ok)
	// All four violations must be reported in the same pass, not just the
	// first one encountered.
	assert.Equal(t, 4, ve.Len())
	msg := err.Error()
	assert.Contains(t, msg, "agent.model: must be set")
	assert.Contains(t, msg, "agent.max_iterations: must be between 1 and 100")
	assert.Contains(t, msg, "agent.max_history_turns: must be at least 2")
	assert.Contains(t, msg, "agent.temperature: must be between 0 and 2")
}

func TestValidate_OpenAI(t *testing.T) {
	cfg := validConfig(t)
	cfg.OpenAI.BaseURL = "not-a-url"
	cfg.OpenAI.Timeout = Duration(0)

	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "openai.base_url: must be an absolute http(s) URL")
	assert.Contains(t, err.Error(), "openai.timeout: must be greater than 0")
}

func TestValidate_InlineSecretsRejected(t *testing.T) {
	cfg := validConfig(t)
	cfg.OpenAI.APIKeyInline = "sk-hardcoded"
	cfg.Channels.Telegram.TokenInline = "hardcoded-token"

	err := Validate(cfg)
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, "openai.api_key: must not be set inline")
	assert.Contains(t, msg, "openai.api_key_env")
	assert.Contains(t, msg, "channels.telegram.token: must not be set inline")
	assert.Contains(t, msg, "channels.telegram.token_env")
}

func TestValidate_TelegramEmptyAllowlist(t *testing.T) {
	cfg := validConfig(t)
	cfg.Channels.Telegram.Enabled = true
	cfg.Channels.Telegram.AllowFrom = nil
	// Only the default "*" group, with an empty allow_from, is present. This
	// must NOT satisfy the check - it still fails closed.
	cfg.Channels.Telegram.Groups = map[string]TelegramGroupConfig{
		"*": {RequireMention: Bool(true)},
	}

	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "channels.telegram.allow_from: would accept no one")
}

func TestValidate_TelegramAllowlistSatisfiedByGroup(t *testing.T) {
	cfg := validConfig(t)
	cfg.Channels.Telegram.Enabled = true
	cfg.Channels.Telegram.AllowFrom = nil
	cfg.Channels.Telegram.Groups = map[string]TelegramGroupConfig{
		"*":              {RequireMention: Bool(true)},
		"-1001234567890": {RequireMention: Bool(true), AllowFrom: []int64{111}},
	}

	err := Validate(cfg)
	assert.NoError(t, err)
}

func TestValidate_TelegramAllowlistSatisfiedByChannelList(t *testing.T) {
	cfg := validConfig(t)
	cfg.Channels.Telegram.Enabled = true
	cfg.Channels.Telegram.AllowFrom = []int64{111}

	err := Validate(cfg)
	assert.NoError(t, err)
}

func TestValidate_TelegramGroupKeys(t *testing.T) {
	cfg := validConfig(t)
	cfg.Channels.Telegram.Enabled = true
	cfg.Channels.Telegram.AllowFrom = []int64{111}
	cfg.Channels.Telegram.Groups = map[string]TelegramGroupConfig{
		"*":         {RequireMention: Bool(true)},
		"not-a-num": {RequireMention: Bool(true)},
		"123456789": {RequireMention: Bool(true)}, // positive: looks like a user id, not a group
	}

	err := Validate(cfg)
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, "channels.telegram.groups.not-a-num: invalid chat id")
	assert.Contains(t, msg, "channels.telegram.groups.123456789")
	assert.Contains(t, msg, "looks like a user id")
}

func TestValidate_TelegramAPIBaseURL(t *testing.T) {
	cfg := validConfig(t)
	cfg.Channels.Telegram.APIBaseURL = "not-a-url"

	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `channels.telegram.api_base_url: must be an absolute http(s) URL, got "not-a-url"`)
}

func TestValidate_TelegramAPIBaseURLEmptyIsValid(t *testing.T) {
	cfg := validConfig(t)
	cfg.Channels.Telegram.APIBaseURL = ""
	assert.NoError(t, Validate(cfg))
}

func TestValidate_TelegramAPIBaseURLAbsoluteHTTPIsValid(t *testing.T) {
	cfg := validConfig(t)
	cfg.Channels.Telegram.APIBaseURL = "http://127.0.0.1:8080"
	assert.NoError(t, Validate(cfg))
}

func TestValidate_ExecMode(t *testing.T) {
	cfg := validConfig(t)
	cfg.Tools.Exec.Mode = "yolo"
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `tools.exec.mode: must be one of "approval", "auto", "off", got "yolo"`)
}

func TestValidate_ExecRegexPatterns(t *testing.T) {
	cfg := validConfig(t)
	cfg.Tools.Exec.Deny = []string{"rm\\s+-rf", "(unterminated"}
	cfg.Tools.Exec.Allow = []string{"["}

	err := Validate(cfg)
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, `tools.exec.deny[1]: invalid regex "(unterminated"`)
	assert.Contains(t, msg, `tools.exec.allow[0]: invalid regex "["`)
	assert.NotContains(t, msg, `deny[0]`) // the valid pattern must not be flagged
}

func TestValidate_FilesystemRootsRequiredWhenEnabled(t *testing.T) {
	cfg := validConfig(t)
	cfg.Tools.Filesystem.Enabled = true
	cfg.Tools.Filesystem.Roots = nil
	cfg.Tools.Exec.Enabled = false // avoid also tripping the cwd-containment rule

	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tools.filesystem.roots: must list at least one root")
}

func TestValidate_ExecCWDOutsideRootsIsNotAValidationError(t *testing.T) {
	// exec.cwd is documented as "not a jail": config.Validate never compares
	// it against tools.filesystem.roots, and neither does anything else in
	// the product, at any point - a cwd outside the roots is not a
	// validation failure. `mtclaw doctor`'s checkExecCWD only checks that
	// the directory exists, nothing about its relationship to the roots.
	cfg := validConfig(t)
	cfg.Tools.Exec.CWD = t.TempDir() // a different temp dir than the configured root

	assert.NoError(t, Validate(cfg))
}

func TestValidate_ToolBounds(t *testing.T) {
	cfg := validConfig(t)
	cfg.Tools.Filesystem.Enabled = true
	cfg.Tools.Filesystem.MaxReadBytes = -5
	cfg.Tools.Filesystem.MaxWriteBytes = 0
	cfg.Tools.WebFetch.Enabled = true
	cfg.Tools.WebFetch.Timeout = Duration(0)
	cfg.Tools.WebFetch.MaxBytes = 0
	cfg.Tools.Exec.Enabled = true
	cfg.Tools.Exec.Timeout = Duration(0)
	cfg.Tools.Exec.ApprovalTimeout = Duration(0)
	cfg.Tools.Exec.MaxOutputBytes = 0

	err := Validate(cfg)
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, "tools.filesystem.max_read_bytes: must be greater than 0, got -5")
	assert.Contains(t, msg, "tools.filesystem.max_write_bytes: must be greater than 0, got 0")
	assert.Contains(t, msg, "tools.web_fetch.timeout: must be greater than 0")
	assert.Contains(t, msg, "tools.web_fetch.max_bytes: must be greater than 0, got 0")
	assert.Contains(t, msg, "tools.exec.timeout: must be greater than 0")
	assert.Contains(t, msg, "tools.exec.approval_timeout: must be greater than 0")
	assert.Contains(t, msg, "tools.exec.max_output_bytes: must be greater than 0, got 0")
}

func TestValidate_ToolBoundsSkippedWhenDisabled(t *testing.T) {
	// A negative/zero bound must be skipped while its tool is disabled -
	// nothing registers the tool, so nothing will ever read the value - but
	// the exact same value must fail the moment the tool is turned on.
	// Asserting both halves is what makes this discriminating: a guard that
	// always passes regardless of Enabled (or that never actually reads
	// Enabled) would still pass the "disabled" half alone.
	t.Run("filesystem.max_read_bytes", func(t *testing.T) {
		cfg := validConfig(t)
		cfg.Tools.Filesystem.Enabled = false
		cfg.Tools.Filesystem.MaxReadBytes = -5
		assert.NoError(t, Validate(cfg))

		cfg.Tools.Filesystem.Enabled = true
		err := Validate(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "tools.filesystem.max_read_bytes: must be greater than 0, got -5")
	})

	t.Run("web_fetch.timeout", func(t *testing.T) {
		cfg := validConfig(t)
		cfg.Tools.WebFetch.Enabled = false
		cfg.Tools.WebFetch.Timeout = Duration(0)
		assert.NoError(t, Validate(cfg))

		cfg.Tools.WebFetch.Enabled = true
		err := Validate(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "tools.web_fetch.timeout: must be greater than 0")
	})

	t.Run("exec.timeout", func(t *testing.T) {
		cfg := validConfig(t)
		cfg.Tools.Exec.Enabled = false
		cfg.Tools.Exec.Timeout = Duration(0)
		assert.NoError(t, Validate(cfg))

		cfg.Tools.Exec.Enabled = true
		err := Validate(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "tools.exec.timeout: must be greater than 0")
	})
}

func TestValidate_OpenAIMaxRetriesNegative(t *testing.T) {
	cfg := validConfig(t)
	cfg.OpenAI.MaxRetries = -1

	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "openai.max_retries: must be 0 or greater, got -1")
}

func TestValidate_CronTimezone(t *testing.T) {
	cfg := validConfig(t)
	cfg.Cron.Timezone = "Not/AZone"
	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cron.timezone: invalid IANA timezone")
}

func TestValidate_CronJobs(t *testing.T) {
	cfg := validConfig(t)
	cfg.Channels.Telegram.Enabled = true
	cfg.Channels.Telegram.AllowFrom = []int64{1}
	cfg.Cron.Enabled = true
	cfg.Cron.Jobs = []CronJob{
		{
			Name:      "job-a",
			Schedule:  "0 8 * * *",
			Prompt:    "do a thing",
			Enabled:   true,
			Session:   "persistent",
			Timeout:   Duration(time.Minute),
			DeliverTo: CronDeliverTo{Channel: "telegram", ChatID: "1"},
		},
		{
			// duplicate name, invalid schedule, empty prompt, bad session,
			// zero timeout, empty deliver_to.
			Name:     "job-a",
			Schedule: "this is not a cron expression at all",
			Prompt:   "",
			Enabled:  true,
			Session:  "sometimes",
		},
		{
			// empty name, chat_id not reachable.
			Name:      "",
			Schedule:  "0 8 * * *",
			Prompt:    "something",
			Enabled:   true,
			Session:   "ephemeral",
			Timeout:   Duration(time.Minute),
			DeliverTo: CronDeliverTo{Channel: "telegram", ChatID: "999"},
		},
	}

	err := Validate(cfg)
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, `cron.jobs[1].name: duplicate job name "job-a"`)
	assert.Contains(t, msg, `job "job-a": invalid cron expression`)
	assert.Contains(t, msg, `job "job-a": prompt must not be empty`)
	assert.Contains(t, msg, `job "job-a": session must be "persistent" or "ephemeral"`)
	assert.Contains(t, msg, `job "job-a": timeout must be greater than 0`)
	assert.Contains(t, msg, `job "job-a": must set both channel and chat_id`)
	assert.Contains(t, msg, "cron.jobs[2].name: must not be empty")
	assert.Contains(t, msg, `chat_id "999" is not in channels.telegram.allow_from`)
}

func TestValidate_CronDeliverTo(t *testing.T) {
	base := func(t *testing.T) *Config {
		cfg := validConfig(t)
		cfg.Cron.Enabled = true
		cfg.Cron.Jobs = []CronJob{{
			Name:     "job-a",
			Schedule: "0 8 * * *",
			Prompt:   "do a thing",
			Enabled:  true,
			Session:  "persistent",
			Timeout:  Duration(time.Minute),
		}}
		return cfg
	}

	t.Run("unsupported channel", func(t *testing.T) {
		cfg := base(t)
		cfg.Cron.Jobs[0].DeliverTo = CronDeliverTo{Channel: "slack", ChatID: "1"}
		err := Validate(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), `job "job-a": only "telegram" is supported`)
	})

	t.Run("telegram disabled", func(t *testing.T) {
		cfg := base(t)
		cfg.Channels.Telegram.Enabled = false
		cfg.Cron.Jobs[0].DeliverTo = CronDeliverTo{Channel: "telegram", ChatID: "1"}
		err := Validate(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), `job "job-a": channels.telegram.enabled must be true`)
	})

	t.Run("chat_id reachable via group", func(t *testing.T) {
		cfg := base(t)
		cfg.Channels.Telegram.Enabled = true
		cfg.Channels.Telegram.Groups = map[string]TelegramGroupConfig{"-100": {AllowFrom: []int64{1}}}
		cfg.Cron.Jobs[0].DeliverTo = CronDeliverTo{Channel: "telegram", ChatID: "-100"}
		assert.NoError(t, Validate(cfg))
	})

	t.Run("chat_id reachable via allow_from", func(t *testing.T) {
		cfg := base(t)
		cfg.Channels.Telegram.Enabled = true
		cfg.Channels.Telegram.AllowFrom = []int64{42}
		cfg.Cron.Jobs[0].DeliverTo = CronDeliverTo{Channel: "telegram", ChatID: "42"}
		assert.NoError(t, Validate(cfg))
	})
}

func TestValidate_StorageParentDirCreatable(t *testing.T) {
	cfg := validConfig(t)
	// Parent does not exist yet but is creatable under a writable temp dir.
	nested := filepath.Join(t.TempDir(), "nested", "dirs")
	cfg.Storage.DSN = filepath.Join(nested, "mtclaw.db")
	assert.NoError(t, Validate(cfg))

	// Validate must not write to disk: it only stats up to the nearest
	// existing ancestor, never creates the directory itself.
	_, err := os.Stat(nested)
	assert.True(t, os.IsNotExist(err), "Validate must not create %s", nested)
}

func TestValidate_StorageParentDirNotCreatable(t *testing.T) {
	cfg := validConfig(t)
	// A regular file cannot be treated as a directory.
	blocker := filepath.Join(t.TempDir(), "blocker")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o644))
	cfg.Storage.DSN = filepath.Join(blocker, "mtclaw.db")

	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "storage.dsn: parent directory")
}

func TestValidate_StorageBothDSNAndPathSet(t *testing.T) {
	cfg := validConfig(t)
	cfg.Storage.DSN = filepath.Join(t.TempDir(), "via-dsn.db")
	cfg.Storage.Path = filepath.Join(t.TempDir(), "via-path.db")

	err := Validate(cfg)
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, "storage.dsn: set either storage.dsn or the deprecated storage.path, not both")
}

func TestValidate_StorageUnsupportedDriver(t *testing.T) {
	cfg := validConfig(t)
	cfg.Storage.Driver = "postgres"

	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `storage.driver: must be one of: sqlite, got "postgres"`)
}

// TestValidate_CronDeliverToSkippedWhenCronOrJobDisabled proves an
// unreachable deliver_to.chat_id does not block loading while cron itself,
// or the individual job, is disabled: a job that can never fire must not
// stop the rest of the config from loading over a chat_id nobody will ever
// use while it stays off.
func TestValidate_CronDeliverToSkippedWhenCronOrJobDisabled(t *testing.T) {
	unreachable := CronJob{
		Name:      "job-a",
		Schedule:  "0 8 * * *",
		Prompt:    "do a thing",
		Session:   "persistent",
		Timeout:   Duration(time.Minute),
		DeliverTo: CronDeliverTo{Channel: "telegram", ChatID: "999"},
	}

	t.Run("cron.enabled false", func(t *testing.T) {
		cfg := validConfig(t)
		cfg.Cron.Enabled = false
		job := unreachable
		job.Enabled = true
		cfg.Cron.Jobs = []CronJob{job}
		assert.NoError(t, Validate(cfg))
	})

	t.Run("job.enabled false", func(t *testing.T) {
		cfg := validConfig(t)
		cfg.Cron.Enabled = true
		job := unreachable
		job.Enabled = false
		cfg.Cron.Jobs = []CronJob{job}
		assert.NoError(t, Validate(cfg))
	})

	t.Run("both enabled still enforces reachability", func(t *testing.T) {
		cfg := validConfig(t)
		cfg.Channels.Telegram.Enabled = true
		cfg.Channels.Telegram.AllowFrom = []int64{1}
		cfg.Cron.Enabled = true
		job := unreachable
		job.Enabled = true
		cfg.Cron.Jobs = []CronJob{job}
		err := Validate(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), `chat_id "999" is not in channels.telegram.allow_from`)
	})
}

// TestValidate_CronScheduleFieldCount proves a 6-field (seconds-first) or
// 7-field (year-suffixed) expression - which gronx.IsValid itself accepts -
// is rejected here: internal/cron's scheduler only ticks once per minute, so
// either shape would pass gronx alone and then never fire (or fire years
// late) with no signal anything was wrong.
func TestValidate_CronScheduleFieldCount(t *testing.T) {
	newCfg := func(schedule string) *Config {
		cfg := validConfig(t)
		// Enabled is deliberately left false: this test is only about
		// schedule field-count validation, which (unlike deliver_to
		// reachability) always runs regardless of job.Enabled/cron.Enabled,
		// so there is no need to also satisfy deliver_to here.
		cfg.Cron.Jobs = []CronJob{{
			Name: "job-a", Schedule: schedule, Prompt: "hi",
			Session: "persistent", Timeout: Duration(time.Minute),
		}}
		return cfg
	}

	t.Run("6-field seconds-first is rejected", func(t *testing.T) {
		err := Validate(newCfg("30 0 9 * * *"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid cron expression")
	})

	t.Run("7-field year-suffixed is rejected", func(t *testing.T) {
		err := Validate(newCfg("0 9 * * * * 2030"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid cron expression")
	})

	t.Run("plain 5-field expression passes", func(t *testing.T) {
		assert.NoError(t, Validate(newCfg("0 9 * * *")))
	})

	t.Run("@daily macro passes", func(t *testing.T) {
		assert.NoError(t, Validate(newCfg("@daily")))
	})
}

func TestValidate_LogFields(t *testing.T) {
	cfg := validConfig(t)
	cfg.Log.Level = "debgu"
	cfg.Log.Format = "jsn"

	err := Validate(cfg)
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, `log.level: must be one of "debug", "info", "warn", "error" (case-insensitive; "warning" also accepted), got "debgu"`)
	assert.Contains(t, msg, `log.format: must be one of "text", "json" (case-insensitive), got "jsn"`)
}

// TestValidate_LogFields_AcceptsMixedCaseWhitespaceAndWarningAlias proves a
// spelling that always loaded and logged correctly before this validation
// existed - mixed case, incidental whitespace, or "warning" in place of
// "warn" - still passes: internal/logging.parseLevel and logging.New's
// format comparison already normalize the same way, so validation must not
// reject something they would have accepted.
func TestValidate_LogFields_AcceptsMixedCaseWhitespaceAndWarningAlias(t *testing.T) {
	cases := []struct {
		level, format string
	}{
		{"warning", "text"},
		{"WARN", "text"},
		{" Info ", "text"},
		{"debug", "JSON"},
		{"ERROR", " json "},
	}
	for _, tc := range cases {
		cfg := validConfig(t)
		cfg.Log.Level = tc.level
		cfg.Log.Format = tc.format
		assert.NoErrorf(t, Validate(cfg), "level=%q format=%q must be accepted", tc.level, tc.format)
	}
}

func TestTelegramGroupConfig_MentionRequiredDefaultsTrueWhenUnset(t *testing.T) {
	assert.True(t, TelegramGroupConfig{}.MentionRequired(), "an omitted require_mention key must still require a mention")
	assert.True(t, TelegramGroupConfig{RequireMention: Bool(true)}.MentionRequired())
	assert.False(t, TelegramGroupConfig{RequireMention: Bool(false)}.MentionRequired())
}

func TestValidate_StorageDSNEmpty(t *testing.T) {
	cfg := validConfig(t)
	cfg.Storage.DSN = ""

	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "storage.dsn: must not be empty")
}
