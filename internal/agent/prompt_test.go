package agent

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tiennm99/MTClaw/internal/config"
	"github.com/tiennm99/MTClaw/internal/provider"
)

func testAgentConfig() config.Config {
	cfg := *config.Default()
	cfg.Agent.Name = "TestBot"
	cfg.Agent.Model = "test-model"
	cfg.Agent.MaxIterations = 5
	cfg.Agent.MaxHistoryTurns = 10
	cfg.Agent.Workspace = filepath.FromSlash("/workspace")
	cfg.Tools.Filesystem.Roots = []string{filepath.FromSlash("/workspace")}
	cfg.Tools.Exec.Mode = "approval"
	return cfg
}

func TestBuild_ToolLinesMatchRegistrySpecs(t *testing.T) {
	cfg := testAgentConfig()
	tools := []provider.ToolSpec{
		{Name: "exec", Description: "Run a shell command"},
		{Name: "fs_read", Description: "Read a file within the workspace"},
	}

	out := Build(cfg, tools, time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC), slog.Default())

	for _, spec := range tools {
		assert.Contains(t, out, spec.Name)
		assert.Contains(t, out, spec.Description)
	}
	assert.Contains(t, out, "TestBot")
	assert.Contains(t, out, cfg.Agent.Workspace)
	assert.Contains(t, out, `"approval"`)
	assert.Contains(t, out, "NO_REPLY")
}

func TestBuild_NoToolsRegistered(t *testing.T) {
	cfg := testAgentConfig()
	out := Build(cfg, nil, time.Now(), slog.Default())
	assert.Contains(t, out, "No tools are registered")
}

func TestBuild_MissingPromptFileLogsAndContinues(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "AGENTS.md")
	require.NoError(t, os.WriteFile(present, []byte("custom instructions here"), 0o644))
	missing := filepath.Join(dir, "does-not-exist.md")

	cfg := testAgentConfig()
	cfg.Agent.SystemPromptFiles = []string{present, missing}

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	out := Build(cfg, nil, time.Now(), logger)

	assert.Contains(t, out, "custom instructions here", "the readable prompt file must still be included")
	assert.NotContains(t, out, "does-not-exist.md", "a missing prompt file must not appear in the assembled prompt")
	assert.Contains(t, logBuf.String(), "unreadable", "a missing prompt file must log a warning")
	assert.Contains(t, logBuf.String(), missing)
}
