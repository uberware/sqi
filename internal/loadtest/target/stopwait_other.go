// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !linux

package target

// awaitStop returns at once off Linux. On Windows stall never sends a stop;
// on the other POSIX systems golang.org/x/sys offers no waitid to observe a
// stop without consuming the report the process's reaper needs, so stall
// returns as soon as SIGSTOP is queued.
func (*proc) awaitStop() error { return nil }
