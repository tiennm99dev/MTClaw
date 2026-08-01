package gateway

import (
	"context"
	"encoding/json"
	"log/slog"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tiennm99/MTClaw/internal/agent"
	"github.com/tiennm99/MTClaw/internal/config"
	"github.com/tiennm99/MTClaw/internal/provider"
	"github.com/tiennm99/MTClaw/internal/provider/mock"
	"github.com/tiennm99/MTClaw/internal/tools"
)

// --- approverMux unit behavior -----------------------------------------

// TestApproverMux_UnrecognizedOrCronChannel_FallsBackToDenyAll pins down
// the routing every cron turn depends on: only req.Channel == "telegram"
// reaches a wired interactive approver; "cron" (and anything else) always
// gets tools.DenyAllApprover, whether or not a Telegram approver has been
// set.
func TestApproverMux_UnrecognizedOrCronChannel_FallsBackToDenyAll(t *testing.T) {
	mux := newApproverMux()

	approved, err := mux.Ask(context.Background(), tools.Request{Channel: "cron", Tool: "exec", Command: "ls"})
	assert.False(t, approved)
	assert.ErrorIs(t, err, tools.ErrNoApprover)

	// Wiring a Telegram approver must not change cron's outcome.
	mux.setTelegram(alwaysApprove{})
	approved, err = mux.Ask(context.Background(), tools.Request{Channel: "cron", Tool: "exec", Command: "ls"})
	assert.False(t, approved)
	assert.ErrorIs(t, err, tools.ErrNoApprover)

	// telegram itself does reach the wired approver.
	approved, err = mux.Ask(context.Background(), tools.Request{Channel: "telegram", Tool: "exec", Command: "ls"})
	require.NoError(t, err)
	assert.True(t, approved)
}

type alwaysApprove struct{}

func (alwaysApprove) Ask(context.Context, tools.Request) (bool, error) { return true, nil }

// --- full-turn integration ------------------------------------------------

// TestCronTurn_ExecApprover_UnmatchedRefused_AllowListedRuns exercises the
// exact path a real cron job takes: a turn against a session whose Channel
// is "cron" reaches the exec tool through a real tools.Registry wired to
// approverMux (the same mux internal/gateway.New builds), and:
//   - an unmatched exec command is refused with the "no interactive
//     approver" message (never reaching a human, since none exists for a
//     scheduled job)
//   - an allow-listed command runs unattended
//
// This is what makes DenyAllApprover's selection-by-channel a real security
// boundary rather than an assumption: it is proven end to end through the
// registry and policy engine, not just at the mux in isolation.
func TestCronTurn_ExecApprover_UnmatchedRefused_AllowListedRuns(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	sess, err := st.Sessions().Ensure(ctx, "cron", "job:test", "")
	require.NoError(t, err)

	cfg := config.Config{
		Version: 1,
		Agent:   config.AgentConfig{Model: "gpt-5", MaxIterations: 5, MaxHistoryTurns: 40},
		Tools: config.ToolsConfig{
			Exec: config.ExecConfig{
				Enabled:         true,
				Mode:            "approval",
				Shell:           testShell(),
				CWD:             t.TempDir(),
				Timeout:         config.Duration(5 * time.Second),
				MaxOutputBytes:  4096,
				ApprovalTimeout: config.Duration(200 * time.Millisecond),
				Allow:           []string{`^echo allow-listed$`},
			},
		},
	}

	mux := newApproverMux() // no Telegram wired: exactly a real cron-only gateway's state
	registry, err := tools.New(cfg, st, mux, discardLogger())
	require.NoError(t, err)

	prov := mock.New(
		mock.Step{ToolCalls: []provider.ToolCall{{ID: "1", Name: "exec", Args: json.RawMessage(`{"command":"rm -rf /tmp/whatever"}`)}}},
		mock.Step{ToolCalls: []provider.ToolCall{{ID: "2", Name: "exec", Args: json.RawMessage(`{"command":"echo allow-listed"}`)}}},
		mock.Step{Content: "done"},
	)

	loop := agent.New(cfg, prov, st, registry, discardLogger())
	result := loop.Run(ctx, sess.ID, "run the job", "", nil)
	require.NoError(t, result.Err)
	assert.Equal(t, "done", result.Text)

	msgs, err := st.Messages().Recent(ctx, sess.ID, 0)
	require.NoError(t, err)

	var toolTexts []string
	for _, m := range msgs {
		if m.Role == "tool" {
			toolTexts = append(toolTexts, m.Content)
		}
	}
	require.Len(t, toolTexts, 2, "both tool calls must have produced a result message")
	assert.Contains(t, toolTexts[0], "no interactive approver is available for this turn",
		"the unmatched command must be refused because no interactive approver exists for a cron turn")
	assert.Contains(t, toolTexts[1], "exit_code: 0", "the allow-listed command must actually run")
}

// testShell returns the platform-appropriate default shell, mirroring
// internal/tools' own resolveShell so this test's exec calls behave the
// same on Windows and POSIX CI.
func testShell() []string {
	if runtime.GOOS == "windows" {
		return []string{"powershell", "-NoProfile", "-Command"}
	}
	return []string{"/bin/bash", "-lc"}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(discardWriter{}, nil))
}
