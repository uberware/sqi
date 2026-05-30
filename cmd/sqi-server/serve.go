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

	"github.com/uberware/sqi/internal/config"
	"github.com/uberware/sqi/internal/server"
)

// serveFlags holds values for flags specific to the serve subcommand.
var serveFlags struct {
	HTTPAddr string
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
}

func runServe(cmd *cobra.Command, _ []string) error {
	// ── Configuration ─────────────────────────────────────────────────────────
	overrides := persistentFlagOverrides()
	if cmd.Flags().Changed("http-addr") {
		overrides.HTTPAddr = serveFlags.HTTPAddr
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

	// ── Logger ────────────────────────────────────────────────────────────────
	// TODO(tasks 20–21): replace with internal/log setup for structured logging
	// with request middleware. For now, wire up slog directly from config.
	var logLevel slog.Level
	switch strings.ToLower(cfg.Log.Level) {
	case "debug":
		logLevel = slog.LevelDebug
	case "warn":
		logLevel = slog.LevelWarn
	case "error":
		logLevel = slog.LevelError
	default:
		logLevel = slog.LevelInfo
	}

	var handler slog.Handler
	opts := &slog.HandlerOptions{Level: logLevel}
	if strings.EqualFold(cfg.Log.Format, "text") {
		handler = slog.NewTextHandler(os.Stderr, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stderr, opts)
	}
	logger := slog.New(handler)

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
	srv := server.New(server.Config{
		HTTPAddr:    cfg.HTTP.Addr,
		NATSAddr:    cfg.NATS.Addr,
		NATSDataDir: cfg.NATS.DataDir,
		SQLitePath:  cfg.Store.SQLitePath,
	}, logger)
	if err := srv.Run(ctx); err != nil {
		logger.ErrorContext(ctx, "sqi-server exited with error", slog.Any("error", err))
		return err
	}
	return nil
}
