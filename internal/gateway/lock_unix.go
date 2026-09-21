//go:build !windows

package gateway

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// Acquire opens (creating if necessary) path and takes an exclusive,
// kernel-enforced advisory lock on it via flock(2): self-releasing if this
// process dies or is killed, so no stale-lock heuristic is needed - unlike
// a create-and-probe-a-PID scheme, there is no window where a second
// starter reads a not-yet-written or corrupt PID and deletes a live
// winner's lock. The PID is still written into the file, but purely to name
// the holder in an error message; Acquire itself never reads it back to
// decide anything.
func Acquire(path string) (release func() error, err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("gateway: open lock file %s: %w", path, err)
	}

	if flockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); flockErr != nil {
		pid, _ := readLockPID(path)
		f.Close()
		if pid > 0 {
			return nil, fmt.Errorf("gateway: another instance is already running (pid %d); remove %s if this is wrong", pid, path)
		}
		return nil, fmt.Errorf("gateway: another instance is already running; remove %s if this is wrong", path)
	}

	if err := writeLockPID(f); err != nil {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
		return nil, fmt.Errorf("gateway: write lock file %s: %w", path, err)
	}

	return func() error {
		unlockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		closeErr := f.Close()
		if unlockErr != nil {
			return fmt.Errorf("gateway: unlock file %s: %w", path, unlockErr)
		}
		if closeErr != nil {
			return fmt.Errorf("gateway: close lock file %s: %w", path, closeErr)
		}
		return nil
	}, nil
}

// Held reports whether path names a lock file currently held by another
// process, without creating, mutating, or removing anything: it opens path
// read-only (never O_CREATE) and attempts the same non-blocking flock
// Acquire uses, on its own distinct file descriptor - flock works on a
// read-only fd just as well, and read-only lets `mtclaw doctor` report
// "held" correctly even for a user who cannot write the lock file, instead
// of failing to open it at all. Success (immediately released again) means
// nothing else holds it; failure means it is held. A missing lock file is
// reported as not held, not as an error - `cron run` uses this to refuse
// firing a persistent job while a gateway holds the lock.
func Held(path string) (pid int, held bool, err error) {
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("gateway: open lock file %s: %w", path, err)
	}
	defer f.Close()

	pid, _ = readLockPID(path)
	if flockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); flockErr != nil {
		return pid, true, nil
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return pid, false, nil
}
