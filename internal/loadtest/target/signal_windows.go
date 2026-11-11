// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package target

import "os"

// killOne terminates the process alone (TerminateProcess on its handle). The
// job object the harness put it in stays open, so what the process started
// keeps running until the job is released when the target closes.
func killOne(p *os.Process) error { return p.Kill() }

// Windows has no SIGSTOP, so a process cannot be stalled.
func stopProcess(int) error { return errStallUnsupported }
func stopGroup(int) error   { return errStallUnsupported }
func contProcess(int) error { return errStallUnsupported }
func contGroup(int) error   { return errStallUnsupported }

// Windows has no process groups to signal: the job object the harness put a
// worker in holds what its tasks started.
func killGroup(int) error { return nil }

func groupGone(error) bool { return false }

// ownGroup is 0: Windows has no process groups to signal.
func ownGroup() int { return 0 }

// safeGroup applies the POSIX rule, so markers are read alike everywhere;
// the group signals themselves do nothing on Windows.
func safeGroup(pgid int) bool { return pgid > 1 }
