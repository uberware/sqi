// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	workerconfig "github.com/uberware/sqi/internal/worker/config"
)

// configCmd groups configuration-related subcommands.
var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Inspect and validate configuration",
	Long: `Inspect and validate the sqi-worker configuration.

Subcommands:
  print   Emit the effective merged configuration to stdout as YAML.`,
	// No RunE — bare "config" prints usage.
}

var configPrintCmd = &cobra.Command{
	Use:   "print",
	Short: "Print the effective merged configuration",
	Long: `Print the effective merged sqi-worker configuration to stdout as YAML.

The output reflects the final merged value from all configuration sources in
override order:

  built-in defaults → config file → SQI_WORKER_* environment variables → CLI flags

The YAML output is a valid config file — you can redirect it to a file and
use it as a starting point for customisation.`,
	RunE: runConfigPrint,
}

func init() {
	configCmd.AddCommand(configPrintCmd)
}

func runConfigPrint(_ *cobra.Command, _ []string) error {
	cfg, err := workerconfig.Load(persistentFlags.ConfigFile, flagOverrides())
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	out, err := yaml.Marshal(&cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	fmt.Fprint(os.Stdout, string(out))
	return nil
}
