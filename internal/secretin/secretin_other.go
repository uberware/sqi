// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package secretin

import "github.com/spf13/cobra"

// ReadLine reads one line from cmd's stdin for a secret prompt (isolation
// set-credential, service install --user), exactly as set-credential has
// always read it. Deliberately unchanged: no-echo console suppression is
// added on Windows only (see secretin_windows.go) — set-credential's
// stored-credential path (isolation.CredentialStore.Put) and the Windows
// service installer are themselves Windows-only, and this must not alter POSIX
// behavior.
func ReadLine(cmd *cobra.Command) (string, error) {
	return readLineEcho(cmd.InOrStdin())
}
