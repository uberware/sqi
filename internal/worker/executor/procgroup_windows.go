// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package executor

import (
	"errors"
	"fmt"
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
)

// processTree owns a Windows job object containing a launched task process
// and everything it spawns.
//
// "taskkill /F /T" walks the parent-PID tree at the moment of the call, so a
// grandchild whose parent has already exited is orphaned and survives it. A
// job object reaches that grandchild. Under run-as-user isolation such a
// survivor runs as the target account after its task ends, holding its
// license and file handles.
type processTree struct {
	proc *exec.Cmd
	job  windows.Handle
}

// superviseTree places cmd's already-started process into a fresh job object
// and then lets it run. configureProcessGroup starts the process suspended,
// so nothing it starts can be outside the job: a child it created before the
// assignment would escape it. A process started without that flag is
// assigned while running, and resuming it does nothing.
//
// KILL_ON_JOB_CLOSE kills the whole tree if the worker process exits while
// the task is running: the server reclaims that task and runs it elsewhere,
// so a surviving tree would duplicate the work. release() clears the flag
// before closing the job; see its doc. Windows 8 and later support nested
// jobs, so a child that creates its own job stays in this one.
//
// On an error the job is closed, which kills a process already assigned to
// it; the caller kills one that was not.
func superviseTree(cmd *exec.Cmd) (*processTree, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("create job object: %w", err)
	}
	if err := setKillOnClose(job, true); err != nil {
		windows.CloseHandle(job) //nolint:errcheck // already returning a more useful error
		return nil, err
	}
	pid := uint32(cmd.Process.Pid) //nolint:gosec // G115: PIDs never approach uint32's range
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, pid)
	if err != nil {
		windows.CloseHandle(job) //nolint:errcheck // already returning a more useful error
		return nil, fmt.Errorf("open task process: %w", err)
	}
	defer windows.CloseHandle(h) //nolint:errcheck // best-effort close of a handle we are done with
	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		windows.CloseHandle(job) //nolint:errcheck // already returning a more useful error
		return nil, fmt.Errorf("assign task to job object: %w", err)
	}
	if err := resumeThreads(pid); err != nil {
		windows.CloseHandle(job) //nolint:errcheck // already returning a more useful error
		return nil, fmt.Errorf("resume task process: %w", err)
	}
	return &processTree{proc: cmd, job: job}, nil
}

// resumeThreads resumes every thread of process pid. A process created
// suspended has one, its initial thread; Go closes its handle once the
// process is created, so it is found by listing the system's threads.
func resumeThreads(pid uint32) error {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return fmt.Errorf("list threads: %w", err)
	}
	defer windows.CloseHandle(snap) //nolint:errcheck // best-effort close of a handle we are done with
	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	resumed := 0
	for err = windows.Thread32First(snap, &entry); err == nil; err = windows.Thread32Next(snap, &entry) {
		if entry.OwnerProcessID != pid {
			continue
		}
		if err := resumeThread(entry.ThreadID); err != nil {
			return err
		}
		resumed++
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return fmt.Errorf("list threads: %w", err)
	}
	if resumed == 0 {
		return errors.New("the process has no thread")
	}
	return nil
}

// resumeThread decrements thread id's suspend count once.
func resumeThread(id uint32) error {
	h, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, id)
	if err != nil {
		return fmt.Errorf("open thread %d: %w", id, err)
	}
	defer windows.CloseHandle(h) //nolint:errcheck // best-effort close of a handle we are done with
	if _, err := windows.ResumeThread(h); err != nil {
		return fmt.Errorf("resume thread %d: %w", id, err)
	}
	return nil
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

// release closes the job without killing whatever is still inside it.
//
// It clears KILL_ON_JOB_CLOSE first, which closing the handle would otherwise
// trigger, so a background child still running after a successful exit keeps
// running, as on POSIX. internal/worker/staging's stage-out checks
// (staging.go, openStageOutSource) allow for such a process, and no test in
// that package would catch the two platforms differing.
func (t *processTree) release() error {
	if t == nil || t.job == 0 {
		return nil
	}
	// Close the handle whether or not clearing the kill-on-close flag
	// succeeded, so a setKillOnClose failure does not leak the job handle for
	// the life of the worker. t.job is zeroed either way, keeping release
	// idempotent.
	killErr := setKillOnClose(t.job, false)
	closeErr := windows.CloseHandle(t.job)
	t.job = 0
	if killErr != nil {
		return killErr
	}
	return closeErr
}

// jobBasicAccounting is JOBOBJECT_BASIC_ACCOUNTING_INFORMATION, which
// golang.org/x/sys/windows does not define.
type jobBasicAccounting struct {
	TotalUserTime             int64
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaultCount       uint32
	TotalProcesses            uint32
	ActiveProcesses           uint32
	TotalTerminatedProcesses  uint32
}

// alive reports whether any process of the task's job object is still
// running. A job that cannot be queried counts as empty, so the output drain
// starts rather than waiting on a question that has no answer.
func (t *processTree) alive() bool {
	if t == nil || t.job == 0 {
		return false
	}
	var info jobBasicAccounting
	err := windows.QueryInformationJobObject(
		t.job,
		windows.JobObjectBasicAccountingInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
		nil,
	)
	return err == nil && info.ActiveProcesses > 0
}
