// SPDX-License-Identifier: AGPL-3.0-or-later

package proctree

import (
	"os"
	"strings"
)

// bootID identifies the current boot, so records from before a reboot are
// dropped without a scan.
func bootID() (string, error) {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
