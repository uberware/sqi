// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !linux

package target

// fdLimitSupported says whether the harness can lower another process's
// open-file limit here.
const fdLimitSupported = false

// fdLimit is a process's open-file limit; nothing sets one off Linux.
type fdLimit struct{}

func limitFDs(int, int) (open int, old fdLimit, err error) {
	return 0, fdLimit{}, errFDLimitUnsupported
}

func restoreFDs(int, fdLimit) error { return errFDLimitUnsupported }
