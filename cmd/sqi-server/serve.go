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
	HTTPAddr             string
	HTTPCORSOrigins      []string
	OpenJDEnforceLimits  bool
	AuthEnabled          bool
	AuthValidateJobOwner bool
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
	serveCmd.Flags().StringSliceVar(
		&serveFlags.HTTPCORSOrigins,
		"http-cors-origins", nil,
		"comma-separated browser origins allowed by CORS "+
			"(overrides config file and SQI_HTTP_CORS_ORIGINS)",
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
	serveCmd.Flags().BoolVar(
		&serveFlags.AuthValidateJobOwner,
		"auth-validate-job-owner", true,
		"reject a job submission whose owner names no known user "+
			"(overrides config file and SQI_AUTH_VALIDATE_JOB_OWNER)",
	)
}

func runServe(cmd *cobra.Command, _ []string) error {
	// ── Configuration ─────────────────────────────────────────────────────────
	overrides := persistentFlagOverrides()
	if cmd.Flags().Changed("http-addr") {
		overrides.HTTPAddr = serveFlags.HTTPAddr
	}
	if cmd.Flags().Changed("http-cors-origins") {
		overrides.HTTPCORSOrigins = serveFlags.HTTPCORSOrigins
	}
	if cmd.Flags().Changed("openjd-enforce-limits") {
		overrides.EnforceLimits = &serveFlags.OpenJDEnforceLimits
	}
	if cmd.Flags().Changed("auth-enabled") {
		overrides.AuthEnabled = &serveFlags.AuthEnabled
	}
	if cmd.Flags().Changed("auth-validate-job-owner") {
		overrides.ValidateJobOwner = &serveFlags.AuthValidateJobOwner
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

	srv := server.New(serverConfig(cfg, schedCfg), logger, diagBuf)
	if err := srv.Run(ctx); err != nil {
		logger.ErrorContext(ctx, "sqi-server exited with error", slog.Any("error", err))
		return err
	}
	return nil
}

// serverConfig maps the layered [config.Config] the CLI loaded onto the
// [server.Config] the server actually runs with.
//
// Split out of runServe so the mapping can be tested: runServe boots NATS,
// SQLite and a listening socket and then blocks on signals, so nothing could
// observe this literal in a test while it lived inside it. Every field here is
// a hand-written assignment between two structs with matching field types,
// which is exactly where a dropped or transposed line compiles and starts —
// see TestServerConfig_CarriesTheExprCostBounds.
func serverConfig(cfg config.Config, schedCfg scheduler.Config) server.Config {
	return server.Config{
		HTTPAddr:                          cfg.HTTP.Addr,
		CORSOrigins:                       cfg.HTTP.CORSOrigins,
		NATSAddr:                          cfg.NATS.Addr,
		NATSAuthEnabled:                   cfg.NATS.Auth.Enabled,
		NATSAuthEnrollmentEndpointEnabled: cfg.NATS.Auth.EnrollmentEndpointEnabled,
		NATSAuthJoinTokenTTL:              cfg.NATS.Auth.JoinTokenTTL,
		NATSAuthJoinTokenSingleUse:        cfg.NATS.Auth.JoinTokenSingleUse,
		NATSDataDir:                       cfg.NATS.DataDir,
		NATSMaxStoreMB:                    cfg.NATS.MaxStoreMB,
		SQLitePath:                        cfg.Store.SQLitePath,
		EnablePprof:                       cfg.HTTP.EnablePprof,
		CheckpointInterval:                cfg.Store.CheckpointInterval,
		DiscoveryEnabled:                  cfg.Discovery.Enabled,
		DiscoveryInstanceName:             cfg.Discovery.InstanceName,
		EnforceOpenJDLimits:               cfg.OpenJD.EnforceLimits,
		OpenJDExprLimits:                  server.ExprLimitsFromConfig(cfg.OpenJD),
		OpenJDExprSubmissionDeadline:      cfg.OpenJD.ExprSubmissionDeadline,
		PresetLibraryURL:                  cfg.PresetLibrary.URL,
		AuthEnabled:                       cfg.Auth.Enabled,
		AuthValidateJobOwner:              cfg.Auth.ValidateJobOwner,
		AuthSessionTTL:                    cfg.Auth.Session.TTL,
		AuthCookieName:                    cfg.Auth.Session.CookieName,
		AuthCookieSecure:                  cfg.Auth.Session.CookieSecure,
		AuthBootstrapUsername:             cfg.Auth.Bootstrap.Username,
		AuthBootstrapPassword:             cfg.Auth.Bootstrap.Password,
		AuthLDAP:                          cfg.Auth.LDAP,
		AuthOIDC:                          cfg.Auth.OIDC,
		Scheduler:                         schedCfg,
		// Phase 1: always seed. Replace with cfg.Store.SeedDefaults when
		// internal/config grows a setting for it.
		SeedDefaults: true,
	}
}
