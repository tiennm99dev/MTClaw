package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/adhocore/gronx"
)

// ValidationErrors collects every rule violation found by Validate. Callers
// should treat a nil *ValidationErrors (or a nil error) as success; a
// non-nil instance always contains at least one entry.
type ValidationErrors struct {
	errs []error
}

// Error joins every violation onto its own line, each prefixed with the
// dotted config key path it applies to.
func (e *ValidationErrors) Error() string {
	lines := make([]string, len(e.errs))
	for i, err := range e.errs {
		lines[i] = err.Error()
	}
	return strings.Join(lines, "\n")
}

// Unwrap exposes the individual errors so errors.Is/As can inspect them.
func (e *ValidationErrors) Unwrap() []error { return e.errs }

// Len reports how many violations were collected.
func (e *ValidationErrors) Len() int { return len(e.errs) }

func (e *ValidationErrors) add(path, format string, args ...any) {
	e.errs = append(e.errs, fmt.Errorf("%s: %s", path, fmt.Sprintf(format, args...)))
}

// Validate checks cfg against every structural and semantic rule in the
// config schema, accumulating all failures instead of stopping at the
// first one. It assumes paths have already been expanded (ExpandPath) and
// secrets already resolved (resolveSecrets); it never writes to the
// filesystem, and the only filesystem read is a stat walk up storage.path's
// parent-directory chain to check it is creatable.
func Validate(cfg *Config) error {
	errs := &ValidationErrors{}

	validateVersion(cfg, errs)
	validateAgent(cfg, errs)
	validateOpenAI(cfg, errs)
	validateTelegram(cfg, errs)
	validateTools(cfg, errs)
	validateCron(cfg, errs)
	validateStorage(cfg, errs)
	validateLog(cfg, errs)

	if errs.Len() == 0 {
		return nil
	}
	return errs
}

func validateVersion(cfg *Config, errs *ValidationErrors) {
	if cfg.Version != 1 {
		errs.add("version", "unsupported schema version %d; this build only understands version 1", cfg.Version)
	}
}

func validateAgent(cfg *Config, errs *ValidationErrors) {
	a := cfg.Agent
	if strings.TrimSpace(a.Model) == "" {
		errs.add("agent.model", "must be set; there is no built-in default model")
	}
	if a.MaxIterations < 1 || a.MaxIterations > 100 {
		errs.add("agent.max_iterations", "must be between 1 and 100, got %d", a.MaxIterations)
	}
	if a.MaxHistoryTurns < 2 {
		errs.add("agent.max_history_turns", "must be at least 2, got %d", a.MaxHistoryTurns)
	}
	if a.Temperature != nil && (*a.Temperature < 0 || *a.Temperature > 2) {
		errs.add("agent.temperature", "must be between 0 and 2, got %v", *a.Temperature)
	}
}

func validateOpenAI(cfg *Config, errs *ValidationErrors) {
	o := cfg.OpenAI
	if o.APIKeyInline != "" {
		errs.add("openai.api_key", "must not be set inline in the config file; use openai.api_key_env (or openai.api_key_file) instead")
	}

	u, err := url.Parse(o.BaseURL)
	if err != nil || !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") {
		errs.add("openai.base_url", "must be an absolute http(s) URL, got %q", o.BaseURL)
	}
	if o.Timeout.Std() <= 0 {
		errs.add("openai.timeout", "must be greater than 0, got %s", o.Timeout.Std())
	}
	if o.MaxRetries < 0 {
		errs.add("openai.max_retries", "must be 0 or greater, got %d", o.MaxRetries)
	}
}

