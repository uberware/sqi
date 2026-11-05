// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/uberware/sqi/internal/version"
)

// versionCmd prints build metadata, as sqi-server and sqi-worker do.
func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version and build metadata",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "sqi-load %s\n", version.Version)
			fmt.Fprintf(out, "  commit:  %s\n", version.Commit)
			fmt.Fprintf(out, "  built:   %s\n", version.BuildDate)
			fmt.Fprintf(out, "  go:      %s\n", version.GoVersion)
		},
	}
}
