//go:build unix

package config

import (
	"fmt"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestLoad_SecretFileRefusesNonRegularFile proves a *_file secret pointed
// at a FIFO (or any non-regular file) is refused outright, not read from
// indefinitely - the same class of fix as bounding the read length, for a
// file that would otherwise never produce EOF at all.
func TestLoad_SecretFileRefusesNonRegularFile(t *testing.T) {
	baseDir := t.TempDir()
	fifoPath := filepath.Join(baseDir, "openai-key.fifo")
	require.NoError(t, syscall.Mkfifo(fifoPath, 0o600))

	yaml := fmt.Sprintf(`
version: 1
agent:
  model: gpt-5
channels:
  telegram:
    enabled: false
openai:
  api_key_file: %q
`, fifoPath)
	_, err := Load([]byte(yaml), baseDir, map[string]string{})
	require.Error(t, err)
}
