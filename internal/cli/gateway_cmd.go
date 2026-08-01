package cli

import (
	"fmt"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/tiennm99/MTClaw/internal/gateway"
	"github.com/tiennm99/MTClaw/internal/version"
)

// newGatewayCmd builds `mtclaw gateway`: the long-running process that
// polls Telegram, serializes turns per chat session, and drains cleanly on
// SIGINT/SIGTERM. It is the only command that opens the store for
// sustained writing without the CLI's own openStore helper - internal/
// gateway.New wires its own store, provider, registry, and channel end to
// end, since it also needs the instance lock acquired first.
func newGatewayCmd(s *state) *cobra.Command {
	return &cobra.Command{
		Use:   "gateway",
		Short: "Run the long-running Telegram gateway",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !s.cfg.Channels.Telegram.Enabled {
				return fmt.Errorf("gateway: channels.telegram.enabled is false in %s; the gateway has no channel to serve", s.configPath)
			}

			log := slog.Default()
			logStartupBanner(log, s)

			gw, err := gateway.New(*s.cfg, log)
			if err != nil {
				return err
			}

			return gw.Run(cmd.Context())
		},
	}
}

// logStartupBanner reports what this gateway process is about to do, at
// info level, plus a warn line when exec mode is auto (a beta mode that
// interrupts less often than the default approval mode). The Telegram bot
// username is reported separately by the channel itself once it resolves
// (telegram.Channel.Start's own "bot ready" log line), since it is not
// known until the long-poll loop's first getMe call completes.
func logStartupBanner(log *slog.Logger, s *state) {
	log.Info("mtclaw: starting gateway",
		"version", version.String(),
		"model", s.cfg.Agent.Model,
		"exec_mode", s.cfg.Tools.Exec.Mode,
		"workspace", s.cfg.Agent.Workspace,
		"db_path", s.cfg.Storage.Path,
	)
	if s.cfg.Tools.Exec.Mode == "auto" {
		log.Warn("mtclaw: tools.exec.mode is \"auto\" (beta) - the LLM classifier is not a security control; the deny-list is the only real enforcement boundary")
	}
}
