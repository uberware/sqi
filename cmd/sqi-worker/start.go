// SPDX-License-Identifier: AGPL-3.0-or-later

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
	"time"

	nats "github.com/nats-io/nats.go"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/health"
	sqilog "github.com/uberware/sqi/internal/log"
	"github.com/uberware/sqi/internal/worker/cancel"
	"github.com/uberware/sqi/internal/worker/capabilities"
	workerconfig "github.com/uberware/sqi/internal/worker/config"
	"github.com/uberware/sqi/internal/worker/diaglog"
	workerdiscovery "github.com/uberware/sqi/internal/worker/discovery"
	"github.com/uberware/sqi/internal/worker/executor"
	"github.com/uberware/sqi/internal/worker/heartbeat"
	"github.com/uberware/sqi/internal/worker/lease"
	"github.com/uberware/sqi/internal/worker/logstreamer"
	workmetrics "github.com/uberware/sqi/internal/worker/metrics"
	"github.com/uberware/sqi/internal/worker/natsclient"
	"github.com/uberware/sqi/internal/worker/obs"
	"github.com/uberware/sqi/internal/worker/openjd"
	"github.com/uberware/sqi/internal/worker/registration"
	"github.com/uberware/sqi/internal/worker/session"
	"github.com/uberware/sqi/internal/worker/status"
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
compute location, and begins requesting task leases over NATS.

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

	// ── Root-user check ─────────────────────────────────────────────
	//
	// Refuse to run as root on Linux/macOS unless allow_root is explicitly set,
	// because executing render processes as root is a security risk (see
	// docs/worker-configuration.md, "worker.allow_root").  No-op on Windows.
	if err := executor.CheckRootUser(cfg.Worker.AllowRoot, logger); err != nil {
		return err
	}

	// ── Worker ID ─────────────────────────────────────────────────────────────
	workerID, err := workerconfig.LoadOrCreateWorkerID(cfg.Worker.DataDir)
	if err != nil {
		return fmt.Errorf("load worker id: %w", err)
	}

	// ── Signal context ────────────────────────────────────────────────────────
	//
	// shutdownSig captures the actual OS signal so shutdown can log the trigger
	// name ("interrupt" vs "terminated") rather than the generic ctx.Err()
	// string.  signal.NotifyContext registers for the same signals and cancels
	// ctx; both registrations receive the same delivery.
	shutdownSig := make(chan os.Signal, 1)
	signal.Notify(shutdownSig, os.Interrupt, syscall.SIGTERM)

	ctx, stop := signal.NotifyContext(
		context.Background(),
		os.Interrupt,    // SIGINT  (Ctrl-C)
		syscall.SIGTERM, // sent by systemd / Docker
	)
	defer stop()
	defer signal.Stop(shutdownSig)

	// ── Server discovery ───────────────────────────────────────
	//
	// Resolve the NATS URL before dialing. If an explicit URL is configured
	// it is used as-is (mDNS bypassed). Otherwise mDNS is browsed
	// for "_sqi._tcp" services on the local network. If mDNS is
	// disabled and no explicit URL is set, discovery.ResolveNATSURL returns a
	// clear error that is surfaced to the operator.
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

	// ── NATS connection ─────────────────────────────────────────
	//
	// Connect failure at boot is fatal. closedCh is closed by the
	// NATS ClosedHandler when the connection permanently closes so we can
	// detect unexpected disconnects after the initial handshake.
	nc, natsClosed, err := natsclient.Connect(ctx, cfg.NATS, logger)
	if err != nil {
		// Boot-time connect failure is a fatal error.
		return fmt.Errorf("nats connect: %w", err)
	}

	// ── Diagnostic-log sink ─────────────────────────────────────
	//
	// Now that NATS is connected and the stable worker ID is resolved, rebuild
	// the logger so the worker's own slog output is mirrored to sqi-server on
	// worker.diag.<workerID> (in addition to stderr) for display in the web UI.
	// All components constructed below receive this sink-enabled logger;
	// NewWithSink also installs it as slog.Default(). The early logger was used
	// only for startup messages emitted before the NATS connection existed.
	logger, err = withDiagnosticSink(cfg, logger, nc, workerID)
	if err != nil {
		return err
	}

	// Drain in-flight subscriptions and flush pending publishes on exit.
	// natsclient.Drain blocks until complete or the shutdown grace period expires,
	// at which point it force-closes the connection.
	defer natsclient.Drain(nc, cfg.Worker.ShutdownGracePeriod, logger)

	// Watch for unexpected permanent NATS closure after initial connect.
	//
	// If NATS exhausts MaxReconnectAttempts and permanently closes while the
	// worker is running, initiate a graceful worker shutdown so that in-flight
	// tasks are flushed and the server receives departure messages rather than
	// waiting for heartbeat timeout.
	//
	// When MaxReconnectAttempts is -1 (unlimited) this goroutine exits only
	// via ctx.Done() (normal shutdown path), since the connection will never
	// permanently close on its own.
	go watchNATSClosure(ctx, natsClosed, stop, logger)

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
	caps, err := capabilities.BuildWorkerCapabilities(cfg.Capabilities, cfg.Worker.CapabilityTags, capabilities.OSCheckEnv())
	if err != nil {
		return fmt.Errorf("invalid capability tags: %w", err)
	}

	// ── Registration ────────────────────────────────────────────
	//
	// Build the registrar with the merged capability set. Register() is called
	// once at boot; SetupReconnectHook() ensures the registration is re-sent
	// on any subsequent NATS reconnect so the server always has a live record.
	reg := registration.New(nc, workerID, cfg.Worker, caps, logger)

	// Wire re-registration into the NATS reconnect callback.
	// This replaces the reconnect logging that was previously in natsclient;
	// the new handler logs the reconnect and re-registers in one step.
	reg.SetupReconnectHook(ctx)

	// Publish the initial registration.
	// A boot-time registration failure is fatal: the server cannot assign
	// tasks to a worker it has no record of.
	if err := reg.Register(ctx); err != nil {
		return fmt.Errorf("worker registration: %w", err)
	}

	// ── Session manager ─────────────────────────────────────────
	//
	// The session Manager creates isolated working directories and manages
	// environment setup/teardown for each task execution.
	//
	// keepFailedSessions retains working directories for failed sessions so
	// operators can inspect partial outputs (SQI_WORKER_KEEP_FAILED_SESSIONS).
	sessionMgr := session.NewManager(cfg.Worker.DataDir, cfg.Worker.KeepFailedSessions, logger)

	// ── Log chunk publisher ────────────────────────────────────
	//
	// The logstreamer Publisher buffers task process output lines and batches
	// them into LogChunkMsg messages published to NATS JetStream.  It
	// implements executor.OutputHandler and executor.LogFlusher so the executor
	// can flush remaining logs before publishing the terminal task status.
	// SYNC: logstreamer.Config and workerconfig.LogStreamerConfig have matching
	// fields.  If a field is added to one, it must be added to both and mapped
	// here.
	logPub := logstreamer.New(nc, logstreamer.Config{
		MaxLinesPerChunk: cfg.LogStreamer.MaxLinesPerChunk,
		MaxBytesPerChunk: cfg.LogStreamer.MaxBytesPerChunk,
		FlushInterval:    cfg.LogStreamer.FlushInterval,
	}, logger)

	// ── OpenJD progress/status/fail interceptor ────────────────
	//
	// The OpenJD interceptor wraps the log publisher and intercepts recognized
	// OpenJD directive lines (openjd_progress, openjd_status, openjd_fail)
	// before forwarding everything else downstream.  It implements
	// executor.TaskLifecycleHook (for openjd_fail→cancel integration) and
	// provides LastProgress() for last_progress injection into status messages.
	openjdInterceptor := openjd.New(logPub, nc, workerID, logger)

	// ── Task status publisher ───────────────────────────────────
	//
	// The status Publisher is responsible for all task state-transition messages
	// (running, succeeded, failed, canceled) with worker_id and last_progress
	// fields, and retry-with-backoff on transient NATS publish failures.
	statusPub := status.New(nc, status.Config{WorkerID: workerID}, logger)

	// ── Task executor ───────────────────────────────────────────
	//
	// The Executor starts OS processes for assigned tasks and reports their
	// status back to sqi-server via NATS.  It implements lease.Dispatcher (via
	// Dispatch) and heartbeat.StateSource.
	exec := executor.New(
		statusPub,
		sessionMgr,
		m,
		openjdInterceptor, // openjd_progress/status/fail interception + log streaming
		executor.Config{
			KillGracePeriod:    cfg.Worker.ShutdownGracePeriod / 3, // 1/3 of grace period as kill window
			AllowRoot:          cfg.Worker.AllowRoot,
			StagingScratchDir:  cfg.Staging.ScratchDir,
			StagingSyncCommand: cfg.Staging.SyncCommand,
		},
		logger,
	)

	// ── Task cancel subscriber ─────────────────────────────────
	//
	// The cancel Handler subscribes to task.cancel.<taskID> for each task the
	// executor dispatches.  When the server publishes a cancel signal the handler
	// calls exec.Cancel(taskID), which triggers SIGTERM → SIGKILL escalation in
	// the task's goroutine.  The handler is wired into the executor via
	// SetCancelRegistrar before the pull loop starts so every dispatched task has
	// its cancel subscription in place.
	cancelHandler := cancel.New(nc, exec, logger)
	exec.SetCancelRegistrar(cancelHandler)

	// ── Heartbeat ───────────────────────────────────────────────
	//
	// The heartbeat Publisher ticks on cfg.Worker.HeartbeatInterval and
	// publishes liveness + runtime-state messages to worker.heartbeat.
	// The executor is wired in as the StateSource so each heartbeat carries
	// the current active-task count, active task IDs, and last-assignment time.
	//
	// The Publisher also runs an internal watchdog goroutine that polls NATS
	// connection status and triggers re-registration when a reconnect is
	// detected, complementing the reconnect callback installed above.
	hbPublisher := heartbeat.New(
		nc,
		workerID,
		caps.CPUCount,
		cfg.Worker.HeartbeatInterval,
		exec, // executor implements heartbeat.StateSource
		reg,
		logger,
	)
	go hbPublisher.Run(ctx)

	// ── Work-lease loop ─────────────────────────────────────────
	//
	// The worker asks the server for work on work.lease.<queue> and dispatches
	// whatever the server leases it. The server gates capacity (CPU-core fit,
	// policy, usage pools), so the worker simply runs what it is given.
	leaseLoop := lease.New(
		leaseTransport{nc: nc}, // adapts *nats.Conn to lease.Transport
		exec,                   // *executor.Executor implements lease.Dispatcher
		lease.Config{
			QueueIDs: leaseQueueIDs(cfg.Worker.QueueIDs),
			WorkerID: workerID,
		},
		logger,
	)
	go leaseLoop.Run(ctx)

	logger.InfoContext(
		ctx, "sqi-worker starting",
		slog.String("worker_id", workerID),
		slog.String("worker_name", cfg.Worker.Name),
		slog.String("data_dir", cfg.Worker.DataDir),
		slog.String("metrics_addr", cfg.Metrics.Addr),
		slog.String("nats_url", nc.ConnectedUrl()),
		slog.String("os", caps.OS),
		slog.Int("cpu_count", caps.CPUCount),
		slog.Int("ram_mb", caps.RAMMb),
	)

	<-ctx.Done()

	// ── Log shutdown trigger ─────────────────────────────────────────
	//
	// Determine the signal that triggered shutdown for operators reading logs.
	// NATS-driven shutdowns (permanent connection loss) won't populate sigName;
	// they fall back to the ctx.Err() message.
	var sigName string
	select {
	case sig := <-shutdownSig:
		sigName = sig.String()
	default:
		sigName = ctx.Err().Error()
	}
	logger.InfoContext(
		context.Background(), "sqi-worker shutdown triggered",
		slog.String("trigger", sigName),
		slog.Int("active_tasks", exec.ActiveTaskCount()),
		slog.Duration("grace_period", cfg.Worker.ShutdownGracePeriod),
	)

	// ── Drain in-flight tasks with grace period ──────────────────
	//
	// Stop accepting new assignments immediately (the pull loop already stopped
	// when ctx was canceled). Allow in-flight tasks to run to
	// completion for up to ShutdownGracePeriod; force-kill any that remain
	// after the deadline. DrainAndShutdown blocks until all task
	// goroutines have published their terminal statuses, guaranteeing the
	// server receives complete status information before NATS drains.
	completed, killed := exec.DrainAndShutdown(cfg.Worker.ShutdownGracePeriod)

	// ── Log shutdown outcome ─────────────────────────────────────────
	logger.InfoContext(
		context.Background(), "sqi-worker shutdown complete",
		slog.Int("tasks_completed_cleanly", completed),
		slog.Int("tasks_force_terminated", killed),
	)

	// ── Deregistration ──────────────────────────────────────────────
	//
	// Publish a departure message before draining the NATS connection so the
	// server marks this worker offline immediately rather than waiting for the
	// heartbeat-timeout sweep. Drain (deferred above) flushes remaining
	// publishes after this returns.
	reg.Deregister("graceful shutdown")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), cfg.Worker.ShutdownGracePeriod)
	defer shutdownCancel()
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

	caps, err := capabilities.BuildWorkerCapabilities(cfg.Capabilities, cfg.Worker.CapabilityTags, capabilities.OSCheckEnv())
	if err != nil {
		return fmt.Errorf("invalid capability tags: %w", err)
	}

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
	fmt.Fprintf(w, "  name:             %s\n", cfg.Worker.Name)
	fmt.Fprintf(w, "  compute_location: %s\n", cfg.Worker.ComputeLocation)
	if cfg.NATS.URL != "" {
		fmt.Fprintf(w, "  server_nats_url:      %s\n", cfg.NATS.URL)
	} else {
		fmt.Fprintln(w, "  server_nats_url:      (auto-discover via mDNS)")
	}

	return nil
}

