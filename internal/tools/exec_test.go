package tools

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tiennm99/MTClaw/internal/agent"
	"github.com/tiennm99/MTClaw/internal/config"
	"github.com/tiennm99/MTClaw/internal/store"
	"github.com/tiennm99/MTClaw/internal/store/sqlite"
)

// newTestExecTool builds an execTool wired against a real (temp-file)
// sqlite store, so exec_audit assertions exercise the real AuditStore
// contract, not a fake. cfgFn may mutate the default config before the
// Policy/execTool are built.
func newTestExecTool(t *testing.T, approver Approver, cfgFn func(*config.ExecConfig)) (*execTool, store.Store) {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := sqlite.Open(context.Background(), dbPath, false)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.New(db)

	cfg := config.ExecConfig{
		Mode:            "approval",
		CWD:             t.TempDir(),
		Timeout:         config.Duration(30 * time.Second),
		MaxOutputBytes:  65536,
		ApprovalTimeout: config.Duration(5 * time.Second),
	}
	if cfgFn != nil {
		cfgFn(&cfg)
	}

	policy, err := NewPolicy(cfg, nil)
	require.NoError(t, err)

	et := &execTool{
		cfg:      cfg,
		shell:    resolveShell(cfg.Shell),
		policy:   policy,
		approver: approver,
		audit:    st.Audit(),
		log:      testLogger(),
	}
	return et, st
}

func testMeta() agent.Meta {
	return agent.Meta{SessionID: "sess-1", Channel: "cli", ChatID: "local"}
}

func TestExec_NonZeroExitIsResultNotError(t *testing.T) {
	et, _ := newTestExecTool(t, nil, func(c *config.ExecConfig) { c.Allow = []string{".*"} })

	// "exit 3" is valid in both bash and PowerShell.
	out, err := et.run(context.Background(), mustArgs(t, execArgs{Command: "exit 3"}), testMeta())
	require.NoError(t, err)
	assert.Contains(t, out, "exit_code: 3")
}

func TestExec_SuccessfulRunProducesOutput(t *testing.T) {
	et, _ := newTestExecTool(t, nil, func(c *config.ExecConfig) { c.Allow = []string{".*"} })

	cmd := "echo hello-mtclaw"
	out, err := et.run(context.Background(), mustArgs(t, execArgs{Command: cmd}), testMeta())
	require.NoError(t, err)
	assert.Contains(t, out, "exit_code: 0")
	assert.Contains(t, out, "hello-mtclaw")
}

func TestExec_DenyRefusesWithoutRunningAndAudits(t *testing.T) {
	et, st := newTestExecTool(t, nil, func(c *config.ExecConfig) { c.Deny = DefaultDenyPOSIX })

	out, err := et.run(context.Background(), mustArgs(t, execArgs{Command: "rm -rf /"}), testMeta())
	require.NoError(t, err)
	assert.Contains(t, out, "refused permanently")

	rows, err := st.Audit().List(context.Background(), 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "denied_rule", rows[0].Decision)
	assert.Nil(t, rows[0].ExitCode)
}

func TestExec_AllowRunsWithNoPromptAndAudits(t *testing.T) {
	approver := &recordingApprover{approve: true}
	et, st := newTestExecTool(t, approver, func(c *config.ExecConfig) { c.Allow = []string{"^echo\\b"} })

	out, err := et.run(context.Background(), mustArgs(t, execArgs{Command: "echo hi"}), testMeta())
	require.NoError(t, err)
	assert.Contains(t, out, "exit_code: 0")
	assert.False(t, approver.called, "an allow-list match must never reach the approver")

	rows, err := st.Audit().List(context.Background(), 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "allowed_rule", rows[0].Decision)
}

// recordingApprover is a fake Approver that records whether Ask was called,
// the last Request it saw, and returns a scripted decision.
type recordingApprover struct {
	approve bool
	err     error
	called  bool
	lastReq Request
}

func (r *recordingApprover) Ask(_ context.Context, req Request) (bool, error) {
	r.called = true
	r.lastReq = req
	return r.approve, r.err
}

