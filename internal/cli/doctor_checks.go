package cli

import (
	"context"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/adhocore/gronx"

	"github.com/tiennm99/MTClaw/internal/channel/telegram"
	"github.com/tiennm99/MTClaw/internal/config"
	"github.com/tiennm99/MTClaw/internal/gateway"
	"github.com/tiennm99/MTClaw/internal/provider/openai"
	"github.com/tiennm99/MTClaw/internal/store"

	// Blank import: see root.go's identical import for why store.Open
	// cannot resolve "sqlite" without it.
	_ "github.com/tiennm99/MTClaw/internal/store/sqlite"
)

// doctorNetworkTimeout bounds every check that makes an outbound call
// (OpenAI probe, model list, Telegram getMe), so a hung network never turns
// `doctor` itself into the thing that hangs.
const doctorNetworkTimeout = 15 * time.Second

// doctorChecks returns every registered Check in the table order from
// phase-09-hardening-and-release.md. configPath is closed over only by the
// one check (config file permissions) that needs the config file's own
// path rather than anything inside the loaded *config.Config.
func doctorChecks(configPath string) []Check {
	stateDir, stateDirErr := config.StateDir()
	return []Check{
		{Name: "Config file permissions", Run: checkConfigFilePermissions(configPath)},
		{Name: "State dir writable", Run: checkStateDirWritable(stateDir, stateDirErr)},
		{Name: "DB opens and migrates", Run: checkDatabase},
		{Name: "Instance lock", Run: checkInstanceLock(stateDir, stateDirErr)},
		{Name: "OpenAI key resolves", Run: checkOpenAIKeyResolves},
		{Name: "OpenAI reachable", Run: checkOpenAIReachable},
		{Name: "Model exists", Run: checkModelExists},
		{Name: "Telegram token resolves", Run: checkTelegramTokenResolves},
		{Name: "Telegram getMe", Run: checkTelegramGetMe},
		{Name: "Allowlist non-empty", Run: checkAllowlistNonEmpty},
		{Name: "Workspace exists and writable", Run: checkWorkspace},
		{Name: "filesystem.roots exist", Run: checkFilesystemRoots},
		{Name: "exec.cwd inside a root", Run: checkExecCWD},
		{Name: "Shell exists", Run: checkShellExists},
		{Name: "Deny-list sanity", Run: checkDenyListSanity},
		{Name: "exec.mode: auto", Run: checkExecModeAuto},
		{Name: "Cron", Run: checkCron},
	}
}

// checkConfigFilePermissions warns (never fails) when the config file is
// world-readable on POSIX. Windows' os.FileMode does not reliably reflect
// ACL-based permissions (see internal/config/load.go's warnIfWorldReadable,
// which makes the same call for *_file secrets), so this is a documented
// no-op there rather than a check that would be either always-true or
// always-false noise.
func checkConfigFilePermissions(configPath string) func(context.Context, *config.Config) Result {
	return func(_ context.Context, _ *config.Config) Result {
		if runtime.GOOS == "windows" {
			return Result{StatusOK, "world-readable check is POSIX-only (Windows permissions are ACL-based, not the mode bits this check reads); skipped"}
		}
		info, err := os.Stat(configPath)
		if err != nil {
			return Result{StatusFail, fmt.Sprintf("cannot stat config file %s: %v", configPath, err)}
		}
		if info.Mode().Perm()&0o004 != 0 {
			return Result{StatusWarn, fmt.Sprintf("%s is world-readable (mode %s) - run: chmod 600 %s", configPath, info.Mode().Perm(), configPath)}
		}
		return Result{StatusOK, fmt.Sprintf("%s is not world-readable", configPath)}
	}
}

