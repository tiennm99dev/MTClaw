//go:build !windows

package tools

import (
	"syscall"
	"testing"
)

// requireFIFO creates a FIFO at path, failing the test immediately if that
// is not possible. Used by fs_test.go to exercise read_file/write_file
// against a non-regular file - see fifo_windows_test.go for the
// counterpart on a platform with no mkfifo equivalent.
func requireFIFO(t *testing.T, path string) {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("mkfifo %s: %v", path, err)
	}
}
