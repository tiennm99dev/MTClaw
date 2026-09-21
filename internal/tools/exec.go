package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/tiennm99/MTClaw/internal/agent"
	"github.com/tiennm99/MTClaw/internal/config"
	"github.com/tiennm99/MTClaw/internal/provider"
	"github.com/tiennm99/MTClaw/internal/store"
)

// execWaitDelay bounds how long cmd.Wait() will wait for the process to
// actually exit after Cancel (killProcessTree) has been invoked, before
// giving up and returning anyway. It exists so a process that ignores
// SIGKILL's effect on its pipes (rare, but not impossible) cannot hang the
// tool call forever.
const execWaitDelay = 3 * time.Second

func execToolSpec() provider.ToolSpec {
	return provider.ToolSpec{
		Name:        "exec",
		Description: "Run a shell command under the configured policy: deny-listed commands are refused permanently, allow-listed commands run immediately, and everything else is gated by the configured mode (approval prompt, or an auto-mode risk classifier). A non-zero exit code is a normal result, not a failure.",
		Schema:      objectSchema(map[string]any{"command": stringProp("the shell command line to run")}, "command"),
	}
}

type execTool struct {
	cfg      config.ExecConfig
	shell    []string
	policy   *Policy
	approver Approver
	audit    store.AuditStore
	log      *slog.Logger

	// secretEnvNames lists the environment variable names this process
	// resolved its own secrets from (the OpenAI API key, the Telegram bot
	// token); execute strips them from the spawned child's environment so a
	// command cannot read them back out via `env` or `echo $VAR` and hand
	// them to the model - see filterEnv.
	secretEnvNames []string
}

func registerExecTool(r *Registry, cfg config.Config, st store.Store, approver Approver, log *slog.Logger) error {
	execCfg := cfg.Tools.Exec

	var classifier Classifier
	if execCfg.Mode == "auto" {
		model := execCfg.Auto.Model
		if model == "" {
			model = cfg.Agent.Model
		}
		c, err := NewLLMClassifier(cfg.OpenAI, model)
		if err != nil {
			return fmt.Errorf("tools: registering exec tool: %w", err)
		}
		classifier = c
	}

	policy, err := NewPolicy(execCfg, classifier)
	if err != nil {
		return fmt.Errorf("tools: registering exec tool: %w", err)
	}

	var secretEnvNames []string
	if cfg.OpenAI.APIKeyEnv != "" {
		secretEnvNames = append(secretEnvNames, cfg.OpenAI.APIKeyEnv)
	}
	if cfg.Channels.Telegram.TokenEnv != "" {
		secretEnvNames = append(secretEnvNames, cfg.Channels.Telegram.TokenEnv)
	}

	et := &execTool{
		cfg:            execCfg,
		shell:          resolveShell(execCfg.Shell),
		policy:         policy,
		approver:       approver,
		audit:          st.Audit(),
		log:            log,
		secretEnvNames: secretEnvNames,
	}
	r.Register("exec", Tool{Spec: execToolSpec(), Run: et.run})
	return nil
}

// resolveShell returns configured, or the platform default when configured
// is empty: [/bin/bash -lc] on POSIX, [powershell -NoProfile -Command] on
// Windows. The command line is always passed as shell[1:] plus one final
// argument (the raw command string) - it is never tokenized into argv for
// the OS to exec directly, matching how a user would type it at that
// shell's own prompt.
func resolveShell(configured []string) []string {
	if len(configured) > 0 {
		return configured
	}
	if runtime.GOOS == "windows" {
		return []string{"powershell", "-NoProfile", "-Command"}
	}
	return []string{"/bin/bash", "-lc"}
}

type execArgs struct {
	Command string `json:"command"`
}

// run is the exec tool's entry point: Policy.Evaluate -> act on the verdict.
// The raw command string is what a real shell would receive and what the
// deny/allow regexes are written against, so it goes to Evaluate unmodified
// and first - nothing runs ahead of the deny-list. It never returns a Go
// error except when the outer ctx itself ends mid-command or
// mid-approval-wait, in which case the agent loop's own cancellation
// handling must run.
func (e *execTool) run(ctx context.Context, args json.RawMessage, meta agent.Meta) (string, error) {
	var a execArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return fmt.Sprintf("exec: invalid arguments: %v", err), nil
	}
	rawCmd := strings.TrimSpace(a.Command)
	if rawCmd == "" {
		return "exec: command must not be empty", nil
	}
	displayCmd := RedactSecrets(rawCmd)

	decision := e.policy.Evaluate(ctx, rawCmd)

	switch decision.Verdict {
	case VerdictRefuse:
		e.writeAudit(ctx, meta.SessionID, displayCmd, decision.Audit, decision.Rule, nil, nil, false)
		return fmt.Sprintf("exec: refused permanently by policy (deny rule: %s). This is not retryable by rephrasing the command; tell the user it was blocked and why.", decision.Rule), nil
	case VerdictRun:
		return e.execute(ctx, meta, rawCmd, displayCmd, decision.Audit, decision.Rule)
	case VerdictAsk:
		return e.ask(ctx, meta, rawCmd, displayCmd, decision.Reason)
	default:
		return "exec: internal policy error: unrecognized verdict", nil
	}
}