// checkStateDirWritable verifies ~/.mtclaw (internal/config.StateDir) can be
// created and written to - the database, the instance lock, and (after
// onboard) the starter AGENTS.md all live there. stateDir/resolveErr are
// resolved once by doctorChecks and passed in (rather than calling
// config.StateDir() again here) purely so tests can point this check at a
// t.TempDir() instead of the real, machine-wide ~/.mtclaw.
func checkStateDirWritable(stateDir string, resolveErr error) func(context.Context, *config.Config) Result {
	return func(_ context.Context, _ *config.Config) Result {
		if resolveErr != nil {
			return Result{StatusFail, fmt.Sprintf("cannot resolve the state directory: %v", resolveErr)}
		}
		if err := os.MkdirAll(stateDir, 0o755); err != nil {
			return Result{StatusFail, fmt.Sprintf("cannot create state directory %s: %v - check permissions on its parent", stateDir, err)}
		}
		return checkDirWritable("state directory", stateDir)
	}
}

// checkDirWritable is the shared existence+write-probe used by the state
// dir, workspace, and (indirectly) exec.cwd checks.
func checkDirWritable(label, dir string) Result {
	info, err := os.Stat(dir)
	if err != nil {
		return Result{StatusFail, fmt.Sprintf("%s %q: %v - create it or fix the config value", label, dir, err)}
	}
	if !info.IsDir() {
		return Result{StatusFail, fmt.Sprintf("%s %q exists but is not a directory", label, dir)}
	}
	probe := filepath.Join(dir, ".mtclaw-doctor-write-test")
	if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
		return Result{StatusFail, fmt.Sprintf("%s %q is not writable: %v", label, dir, err)}
	}
	_ = os.Remove(probe)
	return Result{StatusOK, fmt.Sprintf("%s exists and is writable", dir)}
}

// checkDatabase opens storage.dsn (via store.Open) the same way a real
// process would: a database that already exists is opened read-only
// (never disturbing a live gateway's connection), falling back to
// read-write only when that fails (a fresh install with no database file
// yet); either way Open runs pending migrations and refuses a schema newer
// than this binary supports, so the same error text `doctor` reports here
// is what a real startup would have hit.
func checkDatabase(ctx context.Context, cfg *config.Config) Result {
	dsn := cfg.Storage.EffectiveDSN()
	st, err := store.Open(ctx, cfg.Storage, true)
	if err != nil {
		st, err = store.Open(ctx, cfg.Storage, false)
	}
	if err != nil {
		return Result{StatusFail, fmt.Sprintf("cannot open database %s: %v - check storage.dsn's parent directory permissions, or that this binary is at least as new as whatever last wrote this file", dsn, err)}
	}
	defer st.Close()
	return Result{StatusOK, fmt.Sprintf("%s opens and is at the current schema version", dsn)}
}

// checkInstanceLock reports a currently-held lock as INFO, not FAIL: a
// running gateway holding its own lock is the expected, common case, not a
// misconfiguration. stateDir/resolveErr are resolved once by doctorChecks;
// see checkStateDirWritable's doc comment for why.
func checkInstanceLock(stateDir string, resolveErr error) func(context.Context, *config.Config) Result {
	return func(_ context.Context, _ *config.Config) Result {
		if resolveErr != nil {
			return Result{StatusFail, fmt.Sprintf("cannot resolve the state directory: %v", resolveErr)}
		}
		lockPath := filepath.Join(stateDir, "gateway.lock")
		pid, held, err := gateway.Held(lockPath)
		if err != nil {
			return Result{StatusFail, fmt.Sprintf("cannot read instance lock %s: %v", lockPath, err)}
		}
		if held {
			return Result{StatusInfo, fmt.Sprintf("gateway (pid %d) is running and holds the instance lock - expected while it runs; a persistent cron job cannot fire manually (`cron run`) until it stops", pid)}
		}
		return Result{StatusOK, "no gateway instance currently holds the lock"}
	}
}

// checkOpenAIKeyResolves reports which source (env or file) filled
// cfg.OpenAI's secret, without ever printing the value itself.
func checkOpenAIKeyResolves(_ context.Context, cfg *config.Config) Result {
	if cfg.OpenAI.APIKey() == "" {
		envName := cfg.OpenAI.APIKeyEnv
		if envName == "" {
			envName = "OPENAI_API_KEY"
		}
		return Result{StatusFail, fmt.Sprintf("no OpenAI API key resolved (checked env var %s and openai.api_key_file) - run `mtclaw onboard` or export %s", envName, envName)}
	}
	return Result{StatusOK, fmt.Sprintf("resolved from %s", cfg.OpenAI.APIKeySource())}
}

