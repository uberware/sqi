// SPDX-License-Identifier: AGPL-3.0-or-later

package proctree

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// unreadableHint is the sweep's advice when a process's environment cannot be
// read.
const unreadableHint = "a worker running tasks as other accounts needs CAP_SYS_PTRACE to read their environment"

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

// inspectProc reads a process's state and group from /proc/<pid>/stat (the
// fields after the command, which is in parentheses and may itself hold spaces
// and parentheses) and its initial environment from /proc/<pid>/environ.
// Another account's environment needs CAP_SYS_PTRACE even for root; without it
// the process is reported unreadable rather than as an error.
func inspectProc(pid int) (procInfo, error) {
	dir := "/proc/" + strconv.Itoa(pid)
	stat, err := os.ReadFile(dir + "/stat")
	if err != nil {
		return procInfo{}, err
	}
	i := bytes.LastIndexByte(stat, ')')
	if i < 0 {
		return procInfo{}, errors.New("malformed stat")
	}
	// state ppid pgrp ...
	fields := strings.Fields(string(stat[i+1:]))
	if len(fields) < 3 {
		return procInfo{}, errors.New("malformed stat")
	}
	pgid, err := strconv.Atoi(fields[2])
	if err != nil {
		return procInfo{}, fmt.Errorf("malformed stat: %w", err)
	}
	info := procInfo{pgid: pgid, zombie: fields[0] == "Z"}
	if info.zombie {
		return info, nil
	}
	env, err := os.ReadFile(dir + "/environ")
	switch {
	case errors.Is(err, os.ErrPermission):
		info.envUnreadable = true
		return info, nil
	case err != nil:
		return procInfo{}, err
	}
	info.token = envToken(strings.Split(string(env), "\x00"))
	return info, nil
}
