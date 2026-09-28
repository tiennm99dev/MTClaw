package tools

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tiennm99/MTClaw/internal/agent"
	"github.com/tiennm99/MTClaw/internal/config"
	"github.com/tiennm99/MTClaw/internal/provider"
	"github.com/tiennm99/MTClaw/internal/store"
	// This package's own exec_test.go already carries the blank import
	// that registers "sqlite" with store.Open's driver registry.
)

func TestRegistry_Run_UnknownToolReturnsResultStringNotError(t *testing.T) {
	r := NewRegistry()
	out, err := r.Run(context.Background(), provider.ToolCall{Name: "does_not_exist"}, agent.Meta{})
	require.NoError(t, err)
	assert.Contains(t, out, "unknown tool")
}

// TestRegistry_Run_CtxEndedAfterToolCompletesSurfacesAsError proves the one
// centralized rule every ToolFunc relies on instead of re-implementing its
// own ctx check: even when a tool's own Run returns a normal (result, nil),
// ctx having already ended by the time it does - canceled, or its deadline
// elapsed, while the call was still in flight - still surfaces as a Go
// error from Run, so the agent loop's cancellation handling always runs on
// a dead context.
func TestRegistry_Run_CtxEndedAfterToolCompletesSurfacesAsError(t *testing.T) {
	r := NewRegistry()
	r.Register("noop", Tool{Spec: provider.ToolSpec{Name: "noop"}, Run: func(_ context.Context, _ json.RawMessage, _ agent.Meta) (string, error) {
		return "done", nil
	}})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	out, err := r.Run(ctx, provider.ToolCall{Name: "noop"}, agent.Meta{})
	assert.Equal(t, "done", out, "the tool's own result must still be returned")
	assert.ErrorIs(t, err, context.Canceled)
}

// TestRegistry_Run_ToolOwnErrorIsNeverOverriddenByCtx proves the reverse
// side of the same rule: a tool that fails for its own reason keeps that
// error untouched, even when ctx also happens to have ended.
func TestRegistry_Run_ToolOwnErrorIsNeverOverriddenByCtx(t *testing.T) {
	wantErr := errors.New("boom")
	r := NewRegistry()
	r.Register("failer", Tool{Spec: provider.ToolSpec{Name: "failer"}, Run: func(_ context.Context, _ json.RawMessage, _ agent.Meta) (string, error) {
		return "", wantErr
	}})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := r.Run(ctx, provider.ToolCall{Name: "failer"}, agent.Meta{})
	assert.ErrorIs(t, err, wantErr)
}

// TestRegistry_Run_LiveCtxDoesNotAlterANormalResult proves the centralized
// check is genuinely conditional on ctx having ended, not a blanket
// override: an ordinary, uncanceled call is unaffected.
func TestRegistry_Run_LiveCtxDoesNotAlterANormalResult(t *testing.T) {
	r := NewRegistry()
	r.Register("noop", Tool{Spec: provider.ToolSpec{Name: "noop"}, Run: func(_ context.Context, _ json.RawMessage, _ agent.Meta) (string, error) {
		return "done", nil
	}})

	out, err := r.Run(context.Background(), provider.ToolCall{Name: "noop"}, agent.Meta{})
	require.NoError(t, err)
	assert.Equal(t, "done", out)
}

func TestRegistry_Specs_StableRegistrationOrder(t *testing.T) {
	r := NewRegistry()
	r.Register("b", Tool{Spec: provider.ToolSpec{Name: "b"}})
	r.Register("a", Tool{Spec: provider.ToolSpec{Name: "a"}})

	specs := r.Specs()
	require.Len(t, specs, 2)
	assert.Equal(t, "b", specs[0].Name)
	assert.Equal(t, "a", specs[1].Name)
}

func newTestFullConfig(t *testing.T) config.Config {
	t.Helper()
	root := t.TempDir()
	cfg := *config.Default()
	cfg.Agent.Model = "test-model"
	cfg.Tools.Filesystem.Roots = []string{root}
	cfg.Tools.Filesystem.MaxReadBytes = 1024
	cfg.Tools.Filesystem.MaxWriteBytes = 1024
	cfg.Tools.WebFetch.MaxBytes = 1024
	cfg.Tools.Exec.CWD = root
	return cfg
}

