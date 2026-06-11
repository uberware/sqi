// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/uberware/sqi/internal/version"
)

// versionCmd prints build metadata embedded by the release toolchain.
// Variables live in internal/version so the Makefile ldflags can target them
// at the standard path (github.com/uberware/sqi/internal/version.Version …).
//
// Full ldflags wiring: phase1-sqi-server-tasks.md task 13.
var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version and build metadata",
	Long:  `Print the sqi-server version, git commit, build date, and Go runtime version.`,
	Run: func(_ *cobra.Command, _ []string) {
		fmt.Printf("sqi-server %s\n", version.Version)
		fmt.Printf("  commit:  %s\n", version.Commit)
		fmt.Printf("  built:   %s\n", version.BuildDate)
		fmt.Printf("  go:      %s\n", version.GoVersion)
	},
}
