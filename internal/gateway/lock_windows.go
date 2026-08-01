//go:build windows

package gateway

import "os"

// processAlive probes whether pid names a running process. Unlike POSIX,
// os.FindProcess on Windows actually calls OpenProcess and returns an error
// when the pid does not correspond to a running process, so success alone
// (no Signal(0) equivalent exists on Windows - real signals are not
// supported by the os package) is the liveness check. A false negative is
// possible if OpenProcess is denied for a permissions reason on a live
// process; documented as an accepted limitation of a cooperative lock, same
// as the POSIX side's stale-lock caveat.
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
