// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !linux

package target

import "errors"

// errFDLimitUnsupported is what limiting the server's open files returns off
// Linux, where there is no prlimit to lower another process's limit with.
var errFDLimitUnsupported = errors.New("target: limiting the server's open files needs prlimit, which only Linux has")

// fdLimit is a process's open-file limit; nothing sets one off Linux.
type fdLimit struct{}

func limitFDs(int, int) (open int, old fdLimit, err error) {
	return 0, fdLimit{}, errFDLimitUnsupported
}

// restoreFDs is never reached: with no limit set there is none to restore.
func restoreFDs(int, fdLimit) error { return errFDLimitUnsupported }
