// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/uberware/sqi/internal/config"
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
		backupCmd,
	)
}

// persistentFlagOverrides returns a [config.FlagOverrides] populated only from
// flags the user explicitly set on the command line. Flags that were not
// provided keep their zero value so they do not shadow env-var or config-file
// values in the lower layers.
func persistentFlagOverrides() config.FlagOverrides {
	pf := rootCmd.PersistentFlags()
	var ov config.FlagOverrides
	if pf.Changed("log-level") {
		ov.LogLevel = persistentFlags.LogLevel
	}
	if pf.Changed("log-format") {
		ov.LogFormat = persistentFlags.LogFormat
	}
	return ov
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