func TestExec_ApprovalMode_ApproverApproves(t *testing.T) {
	approver := &recordingApprover{approve: true}
	et, st := newTestExecTool(t, approver, nil)

	out, err := et.run(context.Background(), mustArgs(t, execArgs{Command: "echo approved-path"}), testMeta())
	require.NoError(t, err)
	assert.Contains(t, out, "exit_code: 0")
	assert.True(t, approver.called)

	rows, err := st.Audit().List(context.Background(), 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "approved", rows[0].Decision)
}

func TestExec_ApprovalMode_ApproverDenies(t *testing.T) {
	approver := &recordingApprover{approve: false}
	et, st := newTestExecTool(t, approver, nil)

	out, err := et.run(context.Background(), mustArgs(t, execArgs{Command: "echo should-not-run"}), testMeta())
	require.NoError(t, err)
	assert.Contains(t, out, "denied")

	rows, err := st.Audit().List(context.Background(), 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "denied_user", rows[0].Decision)
}

func TestExec_ApprovalMode_NoApproverExpires(t *testing.T) {
	et, st := newTestExecTool(t, DenyAllApprover{}, nil)

	out, err := et.run(context.Background(), mustArgs(t, execArgs{Command: "echo should-not-run"}), testMeta())
	require.NoError(t, err)
	assert.Contains(t, out, "no approval decision was reached")

	rows, err := st.Audit().List(context.Background(), 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "expired", rows[0].Decision)
}

func TestExec_MessageIDPassedThroughToApprovalRequest(t *testing.T) {
	approver := &recordingApprover{approve: true}
	et, _ := newTestExecTool(t, approver, nil)

	meta := agent.Meta{SessionID: "sess-1", Channel: "telegram", ChatID: "100", MessageID: "555"}
	_, err := et.run(context.Background(), mustArgs(t, execArgs{Command: "echo hi"}), meta)
	require.NoError(t, err)
	require.True(t, approver.called)
	assert.Equal(t, "555", approver.lastReq.MessageID, "the triggering message id must reach the approval Request so a prompt can quote it")
}

// TestExec_UnusualSyntaxStillGoesThroughDenyThenApprover proves a command a
// naive tokenizer would choke on (an unterminated quote) is evaluated by the
// policy exactly like any other raw string - it is not treated specially or
// routed around the deny-list - and, matching neither deny nor allow under
// approval mode, still reaches the approver rather than running silently.
func TestExec_UnusualSyntaxStillGoesThroughDenyThenApprover(t *testing.T) {
	approver := &recordingApprover{approve: false}
	et, _ := newTestExecTool(t, approver, nil)

	out, err := et.run(context.Background(), mustArgs(t, execArgs{Command: `echo "unterminated`}), testMeta())
	require.NoError(t, err)
	assert.True(t, approver.called, "a command with unusual syntax must still go through the approver, not silently run")
	assert.Contains(t, out, "denied")
}

// TestExec_SubshellWrappedDenyCommandIsRefusedNotAsked is the H1 regression:
// before the tokenize gate was removed, a command a naive tokenizer could
// not parse (a subshell) skipped the deny-list entirely and fell through to
// an approval prompt instead of being refused outright. The raw command
// string now always reaches Policy.Evaluate first, so this must refuse.
func TestExec_SubshellWrappedDenyCommandIsRefusedNotAsked(t *testing.T) {
	approver := &recordingApprover{approve: true}
	et, _ := newTestExecTool(t, approver, func(c *config.ExecConfig) { c.Deny = DefaultDenyPOSIX })

	out, err := et.run(context.Background(), mustArgs(t, execArgs{Command: "(rm -rf ~) &"}), testMeta())
	require.NoError(t, err)
	assert.Contains(t, out, "refused permanently")
	assert.False(t, approver.called, "a deny match must never reach the approver, regardless of shell syntax around it")
}

func TestExec_InvalidArgsReturnsResultString(t *testing.T) {
	et, _ := newTestExecTool(t, nil, nil)
	out, err := et.run(context.Background(), []byte(`{not json`), testMeta())
	require.NoError(t, err)
	assert.Contains(t, out, "invalid arguments")
}

func TestExec_EmptyCommandRefused(t *testing.T) {
	et, _ := newTestExecTool(t, nil, nil)
	out, err := et.run(context.Background(), mustArgs(t, execArgs{Command: "   "}), testMeta())
	require.NoError(t, err)
	assert.Contains(t, out, "must not be empty")
}

