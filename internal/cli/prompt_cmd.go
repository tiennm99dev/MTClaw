package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/spf13/cobra"

	"github.com/tiennm99/MTClaw/internal/agent"
	"github.com/tiennm99/MTClaw/internal/provider/openai"
	"github.com/tiennm99/MTClaw/internal/store"
	"github.com/tiennm99/MTClaw/internal/tools"
)

// newPromptCmd builds `mtclaw prompt "<text>"`: a terminal-only turn of the
// agent loop against a persistent cli/local session, so a user (and CI) can
// exercise the full loop without any channel.
func newPromptCmd(s *state) *cobra.Command {
	var sessionFlag string
	var newFlag bool

	cmd := &cobra.Command{
		Use:   "prompt <text>",
		Short: "Run one turn of the agent loop from the terminal",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			st, err := s.openStore(ctx, false)
			if err != nil {
				return err
			}

			sessionID, err := resolveCLISession(ctx, st, sessionFlag, newFlag)
			if err != nil {
				return err
			}

			client, err := openai.New(s.cfg.OpenAI)
			if err != nil {
				return fmt.Errorf("build openai client: %w", err)
			}

			approver := tools.NewTerminalApprover(cmd.InOrStdin(), cmd.ErrOrStderr(), s.cfg.Tools.Exec.ApprovalTimeout.Std())
			registry, err := tools.New(*s.cfg, st, approver, slog.Default())
			if err != nil {
				return fmt.Errorf("build tool registry: %w", err)
			}

			loop := agent.New(*s.cfg, client, st, registry, slog.Default())

			result := loop.Run(ctx, sessionID, args[0], "", progressPrinter(cmd.ErrOrStderr()))
			if result.Err != nil {
				return fmt.Errorf("agent turn failed: %w", result.Err)
			}

			if !result.NoReply {
				if _, err := fmt.Fprintln(cmd.OutOrStdout(), result.Text); err != nil {
					return err
				}
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&sessionFlag, "session", "", "reuse a specific session id instead of the default cli/local session")
	cmd.Flags().BoolVar(&newFlag, "new", false, "force a fresh session instead of reusing the default cli/local one")

	return cmd
}

// resolveCLISession returns the session id `prompt` should run its turn
// against. --session reuses an existing session verbatim (erroring if it
// does not exist, rather than silently creating one under a typo'd id).
// Otherwise it ensures the default channel="cli" chat="local" session so
// repeated `mtclaw prompt` invocations share history across restarts,
// unless --new asks for a genuinely fresh one: SessionStore has no
// "always create" method, so --new forces a fresh row by Ensure-ing a
// synthetic, time-unique thread id that can never collide with a prior
// identity triple.
func resolveCLISession(ctx context.Context, st store.Store, sessionFlag string, newFlag bool) (string, error) {
	if sessionFlag != "" {
		sess, err := st.Sessions().Get(ctx, sessionFlag)
		if err != nil {
			return "", fmt.Errorf("session %s: %w", sessionFlag, err)
		}
		return sess.ID, nil
	}

	threadID := ""
	if newFlag {
		threadID = fmt.Sprintf("cli-new-%d", time.Now().UnixNano())
	}
	sess, err := st.Sessions().Ensure(ctx, "cli", "local", threadID)
	if err != nil {
		return "", fmt.Errorf("ensure cli session: %w", err)
	}
	return sess.ID, nil
}

// progressPrinter writes tool activity to w (stderr in normal use) so the
// final answer on stdout stays clean and scriptable.
func progressPrinter(w io.Writer) agent.Progress {
	return func(ev agent.Event) {
		switch ev.Kind {
		case agent.EventIteration:
			fmt.Fprintf(w, "... iteration %d\n", ev.Iteration)
		case agent.EventToolStarted:
			fmt.Fprintf(w, "-> running %s\n", ev.ToolName)
		case agent.EventToolFinished:
			if ev.Err != nil {
				fmt.Fprintf(w, "-> %s failed: %v\n", ev.ToolName, ev.Err)
			} else {
				fmt.Fprintf(w, "-> %s done\n", ev.ToolName)
			}
		}
	}
}