// checkOpenAIReachable issues one minimal completion via Client.Probe.
// Absent credentials degrade to a FAIL naming the fix instead of attempting
// (and failing more confusingly on) a network call with no key.
func checkOpenAIReachable(ctx context.Context, cfg *config.Config) Result {
	if cfg.OpenAI.APIKey() == "" {
		return Result{StatusFail, "cannot probe the OpenAI endpoint: no API key resolved (see \"OpenAI key resolves\" above)"}
	}
	client, err := openai.New(cfg.OpenAI)
	if err != nil {
		return Result{StatusFail, fmt.Sprintf("cannot build the OpenAI client: %v", err)}
	}
	pctx, cancel := context.WithTimeout(ctx, doctorNetworkTimeout)
	defer cancel()
	result := client.Probe(pctx, cfg.Agent.Model)
	if result.Err != nil {
		return Result{StatusFail, fmt.Sprintf("OpenAI endpoint unreachable (base_url=%s model=%s): %v - check openai.base_url, the resolved API key, and network access", result.BaseURL, result.Model, result.Err)}
	}
	return Result{StatusOK, fmt.Sprintf("base_url=%s model=%s latency=%s", result.BaseURL, result.Model, result.Latency)}
}

// checkModelExists lists models and looks for cfg.Agent.Model, warning
// (never failing) on a miss so a model the provider added after this
// binary's release does not read as an error.
func checkModelExists(ctx context.Context, cfg *config.Config) Result {
	if cfg.OpenAI.APIKey() == "" {
		return Result{StatusFail, fmt.Sprintf("cannot verify agent.model %q exists: no OpenAI API key resolved (see \"OpenAI key resolves\" above)", cfg.Agent.Model)}
	}
	client, err := openai.New(cfg.OpenAI)
	if err != nil {
		return Result{StatusFail, fmt.Sprintf("cannot build the OpenAI client: %v", err)}
	}
	lctx, cancel := context.WithTimeout(ctx, doctorNetworkTimeout)
	defer cancel()
	models, err := client.ListModels(lctx)
	if err != nil {
		return Result{StatusFail, fmt.Sprintf("cannot list models to verify agent.model %q: %v", cfg.Agent.Model, err)}
	}
	for _, id := range models {
		if id == cfg.Agent.Model {
			return Result{StatusOK, fmt.Sprintf("%q is in the endpoint's models list", cfg.Agent.Model)}
		}
	}
	return Result{StatusWarn, fmt.Sprintf("agent.model %q was not found in the endpoint's models list (new models can lag the list - only worry if turns start failing with a model-not-found error)", cfg.Agent.Model)}
}

// checkTelegramTokenResolves mirrors checkOpenAIKeyResolves for the bot
// token, skipping outright when the channel is disabled.
func checkTelegramTokenResolves(_ context.Context, cfg *config.Config) Result {
	tg := cfg.Channels.Telegram
	if !tg.Enabled {
		return Result{StatusOK, "channels.telegram.enabled is false; skipping"}
	}
	if tg.Token() == "" {
		envName := tg.TokenEnv
		if envName == "" {
			envName = "TELEGRAM_BOT_TOKEN"
		}
		return Result{StatusFail, fmt.Sprintf("no Telegram bot token resolved (checked env var %s and channels.telegram.token_file) - run `mtclaw onboard` or export %s", envName, envName)}
	}
	return Result{StatusOK, fmt.Sprintf("resolved from %s", tg.TokenSource())}
}

// checkTelegramGetMe calls getMe and reports the bot's username on success.
func checkTelegramGetMe(ctx context.Context, cfg *config.Config) Result {
	tg := cfg.Channels.Telegram
	if !tg.Enabled {
		return Result{StatusOK, "channels.telegram.enabled is false; skipping"}
	}
	if tg.Token() == "" {
		return Result{StatusFail, "cannot call getMe: no Telegram bot token resolved (see \"Telegram token resolves\" above)"}
	}
	gctx, cancel := context.WithTimeout(ctx, doctorNetworkTimeout)
	defer cancel()
	username, err := telegram.GetMe(gctx, tg.Token(), tg.APIBaseURL)
	if err != nil {
		return Result{StatusFail, fmt.Sprintf("Telegram getMe failed: %v - check the bot token", err)}
	}
	return Result{StatusOK, fmt.Sprintf("bot is @%s", username)}
}