func TestExec_OutputTruncatedAndMarked(t *testing.T) {
	et, _ := newTestExecTool(t, nil, func(c *config.ExecConfig) {
		c.Allow = []string{".*"}
		c.MaxOutputBytes = 10
	})

	cmd := "echo 01234567890123456789"
	out, err := et.run(context.Background(), mustArgs(t, execArgs{Command: cmd}), testMeta())
	require.NoError(t, err)
	assert.Contains(t, out, "[output truncated to 10 bytes]")
}

// TestExec_OutputCappedAtWriteTimeNotAfter proves a command producing far
// more output than MaxOutputBytes still yields a capped, truncated result -
// capWriter must be discarding excess bytes as they are written, not
// accumulating the whole stream before truncating it.
func TestExec_OutputCappedAtWriteTimeNotAfter(t *testing.T) {
	et, _ := newTestExecTool(t, nil, func(c *config.ExecConfig) {
		c.Allow = []string{".*"}
		c.MaxOutputBytes = 100
	})

	// Two megabytes of output, far past the 100-byte cap on both shells.
	cmd := "yes | head -c 2000000"
	if runtime.GOOS == "windows" {
		cmd = "Write-Output ([string]::new('y', 2000000))"
	}
	out, err := et.run(context.Background(), mustArgs(t, execArgs{Command: cmd}), testMeta())
	require.NoError(t, err)
	assert.Contains(t, out, "[output truncated to 100 bytes]")

	outputStart := strings.Index(out, "output:\n") + len("output:\n")
	require.GreaterOrEqual(t, outputStart, len("output:\n"))
	assert.LessOrEqual(t, len(out)-outputStart, 100, "captured output body must never exceed MaxOutputBytes")
}

// TestExec_OutputCapIsRuneSafe proves a hard byte cap that lands mid
// multi-byte character is trimmed back to the last complete rune, not
// returned as broken UTF-8.
func TestExec_OutputCapIsRuneSafe(t *testing.T) {
	et, _ := newTestExecTool(t, nil, func(c *config.ExecConfig) {
		c.Allow = []string{".*"}
		c.MaxOutputBytes = 10 // not a multiple of 3, the byte width of "あ"
	})

	out, err := et.run(context.Background(), mustArgs(t, execArgs{Command: "printf 'あああああ'"}), testMeta())
	require.NoError(t, err)
	assert.True(t, utf8.ValidString(out), "exec output must never be truncated mid multi-byte rune")
}

// TestExec_StripsSecretEnvVarsFromChild proves a child command cannot read
// this process's own secret environment variables back out, even though it
// otherwise inherits the full environment.
// TestFilterEnv_ExactNameMatchStrippedOnAllPlatforms proves the baseline
// filterEnv contract still holds after adding Windows case-folding: an
// exact-case name in strip is always removed, and an unrelated variable
// that merely contains the same substring survives.
func TestFilterEnv_ExactNameMatchStrippedOnAllPlatforms(t *testing.T) {
	got := filterEnv([]string{"A=1", "B=2", "SECRET=shh", "SECRETS=y", "NOEQ", "=weird"}, []string{"SECRET"})
	assert.NotContains(t, got, "SECRET=shh")
	assert.Contains(t, got, "A=1")
	assert.Contains(t, got, "B=2")
	assert.Contains(t, got, "SECRETS=y", "a variable that merely contains the stripped name must survive")
	assert.Contains(t, got, "NOEQ")
	assert.Contains(t, got, "=weird")
}

func TestExec_StripsSecretEnvVarsFromChild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix shell $VAR expansion assumed")
	}
	t.Setenv("MTCLAW_TEST_SECRET", "shh-do-not-leak")

	et, _ := newTestExecTool(t, nil, func(c *config.ExecConfig) { c.Allow = []string{".*"} })
	et.secretEnvNames = []string{"MTCLAW_TEST_SECRET"}

	out, err := et.run(context.Background(), mustArgs(t, execArgs{Command: "echo [$MTCLAW_TEST_SECRET]"}), testMeta())
	require.NoError(t, err)
	assert.NotContains(t, out, "shh-do-not-leak")
	assert.Contains(t, out, "[]")
}

