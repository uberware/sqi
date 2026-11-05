// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package target

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// platformProc holds the job object the child runs in. The job is set to
// kill everything in it when its last handle closes, which is when the
// harness exits, however it exits.
type platformProc struct {
	job windows.Handle
}

// release closes the job handle, killing anything still in the job: after a
// stop, that can only be something the child started and left behind.
func (p platformProc) release() {
	if p.job != 0 {
		windows.CloseHandle(p.job) //nolint:errcheck // best effort; the handle dies with the harness anyway
	}
}

// setProcAttrs starts the child in a new console process group. Ctrl-C at the
// console then reaches only the harness, and the harness can send the group
// CTRL_BREAK_EVENT, which Go delivers to sqi-server and sqi-worker as
// os.Interrupt.
func setProcAttrs(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
}

// supervise places the started process in a fresh kill-on-close job object.
// Assignment follows the start, so a child the process starts in the first
// instant can escape the job; sqi-server starts none, and sqi-worker starts
// one only for a task.
func supervise(p *os.Process) (platformProc, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return platformProc{}, fmt.Errorf("create job object: %w", err)
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job) //nolint:errcheck // already returning a more useful error
		return platformProc{}, fmt.Errorf("set job object limits: %w", err)
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(p.Pid)) //nolint:gosec // G115: PIDs fit in uint32
	if err != nil {
		windows.CloseHandle(job) //nolint:errcheck // already returning a more useful error
		return platformProc{}, fmt.Errorf("open process: %w", err)
	}
	defer windows.CloseHandle(h) //nolint:errcheck // done with it either way
	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		windows.CloseHandle(job) //nolint:errcheck // already returning a more useful error
		return platformProc{}, fmt.Errorf("assign process to job object: %w", err)
	}
	return platformProc{job: job}, nil
}

// interrupt sends CTRL_BREAK_EVENT to the child's console process group. It
// fails when the harness shares no console with the child, and the caller
// then kills instead.
func interrupt(p *os.Process) error {
	if p.Pid <= 0 {
		return errors.New("no process id") // group 0 would be every process on the console, the harness included
	}
	return windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(p.Pid)) //nolint:gosec // G115: PIDs fit in uint32
}

// killTree terminates the child's job, which takes everything the child
// started with it, or the child alone when it has no job.
func killTree(p *os.Process, plat platformProc) error {
	if plat.job != 0 {
		if err := windows.TerminateJobObject(plat.job, 1); err == nil {
			return nil
		}
	}
	return p.Kill()
}
