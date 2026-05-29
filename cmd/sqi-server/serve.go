// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/uberware/sqi/internal/server"
)

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

func runServe(cmd *cobra.Command, _ []string) error {
	// ── Logger ────────────────────────────────────────────────────────────────
	// TODO(tasks 20–21): replace with internal/log setup that honours
	// --log-level and --log-format from persistentFlags.
	logLevel := slog.LevelInfo
	if persistentFlags.LogLevel == "debug" {
		logLevel = slog.LevelDebug
	}

	var handler slog.Handler
	opts := &slog.HandlerOptions{Level: logLevel}
	if persistentFlags.LogFormat == "text" {
		handler = slog.NewTextHandler(os.Stderr, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stderr, opts)
	}
	logger := slog.New(handler)

	// ── Configuration ─────────────────────────────────────────────────────────
	// TODO(tasks 16–19): load from layered config (defaults → file → SQI_* env
	// vars → CLI flags) via internal/config, using persistentFlags.ConfigFile.
	cfg := server.DefaultConfig()

	// ── Signal context ────────────────────────────────────────────────────────
	// signal.NotifyContext cancels ctx on the first SIGINT or SIGTERM, which
	// causes server.Run to begin graceful shutdown. Calling stop() afterwards
	// restores default signal handling so a second Ctrl-C hard-kills the
	// process if shutdown stalls.
	ctx, stop := signal.NotifyContext(
		context.Background(),
		os.Interrupt,   // SIGINT  (Ctrl-C)
		syscall.SIGTERM, // sent by systemd / Docker / Kubernetes
	)
	defer stop()

	// ── Run ───────────────────────────────────────────────────────────────────
	srv := server.New(cfg, logger)
	if err := srv.Run(ctx); err != nil {
		logger.Error("sqi-server exited with error", slog.Any("error", err))
		return err
	}
	return nil
}