func newTestStoreForRegistry(t *testing.T) store.Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(context.Background(), config.StorageConfig{Driver: "sqlite", DSN: dbPath}, false)
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestNew_ExecDisabled_ExecToolNotRegistered(t *testing.T) {
	cfg := newTestFullConfig(t)
	cfg.Tools.Exec.Enabled = false
	cfg.Tools.Exec.Mode = "approval"
	st := newTestStoreForRegistry(t)

	r, err := New(cfg, st, nil, slog.Default())
	require.NoError(t, err)

	for _, s := range r.Specs() {
		assert.NotEqual(t, "exec", s.Name)
	}
}

func TestNew_ApprovalMode_RegistersExecTool(t *testing.T) {
	cfg := newTestFullConfig(t)
	cfg.Tools.Exec.Mode = "approval"
	st := newTestStoreForRegistry(t)

	r, err := New(cfg, st, nil, slog.Default())
	require.NoError(t, err)

	found := false
	for _, s := range r.Specs() {
		if s.Name == "exec" {
			found = true
		}
	}
	assert.True(t, found)
}

// TestNew_NilApproverFallsBackToDenyAll proves registry.New's documented
// fallback: forgetting to wire an approver must fail safe (every unmatched
// exec command is refused, not silently run) rather than panic on first
// use.
func TestNew_NilApproverFallsBackToDenyAll(t *testing.T) {
	cfg := newTestFullConfig(t)
	cfg.Tools.Exec.Mode = "approval"
	st := newTestStoreForRegistry(t)
	sess, err := st.Sessions().Ensure(context.Background(), "cli", "local", "")
	require.NoError(t, err)

	r, err := New(cfg, st, nil, slog.Default())
	require.NoError(t, err)

	out, err := r.Run(context.Background(), provider.ToolCall{Name: "exec", Args: []byte(`{"command":"echo hi"}`)}, agent.Meta{SessionID: sess.ID})
	require.NoError(t, err)
	assert.Contains(t, out, "no approval decision was reached")
}

// TestNew_ExecToolStripsConfiguredSecretEnvNamesFromChild proves the wiring
// from config to execTool.secretEnvNames, not just the field: building the
// exec tool through registerExecTool (via New, exactly as production code
// does) with openai.api_key_env and channels.telegram.token_env naming real
// environment variables must result in a child command that cannot read
// either one back out.
func TestNew_ExecToolStripsConfiguredSecretEnvNamesFromChild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix shell $VAR expansion assumed")
	}
	t.Setenv("MTCLAW_TEST_OPENAI_KEY", "openai-secret-value")
	t.Setenv("MTCLAW_TEST_TG_TOKEN", "telegram-secret-value")

	cfg := newTestFullConfig(t)
	cfg.Tools.Exec.Mode = "approval"
	cfg.Tools.Exec.Allow = []string{".*"}
	cfg.OpenAI.APIKeyEnv = "MTCLAW_TEST_OPENAI_KEY"
	cfg.Channels.Telegram.TokenEnv = "MTCLAW_TEST_TG_TOKEN"
	st := newTestStoreForRegistry(t)
	sess, err := st.Sessions().Ensure(context.Background(), "cli", "local", "")
	require.NoError(t, err)

	r, err := New(cfg, st, nil, slog.Default())
	require.NoError(t, err)

	out, err := r.Run(context.Background(), provider.ToolCall{
		Name: "exec",
		Args: []byte(`{"command":"echo [$MTCLAW_TEST_OPENAI_KEY] [$MTCLAW_TEST_TG_TOKEN]"}`),
	}, agent.Meta{SessionID: sess.ID})
	require.NoError(t, err)
	assert.NotContains(t, out, "openai-secret-value")
	assert.NotContains(t, out, "telegram-secret-value")
	assert.Contains(t, out, "[] []")
}