// ask waits on the approver, then records the resulting exec_audit row
// itself (execute records its own row on the approved path, so ask never
// double-writes). The approvals table row is owned by the Approver
// implementation, not by exec: the Telegram approver must create its own row
// (the row id is its inline-button callback payload), and creating a second
// one here would double-book every prompt. Terminal and deny-all approvals
// keep no approvals row at all - exec_audit is their durable record.
func (e *execTool) ask(ctx context.Context, meta agent.Meta, rawCmd, displayCmd, reason string) (string, error) {
	req := Request{
		SessionID: meta.SessionID,
		Channel:   meta.Channel,
		ChatID:    meta.ChatID,
		ThreadID:  meta.ThreadID,
		Tool:      "exec",
		Command:   displayCmd,
		Reason:    reason,
		MessageID: meta.MessageID,
	}
	approved, askErr := e.approver.Ask(ctx, req)

	if errors.Is(askErr, context.Canceled) {
		// The caller's own ctx ended (typically the turn being canceled),
		// not the approver's timeout: propagate so the agent loop's
		// cancellation path runs, same as a canceled exec run would.
		e.writeAudit(ctx, meta.SessionID, displayCmd, "expired", "", nil, nil, false)
		return "exec: approval wait canceled because the turn ended", ctx.Err()
	}

	var label, modelMsg string
	switch {
	case askErr != nil:
		// Approver timeout (context.DeadlineExceeded) or no interactive
		// approver at all (ErrNoApprover): both fail closed the same way.
		label = "expired"
		modelMsg = fmt.Sprintf("exec: no approval decision was reached (%v); command refused. Do not retry immediately.", askErr)
	case !approved:
		label = "denied_user"
		modelMsg = "exec: the command was denied by the approver. Do not retry it; explain to the user that it was refused."
	default:
		label = "approved"
	}

	if label != "approved" {
		e.writeAudit(ctx, meta.SessionID, displayCmd, label, "", nil, nil, false)
		return modelMsg, nil
	}

	return e.execute(ctx, meta, rawCmd, displayCmd, "approved", "")
}

// execute runs rawCmd under e.shell, bounded by e.cfg.Timeout as an
// additional bound derived from ctx (not a replacement for it), and kills
// the whole process tree on either bound firing. A non-zero exit code is a
// normal result; only the caller's own ctx ending mid-run is surfaced as a
// Go error, so the agent loop's cancellation handling can flush and abort
// the turn instead of the exec tool silently swallowing it.
func (e *execTool) execute(ctx context.Context, meta agent.Meta, rawCmd, displayCmd, decision, rule string) (string, error) {
	runCtx, cancel := context.WithTimeout(ctx, e.cfg.Timeout.Std())
	defer cancel()

	fullArgs := append(append([]string{}, e.shell[1:]...), rawCmd)
	cmd := exec.CommandContext(runCtx, e.shell[0], fullArgs...)
	cmd.Dir = e.cfg.CWD
	cmd.Env = filterEnv(os.Environ(), e.secretEnvNames)
	cmd.WaitDelay = execWaitDelay
	setProcessGroup(cmd)
	cmd.Cancel = func() error {
		killProcessTree(cmd)
		return nil
	}

	// Stdout and Stderr must be assigned the identical *capWriter value, not
	// two separate instances: os/exec special-cases c.Stderr == c.Stdout
	// (interfaceEqual) and runs a single copier goroutine reading one pipe
	// into it instead of two goroutines writing to it concurrently, and
	// Wait always joins that copier before returning - so capWriter.Write
	// itself never needs to be goroutine-safe, and out.buf/out.over are
	// safe to read once cmd.Run returns. Two distinct writers here would
	// silently reintroduce a concurrent-write race this type does nothing
	// to guard against.
	out := &capWriter{max: e.cfg.MaxOutputBytes}
	cmd.Stdout = out
	cmd.Stderr = out

	start := time.Now()
	runErr := cmd.Run()
	durationMS := time.Since(start).Milliseconds()

	output := out.buf.Bytes()
	truncated := out.over
	if truncated {
		// The cap above cut at a raw byte count with no regard for UTF-8
		// boundaries; trim back to the last complete rune so a truncated
		// multi-byte character is never split in the stored/displayed output.
		output = output[:runeSafeLen(output)]
	}

	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
		return e.finishExecute(ctx, meta, decision, rule, displayCmd, 0, durationMS, truncated, output, "")

	case ctx.Err() != nil:
		// The caller's own context ended (turn canceled, or its own
		// deadline elapsed) - checked before the exitErr case below because
		// a process killed by our own SIGKILL/taskkill still surfaces as a
		// normal-looking *exec.ExitError with a non-zero code, which would
		// otherwise be indistinguishable from a genuine non-zero exit.
		// Audit best-effort and propagate so the agent loop's cancellation
		// handling runs.
		e.writeAudit(context.WithoutCancel(ctx), meta.SessionID, displayCmd, decision, rule, nil, &durationMS, truncated)
		return "exec: command canceled because the turn ended", ctx.Err()

	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		// Only this call's own tools.exec.timeout fired; the turn itself
		// (ctx) is still alive, so this is a normal result, not an error.
		return e.finishExecute(ctx, meta, decision, rule, displayCmd, -1, durationMS, truncated, output, "killed after exceeding tools.exec.timeout")

	case errors.As(runErr, &exitErr):
		return e.finishExecute(ctx, meta, decision, rule, displayCmd, exitErr.ExitCode(), durationMS, truncated, output, "")

	default:
		e.writeAudit(ctx, meta.SessionID, displayCmd, decision, rule, nil, &durationMS, truncated)
		return fmt.Sprintf("exec: command failed to run: %v", runErr), nil
	}
}

