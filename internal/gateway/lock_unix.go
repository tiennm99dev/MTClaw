//go:build !windows

package gateway

import (
	"errors"
	"os"
	"syscall"
)

// processAlive probes whether pid names a running process by sending
// signal 0 (POSIX: no actual signal, just existence/permission checking).
// os.FindProcess always succeeds on POSIX regardless of whether pid exists,
// so the real check is in the Signal call: nil or EPERM means the process
// exists (EPERM: it exists but this user cannot signal it - still alive);
// ESRCH (or any other error) means no such process.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	return errors.Is(err, syscall.EPERM)
}
