package config

import (
	"fmt"
	"net/url"
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
// phase 1 schema, accumulating all failures instead of stopping at the
// first one. It assumes paths have already been expanded (ExpandPath) and
// secrets already resolved (resolveSecrets); it does not read the
// filesystem except for storage.path's parent-directory check.
func Validate(cfg *Config) error {
	errs := &ValidationErrors{}

	validateVersion(cfg, errs)
	validateAgent(cfg, errs)
	validateOpenAI(cfg, errs)
	validateTelegram(cfg, errs)
	validateTools(cfg, errs)
	validateCron(cfg, errs)
	validateStorage(cfg, errs)

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
	if a.Temperature < 0 || a.Temperature > 2 {
		errs.add("agent.temperature", "must be between 0 and 2, got %v", a.Temperature)
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

	if exec.Enabled {
		if len(fs.Roots) == 0 {
			errs.add("tools.exec.cwd", "cannot be validated against tools.filesystem.roots because no roots are configured")
		} else {
			withinAnyRoot := false
			for _, root := range fs.Roots {
				if isWithinRoot(exec.CWD, root) {
					withinAnyRoot = true
					break
				}
			}
			if !withinAnyRoot {
				errs.add("tools.exec.cwd", "must be inside one of tools.filesystem.roots, got %q", exec.CWD)
			}
		}
	}
}

// isWithinRoot reports whether path is root itself or a descendant of root.
func isWithinRoot(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel))
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
		} else if !gronx.IsValid(job.Schedule) {
			errs.add(path+".schedule", "job %q: invalid cron expression %q", ref, job.Schedule)
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

		validateCronDeliverTo(cfg, path, ref, job.DeliverTo, errs)
	}
}

// validateCronDeliverTo checks that a job's deliver_to actually reaches
// somewhere reachable: without this, a config typo (an unlisted chat id, a
// disabled channel) becomes an outbound message to an arbitrary chat, or a
// job that fails to deliver at every run without any load-time signal.
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

// cronChatIDReachable reports whether chatID is either an explicit group
// key in tg.Groups or a numeric id present in tg.AllowFrom - the same two
// ways a real inbound message is allowed through today.
func cronChatIDReachable(tg TelegramConfig, chatID string) bool {
	if _, ok := tg.Groups[chatID]; ok {
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

// validateStorage checks storage.driver first, since every other rule
// here is meaningless against a driver this binary does not support; then
// the storage.dsn/storage.path conflict, since a config that failed that
// check has no single "the DSN" to run the parent-directory check
// against; then - only for sqlite, whose DSN is a filesystem path, unlike
// a future driver's connection-string DSN (see plan.md R7) - that the
// resolved DSN's parent directory is creatable.
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

	dir := filepath.Dir(cfg.Storage.EffectiveDSN())
	if err := ensureDirCreatable(dir); err != nil {
		errs.add("storage.dsn", "parent directory %q is not creatable: %v", dir, err)
	}
}
