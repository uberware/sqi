// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package script

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"sync/atomic"

	"golang.org/x/sys/windows"
)

// startTree starts cmd and places it in a job object of its own, whose
// termination is cmd's cancellation, so a program the script started (aws,
// gcloud) dies with it rather than finishing an action sqi has given up on.
// "taskkill /T" would miss a grandchild whose parent had already exited.
// release closes the job once cmd has been waited for, leaving anything still
// in it running, as POSIX leaves a finished script's process group.
//
// The job is assigned just after the start, so a child the script starts in
// that sub-millisecond window escapes it; the worker's executor accepts the
// same window (see internal/worker/executor's superviseTree). A job that
// cannot be made leaves cancellation killing the script alone.
func startTree(cmd *exec.Cmd, logger *slog.Logger) (release func(), err error) {
	var job atomic.Uintptr
	cmd.Cancel = func() error {
		if h := windows.Handle(job.Load()); h != 0 {
			return windows.TerminateJobObject(h, 1)
		}
		return cmd.Process.Kill()
	}
	if err := cmd.Start(); err != nil {
		return func() {}, err
	}
	h, err := newJob(cmd.Process.Pid)
	if err != nil {
		logger.WarnContext(context.Background(),
			"capacity provider: cannot hold the script's processes in a job; a timeout kills the script alone",
			slog.Any("error", err))
		return func() {}, nil
	}
	job.Store(uintptr(h))
	return func() {
		job.Store(0)
		windows.CloseHandle(h) //nolint:errcheck // best-effort close of a handle we are done with
	}, nil
}

// newJob returns a new job object holding the process pid.
func newJob(pid int) (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, fmt.Errorf("create job object: %w", err)
	}
	proc, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid)) //nolint:gosec // G115: PIDs never approach uint32's range
	if err != nil {
		windows.CloseHandle(job) //nolint:errcheck // already returning a more useful error
		return 0, fmt.Errorf("open script process: %w", err)
	}
	defer windows.CloseHandle(proc) //nolint:errcheck // best-effort close of a handle we are done with
	if err := windows.AssignProcessToJobObject(job, proc); err != nil {
		windows.CloseHandle(job) //nolint:errcheck // already returning a more useful error
		return 0, fmt.Errorf("assign script to job object: %w", err)
	}
	return job, nil
}
