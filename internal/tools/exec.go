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
	"sync"
	"time"

	"github.com/tiennm99/MTClaw/internal/agent"
	"github.com/tiennm99/MTClaw/internal/config"
	"github.com/tiennm99/MTClaw/internal/provider"
	"github.com/tiennm99/MTClaw/internal/store"
)

// execWaitDelay bounds how long cmd.Wait() will wait for the process to
// actually exit after Cancel (trackedProcessTree.kill) has been invoked,
// before giving up and returning anyway. It exists so a process that
// ignores SIGKILL's effect on its pipes (rare, but not impossible) cannot
// hang the tool call forever.
const execWaitDelay = 3 * time.Second

// processTree is a started command's process (and, on Windows, the Job
// Object it was assigned to right after Start) tracked so the whole tree -
// including any child the shell backgrounded - can be killed as a unit. See
// trackProcessTree and the kill method in exec_unix.go and exec_windows.go.
type processTree interface {
	kill()
}

// trackedProcessTree lets a cmd.Cancel callback set up before Start safely
// observe the processTree assigned after Start returns: Start's own
// ctx-watcher goroutine can invoke Cancel concurrently with that
// assignment, so the reference needs a mutex, not a plain variable. kill is
// idempotent (via sync.Once) so it is safe to call from both Cancel and the
// unconditional post-Wait kill below without a double SIGKILL/CloseHandle.
type trackedProcessTree struct {
	mu   sync.Mutex
	tree processTree
	once sync.Once
}

func (t *trackedProcessTree) set(tree processTree) {
	t.mu.Lock()
	t.tree = tree
	t.mu.Unlock()
}

func (t *trackedProcessTree) kill() {
	t.mu.Lock()
	tree := t.tree
	t.mu.Unlock()
	if tree == nil {
		return
	}
	t.once.Do(tree.kill)
}

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
	// them to the model - see filterEnv. This is a display/child-environment
	// mitigation only: it does not stop a same-uid child from reading this
	// process's own environment straight out of /proc/<pid>/environ - see
	// DisableEnvironRead and docs/security.md.
	secretEnvNames []string
}

// defaultOpenAIAPIKeyEnv and defaultTelegramTokenEnv mirror the same
// fallback environment variable names config.Load resolves a secret from
// when openai.api_key_env / channels.telegram.token_env is left empty -
// including an explicit empty override of the onboard-written default, not
// just an absent key. Stripping must use the same fallback: an empty
// *_env field otherwise mistakenly implies "nothing to strip" even though
// the real secret still came from the default variable name.
const (
	defaultOpenAIAPIKeyEnv  = "OPENAI_API_KEY"
	defaultTelegramTokenEnv = "TELEGRAM_BOT_TOKEN"
)

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

	secretEnvNames := []string{
		envNameOrDefault(cfg.OpenAI.APIKeyEnv, defaultOpenAIAPIKeyEnv),
		envNameOrDefault(cfg.Channels.Telegram.TokenEnv, defaultTelegramTokenEnv),
	}

	et := &execTool{
		cfg:            execCfg,
		shell:          ResolveShell(execCfg.Shell),
		policy:         policy,
		approver:       approver,
		audit:          st.Audit(),
		log:            log,
		secretEnvNames: secretEnvNames,
	}
	r.Register("exec", Tool{Spec: execToolSpec(), Run: et.run})
	return nil
}

// envNameOrDefault returns name, or def when name is empty - the same
// fallback config.Load applies when resolving the secret itself, so
// stripping always targets the environment variable name a secret could
// actually have come from.
func envNameOrDefault(name, def string) string {
	if name == "" {
		return def
	}
	return name
}

