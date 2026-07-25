// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package executor

import (
	"fmt"
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
)

// processTree owns a Windows job object containing a launched task process
// and everything it spawns.
//
// It exists because "taskkill /F /T" walks the parent-PID tree at the moment
// of the call: a grandchild whose parent has already exited is orphaned and
// survives the kill. Under run-as-user isolation that leak is at its worst —
// the survivor runs as the target account, outliving its task, holding its
// license and its file handles.
type processTree struct {
	proc *exec.Cmd
	job  windows.Handle
}

// superviseTree places cmd's already-started process into a fresh job object.
//
// KILL_ON_JOB_CLOSE means the whole tree dies if the worker process exits
// while the task is running, which is deliberate: today those processes are
// orphaned while the task is reclaimed and re-run elsewhere, so the current
// behavior is duplicate work plus a runaway process. release() clears the
// flag on the SUCCESS path — see its doc.
//
// Assignment happens immediately after Start rather than atomically with
// process creation, leaving a sub-millisecond window in which the child could
// spawn something that escapes. Closing it fully would need CREATE_SUSPENDED
// plus resuming the child's thread, which Go does not expose (you would have
// to enumerate threads by PID). The window is accepted; nested job support
// since Windows 8 means a child creating its own job does not break this
// either way.
func superviseTree(cmd *exec.Cmd) (*processTree, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("create job object: %w", err)
	}
	if err := setKillOnClose(job, true); err != nil {
		windows.CloseHandle(job) //nolint:errcheck // already returning a more useful error
		return nil, err
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid)) //nolint:gosec // G115: PIDs never approach uint32's range
	if err != nil {
		windows.CloseHandle(job) //nolint:errcheck // already returning a more useful error
		return nil, fmt.Errorf("open task process: %w", err)
	}
	defer windows.CloseHandle(h) //nolint:errcheck // best-effort close of a handle we are done with
	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		windows.CloseHandle(job) //nolint:errcheck // already returning a more useful error
		return nil, fmt.Errorf("assign task to job object: %w", err)
	}
	return &processTree{proc: cmd, job: job}, nil
}

// setKillOnClose sets or clears JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE.
func setKillOnClose(job windows.Handle, on bool) error {
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	if on {
		info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	}
	_, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	)
	if err != nil {
		return fmt.Errorf("set job object limits: %w", err)
	}
	return nil
}

// release closes the job WITHOUT killing whatever is still inside it.
//
// This is the success path, and clearing the flag first is what keeps Windows
// behaviourally identical to POSIX. internal/worker/staging's TOCTOU analysis
// (see staging.go, validateStageOutSource) explicitly depends on the fact
// that "nothing kills a task's process group on a SUCCESSFUL exit" — a
// still-running background child is expected to survive there. Closing a
// KILL_ON_JOB_CLOSE job here would kill it, making the two platforms differ
// in a way no test in that package would catch.
func (t *processTree) release() error {
	if t == nil || t.job == 0 {
		return nil
	}
	// Close the handle regardless of whether clearing the kill-on-close flag
	// succeeded, so a setKillOnClose failure (unlikely on a handle we just
	// created, but possible) can never leak the job handle for the life of
	// the worker. t.job is zeroed either way, keeping release idempotent.
	killErr := setKillOnClose(t.job, false)
	closeErr := windows.CloseHandle(t.job)
	t.job = 0
	if killErr != nil {
		return killErr
	}
	return closeErr
}
