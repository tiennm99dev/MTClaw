//go:build !windows

package tools

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts cmd's child in its own process group (setpgid) so
// killProcessTree can signal the whole group - the child and anything it
// forked - in one syscall instead of only the direct child.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessTree sends SIGKILL to cmd's entire process group. The negative
// pid is the POSIX convention for "the process group led by this pid",
// which setProcessGroup made cmd's own pid.
func killProcessTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
