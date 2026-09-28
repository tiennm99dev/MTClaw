//go:build windows

package tools

import (
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
)

// setProcessGroup is a no-op on Windows: the Job Object windowsProcessTree
// creates below is what tracks the process and everything it spawns, so no
// special process-creation flag is needed at start time.
func setProcessGroup(cmd *exec.Cmd) {}

// windowsProcessTree wraps a Windows Job Object that cmd's process was
// assigned to, with JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE set. A shell's own
// children inherit the job automatically (a job's membership propagates to
// descendants unless a child is itself created with
// CREATE_BREAKAWAY_FROM_JOB), so closing the job's one remaining handle
// terminates the whole tree in one call. Unlike `taskkill /F /T /PID <pid>`
// run after the process has already been reaped, this always operates on a
// handle this process still holds open, so it can never be pointed at a PID
// Windows has since reused for an unrelated process.
type windowsProcessTree struct {
	job windows.Handle
}

// trackProcessTree must be called after cmd.Start succeeds, once
// cmd.Process is populated - a process handle has to exist before it can be
// assigned to a job. On any failure it returns nil: the caller then has
// nothing to kill through the job, same as if cmd.Process were nil, which
// is the pre-existing fallback behaviour for a process that could not be
// tracked.
func trackProcessTree(cmd *exec.Cmd) processTree {
	if cmd.Process == nil {
		return nil
	}

	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil
	}

	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(job)
		return nil
	}

	proc, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		_ = windows.CloseHandle(job)
		return nil
	}
	defer windows.CloseHandle(proc)

	if err := windows.AssignProcessToJobObject(job, proc); err != nil {
		_ = windows.CloseHandle(job)
		return nil
	}
	return &windowsProcessTree{job: job}
}

// kill terminates every process still in the job - cmd's own process and
// anything it spawned - by closing the job's handle.
func (t *windowsProcessTree) kill() {
	_ = windows.CloseHandle(t.job)
}
