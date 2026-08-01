//go:build windows

package tools

import (
	"os/exec"
	"strconv"
)

// setProcessGroup is a no-op on Windows: killProcessTree below uses
// `taskkill /T`, which walks the process tree by parent-PID rather than
// relying on a POSIX-style process group, so no special creation flag is
// needed when starting the child.
func setProcessGroup(cmd *exec.Cmd) {}

// killProcessTree kills cmd's process and its descendants via
// `taskkill /F /T /PID <pid>`. This is the simpler of the two documented
// options for a Windows equivalent to POSIX's "kill the process group"
// (the other being a Job Object); taskkill needs no extra Windows-specific
// API surface and taskkill /T's tree-kill covers the child processes a
// shell like PowerShell spawns, which is what the exec tool needs to
// guarantee on timeout or turn cancellation.
func killProcessTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	kill := exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(cmd.Process.Pid))
	_ = kill.Run()
}
