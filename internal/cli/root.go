// Package cli wires cobra commands to the config loader and logger. It is
// the only package main.go calls into.
package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/tiennm99/MTClaw/internal/agent"
	"github.com/tiennm99/MTClaw/internal/channel/telegram"
	"github.com/tiennm99/MTClaw/internal/config"
	"github.com/tiennm99/MTClaw/internal/logging"
	"github.com/tiennm99/MTClaw/internal/provider/openai"
	"github.com/tiennm99/MTClaw/internal/store"
	"github.com/tiennm99/MTClaw/internal/store/sqlite"
	"github.com/tiennm99/MTClaw/internal/tools"
)

// state is process-wide command state populated by the root command's
// PersistentPreRunE and read by subcommands. It exists because cobra
// subcommands are constructed once at startup, before flags are parsed.
type state struct {
	configFlag   string
	logLevelFlag string

	configPath   string
	configSource string
	cfg          *config.Config

	store store.Store
	// closeLog releases the log file prepare opened; nil until then.
	closeLog func() error
}

// newRootContext builds the context Execute drives the whole command tree
// with: cancelled on the first SIGINT/SIGTERM so a long-running command
// (`prompt`, `cron run`) reaches the agent loop's own flush-on-cancel path
// instead of the process dying outright. A second signal must still kill
// the process outright - e.g. a command stuck in a loop that never observes
// ctx - so once ctx is done, the background goroutine calls stop() itself,
// which un-registers the handler and restores Go's default disposition
// (process death) for any further SIGINT/SIGTERM.
func newRootContext() (context.Context, func()) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ctx.Done()
		stop()
	}()
	return ctx, stop
}

// ErrInterrupted wraps whatever error a command returned after the root
// context was cancelled by a signal (see newRootContext), so main can exit
// 130 (128+SIGINT) - the conventional "killed by signal" code - instead of
// the generic 1 an ordinary command failure gets. cobra has already printed
// the underlying error (SilenceErrors is left at its default, false), so
// nothing here needs to preserve or re-print its text.
var ErrInterrupted = errors.New("interrupted")

// Execute builds and runs the mtclaw command tree, returning any error so
// main can set the process exit code. See newRootContext for the signal
// handling ctx carries. The store (see state.openStore) is closed here,
// after the whole command tree has run, rather than from a
// PersistentPostRunE: cobra skips PersistentPostRunE entirely when RunE
// returns an error, which would leak the handle on every failing command
// that had opened one.
func Execute() error {
	ctx, stop := newRootContext()
	defer stop()

	s := &state{}
	err := newRootCmd(s).ExecuteContext(ctx)
	if closeErr := s.closeStore(); closeErr != nil && err == nil {
		err = closeErr
	}
	// The log file is released last so the store's own close errors, if
	// any, are still logged.
	if s.closeLog != nil {
		_ = s.closeLog()
	}
	return finalizeExecuteError(err, ctx.Err())
}

// finalizeExecuteError applies Execute's interrupted-exit-code override: a
// failing command whose failure is explained by the root context itself
// having ended (a signal) is reported as ErrInterrupted instead of its own
// error text, which main.go maps to exit code 130 - see ErrInterrupted's
// doc comment. A command that fails for its own reason while ctx is still
// healthy, or one that succeeds despite ctx having ended in the meantime,
// is untouched.
func finalizeExecuteError(err, ctxErr error) error {
	if err != nil && ctxErr != nil {
		return ErrInterrupted
	}
	return err
}

// openStore lazily opens the sqlite store backing cfg.Storage.Path,
// reusing the same handle across multiple calls within one process.
// readOnly selects a read-only connection for inspection commands
// (`sessions list`, `sessions show`); write commands (`sessions rm`,
// `prompt`, `cron run`) must pass false. A write open also creates
// storage.path's parent directory if missing - config.Validate itself
// never touches the filesystem, so this is the only place in a CLI command
// that does. See Execute for where the handle gets closed.
func (s *state) openStore(ctx context.Context, readOnly bool) (store.Store, error) {
	if s.store != nil {
		return s.store, nil
	}
	if s.cfg == nil {
		return nil, fmt.Errorf("open store: config not loaded")
	}
	if readOnly {
		if _, err := os.Stat(s.cfg.Storage.Path); errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("no database yet at %s; run `mtclaw gateway` or `mtclaw prompt` first", s.cfg.Storage.Path)
		}
	}
	db, err := sqlite.Open(ctx, s.cfg.Storage.Path, readOnly)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	s.store = sqlite.New(db)
	return s.store, nil
}

