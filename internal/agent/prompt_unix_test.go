//go:build unix

package agent

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBuild_FIFOPromptFile_SkippedWithoutBlocking proves a system_prompt_
// files entry pointing at a FIFO with no writer connected is skipped
// instead of hanging the turn forever: Stat sees it is not a regular file
// and Open is never called, so this test would hang past `go test`'s own
// deadline rather than fail an assertion if the fix regressed.
func TestBuild_FIFOPromptFile_SkippedWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "prompt.fifo")
	require.NoError(t, syscall.Mkfifo(fifo, 0o600))

	cfg := testAgentConfig()
	cfg.Agent.SystemPromptFiles = []string{fifo}

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	out := Build(cfg, nil, time.Now(), logger)

	assert.NotContains(t, out, "prompt.fifo", "a non-regular prompt file must not appear in the assembled prompt")
	assert.Contains(t, logBuf.String(), "not a regular file", "a FIFO prompt file must log a warning naming the reason")
	assert.Contains(t, logBuf.String(), fifo)
}

// TestBuild_SymlinkToDeviceFile_SkippedWithoutUnboundedRead proves a
// system_prompt_files entry that resolves (via symlink) to a character
// device is rejected before any read, instead of reading until OOM.
func TestBuild_SymlinkToDeviceFile_SkippedWithoutUnboundedRead(t *testing.T) {
	if _, err := os.Stat("/dev/zero"); err != nil {
		t.Skipf("no /dev/zero on this system: %v", err)
	}

	dir := t.TempDir()
	link := filepath.Join(dir, "prompt-link")
	require.NoError(t, os.Symlink("/dev/zero", link))

	cfg := testAgentConfig()
	cfg.Agent.SystemPromptFiles = []string{link}

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	out := Build(cfg, nil, time.Now(), logger)

	assert.NotContains(t, out, "prompt-link", "a symlink to a device file must not appear in the assembled prompt")
	assert.Contains(t, logBuf.String(), "not a regular file", "a device-backed prompt file must log a warning naming the reason")
	assert.Contains(t, logBuf.String(), link)
}