func validateTelegram(cfg *Config, errs *ValidationErrors) {
	tg := cfg.Channels.Telegram
	if tg.TokenInline != "" {
		errs.add("channels.telegram.token", "must not be set inline in the config file; use channels.telegram.token_env (or channels.telegram.token_file) instead")
	}

	if tg.APIBaseURL != "" {
		// Mirrors validateOpenAI's base_url check: an absolute http(s) URL is
		// the only shape the bot token can safely be sent to. This is also
		// the seam an httptest fake plugs into, so the check must accept a
		// bare "http://127.0.0.1:PORT" with no path.
		u, err := url.Parse(tg.APIBaseURL)
		if err != nil || !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") {
			errs.add("channels.telegram.api_base_url", "must be an absolute http(s) URL, got %q", tg.APIBaseURL)
		}
	}

	if tg.Enabled && len(tg.AllowFrom) == 0 {
		hasGroupAllow := false
		for _, g := range tg.Groups {
			if len(g.AllowFrom) > 0 {
				hasGroupAllow = true
				break
			}
		}
		if !hasGroupAllow {
			// The default "*" group entry carries an empty allow_from (it
			// inherits the channel list), so its mere presence must not
			// satisfy this check - only an explicit, non-empty group
			// allowlist or a non-empty channel allowlist does.
			errs.add("channels.telegram.allow_from", "would accept no one; set channels.telegram.allow_from")
		}
	}

	for key := range tg.Groups {
		if key == "*" {
			continue
		}
		id, err := strconv.ParseInt(key, 10, 64)
		if err != nil {
			errs.add(fmt.Sprintf("channels.telegram.groups.%s", key),
				"invalid chat id: must be numeric (e.g. -1001234567890 for a supergroup)")
			continue
		}
		if id > 0 {
			errs.add(fmt.Sprintf("channels.telegram.groups.%s", key),
				"chat id %d is positive, which looks like a user id, not a group; group and supergroup chat ids are negative (e.g. -1001234567890 for a supergroup)", id)
		}
	}
}

func validateTools(cfg *Config, errs *ValidationErrors) {
	fs := cfg.Tools.Filesystem
	if fs.Enabled && len(fs.Roots) == 0 {
		errs.add("tools.filesystem.roots", "must list at least one root when tools.filesystem.enabled is true")
	}
	for i, root := range fs.Roots {
		if !filepath.IsAbs(root) {
			errs.add(fmt.Sprintf("tools.filesystem.roots[%d]", i), "must be absolute after expansion, got %q", root)
		}
	}
	if fs.Enabled && fs.MaxReadBytes < 1 {
		errs.add("tools.filesystem.max_read_bytes", "must be greater than 0, got %d", fs.MaxReadBytes)
	}
	if fs.Enabled && fs.MaxWriteBytes < 1 {
		errs.add("tools.filesystem.max_write_bytes", "must be greater than 0, got %d", fs.MaxWriteBytes)
	}

	wf := cfg.Tools.WebFetch
	if wf.Enabled && wf.Timeout.Std() <= 0 {
		errs.add("tools.web_fetch.timeout", "must be greater than 0, got %s", wf.Timeout.Std())
	}
	if wf.Enabled && wf.MaxBytes < 1 {
		errs.add("tools.web_fetch.max_bytes", "must be greater than 0, got %d", wf.MaxBytes)
	}

	exec := cfg.Tools.Exec
	switch exec.Mode {
	case "approval", "auto", "off":
	default:
		errs.add("tools.exec.mode", `must be one of "approval", "auto", "off", got %q`, exec.Mode)
	}
	for i, pattern := range exec.Deny {
		if _, err := regexp.Compile(pattern); err != nil {
			errs.add(fmt.Sprintf("tools.exec.deny[%d]", i), "invalid regex %q: %v", pattern, err)
		}
	}
	for i, pattern := range exec.Allow {
		if _, err := regexp.Compile(pattern); err != nil {
			errs.add(fmt.Sprintf("tools.exec.allow[%d]", i), "invalid regex %q: %v", pattern, err)
		}
	}
	if len(exec.Shell) == 1 {
		errs.add("tools.exec.shell", "must list the shell and its flag that takes the command (for example [/bin/bash, -lc]), or be empty for the OS default; got only %q", exec.Shell[0])
	}
	if exec.Enabled && exec.Timeout.Std() <= 0 {
		errs.add("tools.exec.timeout", "must be greater than 0, got %s", exec.Timeout.Std())
	}
	if exec.Enabled && exec.ApprovalTimeout.Std() <= 0 {
		errs.add("tools.exec.approval_timeout", "must be greater than 0, got %s", exec.ApprovalTimeout.Std())
	}
	if exec.Enabled && exec.MaxOutputBytes < 1 {
		errs.add("tools.exec.max_output_bytes", "must be greater than 0, got %d", exec.MaxOutputBytes)
	}
}

// isValidCronSchedule requires expr to be exactly 5 whitespace-separated
// fields (minute hour day month weekday), or one of gronx's built-in
// @macros (@daily, @hourly, ...), before ever calling gronx.IsValid. gronx
// itself also accepts a 6-field (seconds-first) or 7-field (year-suffixed)
// expression, but internal/cron's scheduler only ticks once per minute, so
// a seconds-first expression would pass validation and then never fire (or
// fire years late) with no signal that anything is wrong.
func isValidCronSchedule(expr string) bool {
	trimmed := strings.TrimSpace(expr)
	if strings.HasPrefix(trimmed, "@") {
		return gronx.IsValid(trimmed)
	}
	if len(strings.Fields(trimmed)) != 5 {
		return false
	}
	return gronx.IsValid(trimmed)
}

