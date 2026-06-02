// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/uberware/sqi/internal/health"
	sqilog "github.com/uberware/sqi/internal/log"
	"github.com/uberware/sqi/internal/worker/capabilities"
	workerconfig "github.com/uberware/sqi/internal/worker/config"
	workmetrics "github.com/uberware/sqi/internal/worker/metrics"
	"github.com/uberware/sqi/internal/worker/natsclient"
	"github.com/uberware/sqi/internal/worker/obs"
)

// startFlags holds values for flags specific to the start subcommand.
var startFlags struct {
	DryRun                 bool
	NATSInsecureSkipVerify bool
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
	startCmd.Flags().BoolVar(
		&startFlags.NATSInsecureSkipVerify,
		"nats-insecure-skip-verify", false,
		"disable TLS certificate verification for the NATS connection (development only)",
	)
}

func runStart(cmd *cobra.Command, _ []string) error {
	// ── Configuration ─────────────────────────────────────────────────────────
	overrides := flagOverrides()
	if cmd.Flags().Changed("dry-run") {
		overrides.DryRun = startFlags.DryRun
	}
	if cmd.Flags().Changed("nats-insecure-skip-verify") {
		overrides.NATSInsecureSkipVerify = startFlags.NATSInsecureSkipVerify
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

	// ── NATS connection (tasks 20–22) ─────────────────────────────────────────
	nc, err := natsclient.Connect(ctx, cfg.NATS, logger)
	if err != nil {
		return fmt.Errorf("nats connect: %w", err)
	}
	// Drain the NATS connection on exit. nc.Drain() is async — it signals
	// the connection to flush and close; we give it the shutdown grace period
	// before giving up and force-closing.
	defer func() {
		done := make(chan struct{})
		go func() {
			if err := nc.Drain(); err != nil {
				logger.WarnContext(context.Background(), "natsclient: drain error", slog.Any("error", err))
			}
			close(done)
		}()
		drainCtx, cancel := context.WithTimeout(context.Background(), cfg.Worker.ShutdownGracePeriod)
		defer cancel()
		select {
		case <-done:
		case <-drainCtx.Done():
			logger.WarnContext(drainCtx, "natsclient: drain timed out — closing immediately")
			nc.Close()
		}
	}()

	// ── Observability (metrics, health, pprof) ────────────────────────────────
	m := workmetrics.New()
	h := health.NewRegistry()

	// Register the NATS connection as a readiness check: /readyz returns 503
	// while the connection is not in a connected state.
	h.Register("nats", health.CheckerFunc(func(_ context.Context) error {
		if !nc.IsConnected() {
			return errors.New("nats connection is not connected")
		}
		return nil
	}))

	obsServer := obs.New(
		cfg.Metrics.Addr,
		cfg.Metrics.EnablePprof,
		logger,
		m,
		h,
	)
	go obsServer.Run(ctx)

	logger.InfoContext(
		ctx, "sqi-worker starting",
		slog.String("worker_id", workerID),
		slog.String("worker_name", cfg.Worker.Name),
		slog.String("data_dir", cfg.Worker.DataDir),
		slog.Int("max_concurrent_tasks", cfg.Worker.MaxConcurrentTasks),
		slog.String("metrics_addr", cfg.Metrics.Addr),
		slog.String("nats_url", nc.ConnectedUrl()),
	)

	<-ctx.Done()

	logger.InfoContext(
		context.Background(), "sqi-worker shutting down",
		slog.String("reason", ctx.Err().Error()),
	)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Worker.ShutdownGracePeriod)
	defer cancel()
	obsServer.Shutdown(shutdownCtx)
	return nil
}

// runDryRun prints the effective configuration and the capabilities that would
// be registered with sqi-server, then exits without connecting.
func runDryRun(cfg workerconfig.WorkerConfig) error {
	w := os.Stdout

	fmt.Fprintln(w, "# sqi-worker dry-run")
	fmt.Fprintln(w, "# No server connection will be made.")
	fmt.Fprintln(w)

	// ── Effective configuration ───────────────────────────────────────────────
	fmt.Fprintln(w, "## Effective configuration")
	fmt.Fprintln(w)
	out, err := yaml.Marshal(&cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	fmt.Fprint(w, string(out))

	// ── Detected capabilities ─────────────────────────────────────────────────
	fmt.Fprintln(w)
	fmt.Fprintln(w, "## Detected capabilities")
	fmt.Fprintln(w)

	caps := capabilities.Detect(nil)
	caps.MergeManualTags(cfg.Worker.CapabilityTags)

	fmt.Fprintf(w, "os:          %s\n", caps.OS)
	if caps.OSVersion != "" {
		fmt.Fprintf(w, "os_version:  %s\n", caps.OSVersion)
	}
	fmt.Fprintf(w, "cpu_count:   %d\n", caps.CPUCount)
	fmt.Fprintf(w, "ram_mb:      %d\n", caps.RAMMb)
	if caps.GPU.Count > 0 {
		fmt.Fprintf(w, "gpu_count:   %d\n", caps.GPU.Count)
		fmt.Fprintf(w, "gpu_vendor:  %s\n", caps.GPU.Vendor)
		fmt.Fprintf(w, "gpu_model:   %s\n", caps.GPU.Model)
		fmt.Fprintf(w, "gpu_vram_mb: %d\n", caps.GPU.VRAMMb)
	} else {
		fmt.Fprintln(w, "gpu:         none detected")
	}

	if len(caps.Tags) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "tags:")
		keys := make([]string, 0, len(caps.Tags))
		for k := range caps.Tags {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if v := caps.Tags[k]; v != "" {
				fmt.Fprintf(w, "  %s: %s\n", k, v)
			} else {
				fmt.Fprintf(w, "  %s\n", k)
			}
		}
	}

	// ── Registration summary ──────────────────────────────────────────────────
	fmt.Fprintln(w)
	fmt.Fprintln(w, "## Would register as")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  name:                 %s\n", cfg.Worker.Name)
	fmt.Fprintf(w, "  compute_location:     %s\n", cfg.Worker.ComputeLocation)
	fmt.Fprintf(w, "  max_concurrent_tasks: %d\n", cfg.Worker.MaxConcurrentTasks)
	if cfg.NATS.URL != "" {
		fmt.Fprintf(w, "  server_nats_url:      %s\n", cfg.NATS.URL)
	} else {
		fmt.Fprintln(w, "  server_nats_url:      (auto-discover via mDNS)")
	}

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
