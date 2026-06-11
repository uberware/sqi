// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"os"

	"github.com/spf13/cobra"
)

// rootCmd is the top-level sqi-worker command. It does not run any logic
// itself; all work is delegated to subcommands.
var rootCmd = &cobra.Command{
	Use:   "sqi-worker",
	Short: "sqi distributed task management worker agent",
	Long: `sqi-worker is the worker agent for the sqi distributed task management platform.

It discovers and connects to a running sqi-server, registers itself with its
capability tags and compute location, pulls task assignments over NATS
JetStream, and executes bare-metal OS processes inside OpenJD sessions.

Use "sqi-worker start" to start the worker agent.
Use "sqi-worker --help" for a list of available subcommands.`,
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
		"path to config file (default: searches for sqi-worker.yaml in ./config, $HOME/.sqi, /etc/sqi)",
	)
	pf.StringVar(
		&persistentFlags.LogLevel,
		"log-level", "",
		"log verbosity: debug, info, warn, error (overrides config file and SQI_WORKER_LOG_LEVEL)",
	)
	pf.StringVar(
		&persistentFlags.LogFormat,
		"log-format", "",
		"log output format: json, text (overrides config file and SQI_WORKER_LOG_FORMAT)",
	)

	rootCmd.AddCommand(
		startCmd,
		versionCmd,
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
