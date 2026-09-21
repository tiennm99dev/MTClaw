package gateway

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// lockFileName is the instance lock's file name inside the state directory
// (see internal/config.StateDir). Acquire and Held (platform-specific: see
// lock_unix.go and lock_windows.go) are what actually decide whether the
// lock is held.
const lockFileName = "gateway.lock"

// readLockPID reads and parses the PID recorded in a lock file at path, for
// naming the holder in an error message only. On unix, the kernel lock
// (syscall.Flock), not this file's content, is what Acquire and Held
// actually trust to decide whether the lock is held - a missing, empty, or
// corrupt file here just means the error message omits the pid.
func readLockPID(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, fmt.Errorf("gateway: lock file %s does not contain a valid pid: %w", path, err)
	}
	return pid, nil
}

// writeLockPID truncates the already-open file f and writes the current
// process's PID into it, purely so a blocked caller's error message (or
// `mtclaw doctor`) can name who holds the lock.
func writeLockPID(f *os.File) error {
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.WriteAt([]byte(strconv.Itoa(os.Getpid())), 0); err != nil {
		return err
	}
	return nil
}