func validateCron(cfg *Config, errs *ValidationErrors) {
	if _, err := time.LoadLocation(cfg.Cron.Timezone); err != nil {
		errs.add("cron.timezone", "invalid IANA timezone or \"Local\": %v", err)
	}

	seen := make(map[string]bool, len(cfg.Cron.Jobs))
	for i, job := range cfg.Cron.Jobs {
		path := fmt.Sprintf("cron.jobs[%d]", i)
		name := strings.TrimSpace(job.Name)
		switch {
		case name == "":
			errs.add(path+".name", "must not be empty")
		case seen[name]:
			errs.add(path+".name", "duplicate job name %q", name)
		default:
			seen[name] = true
		}

		// ref names the job in every other message below, falling back to
		// the index when the name itself is what's wrong (empty), so every
		// violation is traceable to one job even without a valid name.
		ref := name
		if ref == "" {
			ref = fmt.Sprintf("cron.jobs[%d]", i)
		}

		if strings.TrimSpace(job.Schedule) == "" {
			errs.add(path+".schedule", "job %q: schedule must not be empty", ref)
		} else if !isValidCronSchedule(job.Schedule) {
			errs.add(path+".schedule", "job %q: invalid cron expression %q (must be exactly 5 fields, or an @macro)", ref, job.Schedule)
		}

		if strings.TrimSpace(job.Prompt) == "" {
			errs.add(path+".prompt", "job %q: prompt must not be empty", ref)
		}

		if job.Session != "persistent" && job.Session != "ephemeral" {
			errs.add(path+".session", "job %q: session must be \"persistent\" or \"ephemeral\", got %q", ref, job.Session)
		}

		if job.Timeout.Std() <= 0 {
			errs.add(path+".timeout", "job %q: timeout must be greater than 0, got %s", ref, job.Timeout.Std())
		}

		// A disabled job (or cron turned off entirely) never fires, so its
		// deliver_to is never dereferenced; skipping the reachability check
		// for it means toggling channels.telegram.enabled: false to pause
		// Telegram temporarily does not also make an unrelated, currently-
		// inert cron job's config unloadable.
		if cfg.Cron.Enabled && job.Enabled {
			validateCronDeliverTo(cfg, path, ref, job.DeliverTo, errs)
		}
	}
}

// validateCronDeliverTo checks that a job's deliver_to actually reaches
// somewhere reachable: without this, a config typo (an unlisted chat id, a
// disabled channel) becomes an outbound message to an arbitrary chat, or a
// job that fails to deliver at every run without any load-time signal. Only
// called for a job that can actually fire (see validateCron).
func validateCronDeliverTo(cfg *Config, path, ref string, d CronDeliverTo, errs *ValidationErrors) {
	if strings.TrimSpace(d.Channel) == "" || strings.TrimSpace(d.ChatID) == "" {
		errs.add(path+".deliver_to", "job %q: must set both channel and chat_id", ref)
		return
	}
	if d.Channel != "telegram" {
		errs.add(path+".deliver_to.channel", "job %q: only \"telegram\" is supported, got %q", ref, d.Channel)
		return
	}

	tg := cfg.Channels.Telegram
	if !tg.Enabled {
		errs.add(path+".deliver_to.channel", "job %q: channels.telegram.enabled must be true to deliver to telegram", ref)
		return
	}
	if !cronChatIDReachable(tg, d.ChatID) {
		errs.add(path+".deliver_to.chat_id", "job %q: chat_id %q is not in channels.telegram.allow_from and is not a configured group", ref, d.ChatID)
	}
}

// ValidateCronDeliverTo applies the same deliver_to reachability rules that
// Validate applies to a job that can fire, for a caller that delivers a
// job's result outside the scheduler (`mtclaw cron run --deliver`): a manual
// run must not reach a chat the scheduled path would have refused to load.
func ValidateCronDeliverTo(cfg *Config, job CronJob) error {
	errs := &ValidationErrors{}
	validateCronDeliverTo(cfg, "cron.jobs", strings.TrimSpace(job.Name), job.DeliverTo, errs)
	if errs.Len() == 0 {
		return nil
	}
	return errs
}