// checkAllowlistNonEmpty is defensive rather than load-bearing:
// config.Validate already refuses to load a config where telegram is
// enabled and every allowlist (channel-level and every group's) is empty,
// so reaching this check with cfg loaded means it cannot actually be empty.
// It stays in the table as the documented, always-checkable signal rather
// than assuming validation ran (`onboard` calls these checks directly,
// against a config it just wrote, before config.Load ever gets a chance to
// reject anything).
func checkAllowlistNonEmpty(_ context.Context, cfg *config.Config) Result {
	tg := cfg.Channels.Telegram
	if !tg.Enabled {
		return Result{StatusOK, "channels.telegram.enabled is false; skipping"}
	}
	groupAllows := 0
	for _, g := range tg.Groups {
		groupAllows += len(g.AllowFrom)
	}
	if len(tg.AllowFrom) == 0 && groupAllows == 0 {
		return Result{StatusFail, "channels.telegram.allow_from (and every group's allow_from) is empty; the bot would accept no one - run `mtclaw onboard` or add your user id to channels.telegram.allow_from (get it with /whoami)"}
	}
	return Result{StatusOK, fmt.Sprintf("%d channel-level id(s), %d group-level id(s) allowed", len(tg.AllowFrom), groupAllows)}
}

// checkWorkspace verifies agent.workspace exists and is writable.
func checkWorkspace(_ context.Context, cfg *config.Config) Result {
	return checkDirWritable("agent.workspace", cfg.Agent.Workspace)
}

// checkFilesystemRoots verifies every tools.filesystem.roots entry exists
// as a directory. config.Validate only checks that each root is an
// absolute path after expansion, not that it exists on disk.
func checkFilesystemRoots(_ context.Context, cfg *config.Config) Result {
	fs := cfg.Tools.Filesystem
	if !fs.Enabled {
		return Result{StatusOK, "tools.filesystem.enabled is false; skipping"}
	}
	var missing []string
	for _, root := range fs.Roots {
		if info, err := os.Stat(root); err != nil || !info.IsDir() {
			missing = append(missing, root)
		}
	}
	if len(missing) > 0 {
		return Result{StatusFail, fmt.Sprintf("tools.filesystem.roots missing or not a directory: %s - create them or fix the config", strings.Join(missing, ", "))}
	}
	return Result{StatusOK, fmt.Sprintf("%d root(s) exist", len(fs.Roots))}
}

// checkExecCWD verifies tools.exec.cwd exists on disk. config.Validate only
// checks the string relationship (cwd is inside one of the filesystem
// roots), not that the directory physically exists.
func checkExecCWD(_ context.Context, cfg *config.Config) Result {
	execCfg := cfg.Tools.Exec
	if !execCfg.Enabled {
		return Result{StatusOK, "tools.exec.enabled is false; skipping"}
	}
	info, err := os.Stat(execCfg.CWD)
	if err != nil {
		return Result{StatusFail, fmt.Sprintf("tools.exec.cwd %q does not exist: %v - create it", execCfg.CWD, err)}
	}
	if !info.IsDir() {
		return Result{StatusFail, fmt.Sprintf("tools.exec.cwd %q is not a directory", execCfg.CWD)}
	}
	return Result{StatusOK, fmt.Sprintf("%q exists (confinement to tools.filesystem.roots was already checked at config load)", execCfg.CWD)}
}

// checkShellExists resolves tools.exec.shell (or its OS default) and looks
// it up on PATH.
func checkShellExists(_ context.Context, cfg *config.Config) Result {
	execCfg := cfg.Tools.Exec
	if !execCfg.Enabled {
		return Result{StatusOK, "tools.exec.enabled is false; skipping"}
	}
	shell := execCfg.Shell
	if len(shell) == 0 {
		shell = defaultShellArgv()
	}
	path, err := osexec.LookPath(shell[0])
	if err != nil {
		return Result{StatusFail, fmt.Sprintf("shell %q is not on PATH: %v - install it or set tools.exec.shell", shell[0], err)}
	}
	return Result{StatusOK, fmt.Sprintf("%s -> %s", shell[0], path)}
}

