// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package natsclient

import "syscall"

// connResetErrnos are the errors a reset connection reports.
var connResetErrnos = []syscall.Errno{syscall.ECONNRESET}