// watchNATSClosure blocks until either ctx is canceled (normal shutdown) or the
// NATS connection permanently closes. On unexpected closure it logs and calls
// stop to trigger a graceful worker shutdown. Extracted from [runStart] to keep
// that function's cyclomatic complexity within the project limit.
func watchNATSClosure(ctx context.Context, natsClosed <-chan struct{}, stop func(), logger *slog.Logger) {
	select {
	case <-ctx.Done():
		// Normal shutdown initiated by signal — NATS closure expected.
	case <-natsClosed:
		// NATS permanently closed outside of a planned shutdown. Cancel the
		// signal context to trigger the worker shutdown sequence.
		logger.ErrorContext(context.Background(),
			"natsclient: connection permanently closed — initiating worker shutdown")
		stop()
	}
}

// withDiagnosticSink rebuilds logger so the worker's own slog output is mirrored
// to sqi-server on worker.diag.<workerID> (in addition to stderr) when
// diagnostics are enabled; otherwise it returns logger unchanged. Extracted from
// [runStart] to keep that function's cyclomatic complexity within the project
// limit.
func withDiagnosticSink(
	cfg workerconfig.WorkerConfig,
	logger *slog.Logger,
	nc *nats.Conn,
	workerID string,
) (*slog.Logger, error) {
	if !cfg.Diagnostics.Enabled {
		return logger, nil
	}
	l, err := sqilog.NewWithSink(cfg.Log.Level, cfg.Log.Format, os.Stderr, diaglog.New(nc, workerID))
	if err != nil {
		return nil, fmt.Errorf("init diagnostic logger: %w", err)
	}
	return l, nil
}