// newLoop builds the same store/provider/registry/loop wiring both `prompt`
// and `cron run` need, differing only in the approver each supplies
// (TerminalApprover / DenyAllApprover). gateway.New keeps its own copy of
// this wiring - it also owns the lock, dispatcher, and mux, and internal/
// gateway cannot import internal/cli; gateway_cmd.go calls
// tools.DisableEnvironRead itself for that path.
//
// Closing the one remaining vector filterEnv itself cannot - a same-uid
// process reading this one's secrets straight out of /proc/<pid>/environ,
// regardless of what the exec tool's own child inherits (see
// docs/security.md) - belongs here, not in each caller: every exec-capable
// command reaches the tool registry through this one function, so calling
// tools.DisableEnvironRead here once covers `prompt` and `cron run` alike,
// with no risk of a future caller forgetting it. Best-effort: a platform
// where this is a no-op (anything but Linux), or a permission error setting
// PR_SET_DUMPABLE, must not stop the command from running.
func (s *state) newLoop(st store.Store, approver tools.Approver) (*agent.Loop, error) {
	if err := tools.DisableEnvironRead(); err != nil {
		slog.Default().Warn("disable /proc/<pid>/environ read failed", "error", err)
	}

	client, err := openai.New(s.cfg.OpenAI)
	if err != nil {
		return nil, fmt.Errorf("build openai client: %w", err)
	}
	registry, err := tools.New(*s.cfg, st, approver, slog.Default())
	if err != nil {
		return nil, fmt.Errorf("build tool registry: %w", err)
	}
	return agent.New(*s.cfg, client, st, registry, slog.Default()), nil
}

// sendTelegram sends one message over a directly-constructed Telegram bot,
// the one-shot path `send` and `cron run --deliver` both need without a
// running gateway to hand the message to.
func (s *state) sendTelegram(ctx context.Context, chatID, threadID, text string) error {
	token := s.cfg.Channels.Telegram.Token()
	if token == "" {
		return fmt.Errorf("no telegram bot token resolved; set channels.telegram.token_env or channels.telegram.token_file")
	}
	return telegram.SendOnce(ctx, token, chatID, threadID, text)
}

// closeStore closes the store handle if openStore ever opened one.
func (s *state) closeStore() error {
	if s.store == nil {
		return nil
	}
	err := s.store.Close()
	s.store = nil
	return err
}

func newRootCmd(s *state) *cobra.Command {
	root := &cobra.Command{
		Use:           "mtclaw",
		Short:         "MTClaw: a single-binary personal AI agent gateway",
		SilenceUsage:  true,
		SilenceErrors: false,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			return s.prepare(cmd)
		},
	}

	root.PersistentFlags().StringVar(&s.configFlag, "config", "", "path to the config file (default: $MTCLAW_CONFIG or ~/.mtclaw/config.yaml)")
	root.PersistentFlags().StringVar(&s.logLevelFlag, "log-level", "", "override log.level from the config file (debug|info|warn|error)")

	root.AddCommand(newVersionCmd())
	root.AddCommand(newConfigCmd(s))
	root.AddCommand(newSessionsCmd(s))
	root.AddCommand(newPromptCmd(s))
	root.AddCommand(newApprovalsCmd(s))
	root.AddCommand(newSendCmd(s))
	root.AddCommand(newGatewayCmd(s))
	root.AddCommand(newCronCmd(s))
	root.AddCommand(newOnboardCmd(s))
	root.AddCommand(newDoctorCmd(s))

	return root
}

// annotationConfig is the cobra Annotations key every runnable command
// declares (see configNone/configInspect/configFull): what it needs from
// the config file, read once here in prepare instead of two separate
// hard-coded CommandPath() string lists that a renamed or new command could
// silently fall out of. See configLevel for how an unannotated command
// (a cobra built-in, or one that forgot to declare itself) is treated.
const annotationConfig = "config"

const (
	// configNone means the command must work with a broken or absent
	// config file entirely: `version`, `config path`, `onboard` (which
	// writes the config that does not exist yet), and `doctor` (which
	// loads the config itself, so a parse/validation failure becomes a
	// diagnosable FAIL row instead of a command that cannot even start).
	configNone = "none"
	// configInspect means the command only inspects existing state (config,
	// sessions, approvals, cron list) rather than running the agent loop,
	// sending a message, or otherwise doing work worth a persistent log
	// file for: the config loads and validates normally, but the logger
	// never opens log.file (see prepare).
	configInspect = "inspect"
	// configFull is every other command: config loads, validates, and logs
	// to log.file when set. It is also the default a command gets if it
	// forgets to declare Annotations[annotationConfig] at all - the safer
	// direction to fail in, and the one every command had before this
	// existed.
	configFull = "full"
)

