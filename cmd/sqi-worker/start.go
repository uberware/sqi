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

	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/uberware/sqi/internal/health"
	sqilog "github.com/uberware/sqi/internal/log"
	"github.com/uberware/sqi/internal/worker/capabilities"
	workerconfig "github.com/uberware/sqi/internal/worker/config"
	workerdiscovery "github.com/uberware/sqi/internal/worker/discovery"
	"github.com/uberware/sqi/internal/worker/heartbeat"
	workmetrics "github.com/uberware/sqi/internal/worker/metrics"
	"github.com/uberware/sqi/internal/worker/natsclient"
	"github.com/uberware/sqi/internal/worker/obs"
	"github.com/uberware/sqi/internal/worker/pull"
	"github.com/uberware/sqi/internal/worker/registration"
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
	cfg, err := loadAndValidateConfig(cmd)
	if err != nil {
		return err
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

	// ── Server discovery (tasks 34–37) ───────────────────────────────────────
	//
	// Resolve the NATS URL before dialing. If an explicit URL is configured
	// it is used as-is (mDNS bypassed, task 36). Otherwise mDNS is browsed
	// for "_sqi._tcp" services on the local network (tasks 34–35). If mDNS is
	// disabled and no explicit URL is set, discovery.ResolveNATSURL returns a
	// clear error that is surfaced to the operator (task 37).
	natsURL, err := workerdiscovery.ResolveNATSURL(
		ctx,
		cfg.NATS.URL,
		cfg.Discovery.EnableMDNS,
		cfg.Discovery.MDNSTimeout,
		logger,
	)
	if err != nil {
		return fmt.Errorf("server discovery: %w", err)
	}

	// Overwrite NATS.URL with the resolved address so natsclient.Connect and
	// all downstream log statements use the concrete URL.
	cfg.NATS.URL = natsURL

	// ── NATS connection (tasks 20–24) ─────────────────────────────────────────
	//
	// Connect failure at boot is fatal (task 24). closedCh is closed by the
	// NATS ClosedHandler when the connection permanently closes so we can
	// detect unexpected disconnects after the initial handshake.
	nc, natsClosed, err := natsclient.Connect(ctx, cfg.NATS, logger)
	if err != nil {
		// Boot-time connect failure is a fatal error (task 24).
		return fmt.Errorf("nats connect: %w", err)
	}
	// Drain in-flight subscriptions and flush pending publishes on exit (task 23).
	// natsclient.Drain blocks until complete or the shutdown grace period expires,
	// at which point it force-closes the connection.
	defer natsclient.Drain(nc, cfg.Worker.ShutdownGracePeriod, logger)

	// Watch for unexpected permanent NATS closure after initial connect (task 24).
	//
	// If NATS exhausts MaxReconnectAttempts and permanently closes while the
	// worker is running, initiate a graceful worker shutdown so that in-flight
	// tasks are flushed and the server receives departure messages rather than
	// waiting for heartbeat timeout.
	//
	// When MaxReconnectAttempts is -1 (unlimited) this goroutine exits only
	// via ctx.Done() (normal shutdown path), since the connection will never
	// permanently close on its own.
	go func() {
		select {
		case <-ctx.Done():
			// Normal shutdown initiated by signal — NATS closure expected.
		case <-natsClosed:
			// NATS permanently closed outside of a planned shutdown. Cancel
			// the signal context to trigger the worker shutdown sequence.
			logger.ErrorContext(context.Background(),
				"natsclient: connection permanently closed — initiating worker shutdown")
			stop()
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

	// ── Capabilities ──────────────────────────────────────────────────────────
	caps := capabilities.Detect(nil)
	caps.MergeManualTags(cfg.Worker.CapabilityTags)

	// ── Registration (tasks 25–29) ────────────────────────────────────────────
	//
	// Build the registrar with the merged capability set. Register() is called
	// once at boot; SetupReconnectHook() ensures the registration is re-sent
	// on any subsequent NATS reconnect so the server always has a live record.
	reg := registration.New(nc, workerID, cfg.Worker, caps, logger)

	// Wire re-registration into the NATS reconnect callback (task 28).
	// This replaces the reconnect logging that was previously in natsclient;
	// the new handler logs the reconnect and re-registers in one step.
	reg.SetupReconnectHook(ctx)

	// Publish the initial registration (tasks 25–26).
	// A boot-time registration failure is fatal: the server cannot assign
	// tasks to a worker it has no record of.
	if err := reg.Register(ctx); err != nil {
		return fmt.Errorf("worker registration: %w", err)
	}

	// ── Heartbeat (tasks 30–33) ───────────────────────────────────────────────
	//
	// The heartbeat Publisher ticks on cfg.Worker.HeartbeatInterval and
	// publishes liveness + runtime-state messages to worker.heartbeat.
	// A NoopStateSource is used here until the executor (task 49+) is wired
	// in; replace it with the executor's StateSource once available.
	//
	// The Publisher also runs an internal watchdog goroutine that polls NATS
	// connection status and triggers re-registration when a reconnect is
	// detected, complementing the reconnect callback installed above.
	hbPublisher := heartbeat.New(
		nc,
		workerID,
		cfg.Worker.MaxConcurrentTasks,
		cfg.Worker.HeartbeatInterval,
		heartbeat.NoopStateSource{},
		reg,
		logger,
	)
	go hbPublisher.Run(ctx)

	// ── Work assignment pull loop (tasks 38–42) ───────────────────────────────
	puller, err := newPuller(nc, cfg, logger)
	if err != nil {
		return err
	}
	go puller.Run(ctx)

	logger.InfoContext(
		ctx, "sqi-worker starting",
		slog.String("worker_id", workerID),
		slog.String("worker_name", cfg.Worker.Name),
		slog.String("data_dir", cfg.Worker.DataDir),
		slog.Int("max_concurrent_tasks", cfg.Worker.MaxConcurrentTasks),
		slog.String("metrics_addr", cfg.Metrics.Addr),
		slog.String("nats_url", nc.ConnectedUrl()),
		slog.String("os", caps.OS),
		slog.Int("cpu_count", caps.CPUCount),
		slog.Int("ram_mb", caps.RAMMb),
	)

	<-ctx.Done()

	logger.InfoContext(
		context.Background(), "sqi-worker shutting down",
		slog.String("reason", ctx.Err().Error()),
	)

	// ── Deregistration (task 27) ──────────────────────────────────────────────
	//
	// Publish a departure message before draining the NATS connection so the
	// server marks this worker offline immediately rather than waiting for the
	// heartbeat-timeout sweep. Drain (deferred above) flushes remaining
	// publishes after this returns.
	reg.Deregister("graceful shutdown")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Worker.ShutdownGracePeriod)
	defer cancel()
	obsServer.Shutdown(shutdownCtx)
	return nil
}

// loadAndValidateConfig resolves CLI flag overrides, loads the layered
// configuration, and runs validation — returning a ready-to-use [WorkerConfig]
// or an error with an actionable message. Extracted from [runStart] to keep
// that function's cyclomatic complexity within the project limit.
func loadAndValidateConfig(cmd *cobra.Command) (workerconfig.WorkerConfig, error) {
	overrides := flagOverrides()
	if cmd.Flags().Changed("dry-run") {
		overrides.DryRun = startFlags.DryRun
	}
	if cmd.Flags().Changed("nats-insecure-skip-verify") {
		overrides.NATSInsecureSkipVerify = startFlags.NATSInsecureSkipVerify
	}

	cfg, err := workerconfig.Load(persistentFlags.ConfigFile, overrides)
	if err != nil {
		return workerconfig.WorkerConfig{}, fmt.Errorf("load config: %w", err)
	}

	if errs := workerconfig.Validate(cfg); len(errs) > 0 {
		var b strings.Builder
		for _, e := range errs {
			fmt.Fprintf(&b, "  %s\n", e)
		}
		return workerconfig.WorkerConfig{}, fmt.Errorf("%d configuration error(s):\n%s", len(errs), b.String())
	}
	return cfg, nil
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

// newPuller creates a JetStream context from nc and returns a configured
// [pull.Puller]. Extracted from [runStart] to keep that function's cyclomatic
// complexity within the project limit.
func newPuller(nc *nats.Conn, cfg workerconfig.WorkerConfig, logger *slog.Logger) (*pull.Puller, error) {
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("worker: jetstream context: %w", err)
	}
	return pull.New(
		js,
		pull.Config{
			QueueIDs:           cfg.Worker.QueueIDs,
			MaxConcurrentTasks: cfg.Worker.MaxConcurrentTasks,
			ComputeLocation:    cfg.Worker.ComputeLocation,
			IdleBackoff:        cfg.Worker.PullIdleBackoff,
			NackDelay:          cfg.Worker.PullNackDelay,
		},
		pull.NoopStateSource{}, // replaced by executor in tasks 49+
		pull.NoopDispatcher{},  // replaced by executor in tasks 49+
		logger,
	), nil
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
