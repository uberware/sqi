// SPDX-License-Identifier: AGPL-3.0-or-later

package power

// defaultCommands returns the commands that suspend and power off a Windows
// machine. Suspend calls SetSuspendState('Suspend', force=false,
// disableWakeEvent=false): sleep rather than hibernate, not forced, with wake
// events left enabled.
func defaultCommands() (sleep, shutdown []string) {
	return []string{
			"powershell.exe", "-NoProfile", "-NonInteractive", "-Command",
			"Add-Type -AssemblyName System.Windows.Forms; [void][System.Windows.Forms.Application]::SetSuspendState('Suspend',$false,$false)",
		},
		[]string{"shutdown.exe", "/s", "/t", "0"}
}