// prepare resolves the config path for every command (cheap, no I/O beyond
// os.UserHomeDir) and, per cmd's declared configLevel, loads and validates
// the config file and installs the resulting logger as slog's default.
func (s *state) prepare(cmd *cobra.Command) error {
	path, source, err := config.ConfigPath(s.configFlag)
	if err != nil {
		return err
	}
	s.configPath, s.configSource = path, source

	level := configLevel(cmd)
	if level == configNone {
		return nil
	}

	cfg, err := config.LoadFile(path)
	if err != nil {
		var notFound *config.FileNotFoundError
		if errors.As(err, &notFound) {
			return fmt.Errorf("%w; run `mtclaw onboard` to create one", err)
		}
		return err
	}
	if s.logLevelFlag != "" {
		if !isValidLogLevel(s.logLevelFlag) {
			return fmt.Errorf("--log-level: must be one of debug, info, warn, error; got %q", s.logLevelFlag)
		}
		cfg.Log.Level = s.logLevelFlag
	}
	s.cfg = cfg

	// Read-only inspection commands must not create a log file (or its
	// parent directory) as a side effect of merely looking at state:
	// pointing --config at someone else's config to inspect it must stay
	// read-only regardless of what log.file that config sets. They still
	// log to stderr, just never open a file. cfg.Log itself (used by
	// `config show`) is left untouched - only the copy handed to the
	// logger is adjusted.
	logCfg := cfg.Log
	if level == configInspect {
		logCfg.File = ""
	}
	logger, closeLog, err := logging.New(logCfg)
	if err != nil {
		return err
	}
	s.closeLog = closeLog
	slog.SetDefault(logger)

	return nil
}

// isValidLogLevel mirrors config.Validate's own log.level enum (see
// validateLog), applied a second time here because --log-level overwrites
// cfg.Log.Level after LoadFile has already validated the file's own value -
// an invalid flag value must not silently degrade to logging's own info
// fallback the way an invalid file value no longer does. Trimmed and
// lowercased, with "warning" accepted as an alias of "warn", the same as
// logging.parseLevel itself already treats a config file's log.level - a
// case or whitespace difference here is not the kind of typo this check
// exists to catch, and rejecting it would be a stricter, newly-introduced
// break from what always worked.
func isValidLogLevel(level string) bool {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug", "info", "warn", "warning", "error":
		return true
	default:
		return false
	}
}

// configLevel resolves cmd's declared config-loading requirement: an
// explicit Annotations[annotationConfig] wins. A cobra-builtin meta command
// - `help`, `completion` (and its shell subcommands), or the
// `__complete`/`__completeNoDesc` machinery TAB completion drives - defaults
// to configNone when it has no annotation of its own, since none of them
// ever need cfg and all must work on a fresh install with no config file
// yet. Any other unannotated command defaults to configFull, the safer
// direction and the behavior every command had before annotations existed.
func configLevel(cmd *cobra.Command) string {
	if v, ok := cmd.Annotations[annotationConfig]; ok {
		return v
	}
	if isCobraBuiltinMetaCommand(cmd) {
		return configNone
	}
	return configFull
}

// configAnnotation builds the Annotations map a command's constructor
// attaches to declare its configLevel; see the configNone/configInspect/
// configFull constants above.
func configAnnotation(level string) map[string]string {
	return map[string]string{annotationConfig: level}
}

// isCobraBuiltinMetaCommand reports whether cmd is one cobra registers
// itself rather than one newRootCmd adds: `help`, the `completion` group
// command and its per-shell subcommands, and the hidden `__complete`/
// `__completeNoDesc` commands shell TAB completion invokes on every
// keystroke.
func isCobraBuiltinMetaCommand(cmd *cobra.Command) bool {
	switch cmd.Name() {
	case "help", "completion", cobra.ShellCompRequestCmd, cobra.ShellCompNoDescRequestCmd:
		return true
	}
	if parent := cmd.Parent(); parent != nil && parent.Name() == "completion" {
		return true
	}
	return false
}