func (e *execTool) finishExecute(ctx context.Context, meta agent.Meta, decision, rule, displayCmd string, exitCode int, durationMS int64, truncated bool, output []byte, statusNote string) (string, error) {
	e.writeAudit(ctx, meta.SessionID, displayCmd, decision, rule, &exitCode, &durationMS, truncated)

	var b strings.Builder
	fmt.Fprintf(&b, "exit_code: %d\nduration_ms: %d\n", exitCode, durationMS)
	if statusNote != "" {
		fmt.Fprintf(&b, "status: %s\n", statusNote)
	}
	if truncated {
		fmt.Fprintf(&b, "[output truncated to %d bytes]\n", e.cfg.MaxOutputBytes)
	}
	b.WriteString("output:\n")
	b.Write(output)
	return b.String(), nil
}

// writeAudit appends one exec_audit row on a detached context (so a turn
// cancellation in progress does not also lose the audit write) and only
// logs, never fails the tool call, on a store error: an audit gap must
// never turn into a user-visible tool failure or a second decision.
func (e *execTool) writeAudit(ctx context.Context, sessionID, command, decision, rule string, exitCode *int, durationMS *int64, truncated bool) {
	if e.audit == nil {
		return
	}
	bg := context.WithoutCancel(ctx)
	row := &store.ExecAudit{
		SessionID:  sessionID,
		Command:    command,
		CWD:        e.cfg.CWD,
		Decision:   decision,
		Rule:       rule,
		ExitCode:   exitCode,
		DurationMS: durationMS,
		Truncated:  truncated,
	}
	if err := e.audit.Append(bg, row); err != nil {
		e.log.Error("tools: append exec_audit row failed", "session_id", sessionID, "decision", decision, "error", err)
	}
}

// capWriter bounds how much of a running command's output is kept in
// memory: unlike truncating a fully-buffered bytes.Buffer after the command
// exits, it discards past max at write time, so a command that emits
// gigabytes before its timeout fires (yes, cat /dev/urandom, a runaway log
// tail) cannot grow this process's memory past max. It always reports a
// full-length write (never a short write or an error) so the child is never
// blocked or killed by what would otherwise look like a broken pipe.
type capWriter struct {
	buf  bytes.Buffer
	max  int
	over bool
}

func (w *capWriter) Write(p []byte) (int, error) {
	if room := w.max - w.buf.Len(); room > 0 {
		if len(p) > room {
			w.buf.Write(p[:room])
			w.over = true
		} else {
			w.buf.Write(p)
		}
	} else if len(p) > 0 {
		w.over = true
	}
	return len(p), nil
}

// filterEnv returns environ with every variable named in strip removed, so
// a spawned command inherits this process's environment minus the secrets
// (API keys, bot tokens) it was configured to read - a command cannot hand
// them to the model via `env` or `echo $VAR` if they were never in its
// environment to begin with. Names are compared case-insensitively on
// Windows, where environment variable names are themselves
// case-insensitive (api_key_env: openai_api_key must still strip a real
// OPENAI_API_KEY= entry there); POSIX environments are case-sensitive, so
// the comparison stays exact everywhere else.
func filterEnv(environ, strip []string) []string {
	if len(strip) == 0 {
		return environ
	}
	skip := make(map[string]bool, len(strip))
	for _, name := range strip {
		if runtime.GOOS == "windows" {
			name = strings.ToUpper(name)
		}
		skip[name] = true
	}
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if runtime.GOOS == "windows" {
			name = strings.ToUpper(name)
		}
		if !skip[name] {
			out = append(out, kv)
		}
	}
	return out
}
