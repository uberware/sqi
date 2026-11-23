// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package secretin

import "github.com/spf13/cobra"

// ReadLine reads one line from cmd's stdin for a secret prompt (isolation
// set-credential, service install --user) with a plain line read. Echo
// suppression exists on Windows only (see secretin_windows.go), where
// set-credential's stored-credential path (isolation.CredentialStore.Put) and
// the service installer run.
func ReadLine(cmd *cobra.Command) (string, error) {
	return readLineEcho(cmd.InOrStdin())
}
