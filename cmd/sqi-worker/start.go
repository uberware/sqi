// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	sqilog "github.com/uberware/sqi/internal/log"
	workerconfig "github.com/uberware/sqi/internal/worker/config"
)

// startFlags holds values for flags specific to the start subcommand.
var startFlags struct {
	DryRun bool
}

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the sqi-worker agent",
	Long: `Start the sqi-worker agent.

The worker discovers and connects to a running sqi-server (via explicit NATS
URL or mDNS auto-discovery), registers itself with its capability tags and
compute location, and begins pulling task assignments over NATS JetStream.

The worker runs until it receives SIGINT or SIGTERM, at which point it
performs a graceful shutdown: it stops accepting new task assignments and
waits for in-flight tasks to complete (up to the configured shutdown grace
period), then force-terminates any remaining tasks and closes the NATS
connection.

Use --dry-run to resolve and print the effective configuration and detected
capabilities without connecting to the server.`,
	RunE: runStart,
}

func init() {
	startCmd.Flags().BoolVar(
		&startFlags.DryRun,
		"dry-run", false,
		"resolve configuration, detect capabilities, and print what would be registered — then exit without connecting",
	)
}

func runStart(cmd *cobra.Command, _ []string) error {
	// ── Configuration ─────────────────────────────────────────────────────────
	overrides := flagOverrides()
	if cmd.Flags().Changed("dry-run") {
		overrides.DryRun = startFlags.DryRun
	}

	cfg, err := workerconfig.Load(persistentFlags.ConfigFile, overrides)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	if errs := workerconfig.Validate(cfg); len(errs) > 0 {
		var b strings.Builder
		for _, e := range errs {
			fmt.Fprintf(&b, "  %s\n", e)
		}
		return fmt.Errorf("%d configuration error(s):\n%s", len(errs), b.String())
	}

	// ── Logger ────────────────────────────────────────────────────────────────
	logger, err := sqilog.New(cfg.Log.Level, cfg.Log.Format, os.Stderr)
	if err != nil {
		return fmt.Errorf("init logger: %w", err)
	}

	// ── Dry-run ───────────────────────────────────────────────────────────────
	if startFlags.DryRun {
		return runDryRun(cfg)
	}

	// ── Worker ID ─────────────────────────────────────────────────────────────
	workerID, err := workerconfig.LoadOrCreateWorkerID(cfg.Worker.DataDir)
	if err != nil {
		return fmt.Errorf("load worker id: %w", err)
	}

	// ── Signal context ────────────────────────────────────────────────────────
	ctx, stop := signal.NotifyContext(
		context.Background(),
		os.Interrupt,    // SIGINT  (Ctrl-C)
		syscall.SIGTERM, // sent by systemd / Docker
	)
	defer stop()

	// ── Run ───────────────────────────────────────────────────────────────────
	// TODO(phase1): instantiate and run the worker agent once implemented.
	// For now, block until shutdown signal so the CLI is testable end-to-end.
	logger.InfoContext(
		ctx, "sqi-worker starting",
		slog.String("worker_id", workerID),
		slog.String("worker_name", cfg.Worker.Name),
		slog.String("data_dir", cfg.Worker.DataDir),
		slog.Int("max_concurrent_tasks", cfg.Worker.MaxConcurrentTasks),
	)

	<-ctx.Done()

	logger.InfoContext(
		context.Background(), "sqi-worker shutting down",
		slog.String("reason", ctx.Err().Error()),
	)
	return nil
}

// runDryRun prints the effective configuration and what would be registered,
// then exits without connecting to the server.
func runDryRun(cfg workerconfig.WorkerConfig) error {
	fmt.Fprintln(os.Stdout, "# sqi-worker dry-run: effective configuration")
	fmt.Fprintln(os.Stdout, "# (no server connection will be made)")
	fmt.Fprintln(os.Stdout)

	out, err := yaml.Marshal(&cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	fmt.Fprint(os.Stdout, string(out))
	return nil
}

// flagOverrides returns a [workerconfig.FlagOverrides] populated only from
// flags the user explicitly set on the command line.
func flagOverrides() workerconfig.FlagOverrides {
	pf := rootCmd.PersistentFlags()
	var ov workerconfig.FlagOverrides
	if pf.Changed("log-level") {
		ov.LogLevel = persistentFlags.LogLevel
	}
	if pf.Changed("log-format") {
		ov.LogFormat = persistentFlags.LogFormat
	}
	return ov
}