// ResolveShell returns configured, or the platform default when configured
// is empty: [/bin/bash -lc] on POSIX, [powershell -NoProfile -Command] on
// Windows. The command line is always passed as shell[1:] plus one final
// argument (the raw command string) - it is never tokenized into argv for
// the OS to exec directly, matching how a user would type it at that
// shell's own prompt. Exported so other packages (a `doctor` check
// verifying the configured shell is on PATH) resolve the exact same default
// instead of keeping their own copy that could silently drift from this one.
func ResolveShell(configured []string) []string {
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
	redactedCmd := RedactSecrets(rawCmd)

	decision := e.policy.Evaluate(ctx, rawCmd)

	switch decision.Verdict {
	case VerdictRefuse:
		e.writeAudit(ctx, meta.SessionID, redactedCmd, decision.Audit, decision.Rule, nil, nil, false)
		return fmt.Sprintf("exec: refused permanently by policy (deny rule: %s). This is not retryable by rephrasing the command; tell the user it was blocked and why.", decision.Rule), nil
	case VerdictRun:
		return e.execute(ctx, meta, rawCmd, redactedCmd, decision.Audit, decision.Rule)
	case VerdictAsk:
		return e.ask(ctx, meta, rawCmd, redactedCmd, decision.Reason)
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
//
// reason, unlike rawCmd/redactedCmd, has not been through RedactSecrets or
// any escaping yet - in auto mode it is free text an LLM classifier wrote
// after seeing the raw command (see Policy.evaluateAuto) - so this is the
// one place it is sanitized (see sanitizeReason) before it reaches Request,
// which every approval surface and the approvals table read from.
func (e *execTool) ask(ctx context.Context, meta agent.Meta, rawCmd, redactedCmd, reason string) (string, error) {
	display, ok := displayCommand(redactedCmd)
	if !ok {
		// The command is too long to review in an approval prompt at all -
		// refuse before ever asking, rather than showing a truncated
		// preview a human might approve without seeing the whole thing.
		e.writeAudit(ctx, meta.SessionID, redactedCmd, "refused_too_long", "", nil, nil, false)
		return fmt.Sprintf("exec: command is %d bytes after redaction, too long to review in an approval prompt (limit %d); split it into smaller steps, or write it to a file with write_file and run that file instead", len(redactedCmd), maxDisplayCommandLen), nil
	}

	req := Request{
		SessionID: meta.SessionID,
		Channel:   meta.Channel,
		ChatID:    meta.ChatID,
		ThreadID:  meta.ThreadID,
		Tool:      "exec",
		Command:   display,
		Reason:    sanitizeReason(reason),
		MessageID: meta.MessageID,
	}
	approved, askErr := e.approver.Ask(ctx, req)

	var label, modelMsg string
	switch {
	case askErr != nil:
		// Approver timeout (context.DeadlineExceeded), no interactive
		// approver at all (ErrNoApprover), or the turn's own ctx ending
		// mid-wait: all fail closed the same way here. Registry.Run's
		// uniform "ctx ended after a tool returns (result, nil) still
		// surfaces as a Go error" contract (see registry.go) is what turns
		// this into cancellation propagation when it was really the turn
		// ending, rather than an ordinary approval timeout - ask itself
		// does not need to tell those two apart.
		label = "expired"
		modelMsg = fmt.Sprintf("exec: no approval decision was reached (%v); command refused. Do not retry immediately.", askErr)
	case !approved:
		label = "denied_user"
		modelMsg = "exec: the command was denied by the approver. Do not retry it; explain to the user that it was refused."
	default:
		label = "approved"
	}

	if label != "approved" {
		e.writeAudit(ctx, meta.SessionID, redactedCmd, label, "", nil, nil, false)
		return modelMsg, nil
	}

	return e.execute(ctx, meta, rawCmd, redactedCmd, "approved", "")
}

// execute runs rawCmd under e.shell, bounded by e.cfg.Timeout as an
// additional bound derived from ctx (not a replacement for it). It always
// kills rawCmd's whole process tree once the command finishes, regardless
// of how it finished, so a command that backgrounds a child (`sleep 30 &`)
// never leaves that child running past this call - see trackProcessTree and
// the unconditional call below. A non-zero exit code is a normal result;
// only the caller's own ctx ending mid-run is surfaced as a Go error, so
// the agent loop's cancellation handling can flush and abort the turn
// instead of the exec tool silently swallowing it.
func (e *execTool) execute(ctx context.Context, meta agent.Meta, rawCmd, redactedCmd, decision, rule string) (string, error) {
	runCtx, cancel := context.WithTimeout(ctx, e.cfg.Timeout.Std())
	defer cancel()

	fullArgs := append(append([]string{}, e.shell[1:]...), rawCmd)
	cmd := exec.CommandContext(runCtx, e.shell[0], fullArgs...)
	cmd.Dir = e.cfg.CWD
	cmd.Env = filterEnv(os.Environ(), e.secretEnvNames)
	cmd.WaitDelay = execWaitDelay
	setProcessGroup(cmd)

	// tracked is set from trackProcessTree just below, after Start succeeds.
	// cmd.Cancel must be assigned before Start (os/exec requires it), so it
	// cannot capture the tree directly - it goes through tracked instead,
	// which is safe to read concurrently with the assignment below.
	var tracked trackedProcessTree
	cmd.Cancel = func() error {
		tracked.kill()
		return nil
	}

	// Stdout and Stderr must be assigned the identical *capWriter value, not
	// two separate instances: os/exec special-cases c.Stderr == c.Stdout
	// (interfaceEqual) and runs a single copier goroutine reading one pipe
	// into it instead of two goroutines writing to it concurrently, and
	// Wait always joins that copier before returning - so capWriter.Write
	// itself never needs to be goroutine-safe, and out.buf/out.over are
	// safe to read once cmd.Wait returns. Two distinct writers here would
	// silently reintroduce a concurrent-write race this type does nothing
	// to guard against.
	out := &capWriter{max: e.cfg.MaxOutputBytes}
	cmd.Stdout = out
	cmd.Stderr = out

	start := time.Now()
	runErr := cmd.Start()
	if runErr == nil {
		tracked.set(trackProcessTree(cmd))
		// Start's own ctx-watcher goroutine can invoke cmd.Cancel any time
		// after Start returns, including in the narrow window before the
		// line above runs; if that happened, tracked.kill was a no-op
		// against a still-nil tree. Checking runCtx.Err() again now that
		// the tree is assigned, and killing directly, closes that window -
		// tracked.kill's sync.Once means this never double-kills whether
		// or not cmd.Cancel also fires.
		if runCtx.Err() != nil {
			tracked.kill()
		}
		runErr = cmd.Wait()
	}
	durationMS := time.Since(start).Milliseconds()

	// A background child (e.g. the shell ran `sleep 30 &`) inherits the
	// same stdout/stderr pipe and can keep it open long after the shell
	// itself has exited; cmd.WaitDelay bounds how long Wait waits for that
	// before force-closing the pipe (see the ErrWaitDelay case below), but
	// nothing about a normal, on-time exit stops that child from
	// continuing to run afterward. Killing the whole process tree here,
	// unconditionally, after every return path from Start/Wait - not only
	// on timeout or cancellation, where cmd.Cancel above already does it -
	// is what guarantees a backgrounded child never outlives this call.
	// tracked.kill is a no-op once the tree is already gone, so calling it
	// again when cmd.Cancel already fired costs nothing.
	tracked.kill()

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
		return e.finishExecute(ctx, meta, decision, rule, redactedCmd, 0, durationMS, truncated, output, "")

	case ctx.Err() != nil:
		// The caller's own context ended (turn canceled, or its own
		// deadline elapsed) - checked before the exitErr case below because
		// a process killed by our own SIGKILL/taskkill still surfaces as a
		// normal-looking *exec.ExitError with a non-zero code, which would
		// otherwise be indistinguishable from a genuine non-zero exit.
		// Audit best-effort and propagate so the agent loop's cancellation
		// handling runs; writeAudit already detaches the context it is
		// given from ctx's own cancellation, so ctx itself is passed as-is.
		e.writeAudit(ctx, meta.SessionID, redactedCmd, decision, rule, nil, &durationMS, truncated)
		return "exec: command canceled because the turn ended", ctx.Err()

	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		// Only this call's own tools.exec.timeout fired; the turn itself
		// (ctx) is still alive, so this is a normal result, not an error.
		return e.finishExecute(ctx, meta, decision, rule, redactedCmd, -1, durationMS, truncated, output, "killed after exceeding tools.exec.timeout")

	case errors.Is(runErr, exec.ErrWaitDelay):
		// The shell process itself exited on its own (state.Success(), so
		// cmd.ProcessState.ExitCode() below is always 0 - see os/exec's own
		// Wait: this error only replaces a nil result, never an *ExitError),
		// but a backgrounded child (e.g. `sleep 30 &`) kept stdout/stderr
		// open past execWaitDelay, so cmd.Run reports ErrWaitDelay instead
		// of nil even though the command completed normally. Report it as
		// a normal completion with whatever output was captured before the
		// pipe was force-closed, not a failure - a "failed to run" result
		// with no output would both mislead the model and lose the real
		// result. killProcessTree above already ensures the background
		// child does not outlive this call either way.
		exitCode := 0
		if cmd.ProcessState != nil {
			exitCode = cmd.ProcessState.ExitCode()
		}
		return e.finishExecute(ctx, meta, decision, rule, redactedCmd, exitCode, durationMS, truncated, output, "a background process kept the output pipe open past the command's own exit; any output after that point was discarded")

	case errors.As(runErr, &exitErr):
		return e.finishExecute(ctx, meta, decision, rule, redactedCmd, exitErr.ExitCode(), durationMS, truncated, output, "")

	default:
		e.writeAudit(ctx, meta.SessionID, redactedCmd, decision, rule, nil, &durationMS, truncated)
		return fmt.Sprintf("exec: command failed to run: %v", runErr), nil
	}
}

func (e *execTool) finishExecute(ctx context.Context, meta agent.Meta, decision, rule, redactedCmd string, exitCode int, durationMS int64, truncated bool, output []byte, statusNote string) (string, error) {
	e.writeAudit(ctx, meta.SessionID, redactedCmd, decision, rule, &exitCode, &durationMS, truncated)

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
		Command:    capForAudit(command),
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
