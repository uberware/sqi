// SPDX-License-Identifier: AGPL-3.0-or-later

package proctree

import (
	"errors"

	"golang.org/x/sys/unix"
)

// szomb is p_stat for a zombie (sys/proc.h SZOMB).
const szomb = 5

// unreadableHint is the sweep's advice when a process's environment cannot be
// read.
const unreadableHint = "macOS lets only root read another account's process arguments"

// listProcs returns every process ID from kern.proc.all.
func listProcs() ([]int, error) {
	// The sysctl sizes its buffer, then reads; processes started in between
	// can overflow it (ENOMEM), so try once more.
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if errors.Is(err, unix.ENOMEM) {
		procs, err = unix.SysctlKinfoProcSlice("kern.proc.all")
	}
	if err != nil {
		return nil, err
	}
	pids := make([]int, 0, len(procs))
	for i := range procs {
		if pid := int(procs[i].Proc.P_pid); pid > 0 {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

// inspectProc reads a process's state and group from kern.proc.pid and its
// initial environment from kern.procargs2. A process whose arguments the
// worker may not read (EPERM, or the EINVAL macOS answers for another
// account's process) is reported unreadable rather than as an error.
func inspectProc(pid int) (procInfo, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return procInfo{}, err
	}
	info := procInfo{pgid: int(kp.Eproc.Pgid), zombie: kp.Proc.P_stat == szomb}
	if info.zombie {
		return info, nil
	}
	buf, err := unix.SysctlRaw("kern.procargs2", pid)
	switch {
	case errors.Is(err, unix.EPERM), errors.Is(err, unix.EINVAL):
		info.envUnreadable = true
		return info, nil
	case err != nil:
		return procInfo{}, err
	}
	env, err := parseProcArgs2(buf)
	if err != nil {
		return procInfo{}, err
	}
	info.token = envToken(env)
	return info, nil
}
