// SPDX-License-Identifier: AGPL-3.0-or-later

package power

// defaultCommands returns the commands that put a Mac to sleep and power it
// off. shutdown needs root, so a worker that is not root must have a
// sudoers rule or a configured worker.power.shutdown_command.
func defaultCommands() (sleep, shutdown []string) {
	return []string{"pmset", "sleepnow"}, []string{"shutdown", "-h", "now"}
}
