package logging

import (
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tiennm99/MTClaw/internal/config"
)

func TestNew_InvalidLevelFallsBackToInfo(t *testing.T) {
	logger, err := New(config.LogConfig{Level: "not-a-level"})
	require.NoError(t, err)
	assert.False(t, logger.Enabled(nil, slog.LevelDebug), "debug must be filtered out under the info fallback")
	assert.True(t, logger.Enabled(nil, slog.LevelInfo))
}

func TestNew_JSONFormat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.json")
	logger, err := New(config.LogConfig{Level: "info", Format: "json", File: path})
	require.NoError(t, err)
	logger.Info("hello")

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"msg":"hello"`)
}

func TestNew_TextFormatIsDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.txt")
	logger, err := New(config.LogConfig{Level: "info", Format: "text", File: path})
	require.NoError(t, err)
	logger.Info("hello")

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), "msg=hello")
	assert.NotContains(t, string(data), `"msg"`)
}

func TestNew_EmptyFileWritesToStderr(t *testing.T) {
	logger, err := New(config.LogConfig{Level: "info"})
	require.NoError(t, err)
	require.NotNil(t, logger)
}

func TestNew_LogFileIsCreatedWithMode0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits only")
	}
	path := filepath.Join(t.TempDir(), "mtclaw.log")
	_, err := New(config.LogConfig{Level: "info", File: path})
	require.NoError(t, err)

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestNew_LogDirectoryIsCreatedWithMode0700(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits only")
	}
	dir := filepath.Join(t.TempDir(), "nested", "logs")
	path := filepath.Join(dir, "mtclaw.log")
	_, err := New(config.LogConfig{Level: "info", File: path})
	require.NoError(t, err)

	info, err := os.Stat(dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
}

func TestNew_ExistingLogFileIsNarrowedTo0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits only")
	}
	path := filepath.Join(t.TempDir(), "mtclaw.log")
	require.NoError(t, os.WriteFile(path, []byte("stale\n"), 0o644))

	_, err := New(config.LogConfig{Level: "info", File: path})
	require.NoError(t, err)

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "a pre-existing log file must be narrowed, not left at its old mode")
}

func TestNew_UnwritablePathErrors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits only")
	}
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	_, err := New(config.LogConfig{Level: "info", File: filepath.Join(dir, "mtclaw.log")})
	require.Error(t, err)
}

func TestParseLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"debug":   slog.LevelDebug,
		"DEBUG":   slog.LevelDebug,
		"warn":    slog.LevelWarn,
		"warning": slog.LevelWarn,
		"error":   slog.LevelError,
		"info":    slog.LevelInfo,
		"":        slog.LevelInfo,
		"bogus":   slog.LevelInfo,
	}
	for input, want := range cases {
		assert.Equal(t, want, parseLevel(input), "input %q", input)
	}
}
