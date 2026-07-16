// SPDX-License-Identifier: AGPL-3.0-or-later

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

	"github.com/uberware/sqi/internal/config"
	"github.com/uberware/sqi/internal/diag"
	sqilog "github.com/uberware/sqi/internal/log"
	"github.com/uberware/sqi/internal/scheduler"
	"github.com/uberware/sqi/internal/server"
)

// serveFlags holds values for flags specific to the serve subcommand.
var serveFlags struct {
	HTTPAddr            string
	OpenJDEnforceLimits bool
	AuthEnabled         bool
}

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start the sqi-server",
	Long: `Start the sqi-server, running the scheduler, REST API, WebSocket gateway,
embedded NATS JetStream broker, and embedded web UI.

The server runs until it receives SIGINT or SIGTERM, at which point it
performs a graceful shutdown: draining in-flight NATS messages, waiting
for active HTTP requests to complete, and flushing the state store.`,
	RunE: runServe,
}

func init() {
	serveCmd.Flags().StringVar(
		&serveFlags.HTTPAddr,
		"http-addr", "",
		"HTTP listen address (overrides config file and SQI_HTTP_ADDR)",
	)
	serveCmd.Flags().BoolVar(
		&serveFlags.OpenJDEnforceLimits,
		"openjd-enforce-limits", true,
		"enforce OpenJD quantitative limits during submission (overrides config file and SQI_OPENJD_ENFORCE_LIMITS)",
	)
	serveCmd.Flags().BoolVar(
		&serveFlags.AuthEnabled,
		"auth-enabled", false,
		"enable the authentication gate (overrides config file and SQI_AUTH_ENABLED)",
	)
}

func runServe(cmd *cobra.Command, _ []string) error {
	// ── Configuration ─────────────────────────────────────────────────────────
	overrides := persistentFlagOverrides()
	if cmd.Flags().Changed("http-addr") {
		overrides.HTTPAddr = serveFlags.HTTPAddr
	}
	if cmd.Flags().Changed("openjd-enforce-limits") {
		overrides.EnforceLimits = &serveFlags.OpenJDEnforceLimits
	}
	if cmd.Flags().Changed("auth-enabled") {
		overrides.AuthEnabled = &serveFlags.AuthEnabled
	}
	cfg, err := config.Load(persistentFlags.ConfigFile, overrides)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	if errs := config.Validate(cfg); len(errs) > 0 {
		var b strings.Builder
		for _, e := range errs {
			fmt.Fprintf(&b, "  %s\n", e)
		}
		return fmt.Errorf("%d configuration error(s):\n%s", len(errs), b.String())
	}

	// ── Diagnostic buffer ─────────────────────────────────────────────────────
	// Created before the logger (when enabled) so the server's own logs are
	// captured from the very first line via the server sink wired into the
	// logger below. Remains nil when diagnostics are disabled, in which case the
	// logger is built without a sink and the diagnostics endpoint returns 503.
	var diagBuf *diag.Buffer
	if cfg.Diagnostics.BufferSize > 0 {
		diagBuf = diag.NewBuffer(cfg.Diagnostics.BufferSize, nil)
	}

	// ── Logger ────────────────────────────────────────────────────────────────
	var sink sqilog.Sink
	if diagBuf != nil {
		sink = diag.NewServerSink(diagBuf)
	}
	logger, err := sqilog.NewWithSink(cfg.Log.Level, cfg.Log.Format, os.Stderr, sink)
	if err != nil {
		return fmt.Errorf("init logger: %w", err)
	}

	// ── Signal context ────────────────────────────────────────────────────────
	// signal.NotifyContext cancels ctx on the first SIGINT or SIGTERM, which
	// causes server.Run to begin graceful shutdown. Calling stop() afterwards
	// restores default signal handling so a second Ctrl-C hard-kills the
	// process if shutdown stalls.
	ctx, stop := signal.NotifyContext(
		context.Background(),
		os.Interrupt,    // SIGINT  (Ctrl-C)
		syscall.SIGTERM, // sent by systemd / Docker / Kubernetes
	)
	defer stop()

	// ── Run ───────────────────────────────────────────────────────────────────
	// Map the layered config into the scheduler's tuning parameters. Other
	// scheduler fields keep their normalized defaults.
	schedCfg := scheduler.DefaultConfig()
	schedCfg.WorkerTimeout = cfg.Scheduler.HeartbeatTimeout
	schedCfg.OfflineWorkerRetention = cfg.Scheduler.OfflineWorkerRetention
	schedCfg.JobRetention = cfg.Scheduler.JobRetention
	schedCfg.JobRetentionIncludeFailed = cfg.Scheduler.JobRetentionIncludeFailed
	schedCfg.UnschedulableGrace = cfg.Scheduler.UnschedulableGrace
	schedCfg.DefaultMaxAttempts = cfg.Scheduler.DefaultMaxAttempts
	schedCfg.RetryDelay = cfg.Scheduler.RetryDelay
	schedCfg.DefaultFailureLimit = cfg.Scheduler.DefaultFailureLimit

	srv := server.New(server.Config{
		HTTPAddr:              cfg.HTTP.Addr,
		NATSAddr:              cfg.NATS.Addr,
		NATSDataDir:           cfg.NATS.DataDir,
		NATSMaxStoreMB:        cfg.NATS.MaxStoreMB,
		SQLitePath:            cfg.Store.SQLitePath,
		EnablePprof:           cfg.HTTP.EnablePprof,
		CheckpointInterval:    cfg.Store.CheckpointInterval,
		DiscoveryEnabled:      cfg.Discovery.Enabled,
		DiscoveryInstanceName: cfg.Discovery.InstanceName,
		EnforceOpenJDLimits:   cfg.OpenJD.EnforceLimits,
		PresetLibraryURL:      cfg.PresetLibrary.URL,
		AuthEnabled:           cfg.Auth.Enabled,
		Scheduler:             schedCfg,
		// Phase 1: always seed. Replace with cfg.Store.SeedDefaults when
		// internal/config grows a setting for it.
		SeedDefaults: true,
	}, logger, diagBuf)
	if err := srv.Run(ctx); err != nil {
		logger.ErrorContext(ctx, "sqi-server exited with error", slog.Any("error", err))
		return err
	}
	return nil
}