// TestNew_ExecToolStripsDefaultSecretEnvNamesWhenConfigLeavesThemEmpty
// proves the default environment variable names (OPENAI_API_KEY,
// TELEGRAM_BOT_TOKEN) are still stripped from a spawned command's
// environment even when openai.api_key_env / channels.telegram.token_env
// are explicitly left empty - matching config's own fallback to those same
// default names when resolving the secret in the first place. Stripping
// only non-empty *_env values would otherwise leave the real secret
// readable whenever a config explicitly overrides *_env to "".
func TestNew_ExecToolStripsDefaultSecretEnvNamesWhenConfigLeavesThemEmpty(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix shell $VAR expansion assumed")
	}
	t.Setenv("OPENAI_API_KEY", "default-openai-secret")
	t.Setenv("TELEGRAM_BOT_TOKEN", "default-telegram-secret")

	cfg := newTestFullConfig(t)
	cfg.Tools.Exec.Mode = "approval"
	cfg.Tools.Exec.Allow = []string{".*"}
	cfg.OpenAI.APIKeyEnv = ""
	cfg.Channels.Telegram.TokenEnv = ""
	st := newTestStoreForRegistry(t)
	sess, err := st.Sessions().Ensure(context.Background(), "cli", "local", "")
	require.NoError(t, err)

	r, err := New(cfg, st, nil, slog.Default())
	require.NoError(t, err)

	out, err := r.Run(context.Background(), provider.ToolCall{
		Name: "exec",
		Args: []byte(`{"command":"echo [$OPENAI_API_KEY] [$TELEGRAM_BOT_TOKEN]"}`),
	}, agent.Meta{SessionID: sess.ID})
	require.NoError(t, err)
	assert.NotContains(t, out, "default-openai-secret")
	assert.NotContains(t, out, "default-telegram-secret")
	assert.Contains(t, out, "[] []")
}

// blockingApprover blocks until ctx ends and returns ctx.Err(), simulating
// an approver (like TerminalApprover) that still has no answer when the
// turn's own ctx ends - whether by explicit cancellation or by the turn's
// own deadline elapsing, as opposed to the approver's own separate timeout.
type blockingApprover struct{}

func (blockingApprover) Ask(ctx context.Context, _ Request) (bool, error) {
	<-ctx.Done()
	return false, ctx.Err()
}

// TestRegistry_Run_TurnDeadlineDuringApprovalSurfacesAsGoError proves the
// exec tool's approval wait no longer needs to tell context.Canceled and
// context.DeadlineExceeded apart itself: whichever way ctx ends while
// waiting on the approver, Registry.Run's uniform "ctx ended after a normal
// (result, nil) return" check (see registry.go) turns it into a Go error,
// so the agent loop's cancellation handling runs instead of continuing on a
// dead context.
func TestRegistry_Run_TurnDeadlineDuringApprovalSurfacesAsGoError(t *testing.T) {
	cfg := newTestFullConfig(t)
	cfg.Tools.Exec.Mode = "approval"
	st := newTestStoreForRegistry(t)
	sess, err := st.Sessions().Ensure(context.Background(), "cli", "local", "")
	require.NoError(t, err)

	r, err := New(cfg, st, blockingApprover{}, slog.Default())
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, runErr := r.Run(ctx, provider.ToolCall{Name: "exec", Args: []byte(`{"command":"echo hi"}`)}, agent.Meta{SessionID: sess.ID})
	assert.ErrorIs(t, runErr, context.DeadlineExceeded)
}

func TestNew_FilesystemDisabled_NoFSTools(t *testing.T) {
	cfg := newTestFullConfig(t)
	cfg.Tools.Filesystem.Enabled = false
	cfg.Tools.WebFetch.Enabled = false
	cfg.Tools.Exec.Enabled = false
	st := newTestStoreForRegistry(t)

	r, err := New(cfg, st, nil, slog.Default())
	require.NoError(t, err)
	assert.Empty(t, r.Specs())
}

func TestNew_WebFetchDisabled_NotRegistered(t *testing.T) {
	cfg := newTestFullConfig(t)
	cfg.Tools.WebFetch.Enabled = false
	cfg.Tools.Exec.Enabled = false
	st := newTestStoreForRegistry(t)

	r, err := New(cfg, st, nil, slog.Default())
	require.NoError(t, err)
	for _, s := range r.Specs() {
		assert.NotEqual(t, "web_fetch", s.Name)
	}
}
