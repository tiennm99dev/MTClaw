package cli

import (
	"errors"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/tiennm99/MTClaw/internal/store"
)

func newSessionsCmd(s *state) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sessions",
		Short: "Inspect and manage stored conversations",
	}
	cmd.AddCommand(newSessionsListCmd(s))
	cmd.AddCommand(newSessionsShowCmd(s))
	cmd.AddCommand(newSessionsRmCmd(s))
	return cmd
}

// newSessionsListCmd opens the store read-only: WAL lets it run safely
// while the gateway is writing.
func newSessionsListCmd(s *state) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List stored sessions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			st, err := s.openStore(ctx, true)
			if err != nil {
				return err
			}

			sessions, err := st.Sessions().List(ctx, 0)
			if err != nil {
				return fmt.Errorf("list sessions: %w", err)
			}

			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tCHANNEL\tCHAT\tTITLE\tMESSAGES\tTOKENS\tUPDATED")
			for _, sess := range sessions {
				n, err := st.Messages().CountBySession(ctx, sess.ID)
				if err != nil {
					return fmt.Errorf("count messages for session %s: %w", sess.ID, err)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%d\t%s\n",
					sess.ID, sess.Channel, sess.ChatID, sess.Title, n,
					sess.PromptTokens+sess.CompletionTokens, sess.UpdatedAt.Local().Format("2006-01-02 15:04:05"),
				)
			}
			return w.Flush()
		},
	}
}

// newSessionsShowCmd opens the store read-only, same rationale as list.
func newSessionsShowCmd(s *state) *cobra.Command {
	return &cobra.Command{
		Use:   "show <id>",
		Short: "Print a session's message transcript",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			id := args[0]
			st, err := s.openStore(ctx, true)
			if err != nil {
				return err
			}

			sess, err := st.Sessions().Get(ctx, id)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return fmt.Errorf("session %s not found", id)
				}
				return fmt.Errorf("show session %s: %w", id, err)
			}

			msgs, err := st.Messages().Recent(ctx, id, 0)
			if err != nil {
				return fmt.Errorf("show session %s: %w", id, err)
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "session %s (%s/%s) created %s\n\n", sess.ID, sess.Channel, sess.ChatID, sess.CreatedAt.Local().Format(time.RFC3339))
			for _, m := range msgs {
				fmt.Fprintf(out, "[%d] %s: %s\n", m.Seq, m.Role, m.Content)
				if m.ToolCalls != "" {
					fmt.Fprintf(out, "    tool_calls: %s\n", m.ToolCalls)
				}
				if m.ToolCallID != "" {
					fmt.Fprintf(out, "    tool_call_id: %s tool_name: %s\n", m.ToolCallID, m.ToolName)
				}
			}
			return nil
		},
	}
}

// newSessionsRmCmd is the one sessions subcommand that writes: it is one
// of the deliberate second writers documented in phase 2's architecture
// (dropping a poisoned session without stopping the gateway), so it opens
// read-write while every other sessions command stays read-only.
func newSessionsRmCmd(s *state) *cobra.Command {
	return &cobra.Command{
		Use:   "rm <id>",
		Short: "Delete a session (cascades messages and approvals; preserves exec_audit)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			id := args[0]
			st, err := s.openStore(ctx, false)
			if err != nil {
				return err
			}

			if err := st.Sessions().Delete(ctx, id); err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return fmt.Errorf("session %s not found", id)
				}
				return fmt.Errorf("delete session %s: %w", id, err)
			}

			_, err = fmt.Fprintf(cmd.OutOrStdout(), "deleted session %s\n", id)
			return err
		},
	}
}
