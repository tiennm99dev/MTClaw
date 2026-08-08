package tools

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"

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