// cronChatIDReachable reports whether chatID is either an explicit group
// key in tg.Groups or a numeric id present in tg.AllowFrom - the same two
// ways a real inbound message is allowed through today.
func cronChatIDReachable(tg TelegramConfig, chatID string) bool {
	// "*" is the default-group template, not a chat: sendMessage to it fails.
	if _, ok := tg.Groups[chatID]; ok && chatID != "*" {
		return true
	}
	id, err := strconv.ParseInt(chatID, 10, 64)
	if err != nil {
		return false
	}
	for _, allowed := range tg.AllowFrom {
		if allowed == id {
			return true
		}
	}
	return false
}

// validateLog rejects an unrecognized log.level or log.format outright,
// matching every other enum field in this schema (tools.exec.mode,
// cron.jobs[].session, ...): without this, a typo like "debgu" or "jsn"
// loaded silently as "info"/"text" in internal/logging, with no signal that
// anything was misspelled. The check is applied to a trimmed, lowercased
// copy of each value, with "warning" accepted as an alias of "warn" - the
// same normalization internal/logging.parseLevel and logging.New already
// apply when actually using these values, so this must not reject a
// spelling ("WARN", "log.level: warning", "log.format: JSON") that always
// worked before this validation existed; only a genuine typo is new to
// this.
func validateLog(cfg *Config, errs *ValidationErrors) {
	if !IsValidLogLevel(cfg.Log.Level) {
		errs.add("log.level", `must be one of "debug", "info", "warn", "error" (case-insensitive; "warning" also accepted), got %q`, cfg.Log.Level)
	}
	switch normalizeLogWord(cfg.Log.Format) {
	case "text", "json":
	default:
		errs.add("log.format", `must be one of "text", "json" (case-insensitive), got %q`, cfg.Log.Format)
	}
	if file := cfg.Log.File; file != "" {
		if info, err := os.Stat(file); err == nil && info.IsDir() {
			errs.add("log.file", "%q is a directory, not a file", file)
		} else if err := ensureDirCreatable(filepath.Dir(file)); err != nil {
			errs.add("log.file", "parent directory of %q is not creatable: %v", file, err)
		}
	}
}

// IsValidLogLevel reports whether level is a recognized log.level spelling:
// trimmed and case-insensitive, with "warning" accepted as an alias of
// "warn". The --log-level flag is checked with the same rule, since it
// overwrites cfg.Log.Level after the file's own value was validated.
func IsValidLogLevel(level string) bool {
	switch normalizeLogWord(level) {
	case "debug", "info", "warn", "warning", "error":
		return true
	default:
		return false
	}
}

// normalizeLogWord trims whitespace and lowercases s, so log.level/log.format
// validation and internal/logging's own parsing always agree on what counts
// as a recognized spelling.
func normalizeLogWord(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// validateStorage checks storage.driver first, since every other rule
// here is meaningless against a driver this binary does not support; then
// the storage.dsn/storage.path conflict, since a config that failed that
// check has no single "the DSN" to run the parent-directory check
// against; then - only for sqlite, whose DSN is a filesystem path, unlike
// a future driver's connection-string DSN - that the resolved DSN's
// parent directory is creatable.
//
// By the time this runs via Load, expandStorage (load.go) has already
// folded a legitimate path-only config's value into DSN and cleared Path,
// so the both-set check below can afford to be the simple, unambiguous
// "both fields are non-empty" test: a hand-built Config that reaches this
// function directly (bypassing Load, as most tests do) must set both
// fields explicitly to trigger it, which carries none of expandStorage's
// own default-value ambiguity.
func validateStorage(cfg *Config, errs *ValidationErrors) {
	if cfg.Storage.Driver != "sqlite" {
		errs.add("storage.driver", "must be one of: sqlite, got %q", cfg.Storage.Driver)
		return
	}

	if cfg.Storage.DSN != "" && cfg.Storage.Path != "" {
		errs.add("storage.dsn", "set either storage.dsn or the deprecated storage.path, not both")
		return
	}

	if strings.TrimSpace(cfg.Storage.EffectiveDSN()) == "" {
		errs.add("storage.dsn", "must not be empty")
		return
	}

	dir := filepath.Dir(cfg.Storage.EffectiveDSN())
	if err := ensureDirCreatable(dir); err != nil {
		errs.add("storage.dsn", "parent directory %q is not creatable: %v", dir, err)
	}
}
