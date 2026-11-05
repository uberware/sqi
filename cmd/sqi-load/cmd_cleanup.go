// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/uberware/sqi/internal/loadtest/runner"
	"github.com/uberware/sqi/internal/loadtest/simworker"
	"github.com/uberware/sqi/internal/loadtest/target"
)

func (a *app) cleanupCmd() *cobra.Command {
	var server, apiKey, caFile, runID string
	cmd := &cobra.Command{
		Use:   "cleanup --server <url> --run-id <id> [flags]",
		Short: "Remove what an attached run left on a server",
		Long: `Remove what an attached run left on a server: a run kept with --keep, or one
whose teardown failed. It deletes the run's farm, load-<run-id> (a sharded
run's farms load-<run-id>-s<i>), with its jobs, workers and queues, then the
run's usage pools, load_<run-id>_<pool>. Nothing else is touched. Workers
still online are waited for, up to two minutes.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if server == "" {
				return errors.New("--server is required: the URL of the server the run attached to")
			}
			if err := runner.ValidRunID(runID); err != nil {
				return fmt.Errorf("--run-id: %w", err)
			}
			cfg := target.CleanupConfig{HTTPURL: server, APIKey: apiKey, RunID: runID, Logger: a.logger()}
			if caFile != "" {
				cfg.TLS = simworker.TLSFiles{Enabled: "true", CAFile: caFile}
			}
			done, err := a.cleanup(cmd.Context(), cfg)
			fmt.Fprintf(a.stdout, "sqi-load: removed %d farms (%s) and %d usage pools (%s)\n",
				len(done.Farms), orNone(done.Farms), len(done.Pools), orNone(done.Pools))
			return err
		},
	}
	fs := cmd.Flags()
	fs.StringVar(&server, "server", "", "the server's HTTP URL (required)")
	fs.StringVar(&runID, "run-id", "", "the run to clean up after: the --run-id it ran with (required)")
	fs.StringVar(&apiKey, "api-key", "", "API key for a server with the auth gate on")
	fs.StringVar(&caFile, "ca-file", "", "CA certificate the server's TLS certificates chain to")
	return cmd
}

func orNone(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}
