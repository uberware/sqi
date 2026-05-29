// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"os"

	"github.com/spf13/cobra"
)

// rootCmd is the top-level sqi-server command. It does not run any logic
// itself; all work is delegated to subcommands.
var rootCmd = &cobra.Command{
	Use:   "sqi-server",
	Short: "sqi distributed task management server",
	Long: `sqi-server is the control plane for the sqi distributed task management platform.

It runs the scheduler, REST API, WebSocket gateway, embedded NATS JetStream
broker, and embedded web UI in a single binary.

Use "sqi-server serve" to start the server.
Use "sqi-server --help" for a list of available subcommands.`,
	// No Run/RunE — bare invocation prints usage.
	SilenceUsage: true,
}

// persistentFlags holds the values bound to the root persistent flags so
// subcommands can read them without importing cobra internals.
var persistentFlags struct {
	ConfigFile string
	LogLevel   string
	LogFormat  string
}

func init() {
	pf := rootCmd.PersistentFlags()

	pf.StringVarP(
		&persistentFlags.ConfigFile,
		"config", "c", "",
		"path to config file (default: searches for sqi-server.yaml in ./config, $HOME/.sqi, /etc/sqi)",
	)
	pf.StringVar(
		&persistentFlags.LogLevel,
		"log-level", "info",
		"log verbosity: debug, info, warn, error",
	)
	pf.StringVar(
		&persistentFlags.LogFormat,
		"log-format", "json",
		"log output format: json, text",
	)

	rootCmd.AddCommand(
		serveCmd,
		versionCmd,
		migrateCmd,
		configCmd,
	)
}

// Execute runs the root command and returns any error. main() calls this and
// exits non-zero on failure.
func Execute() error {
	return rootCmd.Execute()
}

// exitOnErr exits with code 1 if err is non-nil. Used by main only.
func exitOnErr(err error) {
	if err != nil {
		os.Exit(1)
	}
}
