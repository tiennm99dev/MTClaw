//go:build !windows

package gateway

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
	"time"
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

	held, flockErr := tryFlock(int(f.Fd()), syscall.LOCK_EX, acquireAttempts)
	if flockErr != nil {
		f.Close()
		return nil, fmt.Errorf("gateway: lock file %s: %w", path, flockErr)
	}
	if held {
		pid, _ := readLockPID(path)
		f.Close()
		if pid > 0 {
			// flock is kernel-enforced and self-releasing: the only way this
			// lock is held is a live process holding an open fd on it, so
			// removing the file (advice that fits Windows' PID-file scheme,
			// not this one) would just let a second gateway flock a fresh
			// inode - two pollers double-firing cron and racing Telegram's
			// getUpdates against each other. Stopping the real holder is the
			// only way to actually clear this.
			return nil, fmt.Errorf("gateway: another instance is already running (pid %d); stop pid %d first", pid, pid)
		}
		return nil, fmt.Errorf("gateway: another instance is already running; find and stop it first (its pid could not be read from %s)", path)
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
	// A shared probe lock: it conflicts with the gateway's exclusive lock
	// (reporting held) but not with another probe, so two concurrent
	// `doctor`/`cron run` calls never see each other as a holder.
	probeHeld, flockErr := tryFlock(int(f.Fd()), syscall.LOCK_SH, 1)
	if flockErr != nil {
		return 0, false, fmt.Errorf("gateway: probe lock file %s: %w", path, flockErr)
	}
	if probeHeld {
		return pid, true, nil
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return pid, false, nil
}

// acquireAttempts and acquireRetryDelay bound how long Acquire waits out a
// momentary conflict - typically another process's Held probe, which takes
// its lock for an instant - before concluding a real holder exists: about
// 100ms in total.
const (
	acquireAttempts   = 5
	acquireRetryDelay = 25 * time.Millisecond
)

// tryFlock attempts a non-blocking flock of kind (LOCK_EX or LOCK_SH) up to
// attempts times, sleeping acquireRetryDelay between tries. held is true
// only when the kernel reported EWOULDBLOCK on every try, meaning another
// open file description holds a conflicting lock. Any other failure (ENOLCK
// on a network filesystem, EBADF, ...) is returned as err: it says nothing
// about a peer, so reporting it as "another instance" would misdirect the
// user.
func tryFlock(fd, kind, attempts int) (held bool, err error) {
	for i := 0; i < attempts; i++ {
		if i > 0 {
			time.Sleep(acquireRetryDelay)
		}
		err = syscall.Flock(fd, kind|syscall.LOCK_NB)
		if err == nil {
			return false, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return false, err
		}
	}
	return true, nil
}
