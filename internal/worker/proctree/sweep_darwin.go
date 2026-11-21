// SPDX-License-Identifier: AGPL-3.0-or-later

package proctree

import (
	"golang.org/x/sys/unix"
)

// szomb is p_stat for a zombie (sys/proc.h SZOMB).
const szomb = 5

// systemOps reads the process table through sysctl.
func systemOps() sweepOps {
	return sweepOps{
		list:     listProcs,
		inspect:  inspectProc,
		kill:     killProcess,
		deadline: sweepDeadline,
		poll:     sweepPoll,
	}
}

// listProcs returns every process ID from kern.proc.all.
func listProcs() ([]int, error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.all")
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

// inspectProc reads a process's state from kern.proc.pid and its initial
// environment from kern.procargs2.
func inspectProc(pid int) (token string, zombie bool, err error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return "", false, err
	}
	if kp.Proc.P_stat == szomb {
		return "", true, nil
	}
	buf, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return "", false, err
	}
	env, err := parseProcArgs2(buf)
	if err != nil {
		return "", false, err
	}
	return envToken(env), false, nil
}