// defaultShellArgv mirrors internal/tools's own (unexported) shell default:
// deliberate small duplication rather than exporting policy-engine
// internals from phase 5 just so a diagnostic can read them.
func defaultShellArgv() []string {
	if runtime.GOOS == "windows" {
		return []string{"powershell", "-NoProfile", "-Command"}
	}
	return []string{"/bin/bash", "-lc"}
}

// checkDenyListSanity warns loudly on an empty deny-list while exec is
// enabled. Every configured pattern is already known to compile -
// config.Validate rejects an invalid regex at load time - so this is an
// informational count, not a re-validation.
func checkDenyListSanity(_ context.Context, cfg *config.Config) Result {
	execCfg := cfg.Tools.Exec
	if !execCfg.Enabled {
		return Result{StatusOK, "tools.exec.enabled is false; skipping"}
	}
	if len(execCfg.Deny) == 0 {
		return Result{StatusWarn, "tools.exec.deny is EMPTY while tools.exec.enabled is true - the deny-list is the only real enforcement boundary in this design (see docs/security.md); run `mtclaw onboard` for the OS-appropriate starter list, or add entries yourself"}
	}
	return Result{StatusOK, fmt.Sprintf("%d deny pattern(s) configured", len(execCfg.Deny))}
}

// checkExecModeAuto always warns when tools.exec.mode is "auto" - not
// because auto mode is broken, but because it is beta and users must not
// mistake the classifier for a security control.
func checkExecModeAuto(_ context.Context, cfg *config.Config) Result {
	execCfg := cfg.Tools.Exec
	if !execCfg.Enabled {
		return Result{StatusOK, "tools.exec.enabled is false; skipping"}
	}
	if execCfg.Mode == "auto" {
		return Result{StatusWarn, "tools.exec.mode is \"auto\": auto mode is beta; the classifier is not a security control - the deny-list is the only real enforcement boundary (see docs/security.md)"}
	}
	return Result{StatusOK, fmt.Sprintf("tools.exec.mode is %q", execCfg.Mode)}
}

// checkCron loads cron.timezone, validates every job's cron expression, and
// prints each enabled job's next due time. Expression validity and
// deliver_to reachability are already enforced by config.Validate at load
// time; this check re-derives the next-run time (the one thing Validate
// does not compute) and would surface a timezone or expression regression
// even if Validate somehow ran against a different config than the one
// loaded here.
func checkCron(_ context.Context, cfg *config.Config) Result {
	if !cfg.Cron.Enabled {
		return Result{StatusOK, "cron.enabled is false; skipping"}
	}
	loc, err := time.LoadLocation(cfg.Cron.Timezone)
	if err != nil {
		return Result{StatusFail, fmt.Sprintf("cron.timezone %q does not load: %v", cfg.Cron.Timezone, err)}
	}
	if len(cfg.Cron.Jobs) == 0 {
		return Result{StatusOK, "cron.enabled is true with no jobs configured"}
	}

	lines := make([]string, 0, len(cfg.Cron.Jobs))
	for _, job := range cfg.Cron.Jobs {
		if !job.Enabled {
			lines = append(lines, fmt.Sprintf("%s: disabled", job.Name))
			continue
		}
		if !gronx.IsValid(job.Schedule) {
			return Result{StatusFail, fmt.Sprintf("cron job %q has an invalid schedule %q", job.Name, job.Schedule)}
		}
		next, err := gronx.NextTickAfter(job.Schedule, time.Now().In(loc), false)
		if err != nil {
			return Result{StatusFail, fmt.Sprintf("cron job %q: cannot compute next run: %v", job.Name, err)}
		}
		lines = append(lines, fmt.Sprintf("%s: next %s", job.Name, next.Format("2006-01-02 15:04:05 MST")))
	}
	return Result{StatusOK, strings.Join(lines, "; ")}
}
