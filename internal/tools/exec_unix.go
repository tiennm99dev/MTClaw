//go:build !windows

package tools

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts cmd's child in its own process group (setpgid) so
// unixProcessTree.kill can signal the whole group - the child and anything
// it forked - in one syscall instead of only the direct child.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// unixProcessTree kills the process group setProcessGroup placed cmd's
// child into. The negative pid is the POSIX convention for "the process
// group led by this pid".
type unixProcessTree struct {
	pid int
}

// trackProcessTree must be called after cmd.Start succeeds, once cmd.Process
// is populated. On unix there is nothing extra to set up - the process
// group was already created via setProcessGroup before Start - so this only
// captures the pid the kill will target.
func trackProcessTree(cmd *exec.Cmd) processTree {
	if cmd.Process == nil {
		return nil
	}
	return &unixProcessTree{pid: cmd.Process.Pid}
}

func (t *unixProcessTree) kill() {
	_ = syscall.Kill(-t.pid, syscall.SIGKILL)
}
