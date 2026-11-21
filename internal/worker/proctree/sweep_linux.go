// SPDX-License-Identifier: AGPL-3.0-or-later

package proctree

import (
	"bytes"
	"errors"
	"os"
	"strconv"
	"strings"
)

// systemOps reads the process table from /proc.
func systemOps() sweepOps {
	return sweepOps{
		list:     listProcs,
		inspect:  inspectProc,
		kill:     killProcess,
		deadline: sweepDeadline,
		poll:     sweepPoll,
	}
}

// listProcs returns the numeric entries of /proc.
func listProcs() ([]int, error) {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	pids := make([]int, 0, len(ents))
	for _, e := range ents {
		if pid, err := strconv.Atoi(e.Name()); err == nil && pid > 0 {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

// inspectProc reads a process's state from /proc/<pid>/stat (the field after
// the command, which is in parentheses and may itself hold spaces and
// parentheses) and its initial environment from /proc/<pid>/environ.
func inspectProc(pid int) (token string, zombie bool, err error) {
	dir := "/proc/" + strconv.Itoa(pid)
	stat, err := os.ReadFile(dir + "/stat")
	if err != nil {
		return "", false, err
	}
	i := bytes.LastIndexByte(stat, ')')
	if i < 0 {
		return "", false, errors.New("malformed stat")
	}
	if strings.HasPrefix(strings.TrimSpace(string(stat[i+1:])), "Z") {
		return "", true, nil
	}
	env, err := os.ReadFile(dir + "/environ")
	if err != nil {
		return "", false, err
	}
	return envToken(strings.Split(string(env), "\x00")), false, nil
}
