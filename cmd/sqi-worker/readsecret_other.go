// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package main

import (
	"bufio"
	"strings"

	"github.com/spf13/cobra"
)

// readSecretLine reads one line from cmd's stdin for `isolation
// set-credential`, exactly as this command has always read it. Deliberately
// unchanged: no-echo console suppression is added on Windows only (see
// readsecret_windows.go) — set-credential's stored-credential path
// (isolation.CredentialStore.Put) is itself Windows-only today, and this
// commit must not alter POSIX behavior.
func readSecretLine(cmd *cobra.Command) (string, error) {
	line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}
