package tools

import (
	"context"
	"log/slog"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tiennm99/MTClaw/internal/agent"
	"github.com/tiennm99/MTClaw/internal/config"
	"github.com/tiennm99/MTClaw/internal/provider"
	"github.com/tiennm99/MTClaw/internal/store/sqlite"
)

func TestRegistry_Run_UnknownToolReturnsResultStringNotError(t *testing.T) {
	r := NewRegistry()
	out, err := r.Run(context.Background(), provider.ToolCall{Name: "does_not_exist"}, agent.Meta{})
	require.NoError(t, err)
	assert.Contains(t, out, "unknown tool")
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

func newTestStoreForRegistry(t *testing.T) *sqlite.Store {
	t.Helper()
	db, err := sqlite.Open(context.Background(), t.TempDir()+"/test.db", false)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return sqlite.New(db)
}

func TestNew_ModeOff_ExecToolNotRegistered(t *testing.T) {
	cfg := newTestFullConfig(t)
	cfg.Tools.Exec.Mode = "off"
	st := newTestStoreForRegistry(t)

	r, err := New(cfg, st, nil, slog.Default())
	require.NoError(t, err)

	for _, s := range r.Specs() {
		assert.NotEqual(t, "exec", s.Name, "mode: off must not register the exec tool")
	}
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

func TestNew_FilesystemDisabled_NoFSTools(t *testing.T) {
	cfg := newTestFullConfig(t)
	cfg.Tools.Filesystem.Enabled = false
	cfg.Tools.WebFetch.Enabled = false
	cfg.Tools.Exec.Mode = "off"
	st := newTestStoreForRegistry(t)

	r, err := New(cfg, st, nil, slog.Default())
	require.NoError(t, err)
	assert.Empty(t, r.Specs())
}

func TestNew_WebFetchDisabled_NotRegistered(t *testing.T) {
	cfg := newTestFullConfig(t)
	cfg.Tools.WebFetch.Enabled = false
	cfg.Tools.Exec.Mode = "off"
	st := newTestStoreForRegistry(t)

	r, err := New(cfg, st, nil, slog.Default())
	require.NoError(t, err)
	for _, s := range r.Specs() {
		assert.NotEqual(t, "web_fetch", s.Name)
	}
}
