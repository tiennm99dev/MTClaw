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
	"sync/atomic"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/tiennm99/MTClaw/internal/agent"
	"github.com/tiennm99/MTClaw/internal/channel/telegram"
	"github.com/tiennm99/MTClaw/internal/config"
	"github.com/tiennm99/MTClaw/internal/logging"
	"github.com/tiennm99/MTClaw/internal/provider/openai"
	"github.com/tiennm99/MTClaw/internal/store"
	"github.com/tiennm99/MTClaw/internal/tools"

	// Blank import: registers "sqlite" with store.Open's driver registry
	// via the sqlite backend package's own init(). store cannot import
	// that package directly - it already imports store, so that would
	// cycle - which is why this line, not a normal import, is what makes
	// storage.driver: sqlite resolvable at all. Forgetting it fails at
	// runtime (store.ErrUnknownDriver), not at compile time.
	_ "github.com/tiennm99/MTClaw/internal/store/sqlite"
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
// ctx - so after the first signal the handler is un-registered, restoring
// Go's default disposition (process death) for any further SIGINT/SIGTERM.
// The returned func reports the number of the signal that cancelled ctx (0
// if none did), for the conventional 128+signal exit code.
func newRootContext() (context.Context, func(), func() int) {
	ctx, cancel := context.WithCancel(context.Background())
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)

	var received atomic.Int32
	go func() {
		defer signal.Stop(sigs)
		select {
		case sig := <-sigs:
			if num, ok := sig.(syscall.Signal); ok {
				received.Store(int32(num))
			}
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel, func() int { return int(received.Load()) }
}

// ErrInterrupted wraps whatever error a command returned after the root
// context was cancelled by a signal (see newRootContext), so main can exit
// 128+signal - the conventional "killed by signal" code - instead of the
// generic 1 an ordinary command failure gets. cobra has already printed the
// underlying error (SilenceErrors is left at its default, false), so
// nothing here needs to preserve or re-print its text.
var ErrInterrupted = errors.New("interrupted")

// interruptedError is ErrInterrupted plus the signal that caused it.
type interruptedError struct{ signal int }

func (e *interruptedError) Error() string        { return ErrInterrupted.Error() }
func (e *interruptedError) Is(target error) bool { return target == ErrInterrupted }

// ExitCode maps the error Execute returned to the process exit status: 0
// for nil, 128 plus the signal number for an interrupted run (130 for
// SIGINT, 143 for SIGTERM), and 1 for any other failure.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	if errors.Is(err, ErrInterrupted) {
		var ie *interruptedError
		if errors.As(err, &ie) && ie.signal > 0 {
			return 128 + ie.signal
		}
		return 128 + int(syscall.SIGINT)
	}
	return 1
}

// Execute builds and runs the mtclaw command tree, returning any error so
// main can set the process exit code. See newRootContext for the signal
// handling ctx carries. The store (see state.openStore) is closed here,
// after the whole command tree has run, rather than from a
// PersistentPostRunE: cobra skips PersistentPostRunE entirely when RunE
// returns an error, which would leak the handle on every failing command
// that had opened one.
func Execute() error {
	ctx, stop, lastSignal := newRootContext()
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
	err = finalizeExecuteError(err, ctx.Err())
	if errors.Is(err, ErrInterrupted) {
		return &interruptedError{signal: lastSignal()}
	}
	return err
}

// finalizeExecuteError applies Execute's interrupted-exit-code override: a
// failing command whose failure is explained by the root context itself
// having ended (a signal) is reported as ErrInterrupted instead of its own
// error text, which main.go maps to 128+signal via ExitCode - see
// ErrInterrupted's doc comment. A command that fails for its own reason while ctx is still
// healthy, or one that succeeds despite ctx having ended in the meantime,
// is untouched.
func finalizeExecuteError(err, ctxErr error) error {
	if err != nil && ctxErr != nil {
		return ErrInterrupted
	}
	return err
}

// openStore lazily opens the store backing cfg.Storage (via store.Open's
// driver registry - see the blank import above), reusing the same handle
// across multiple calls within one process. readOnly selects a read-only
// connection for inspection commands (`sessions list`, `sessions show`);
// write commands (`sessions rm`, `prompt`, `cron run`) must pass false. A
// write open also creates the storage directory if missing -
// config.Validate itself never touches the filesystem, so this is the
// only place in a CLI command that does. See Execute for where the
// handle gets closed.
func (s *state) openStore(ctx context.Context, readOnly bool) (store.Store, error) {
	if s.store != nil {
		return s.store, nil
	}
	if s.cfg == nil {
		return nil, fmt.Errorf("open store: config not loaded")
	}
	if readOnly {
		if err := s.requireExistingDatabase(); err != nil {
			return nil, err
		}
	}
	st, err := store.Open(ctx, s.cfg.Storage, readOnly)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	s.store = st
	return s.store, nil
}

// requireExistingDatabase fails when the configured database file has not
// been created yet. Read-only commands need it because there is nothing to
// read; a command that only operates on existing rows (`sessions rm`) needs
// it so a mistyped storage.dsn is reported instead of silently creating a
// new directory tree and an empty database at the wrong place.
func (s *state) requireExistingDatabase() error {
	dsn := s.cfg.Storage.EffectiveDSN()
	if _, err := os.Stat(dsn); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("no database yet at %s; run `mtclaw gateway` or `mtclaw prompt` first", dsn)
	}
	return nil
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
	return telegram.SendOnce(ctx, token, s.cfg.Channels.Telegram.APIBaseURL, chatID, threadID, text)
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

	root.PersistentFlags().StringVar(&s.configFlag, "config", "", "path to the config file (default: $MTCLAW_CONFIG or ~/.mtclaw/config.yaml, falling back to config.yml)")
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
		if !config.IsValidLogLevel(s.logLevelFlag) {
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
