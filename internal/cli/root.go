// Package cli wires cobra commands to the config loader and logger. It is
// the only package main.go calls into.
package cli

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/tiennm99/MTClaw/internal/config"
	"github.com/tiennm99/MTClaw/internal/logging"
	"github.com/tiennm99/MTClaw/internal/store"
	"github.com/tiennm99/MTClaw/internal/store/sqlite"
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

// Execute builds and runs the mtclaw command tree, returning any error so
// main can set the process exit code. The store (see state.openStore) is
// closed here, after the whole command tree has run, rather than from a
// PersistentPostRunE: cobra skips PersistentPostRunE entirely when RunE
// returns an error, which would leak the handle on every failing command
// that had opened one.
func Execute() error {
	s := &state{}
	err := newRootCmd(s).Execute()
	if closeErr := s.closeStore(); closeErr != nil && err == nil {
		err = closeErr
	}
	return err
}

// openStore lazily opens the sqlite store backing cfg.Storage.Path,
// reusing the same handle across multiple calls within one process.
// readOnly selects a read-only connection for inspection commands
// (`sessions list`, `sessions show`); write commands (`sessions rm` today;
// `prompt` and `cron run` in later phases) must pass false. See
// Execute for where the handle gets closed.
func (s *state) openStore(ctx context.Context, readOnly bool) (store.Store, error) {
	if s.store != nil {
		return s.store, nil
	}
	if s.cfg == nil {
		return nil, fmt.Errorf("open store: config not loaded")
	}
	db, err := sqlite.Open(ctx, s.cfg.Storage.Path, readOnly)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	s.store = sqlite.New(db)
	return s.store, nil
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

	logger, err := logging.New(cfg.Log)
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
