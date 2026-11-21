// SPDX-License-Identifier: AGPL-3.0-or-later

package natsclient

import "syscall"

// connResetErrnos are the errors a reset connection reports. On Windows
// syscall.ECONNRESET is an invented value that the network stack never
// returns; Winsock reports WSAECONNRESET.
var connResetErrnos = []syscall.Errno{syscall.WSAECONNRESET, syscall.ECONNRESET}
