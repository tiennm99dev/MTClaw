package gateway

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
)

// lockFileName is the instance lock's file name inside the state directory
// (see internal/config.StateDir).
const lockFileName = "gateway.lock"

// Acquire creates path exclusively and writes the current process's PID
// into it, returning a release func that removes it. If path already
// exists, Acquire reads the recorded PID and probes whether that process is
// still alive (see processAlive, platform-specific): a live PID means
// another gateway instance is genuinely running, and Acquire refuses,
// naming the PID so the user knows what to check or kill. A dead PID means
// a stale lock left by a killed process; Acquire removes it and retries
// once.
//
// This is cooperative, not kernel-enforced - a container restart with a
// recycled PID could false-positive - but that failure mode is a refused
// start with a clear message, which is an acceptable trade for stopping the
// actual common case: a user running `mtclaw gateway` twice.
func Acquire(path string) (release func() error, err error) {
	for attempt := 0; attempt < 2; attempt++ {
		f, openErr := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if openErr == nil {
			pid := os.Getpid()
			if _, werr := f.WriteString(strconv.Itoa(pid)); werr != nil {
				f.Close()
				os.Remove(path)
				return nil, fmt.Errorf("gateway: write lock file %s: %w", path, werr)
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
// it never mutates the lock file. `cron run` uses this to refuse firing a
// persistent job while a gateway holds the lock: nothing serializes two
// processes appending to the same session, so a concurrent gateway-driven
// fire and a manual one could interleave writes. A missing lock file (no
// gateway running) is reported as not held, not as an error.
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

// readLockPID reads and parses the PID recorded in a lock file at path.
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
