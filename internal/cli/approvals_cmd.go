package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// newApprovalsCmd groups read-only inspection of exec policy decisions.
// There is deliberately no `approvals decide` (or similar) subcommand: a
// pending approval's waiting goroutine lives in the gateway process's
// memory (see internal/tools.TerminalApprover / the phase 6 Telegram
// approver), not in a store row a separate CLI invocation could reach.
// Deciding one from a second process would need an IPC channel to the
// running gateway that v1 does not have; approve or deny from whichever
// surface asked (the terminal prompt for `mtclaw prompt`, inline buttons
// for Telegram).
func newApprovalsCmd(s *state) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "approvals",
		Short: "Inspect exec policy decisions",
	}
	cmd.AddCommand(newApprovalsListCmd(s))
	return cmd
}

// newApprovalsListCmd opens the store read-only, same rationale as
// `sessions list`: it is safe to run alongside a live gateway under WAL.
func newApprovalsListCmd(s *state) *cobra.Command {
	var limit int

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List recent exec_audit rows: every command the policy engine decided on",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			st, err := s.openStore(ctx, true)
			if err != nil {
				return err
			}

			rows, err := st.Audit().List(ctx, limit)
			if err != nil {
				return fmt.Errorf("list exec audit: %w", err)
			}

			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "TIME\tSESSION\tDECISION\tDECIDER\tEXIT\tDURATION_MS\tTRUNC\tCOMMAND")
			for _, row := range rows {
				exit := "-"
				if row.ExitCode != nil {
					exit = fmt.Sprintf("%d", *row.ExitCode)
				}
				dur := "-"
				if row.DurationMS != nil {
					dur = fmt.Sprintf("%d", *row.DurationMS)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%v\t%s\n",
					row.CreatedAt.Local().Format("2006-01-02 15:04:05"),
					row.SessionID, row.Decision, decider(row.Decision), exit, dur, row.Truncated, row.Command,
				)
			}
			return w.Flush()
		},
	}

	cmd.Flags().IntVar(&limit, "limit", 50, "maximum number of rows to show (0 = no limit)")
	return cmd
}

// decider labels who made the call for a given exec_audit.decision value -
// exec_audit itself has no decided_by column (that belongs to the ephemeral
// approvals table), so this is a display-only categorization, not a stored
// fact.
func decider(decision string) string {
	switch decision {
	case "denied_rule", "allowed_rule":
		return "policy"
	case "auto_allowed":
		return "classifier"
	case "approved", "denied_user":
		return "user"
	case "expired":
		return "timeout"
	default:
		return "unknown"
	}
}
