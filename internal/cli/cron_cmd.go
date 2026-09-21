package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/adhocore/gronx"
	"github.com/spf13/cobra"

	"github.com/tiennm99/MTClaw/internal/config"
	"github.com/tiennm99/MTClaw/internal/gateway"
	"github.com/tiennm99/MTClaw/internal/store"
	"github.com/tiennm99/MTClaw/internal/tools"
)

// newCronCmd builds `mtclaw cron`: read-only inspection (list) and a
// one-shot manual fire (run), both operating on the same config and store a
// running gateway would use, but never through one - the gateway is the
// only long-running consumer of internal/cron.Scheduler.
func newCronCmd(s *state) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cron",
		Short: "Inspect and manually fire scheduled cron jobs",
	}
	cmd.AddCommand(newCronListCmd(s))
	cmd.AddCommand(newCronRunCmd(s))
	return cmd
}

// newCronListCmd opens the store read-only, same rationale as `sessions
// list`: WAL lets it run safely alongside a live gateway.
func newCronListCmd(s *state) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List configured cron jobs, their schedule, and last run",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			loc, err := time.LoadLocation(s.cfg.Cron.Timezone)
			if err != nil {
				return fmt.Errorf("cron.timezone: %w", err)
			}

			st, err := s.openStore(ctx, true)
			if err != nil {
				return err
			}

			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tSCHEDULE\tENABLED\tNEXT DUE\tLAST STATUS\tLAST RUN")
			for _, job := range s.cfg.Cron.Jobs {
				nextDue := "-"
				if job.Enabled {
					if next, err := gronx.NextTickAfter(job.Schedule, time.Now().In(loc), false); err != nil {
						nextDue = fmt.Sprintf("error: %v", err)
					} else {
						nextDue = next.Format("2006-01-02 15:04:05 MST")
					}
				}

				lastStatus, lastRun := "-", "-"
				runs, err := st.CronRuns().List(ctx, job.Name, 1)
				if err != nil {
					return fmt.Errorf("list runs for job %s: %w", job.Name, err)
				}
				if len(runs) > 0 {
					lastStatus = runs[0].Status
					lastRun = runs[0].StartedAt.In(loc).Format("2006-01-02 15:04:05 MST")
				}

				fmt.Fprintf(w, "%s\t%s\t%t\t%s\t%s\t%s\n", job.Name, job.Schedule, job.Enabled, nextDue, lastStatus, lastRun)
			}
			return w.Flush()
		},
	}
}

// newCronRunCmd fires one job immediately, in-process against the same
// store a gateway would use - but not through a running gateway. Default
// is print-only so experimenting with a job is safe; --deliver opts into
// actually sending the result. A persistent job (and one not overridden by
// --ephemeral) refuses to run while the gateway lock is held: nothing
// serializes two processes appending to the same session, and the gateway
// may fire this exact job concurrently.
func newCronRunCmd(s *state) *cobra.Command {
	var deliverFlag bool
	var ephemeralFlag bool

	cmd := &cobra.Command{
		Use:   "run <name>",
		Short: "Fire one cron job immediately, in-process",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			name := args[0]

			job := findCronJob(s.cfg.Cron.Jobs, name)
			if job == nil {
				return fmt.Errorf("cron job %q not found in %s", name, s.configPath)
			}

			ephemeral := ephemeralFlag || job.Session == "ephemeral"
			if !ephemeral {
				if err := refuseIfGatewayLocked(name); err != nil {
					return err
				}
			}

			st, err := s.openStore(ctx, false)
			if err != nil {
				return err
			}

			loop, err := s.newLoop(st, newCronRunApprover())
			if err != nil {
				return err
			}

			threadID := ""
			if ephemeral {
				threadID = fmt.Sprintf("run-%d", time.Now().UnixNano())
			}
			sess, err := st.Sessions().Ensure(ctx, "cron", "job:"+job.Name, threadID)
			if err != nil {
				return fmt.Errorf("ensure session: %w", err)
			}
			if ephemeral {
				defer func() {
					if err := st.Sessions().Delete(context.Background(), sess.ID); err != nil {
						fmt.Fprintf(cmd.ErrOrStderr(), "warning: delete ephemeral session %s: %v\n", sess.ID, err)
					}
				}()
			}

			result := loop.Run(ctx, sess.ID, job.Prompt, "", nil)

			recordManualRun(ctx, cmd, st.CronRuns(), job.Name, sess.ID, result.Err)

			if result.Err != nil {
				return fmt.Errorf("job %q failed: %w", name, result.Err)
			}
			if result.NoReply {
				_, err := fmt.Fprintln(cmd.OutOrStdout(), "(no reply)")
				return err
			}

			if !deliverFlag {
				_, err := fmt.Fprintln(cmd.OutOrStdout(), result.Text)
				return err
			}
			return deliverCronResult(cmd, s, job.DeliverTo, result.Text)
		},
	}

	cmd.Flags().BoolVar(&deliverFlag, "deliver", false, "actually send the result to deliver_to instead of printing it")
	cmd.Flags().BoolVar(&ephemeralFlag, "ephemeral", false, "force a fresh session for this run instead of the job's configured persistent one")

	return cmd
}

