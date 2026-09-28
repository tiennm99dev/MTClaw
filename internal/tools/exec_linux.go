//go:build linux

package tools

import "syscall"

// DisableEnvironRead sets PR_SET_DUMPABLE=0 for this process so a same-uid
// child - anything exec starts, or anything that child itself spawns -
// cannot read this process's own environment (including a secret resolved
// into it) back out of /proc/<pid>/environ. filterEnv already strips the
// specific variable names exec resolved its own secrets from before
// starting a child (see exec.go), but that only governs what the direct
// child inherits in its own environment; any same-uid process, not just
// that child, can otherwise read this process's /proc/<ppid>/environ
// directly regardless of what the child's own environment contains. This
// is what closes that second vector. The cost: this process can no longer
// be ptrace-attached to or produce a core dump. It is meant to be called
// once at process startup - see docs/security.md for what it does and does
// not cover.
func DisableEnvironRead() error {
	// PR_SET_DUMPABLE (prctl's first argument) with a second argument of 0
	// makes /proc/self/{environ,maps,...} restricted to root and disables
	// ptrace attach and core dumps for this process. The remaining two
	// syscall.Syscall arguments are unused by this prctl option and must be
	// zero.
	if _, _, errno := syscall.Syscall(syscall.SYS_PRCTL, uintptr(syscall.PR_SET_DUMPABLE), 0, 0); errno != 0 {
		return errno
	}
	return nil
}
