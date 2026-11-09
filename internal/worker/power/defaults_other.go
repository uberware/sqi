// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !linux && !darwin && !windows

package power

// defaultCommands returns nothing: there is no command known to suspend or
// power off the machine on this platform, so a sleep or shutdown is refused
// unless worker.power.sleep_command or shutdown_command names one.
func defaultCommands() (sleep, shutdown []string) {
	return nil, nil
}