// newCronRunApprover builds the approver `cron run` wires into its loop:
// DenyAllApprover, matching exactly what a real gateway's approver mux
// picks for channel "cron" (see internal/gateway/approver.go), so `cron
// run` cannot approve anything a scheduled fire could not. Extracted to its
// own function so cron_cmd_test.go can assert this wiring directly, without
// running a whole turn.
func newCronRunApprover() tools.DenyAllApprover {
	return tools.DenyAllApprover{}
}

// findCronJob returns a pointer into jobs matching name, or nil.
func findCronJob(jobs []config.CronJob, name string) *config.CronJob {
	for i := range jobs {
		if jobs[i].Name == name {
			return &jobs[i]
		}
	}
	return nil
}

// refuseIfGatewayLocked errors out, naming the blocking pid, if a gateway
// instance currently holds the lock file - see gateway.Held's doc comment
// for why a persistent job's manual run must not proceed concurrently with
// one.
func refuseIfGatewayLocked(jobName string) error {
	stateDir, err := config.StateDir()
	if err != nil {
		return fmt.Errorf("resolve state directory: %w", err)
	}
	lockPath := filepath.Join(stateDir, "gateway.lock")
	pid, held, err := gateway.Held(lockPath)
	if err != nil {
		return fmt.Errorf("check gateway lock: %w", err)
	}
	if held {
		return fmt.Errorf("gateway (pid %d) is running and holds job %q's persistent session; pass --ephemeral or stop the gateway first", pid, jobName)
	}
	return nil
}

// recordManualRun writes one cron_runs row for a `cron run` invocation,
// mirroring the scheduler's own started->finished bookkeeping but
// collapsed into a single Append (there is no concurrent tick to race
// against here). A store failure is a warning, not a command failure: the
// turn itself already ran to completion (or was cancelled) by the time this
// is called. It records under context.WithoutCancel(ctx) - same rationale
// as the ephemeral-session cleanup above - so a Ctrl-C that cancelled ctx
// mid-turn does not also cancel the write that records how the turn ended;
// otherwise an interrupted run would vanish from cron_runs with no trace.
func recordManualRun(ctx context.Context, cmd *cobra.Command, runs store.CronRunStore, jobName, sessionID string, turnErr error) {
	status, errMsg := "ok", ""
	if turnErr != nil {
		status, errMsg = "error", turnErr.Error()
	}
	now := time.Now()
	run := &store.CronRun{JobName: jobName, SessionID: sessionID, Status: status, Error: errMsg, StartedAt: now, FinishedAt: &now}
	if err := runs.Append(context.WithoutCancel(ctx), run); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: record cron run: %v\n", err)
	}
}

// deliverCronResult sends text to d over the same one-shot Telegram path
// `mtclaw send` uses - `cron run --deliver` does not go through a gateway,
// so there is no channel to hand it to.
func deliverCronResult(cmd *cobra.Command, s *state, d config.CronDeliverTo, text string) error {
	if d.Channel != "telegram" {
		return fmt.Errorf("deliver_to.channel %q is not supported", d.Channel)
	}
	if err := s.sendTelegram(cmd.Context(), d.ChatID, "", text); err != nil {
		return fmt.Errorf("deliver result: %w", err)
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "delivered to chat %s\n", d.ChatID)
	return err
}
