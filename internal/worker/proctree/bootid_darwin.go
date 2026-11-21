// SPDX-License-Identifier: AGPL-3.0-or-later

package proctree

import (
	"golang.org/x/sys/unix"
)

// bootID identifies the current boot, so records from before a reboot are
// dropped without a scan. It is the kernel's boot session UUID. kern.boottime
// cannot serve: the kernel adjusts it when the wall clock is stepped, which
// would make every record read as another boot's.
func bootID() (string, error) {
	return unix.Sysctl("kern.bootsessionuuid")
}
