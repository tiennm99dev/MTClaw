//go:build windows

package gateway

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// Acquire creates path exclusively and writes the current process's PID
// into it, returning a release func that removes it. If path already
// exists, Acquire reads the recorded PID and probes whether that process is
// still alive: a live PID means another gateway instance is genuinely
// running, and Acquire refuses, naming the PID so the user knows what to
// check or kill. A dead PID means a stale lock left by a killed process;
// Acquire removes it and retries once.
//
// Unlike the unix build (syscall.Flock, kernel-enforced and
// self-releasing), this keeps the PID-probe scheme: a kernel-enforced
// Windows equivalent (LockFileEx) lives in golang.org/x/sys/windows, which
// is only an indirect dependency here - adding it as a direct import for
// this fix alone was out of scope, so the PID heuristic (with its known
// TOCTOU window and inability to detect a peer on another host or
// container) stays Windows-only rather than being removed everywhere. This
// is cooperative, not kernel-enforced - a container restart with a recycled
// PID could false-positive - but that failure mode is a refused start with
// a clear message, an acceptable trade for stopping the actual common case:
// a user running `mtclaw gateway` twice.
func Acquire(path string) (release func() error, err error) {
	for attempt := 0; attempt < 2; attempt++ {
		f, openErr := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if openErr == nil {
			if err := writeLockPID(f); err != nil {
				f.Close()
				os.Remove(path)
				return nil, fmt.Errorf("gateway: write lock file %s: %w", path, err)
			}
			if cerr := f.Close(); cerr != nil {
				os.Remove(path)
				return nil, fmt.Errorf("gateway: close lock file %s: %w", path, cerr)
			}
			return func() error {
				if rmErr := os.Remove(path); rmErr != nil && !os.IsNotExist(rmErr) {
					return fmt.Errorf("gateway: remove lock file %s: %w", path, rmErr)
				}
				return nil
			}, nil
		}

		if !errors.Is(openErr, fs.ErrExist) {
			return nil, fmt.Errorf("gateway: create lock file %s: %w", path, openErr)
		}

		pid, readErr := readLockPID(path)
		if readErr == nil && processAlive(pid) {
			return nil, fmt.Errorf("gateway: another instance is already running (pid %d); remove %s if this is wrong", pid, path)
		}
		// Stale (dead PID) or unreadable/corrupt (cannot verify liveness,
		// so treated as stale rather than blocking forever on a lock file
		// nobody can ever prove is held): remove and retry once.
		if rmErr := os.Remove(path); rmErr != nil && !os.IsNotExist(rmErr) {
			return nil, fmt.Errorf("gateway: remove stale lock file %s: %w", path, rmErr)
		}
	}
	return nil, fmt.Errorf("gateway: could not acquire lock file %s after removing a stale lock", path)
}

// Held reports whether path names a lock file currently held by a live
// gateway process, without creating or removing anything - unlike Acquire,
// it never mutates the lock file. A missing lock file (no gateway running)
// is reported as not held, not as an error.
func Held(path string) (pid int, held bool, err error) {
	pid, err = readLockPID(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, false, nil
		}
		return 0, false, err
	}
	return pid, processAlive(pid), nil
}

// processAlive probes whether pid names a running process. Unlike POSIX,
// os.FindProcess on Windows actually calls OpenProcess and returns an error
// when the pid does not correspond to a running process, so success alone
// (no Signal(0) equivalent exists on Windows - real signals are not
// supported by the os package) is the liveness check. A false negative is
// possible if OpenProcess is denied for a permissions reason on a live
// process; documented as an accepted limitation of a cooperative lock.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	defer proc.Release()
	return true
}
