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
	sqilog "github.com/uberware/sqi/internal/log"
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
	logger, err := sqilog.New(cfg.Log.Level, cfg.Log.Format, os.Stderr)
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
	srv := server.New(server.Config{
		HTTPAddr:           cfg.HTTP.Addr,
		NATSAddr:           cfg.NATS.Addr,
		NATSDataDir:        cfg.NATS.DataDir,
		NATSMaxStoreMB:     cfg.NATS.MaxStoreMB,
		SQLitePath:         cfg.Store.SQLitePath,
		EnablePprof:        cfg.HTTP.EnablePprof,
		CheckpointInterval: cfg.Store.CheckpointInterval,
	}, logger)
	if err := srv.Run(ctx); err != nil {
		logger.ErrorContext(ctx, "sqi-server exited with error", slog.Any("error", err))
		return err
	}
	return nil
}
