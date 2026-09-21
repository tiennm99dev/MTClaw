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
// gateway cannot import internal/cli.
func (s *state) newLoop(st store.Store, approver tools.Approver) (*agent.Loop, error) {
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

// prepare resolves the config path for every command (cheap, no I/O beyond
// os.UserHomeDir) and, unless the command is exempt, loads and validates
// the config file and installs the resulting logger as slog's default.
func (s *state) prepare(cmd *cobra.Command) error {
	path, source, err := config.ConfigPath(s.configFlag)
	if err != nil {
		return err
	}
	s.configPath, s.configSource = path, source

	if skipsConfigLoad(cmd) {
		return nil
	}

	cfg, err := config.LoadFile(path)
	if err != nil {
		return err
	}
	if s.logLevelFlag != "" {
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
	if isReadOnlyCommand(cmd) {
		logCfg.File = ""
	}
	logger, err := logging.New(logCfg)
	if err != nil {
		return err
	}
	slog.SetDefault(logger)

	return nil
}

// skipsConfigLoad reports whether cmd must work with a broken or absent
// config file: `mtclaw version` and `mtclaw config path` always did;
// `mtclaw onboard` (phase 9) exists specifically to write a config that
// does not exist yet, and `mtclaw doctor` loads the config itself so a
// parse/validation failure becomes a diagnosable FAIL row instead of a
// command that cannot even start.
func skipsConfigLoad(cmd *cobra.Command) bool {
	switch cmd.CommandPath() {
	case "mtclaw version", "mtclaw config path", "mtclaw onboard", "mtclaw doctor":
		return true
	default:
		return false
	}
}

// isReadOnlyCommand reports whether cmd only inspects existing state
// (config, sessions, approvals, cron list) rather than running the agent
// loop, sending a message, or otherwise doing work worth a persistent log
// file for. `version` and `doctor` are also read-only but already skip
// config loading entirely (skipsConfigLoad), so they never reach the
// logger construction this guards.
func isReadOnlyCommand(cmd *cobra.Command) bool {
	switch cmd.CommandPath() {
	case "mtclaw config show", "mtclaw config validate",
		"mtclaw sessions list", "mtclaw sessions show", "mtclaw sessions rm",
		"mtclaw approvals list", "mtclaw cron list":
		return true
	default:
		return false
	}
}
