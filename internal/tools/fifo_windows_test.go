//go:build windows

package tools

import "testing"

// requireFIFO skips the test on Windows, which has no mkfifo equivalent -
// see fifo_unix_test.go for the real implementation.
func requireFIFO(t *testing.T, _ string) {
	t.Helper()
	t.Skip("mkfifo is POSIX-only")
}
