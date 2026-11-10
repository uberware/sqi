// SPDX-License-Identifier: AGPL-3.0-or-later

package power

// defaultCommands returns the commands that suspend and power off a Linux
// machine under systemd. Without systemd, set worker.power.sleep_command and
// shutdown_command.
func defaultCommands() (sleep, shutdown []string) {
	return []string{"systemctl", "suspend"}, []string{"systemctl", "poweroff"}
}
