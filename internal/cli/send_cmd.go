package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/tiennm99/MTClaw/internal/channel/telegram"
)

// newSendCmd builds `mtclaw send --chat <id> "<text>"`: a one-shot outbound
// Telegram message sent by constructing a bot directly, without a gateway,
// a store, or an approver. It exists for scripts and for verifying a
// configured token actually works before running the gateway.
func newSendCmd(s *state) *cobra.Command {
	var chatFlag string
	var threadFlag string

	cmd := &cobra.Command{
		Use:   "send <text>",
		Short: "Send one Telegram message directly, without running the gateway",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if chatFlag == "" {
				return fmt.Errorf("--chat is required")
			}
			token := s.cfg.Channels.Telegram.Token()
			if token == "" {
				return fmt.Errorf("no telegram bot token resolved; set channels.telegram.token_env or channels.telegram.token_file")
			}

			if err := telegram.SendOnce(cmd.Context(), token, s.cfg.Channels.Telegram.APIBaseURL, chatFlag, threadFlag, args[0]); err != nil {
				return fmt.Errorf("send message: %w", err)
			}

			_, err := fmt.Fprintf(cmd.OutOrStdout(), "sent to chat %s\n", chatFlag)
			return err
		},
	}

	cmd.Flags().StringVar(&chatFlag, "chat", "", "target Telegram chat id (required)")
	cmd.Flags().StringVar(&threadFlag, "thread", "", "forum topic message_thread_id, if replying inside a specific topic")
	return cmd
}
