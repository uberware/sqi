// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

// Windows-compatible process termination.
//
// Windows does not support POSIX signals, so the OpenJD Action.cancelation modes
// are approximated:
//
//   - TERMINATE (the spec default): the caller calls Kill directly, which
//     terminates the whole job object (TerminateJobObject) — the closest
//     Windows equivalent of SIGKILL, and one that reaps every process in the
//     tree including a grandchild orphaned before the call (see Tree's doc
//     for why "taskkill /F /T" cannot do that).
//   - NOTIFY_THEN_TERMINATE: the caller calls Notify first.  There is no true
//     SIGTERM on Windows; Notify uses "taskkill /T /PID" (without /F) as a
//     best-effort graceful request that lets GUI processes handle WM_CLOSE.
//     A console process may not receive a graceful-shutdown notification
//     equivalent to SIGTERM. After the notify period, the caller escalates
//     to Kill.
//
// Notify falls back to Kill (job-object terminate) if taskkill is
// unavailable or returns an error, so the process is always terminated.
// A Tree without a job handle runs "taskkill /F /T" in Kill, and proc.Kill()
// if that fails.

package proctree

import (
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Tree owns a Windows job object containing a launched task process and
// everything it spawns.
//
// "taskkill /F /T" walks the parent-PID tree at the moment of the call, so a
// grandchild whose parent has already exited is orphaned and survives it. A
// job object reaches that grandchild. Under run-as-user isolation such a
// survivor runs as the target account after its task ends, holding its
// license and file handles.
type Tree struct {
	proc *exec.Cmd
	job  windows.Handle
}

// Supervise places cmd's already-started process into a fresh job object
// and then lets it run. The process must have been started suspended
// (Configure), so every process it or its descendants create starts inside
// the job; Supervise refuses one that was already running, which may have
// created a child before the assignment. A process a system
// service starts at the task's request (Task Scheduler, WMI, an
// out-of-process COM server) is the service's child and outside the job.
//
// KILL_ON_JOB_CLOSE kills the whole tree if the worker process exits while
// the task is running: the server reclaims that task and runs it elsewhere,
// so a surviving tree would duplicate the work. Release() clears the flag
// before closing the job; see its doc. Windows 8 and later support nested
// jobs, so a child that creates its own job stays in this one.
//
// On an error the job is closed, which kills a process already assigned to
// it; the caller kills one that was not.
func Supervise(cmd *exec.Cmd) (*Tree, error) {
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
	return &Tree{proc: cmd, job: job}, nil
}

// resumeThreads resumes the suspended threads of process pid and fails
// unless one of them was suspended. A process created suspended has one, its
// initial thread; Go closes its handle once the process is created, so it is
// found by listing the system's threads. A thread that exits before it is
// opened is skipped.
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
		was, rerr := resumeThread(entry.ThreadID)
		if rerr != nil {
			return rerr
		}
		if was {
			resumed++
		}
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return fmt.Errorf("list threads: %w", err)
	}
	if resumed == 0 {
		return errors.New("the process was not started suspended")
	}
	return nil
}

// resumeThread decrements thread id's suspend count once, and reports whether
// it was suspended. A thread that no longer exists reports false.
func resumeThread(id uint32) (suspended bool, err error) {
	h, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, id)
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return false, nil // exited since the snapshot
	}
	if err != nil {
		return false, fmt.Errorf("open thread %d: %w", id, err)
	}
	defer windows.CloseHandle(h) //nolint:errcheck // best-effort close of a handle we are done with
	prev, err := windows.ResumeThread(h)
	if err != nil {
		return false, fmt.Errorf("resume thread %d: %w", id, err)
	}
	return prev > 0, nil
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

// Track records nothing on Windows: the job object's kill-on-close stops the
// tree if the worker process dies, so there is nothing for a later worker to
// sweep. It exists so the launch sites have one shape on both platforms.
func (*Tree) Track(*Ledger, string, Label) error { return nil }

// Release closes the job without killing whatever is still inside it.
//
// It clears KILL_ON_JOB_CLOSE first, which closing the handle would otherwise
// trigger, so a background child still running after a successful exit keeps
// running, as on POSIX. internal/worker/staging's stage-out checks
// (staging.go, openStageOutSource) allow for such a process.
func (t *Tree) Release() error {
	if t == nil || t.job == 0 {
		return nil
	}
	// Close the handle whether or not clearing the kill-on-close flag
	// succeeded, so a setKillOnClose failure does not leak the job handle for
	// the life of the worker. t.job is zeroed either way, keeping Release
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

// Alive reports whether any process of the task's job object is still
// running. A job that cannot be queried counts as empty, so the output drain
// starts rather than waiting on a question that has no answer.
func (t *Tree) Alive() bool {
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

// Configure starts the task process suspended. A job object can only be
// assigned to a process that exists, so Supervise assigns it after Start and
// only then lets it run: every process the task or its descendants create
// starts inside its job.
func Configure(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED
}

// Notify makes a best-effort graceful request via "taskkill /T" (without
// /F), which lets a GUI process handle WM_CLOSE. It does not terminate the job
// object: NOTIFY_THEN_TERMINATE is a grace window before the hard kill, and
// TerminateJobObject offers none. The caller escalates to Kill after the
// notify period. If taskkill fails, Notify falls back to Kill.
//
// It uses exec.Command rather than exec.CommandContext (noctx), as the task
// process does in internal/worker/executor's run.go. Notify and Kill
// implement the OpenJD cancelation policy and are typically called after the
// caller's own context is already canceled or timed out. os/exec kills a
// command as soon as its context is done, so a canceled context could abort
// taskkill before it signals the target, and Notify would never learn whether
// taskkill succeeded, defeating the Kill fallback.
func (t *Tree) Notify() error {
	pid := strconv.Itoa(t.proc.Process.Pid)
	if err := exec.Command("taskkill", "/T", "/PID", pid).Run(); err != nil { //nolint:noctx // pid is an integer, not user input; context not threaded, see doc comment above
		return t.Kill()
	}
	return nil
}

// Kill force-terminates the entire job object, which reaps every process in
// the tree, including a grandchild orphaned before this call, which
// "taskkill /F /T" would miss (see Tree's doc). If the job handle is absent it
// falls back to taskkill, then to proc.Kill.
func (t *Tree) Kill() error {
	if t.job == 0 {
		pid := strconv.Itoa(t.proc.Process.Pid)
		if err := exec.Command("taskkill", "/F", "/T", "/PID", pid).Run(); err != nil { //nolint:noctx // pid is an integer, not user input; context not threaded, see Notify's doc comment
			return t.proc.Process.Kill()
		}
		return nil
	}
	if err := windows.TerminateJobObject(t.job, 1); err != nil {
		return fmt.Errorf("terminate job object: %w", err)
	}
	return nil
}
