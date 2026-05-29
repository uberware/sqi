// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/uberware/sqi/internal/server"
	"gopkg.in/yaml.v3"
)

// configCmd groups configuration-related subcommands.
var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Inspect and validate configuration",
	Long: `Inspect and validate the sqi-server configuration.

Subcommands:
  print   Emit the effective merged configuration to stdout as YAML.`,
	// No RunE — bare "config" prints usage.
}

var configPrintCmd = &cobra.Command{
	Use:   "print",
	Short: "Print the effective merged configuration",
	Long: `Print the effective merged sqi-server configuration to stdout as YAML.

The output reflects the final merged value from all configuration sources in
override order:

  built-in defaults → config file → SQI_* environment variables → CLI flags

TODO(tasks 16–19): wire in internal/config so this reflects the full layered
load rather than defaults + persistent flags only.`,
	RunE: runConfigPrint,
}

func init() {
	configCmd.AddCommand(configPrintCmd)
}

// effectiveConfig is the printable representation of the merged sqi-server
// configuration. Its shape intentionally mirrors what internal/config.Config
// will define in tasks 16–19; at that point this struct is replaced by
// marshalling the loaded config directly.
//
// YAML tags match the keys that will appear in sqi-server.yaml so that the
// output of "config print" is a valid config file skeleton.
type effectiveConfig struct {
	// ConfigFile is the path that was (or would be) loaded.
	// Empty means the default search path is used.
	ConfigFile string `yaml:"config_file,omitempty"`

	Log   effectiveLogConfig   `yaml:"log"`
	HTTP  effectiveHTTPConfig  `yaml:"http"`
	NATS  effectiveNATSConfig  `yaml:"nats"`
	Store effectiveStoreConfig `yaml:"store"`
}

type effectiveLogConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

type effectiveHTTPConfig struct {
	Addr string `yaml:"addr"`
}

type effectiveNATSConfig struct {
	Addr    string `yaml:"addr"`
	DataDir string `yaml:"data_dir"`
}

type effectiveStoreConfig struct {
	SQLitePath string `yaml:"sqlite_path"`
}

func runConfigPrint(cmd *cobra.Command, _ []string) error {
	// TODO(tasks 16–19): replace this block with a real layered load via
	// internal/config, which will honour the config file, SQI_* env vars,
	// and all CLI flags.
	srv := server.DefaultConfig()

	cfg := effectiveConfig{
		ConfigFile: persistentFlags.ConfigFile,
		Log: effectiveLogConfig{
			Level:  persistentFlags.LogLevel,
			Format: persistentFlags.LogFormat,
		},
		HTTP: effectiveHTTPConfig{
			Addr: srv.HTTPAddr,
		},
		NATS: effectiveNATSConfig{
			Addr:    srv.NATSAddr,
			DataDir: srv.NATSDataDir,
		},
		Store: effectiveStoreConfig{
			SQLitePath: envOr("SQI_SQLITE_PATH", srv.SQLitePath),
		},
	}

	out, err := yaml.Marshal(&cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	fmt.Fprint(os.Stdout, string(out))
	return nil
}
