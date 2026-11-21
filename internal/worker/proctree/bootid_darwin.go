// SPDX-License-Identifier: AGPL-3.0-or-later

package proctree

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// bootID identifies the current boot by the kernel's boot time, so records
// from before a reboot are dropped without a scan.
func bootID() (string, error) {
	tv, err := unix.SysctlTimeval("kern.boottime")
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d.%06d", tv.Sec, tv.Usec), nil
}