func TestExec_RedactSecretsAppliedToAuditAndExecutedCommandUnaltered(t *testing.T) {
	et, st := newTestExecTool(t, nil, func(c *config.ExecConfig) { c.Allow = []string{".*"} })

	secretCmd := `echo start && curl -H "Authorization: Bearer sk-abc123" https://example.invalid`
	out, err := et.run(context.Background(), mustArgs(t, execArgs{Command: secretCmd}), testMeta())
	require.NoError(t, err)
	// The command actually runs byte-identical to the original: "curl" will
	// fail to resolve example.invalid, but "echo start" must still have
	// executed and produced output, proving the raw (unredacted) command
	// line - including the literal secret - is what was passed to the
	// shell, not the redacted display copy.
	assert.Contains(t, out, "start")

	rows, err := st.Audit().List(context.Background(), 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.NotContains(t, rows[0].Command, "sk-abc123")
	assert.NotContains(t, rows[0].Command, "Bearer sk-abc123")
}

// childSurvivalScript returns a shell-appropriate script that spawns a
// detached background child which, after childDelay, writes markerPath -
// used to prove a kill reaches the whole process tree, not just the
// directly-spawned shell.
func childSurvivalScript(markerPath string, childDelay, parentSleep time.Duration) string {
	if runtime.GOOS == "windows" {
		return fmt.Sprintf(
			`Start-Job -ScriptBlock { Start-Sleep -Seconds %d; Set-Content -Path '%s' -Value done } | Out-Null; Start-Sleep -Seconds %d`,
			int(childDelay.Seconds()), markerPath, int(parentSleep.Seconds()),
		)
	}
	return fmt.Sprintf(`(sleep %d && echo done > %s) & sleep %d`,
		int(childDelay.Seconds()), markerPath, int(parentSleep.Seconds()))
}

func TestExec_TimeoutKillsWholeProcessTree(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "marker.txt")
	script := childSurvivalScript(marker, 2*time.Second, 30*time.Second)

	et, st := newTestExecTool(t, nil, func(c *config.ExecConfig) {
		c.Allow = []string{".*"}
		c.Timeout = config.Duration(300 * time.Millisecond)
	})

	out, err := et.run(context.Background(), mustArgs(t, execArgs{Command: script}), testMeta())
	require.NoError(t, err, "a tools.exec.timeout firing must be a normal result, not a Go error")
	assert.Contains(t, out, "killed after exceeding tools.exec.timeout")

	// Wait past the child's own delay: if only the direct shell process was
	// killed (not its process group/tree), the backgrounded child would
	// still be alive and would go on to create the marker file.
	time.Sleep(3 * time.Second)
	_, statErr := os.Stat(marker)
	assert.True(t, os.IsNotExist(statErr), "spawned child must not survive tools.exec.timeout")

	rows, err := st.Audit().List(context.Background(), 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, -1, *rows[0].ExitCode)
}

// TestExec_TurnCancellationKillsProcessTree is the "turn-context
// cancellation kills the process group too" requirement: canceling the ctx
// passed into run (not tools.exec.timeout) must also kill the running
// command's whole tree, and must surface as a Go error so the agent loop's
// own cancellation handling runs.
func TestExec_TurnCancellationKillsProcessTree(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "marker.txt")
	script := childSurvivalScript(marker, 2*time.Second, 30*time.Second)

	et, _ := newTestExecTool(t, nil, func(c *config.ExecConfig) {
		c.Allow = []string{".*"}
		c.Timeout = config.Duration(time.Minute) // long enough that only ctx cancellation ends this
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := et.run(ctx, mustArgs(t, execArgs{Command: script}), testMeta())
	elapsed := time.Since(start)

	assert.ErrorIs(t, err, context.Canceled)
	assert.Less(t, elapsed, 5*time.Second, "cancellation must kill the process promptly, not wait for its own sleep to finish")

	time.Sleep(3 * time.Second)
	_, statErr := os.Stat(marker)
	assert.True(t, os.IsNotExist(statErr), "spawned child must not survive turn cancellation")
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