// leaseTransport adapts a raw *nats.Conn to [lease.Transport], issuing
// core-NATS request/reply work-lease requests on the work.lease.<queue>
// subject. The worker connects via natsclient (which yields a *nats.Conn), so
// this thin wrapper provides the same RequestLease behavior as
// [bus.Client.RequestLease] without a second client connection.
type leaseTransport struct {
	nc *nats.Conn
}

// RequestLease sends a work-lease request for queueID and waits up to timeout
// for the server's reply. It returns the raw reply bytes (a marshaled
// leaseReply) for the lease loop to decode.
func (t leaseTransport) RequestLease(ctx context.Context, queueID string, data []byte, timeout time.Duration) ([]byte, error) {
	reqCtx, cancelReq := context.WithTimeout(ctx, timeout)
	defer cancelReq()
	msg, err := t.nc.RequestWithContext(reqCtx, bus.WorkLeaseSubject(queueID), data)
	if err != nil {
		return nil, fmt.Errorf("worker: request lease for queue %q: %w", queueID, err)
	}
	return msg.Data, nil
}

// leaseQueueIDs maps the worker's configured queue list to the queues it
// requests leases on. A worker with no configured queues serves any queue; it
// must still request on a valid subject, so it uses [bus.WildcardQueueToken]
// (work.lease._any) rather than an empty leaf (work.lease., which routes to no
// responder). The server selects tasks farm-wide and gates by eligibility, so a
// queue-unaffiliated worker is matched to any queue's ready work.
func leaseQueueIDs(configured []string) []string {
	if len(configured) == 0 {
		return []string{bus.WildcardQueueToken}
	}
	return configured
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
