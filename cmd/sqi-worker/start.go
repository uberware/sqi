// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	nats "github.com/nats-io/nats.go"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/uberware/sqi/internal/health"
	sqilog "github.com/uberware/sqi/internal/log"
	"github.com/uberware/sqi/internal/redact"
	"github.com/uberware/sqi/internal/winsvc"
	"github.com/uberware/sqi/internal/worker/cancel"
	"github.com/uberware/sqi/internal/worker/capabilities"
	workerconfig "github.com/uberware/sqi/internal/worker/config"
	"github.com/uberware/sqi/internal/worker/diaglog"
	workerdiscovery "github.com/uberware/sqi/internal/worker/discovery"
	"github.com/uberware/sqi/internal/worker/enroll"
	"github.com/uberware/sqi/internal/worker/executor"
	"github.com/uberware/sqi/internal/worker/heartbeat"
	"github.com/uberware/sqi/internal/worker/lease"
	"github.com/uberware/sqi/internal/worker/logstreamer"
	workmetrics "github.com/uberware/sqi/internal/worker/metrics"
	"github.com/uberware/sqi/internal/worker/natsclient"
	"github.com/uberware/sqi/internal/worker/obs"
	"github.com/uberware/sqi/internal/worker/openjd"
	"github.com/uberware/sqi/internal/worker/preempt"
	"github.com/uberware/sqi/internal/worker/protocol"
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

The worker runs until it receives SIGINT or SIGTERM — or, when started by the
Windows Service Control Manager, a service stop — at which point it performs a
graceful shutdown: it stops accepting new task assignments and waits for
in-flight tasks to complete (up to the configured shutdown grace period), then
force-terminates any remaining tasks and closes the NATS connection.

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
	// winsvc.Run cancels ctx on SIGINT/SIGTERM in a console, or on a service
	// Stop/PreShutdown when the Windows SCM started us. A service runs in its
	// config file's directory (Run makes --config absolute first, and refuses to
	// start without one). Everything — config included — runs inside it, so an
	// early failure reaches the service's log.
	return winsvc.Run("sqi-worker", &persistentFlags.ConfigFile, func(ctx context.Context) error {
		return runWorker(ctx, cmd)
	})
}

// runWorker loads configuration, builds the logger and runs the worker until
// ctx is canceled (a signal, a service stop, or a permanent NATS loss), then
// drains in-flight tasks and deregisters. It returns nil after a clean
// shutdown: a service that exits non-zero on a Stop the operator asked for
// would trigger the SCM's failure-recovery restart.
func runWorker(ctx context.Context, cmd *cobra.Command) error {
	// ── Configuration ─────────────────────────────────────────────────────────
	cfg, err := loadAndValidateConfig(cmd)
	if err != nil {
		return err
	}

	// ── Logger ────────────────────────────────────────────────────────────────
	//
	// out is stderr unless log.file is set (or the process is a service, which
	// defaults to a file under %ProgramData%). It is closed on return; the defer
	// is registered before every other one in this function, so it runs last and
	// the logger's synchronous users all finish first.
	out, err := winsvc.LogOutput(ctx, cfg.Log.File, cfg.Log.MaxSizeMB, cfg.Log.MaxBackups)
	if err != nil {
		return fmt.Errorf("open log output: %w", err)
	}
	defer out.Close()
	logger, err := sqilog.New(cfg.Log.Level, cfg.Log.Format, out)
	if err != nil {
		return fmt.Errorf("init logger: %w", err)
	}

	// ── Dry-run ───────────────────────────────────────────────────────────────
	if startFlags.DryRun {
		return runDryRun(cfg)
	}

	// ── Root-user check + Worker ID ──────────────────────────────────────────
	//
	// Refuse to run as root on Linux/macOS unless allow_root is explicitly set,
	// because executing render processes as root is a security risk (see
	// docs/worker-configuration.md, "worker.allow_root"); no-op on Windows.
	// Then load or create this worker's stable ID.
	workerID, err := checkRootAndLoadWorkerID(cfg, logger)
	if err != nil {
		return err
	}

	// ── Shutdown context ──────────────────────────────────────────────────────
	//
	// The extra cancel lets watchNATSClosure start a shutdown on permanent NATS
	// loss, and the preemption watcher start one with the cause errPreempted.
	// Whichever trigger cancels first sets the cause (a signal that already
	// canceled the parent keeps its own), so a later trigger changes nothing.
	ctx, cancelCause := context.WithCancelCause(ctx)
	stop := func() { cancelCause(nil) }
	defer stop()

	// ── Server discovery ───────────────────────────────────────
	//
	// Resolve the NATS URL before dialing. An explicit URL is used as-is and
	// mDNS is bypassed. Otherwise mDNS is browsed for "_sqi._tcp" services on
	// the local network. With mDNS disabled and no explicit URL,
	// discovery.Resolve returns an error that is shown to the operator.
	found, err := workerdiscovery.Resolve(
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
	cfg.NATS.URL = found.NATSURL
	workerdiscovery.ApplyTLS(ctx, &cfg.NATS, found, logger)

	// ── Broker credential + NATS connection ──────────────────────
	//
	// Connect failure at boot is fatal. natsClosed is closed by the NATS
	// ClosedHandler when the connection permanently closes, which is how an
	// unexpected disconnect after the initial handshake is detected.
	nc, natsClosed, err := connectToBroker(ctx, cfg, workerID, logger)
	if err != nil {
		return err
	}

	// ── Diagnostic-log sink ─────────────────────────────────────
	//
	// Once NATS is connected and the stable worker ID is resolved, the logger is
	// rebuilt so the worker's own slog output is mirrored to sqi-server on
	// worker.diag.<workerID> (in addition to the log output) for display in the
	// web UI. All components constructed below receive this sink-enabled
	// logger; NewWithSink also installs it as slog.Default(). The early logger
	// serves only startup messages emitted before the NATS connection exists.
	logger, err = withDiagnosticSink(cfg, logger, out, nc, workerID)
	if err != nil {
		return err
	}

	// Drain in-flight subscriptions and flush pending publishes on exit.
	// natsclient.Drain blocks until complete or the shutdown grace period expires,
	// at which point it force-closes the connection.
	defer natsclient.Drain(nc, cfg.Worker.ShutdownGracePeriod, logger)

	// Watch for permanent NATS closure after the initial connect.
	//
	// If NATS exhausts MaxReconnectAttempts and permanently closes while the
	// worker is running, a graceful shutdown starts so that in-flight tasks
	// are flushed and the server receives departure messages rather than
	// waiting for the heartbeat timeout.
	//
	// When MaxReconnectAttempts is -1 (unlimited) the connection never closes
	// on its own, so this goroutine exits only via ctx.Done().
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
		obs.TLSConfig{
			Enabled:  cfg.Metrics.TLS.Enabled,
			CertFile: cfg.Metrics.TLS.CertFile,
			KeyFile:  cfg.Metrics.TLS.KeyFile,
		},
	)
	go obsServer.Run(ctx)

	// ── Capabilities ──────────────────────────────────────────────────────────
	caps, _, err := buildCapabilities(cfg, capabilities.OSCheckEnv())
	if err != nil {
		return fmt.Errorf("build capabilities: %w", err)
	}

	// ── Registration ────────────────────────────────────────────
	//
	// Build the registrar with the merged capability set. Register() is called
	// once at boot; SetupReconnectHook() re-sends the registration on every
	// later NATS reconnect so the server keeps a live record.
	reg, err := registerWorker(ctx, nc, workerID, cfg, caps, logger)
	if err != nil {
		return err
	}

	// ── Isolation provider, session root and session manager ──────
	sessionMgr, sessionRoot, stagingIsolationCapable, err := newSessionManager(cfg, logger)
	if err != nil {
		return err
	}

	// ── Log chunk publisher ────────────────────────────────────
	//
	// The logstreamer Publisher buffers task process output lines and batches
	// them into LogChunkMsg messages published to NATS JetStream. It
	// implements executor.OutputHandler and executor.LogFlusher so the executor
	// can flush remaining logs before publishing the terminal task status.
	// SYNC: logstreamer.Config and workerconfig.LogStreamerConfig have matching
	// fields. A field added to one must be added to both and mapped here.
	logPub := logstreamer.New(nc, workerID, logstreamer.Config{
		MaxLinesPerChunk: cfg.LogStreamer.MaxLinesPerChunk,
		MaxBytesPerChunk: cfg.LogStreamer.MaxBytesPerChunk,
		FlushInterval:    cfg.LogStreamer.FlushInterval,
	}, logger)

	// ── OpenJD progress/status/fail interceptor ────────────────
	//
	// The OpenJD interceptor wraps the log publisher and intercepts recognized
	// OpenJD directive lines (openjd_progress, openjd_status, openjd_fail)
	// before forwarding everything else downstream. It implements
	// executor.TaskLifecycleHook (so openjd_fail cancels the task) and
	// provides LastProgress() for the last_progress field of status messages.
	openjdInterceptor := openjd.New(logPub, nc, workerID, logger)

	// ── Task status publisher ───────────────────────────────────
	//
	// The status Publisher publishes all task state-transition messages
	// (running, succeeded, failed, canceled) with worker_id and last_progress
	// fields, and retries with backoff on transient NATS publish failures.
	statusPub := status.New(nc, status.Config{WorkerID: workerID}, logger)

	// ── Task executor ───────────────────────────────────────────
	//
	// The Executor starts OS processes for assigned tasks and reports their
	// status back to sqi-server via NATS. It implements lease.Dispatcher (via
	// Dispatch) and heartbeat.StateSource.
	exec := executor.New(
		statusPub,
		sessionMgr,
		m,
		openjdInterceptor, // openjd_progress/status/fail interception + log streaming
		executor.Config{
			KillGracePeriod:         shutdownKillGrace(cfg.Worker.ShutdownGracePeriod),
			AllowRoot:               cfg.Worker.AllowRoot,
			StagingScratchDir:       cfg.Staging.ScratchDir,
			StagingSyncCommand:      cfg.Staging.SyncCommand,
			StagingDefaults:         cfg.Staging.Defaults,
			StagingIsolationCapable: stagingIsolationCapable,
		},
		logger,
	)

	// ── Task cancel subscriber ─────────────────────────────────
	//
	// The server sends every cancel for this worker's runs to
	// task.cancel.<workerID>; the subscription hands each to exec.Cancel,
	// which triggers SIGTERM → SIGKILL escalation in the run's goroutine.
	// Subscribe returns once the server has the subscription, and it is made
	// before the lease loop starts, so a cancel for any lease this worker
	// takes finds it in place.
	if _, err := cancel.Subscribe(nc, workerID, exec, logger); err != nil {
		return err
	}

	// ── Heartbeat ───────────────────────────────────────────────
	//
	// The heartbeat Publisher ticks on cfg.Worker.HeartbeatInterval and
	// publishes liveness + runtime-state messages to worker.heartbeat.<worker>.
	// The executor is wired in as the StateSource so each heartbeat carries
	// the current active-task count, active task IDs, and last-assignment time.
	//
	// The Publisher also runs an internal watchdog goroutine that polls NATS
	// connection status and triggers re-registration when a reconnect is
	// detected, complementing the reconnect callback installed by the registrar.
	hbPublisher := heartbeat.New(
		nc,
		workerID,
		caps.CPUCount,
		cfg.Worker.HeartbeatInterval,
		exec, // executor implements heartbeat.StateSource
		reg,
		logger,
	)

	// The Publisher runs on its own context, not ctx: ctx is canceled the moment shutdown
	// begins, and the worker must keep heartbeating through the drain that
	// follows. A drain can outlast the server's heartbeat timeout, and a silent
	// worker is declared offline and has the tasks it is still running
	// reclaimed. shutdownSequence cancels hbCtx once the drain is over (it
	// does not wait for Run to return); the defer covers every earlier return.
	hbCtx, hbStop := context.WithCancel(context.Background())
	defer hbStop()
	go hbPublisher.Run(hbCtx)

	// ── Work-lease loop ─────────────────────────────────────────
	//
	// The worker asks the server for work on work.lease.<worker>.<queue> and
	// dispatches whatever the server leases it. The server gates capacity
	// (CPU-core fit, policy, usage pools), so the worker runs what it is given.
	leaseLoop := lease.New(
		lease.NATSTransport{NC: nc},
		exec, // *executor.Executor implements lease.Dispatcher
		leaseConfig(cfg.Worker.QueueIDs, workerID, reg.InstanceID()),
		logger,
	)
	go leaseLoop.Run(ctx)

	// ── Preemption watcher ──────────────────────────────────────
	//
	// Off unless worker.preemption.watch names a provider. A notice cancels ctx
	// with errPreempted, which starts the same shutdown as a signal (below) but
	// with no grace period.
	if err := startPreemptionWatch(ctx, cfg.Worker.Preemption, logger, cancelCause); err != nil {
		return err
	}

	logger.InfoContext(
		ctx, "sqi-worker starting",
		slog.String("worker_id", workerID),
		slog.String("worker_name", cfg.Worker.Name),
		slog.String("data_dir", cfg.Worker.DataDir),
		slog.String("session_root", sessionRoot),
		slog.String("metrics_addr", cfg.Metrics.Addr),
		slog.String("nats_url", redact.URL(nc.ConnectedUrl())),
		slog.String("os", caps.OS),
		slog.Int("cpu_count", caps.CPUCount),
		slog.Int("ram_mb", caps.RAMMb),
	)

	<-ctx.Done()

	// ── Log shutdown trigger ─────────────────────────────────────────
	//
	// Name what triggered shutdown for operators reading logs: the OS signal
	// in a console run ("interrupt", "terminated"), the SCM request for a
	// Windows service ("service stop"). NATS-driven shutdowns (permanent
	// connection loss) cancel with no cause, so they log "context canceled".
	cause := context.Cause(ctx)
	grace, kind := shutdownPlan(cause, cfg.Worker.ShutdownGracePeriod)
	logger.InfoContext(
		context.Background(), "sqi-worker shutdown triggered",
		slog.String("trigger", cause.Error()),
		slog.Int("active_tasks", exec.ActiveTaskCount()),
		slog.Duration("grace_period", grace),
	)

	// ── Drain, stop heartbeating, deregister ─────────────────────────
	completed, killed := shutdownSequence(exec, hbStop, reg, grace, kind)

	// ── Log shutdown outcome ─────────────────────────────────────────
	logger.InfoContext(
		context.Background(), "sqi-worker shutdown complete",
		slog.Int("tasks_completed_cleanly", completed),
		slog.Int("tasks_force_terminated", killed),
	)

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), cfg.Worker.ShutdownGracePeriod)
	defer shutdownCancel()
	obsServer.Shutdown(shutdownCtx)
	return nil
}

// registerWorker builds the registrar, wires re-registration into NATS
// reconnects and publishes the initial registration.
func registerWorker(
	ctx context.Context,
	nc *nats.Conn,
	workerID string,
	cfg workerconfig.WorkerConfig,
	caps capabilities.Capabilities,
	logger *slog.Logger,
) (*registration.Registrar, error) {
	// This is the same exprLimitsFromConfig call the session manager is built
	// with (see newSessionManager): what this worker advertises must be what it
	// enforces.
	reg, err := registration.New(nc, workerID, cfg.Worker, exprLimitsFromConfig(cfg.Expr), caps, logger)
	if err != nil {
		return nil, fmt.Errorf("build registrar: %w", err)
	}

	// Wire re-registration into the NATS reconnect callback. The handler
	// logs the reconnect and re-registers in one step.
	reg.SetupReconnectHook(ctx)

	// Publish the initial registration. This blocks until the server's
	// SQI_WORKER stream acks the message, so a worker booting alongside its
	// server waits out the stream provisioning rather than losing its
	// registration to it.
	//
	// A boot-time registration failure is fatal: the server cannot assign
	// tasks to a worker it has no record of.
	if err := reg.Register(ctx); err != nil {
		return nil, fmt.Errorf("worker registration: %w", err)
	}
	return reg, nil
}

// drainer is the part of *executor.Executor the shutdown sequence uses.
type drainer interface {
	DrainAndShutdown(gracePeriod time.Duration) (completed, killed int)
}

// deregisterer is the part of *registration.Registrar the shutdown sequence uses.
type deregisterer interface {
	Deregister(reason string, kind protocol.DeregisterKind)
}

// errPreempted is the cause the worker's context is canceled with when the
// provider announces the machine is being reclaimed. Every other trigger
// (a signal, a service stop, a lost NATS connection) carries a different cause.
var errPreempted = errors.New("preempted")

// shutdownPlan returns how long the drain waits for running tasks and the kind
// the worker departs with, for the cause that canceled its context. A
// preemption notice allows no wait: the machine is going, so tasks are
// canceled at once (reported as worker_shutdown, which the server reclaims
// rather than failing) and the worker leaves as preempted. Every other
// trigger is a restart, with the configured grace and no kind.
func shutdownPlan(cause error, configured time.Duration) (grace time.Duration, kind protocol.DeregisterKind) {
	if errors.Is(cause, errPreempted) {
		return 0, protocol.DeregisterPreempted
	}
	return configured, ""
}

// startPreemptionWatch starts the metadata-service watcher in the background
// when cfg names a provider, and cancels the worker's context with
// errPreempted on a notice. With watch "none" it does nothing and contacts no
// endpoint.
func startPreemptionWatch(
	ctx context.Context, cfg workerconfig.PreemptionConfig, logger *slog.Logger, stopWith context.CancelCauseFunc,
) error {
	if cfg.Watch == "" || cfg.Watch == workerconfig.PreemptionWatchNone {
		return nil
	}
	w, err := preempt.New(cfg.Watch, cfg.Endpoint, cfg.PollInterval, nil, logger)
	if err != nil {
		return fmt.Errorf("preemption watcher: %w", err)
	}
	logger.InfoContext(ctx, "sqi-worker watching for preemption",
		slog.String("vendor", cfg.Watch),
		slog.String("endpoint", w.Base()),
		slog.Duration("poll_interval", cfg.PollInterval))
	go w.Run(ctx, func() { stopWith(errPreempted) })
	return nil
}

// shutdownSequence runs the worker's shutdown in its required order and
// returns the drain's outcome.
//
//  1. Drain: stop accepting assignments (the lease loop stopped when the
//     worker's context was canceled), let in-flight tasks run for up to grace,
//     force-kill any left after that. DrainAndShutdown blocks until every task
//     goroutine has published its terminal status, so the server has complete
//     status information before NATS drains. The heartbeat keeps running
//     throughout: a drain may outlast the server's heartbeat timeout, and a
//     worker that goes quiet is declared offline and loses its running tasks.
//  2. Stop the heartbeat, so no further beat is started after the departure
//     message. stopHeartbeat only cancels the publisher's context and does not wait
//     for its Run to return, so a beat already being published can still
//     land after the deregister.
//  3. Deregister, so the server marks the worker offline immediately rather
//     than waiting for the heartbeat-timeout sweep. kind is "" for a restart
//     and [protocol.DeregisterPreempted] when the machine is being reclaimed.
func shutdownSequence(
	exec drainer, stopHeartbeat func(), reg deregisterer, grace time.Duration, kind protocol.DeregisterKind,
) (completed, killed int) {
	completed, killed = exec.DrainAndShutdown(grace)
	stopHeartbeat()
	reg.Deregister("graceful shutdown", kind)
	return completed, killed
}

// newSessionManager resolves the isolation provider and session root and
// builds the session manager on them. It also returns the session root, for
// the start-up log, and whether this worker can isolate, for the executor's
// staging.
func newSessionManager(cfg workerconfig.WorkerConfig, logger *slog.Logger) (mgr *session.Manager, sessionRoot string, isolationCapable bool, err error) {
	// ── Isolation provider + session root ────────────────────────
	//
	// Provider built once at boot: POSIX credential switching via
	// setuid/setgid, or the Windows LogonUserW-based provider (see
	// internal/worker/isolation's package doc). isolation.required makes the
	// worker refuse to start when it cannot isolate, rather than accept
	// isolated queues it cannot honor.
	//
	// sessionRoot is separate from cfg.Worker.DataDir, which holds only the
	// persistent worker-id file (see effectiveSessionRoot). When this worker
	// can assume another identity, resolveIsolation refuses to start rather
	// than chmod an ancestor that lacks the traversal bit isolated tasks need.
	isolationProvider, sessionRoot, sessionRootMode, isolationCapable, err := resolveIsolation(cfg, logger)
	if err != nil {
		return nil, "", false, err
	}

	// ── Session manager ─────────────────────────────────────────
	//
	// The session Manager creates isolated working directories and manages
	// environment setup/teardown for each task execution.
	//
	// keepFailedSessions retains working directories for failed sessions so
	// operators can inspect partial outputs (SQI_WORKER_KEEP_FAILED_SESSIONS).
	// isolationProvider resolves run-as-user credentials for assignments that
	// carry Isolation; cfg.Isolation.EnvPassthrough governs the additional
	// daemon environment variables an isolated session may inherit.
	// sessionRootMode (see effectiveSessionRoot) is 0711 when sessionRoot was
	// resolved to a location that must be traversable by another identity, or
	// 0750 for the non-root fallback under DataDir.
	// cfg.Expr is this host's operator-configured phase-3 expression budget.
	// It reaches every phase-3 evaluation through the AssignmentBudget the
	// Manager builds per session. This call is the only wiring point: without
	// it the worker is metered against the built-in defaults.
	// The command table: worker.commands plus any default a detector found.
	// Like cfg.Expr, the option below is the only wiring point: without it the
	// configuration is ignored.
	cmdPlan, err := detectCommandPlan(cfg, capabilities.OSCheckEnv(), hostLookPath)
	if err != nil {
		return nil, "", false, err
	}
	mgr = session.NewManager(
		sessionRoot, cfg.Worker.KeepFailedSessions, isolationProvider, cfg.Isolation,
		exprLimitsFromConfig(cfg.Expr), logger,
		session.WithSessionRootMode(sessionRootMode),
		cmdPlan.option(logger),
	)
	return mgr, sessionRoot, isolationCapable, nil
}

// shutdownKillGrace is the executor's kill window during a worker shutdown: a
// task still running when the grace period ends gets SIGTERM, then this long
// before SIGKILL — a third of the grace period. The Windows service's
// PreShutdown timeout is built from it (workerDrainBound), so the two cannot
// drift apart.
func shutdownKillGrace(grace time.Duration) time.Duration { return grace / 3 }

// checkRootAndLoadWorkerID performs the two boot-time identity checks that
// must happen before any network connection: refusing to run as root on
// Linux/macOS unless allow_root is explicitly set (a no-op on Windows), then
// loading or creating this worker's stable ID.
func checkRootAndLoadWorkerID(cfg workerconfig.WorkerConfig, logger *slog.Logger) (string, error) {
	if err := executor.CheckRootUser(cfg.Worker.AllowRoot, logger); err != nil {
		return "", err
	}
	workerID, err := workerconfig.LoadOrCreateWorkerID(cfg.Worker.DataDir)
	if err != nil {
		return "", fmt.Errorf("load worker id: %w", err)
	}
	return workerID, nil
}

// connectToBroker loads or obtains this worker's nkey broker credential and
// dials the broker with it.
//
// A missing credential (enroll.ErrNoCredential) is not fatal by itself: the
// farm may not require worker authentication, in which case the connect
// attempt proceeds with no credential. Only when the broker refuses that
// connection is authentication known to be required, and the operator gets a
// message naming both remediations (obtain a token, or pre-provision a key)
// instead of natsclient's generic rejection message, which assumes a
// credential existed to be rejected.
func connectToBroker(
	ctx context.Context,
	cfg workerconfig.WorkerConfig,
	workerID string,
	logger *slog.Logger,
) (*nats.Conn, <-chan struct{}, error) {
	seed, publicKey, err := enroll.EnsureCredential(ctx, enroll.Config{
		WorkerID:       workerID,
		CredentialFile: cfg.NATS.CredentialFile,
		JoinToken:      cfg.NATS.JoinToken,
		JoinTokenFile:  cfg.NATS.JoinTokenFile,
		ServerURL:      cfg.NATS.ServerURL,
		// Without these the worker cannot enroll against an HTTPS server
		// backed by a private farm CA, which is the first REST call it makes.
		TLSCAFile:          cfg.NATS.ServerTLSCAFile,
		InsecureSkipVerify: cfg.NATS.ServerTLSInsecureSkipVerify,
	}, logger)
	noCredential := errors.Is(err, enroll.ErrNoCredential)
	if err != nil && !noCredential {
		return nil, nil, fmt.Errorf("worker credential: %w", err)
	}

	nc, natsClosed, err := natsclient.Connect(ctx, cfg.NATS, workerID, seed, publicKey, logger)
	if err != nil {
		if noCredential && (errors.Is(err, nats.ErrAuthorization) || errors.Is(err, nats.ErrAuthExpired)) {
			//nolint:staticcheck // ST1005: the message is two sentences so the operator sees cause and remediation separately
			return nil, nil, fmt.Errorf(
				"sqi-worker: this server requires worker authentication, but no credential was found at %s and no join token is configured.\n"+
					"Obtain a token from an operator (`sqi-server worker token issue`) and set worker nats.join_token_file, or pre-provision a key with `sqi-worker keygen`.",
				cfg.NATS.CredentialFile,
			)
		}
		// A cancel during the dial is wrapped in err (see natsclient.Connect).
		return nil, nil, fmt.Errorf("nats connect: %w", err)
	}
	return nc, natsClosed, nil
}

// loadAndValidateConfig resolves CLI flag overrides, loads the layered
// configuration, and runs validation, returning a ready-to-use [WorkerConfig]
// or an error naming what to fix.
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

	caps, _, err := buildCapabilities(cfg, capabilities.OSCheckEnv())
	if err != nil {
		return fmt.Errorf("build capabilities: %w", err)
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

	// ── Commands ──────────────────────────────────────────────────────────────
	fmt.Fprintln(w)
	fmt.Fprintln(w, "## Commands")
	fmt.Fprintln(w)
	plan, err := detectCommandPlan(cfg, capabilities.OSCheckEnv(), hostLookPath)
	if err != nil {
		return err
	}
	if err := plan.write(w); err != nil {
		return fmt.Errorf("write commands: %w", err)
	}

	// ── Registration summary ──────────────────────────────────────────────────
	fmt.Fprintln(w)
	fmt.Fprintln(w, "## Would register as")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  name:             %s\n", cfg.Worker.Name)
	fmt.Fprintf(w, "  compute_location: %s\n", cfg.Worker.ComputeLocation)
	if cfg.NATS.URL != "" {
		fmt.Fprintf(w, "  server_nats_url:      %s\n", redact.List(cfg.NATS.URL))
	} else {
		fmt.Fprintln(w, "  server_nats_url:      (auto-discover via mDNS)")
	}

	return nil
}

// watchNATSClosure blocks until either ctx is canceled (normal shutdown) or the
// NATS connection permanently closes. On unexpected closure it logs and calls
// stop to trigger a graceful worker shutdown.
func watchNATSClosure(ctx context.Context, natsClosed <-chan struct{}, stop func(), logger *slog.Logger) {
	select {
	case <-ctx.Done():
		// Normal shutdown initiated by signal — NATS closure expected.
	case <-natsClosed:
		// NATS permanently closed outside of a planned shutdown. Cancel the
		// signal context to trigger the worker shutdown sequence.
		logger.ErrorContext(ctx,
			"natsclient: connection permanently closed — initiating worker shutdown")
		stop()
	}
}

// withDiagnosticSink rebuilds logger so the worker's own slog output is mirrored
// to sqi-server on worker.diag.<workerID> (in addition to out, the log
// destination the caller opened) when diagnostics are enabled; otherwise it
// returns logger unchanged.
func withDiagnosticSink(
	cfg workerconfig.WorkerConfig,
	logger *slog.Logger,
	out io.Writer,
	nc *nats.Conn,
	workerID string,
) (*slog.Logger, error) {
	if !cfg.Diagnostics.Enabled {
		return logger, nil
	}
	l, err := sqilog.NewWithSink(cfg.Log.Level, cfg.Log.Format, out, diaglog.New(nc, workerID))
	if err != nil {
		return nil, fmt.Errorf("init diagnostic logger: %w", err)
	}
	return l, nil
}

// leaseConfig builds the lease loop's configuration. Every lease request
// carries the same instance ID as this process's registration, so the server
// can tell a request from a process whose registration it has not applied
// yet.
func leaseConfig(queueIDs []string, workerID, instanceID string) lease.Config {
	return lease.Config{
		QueueIDs:   lease.QueueIDs(queueIDs),
		WorkerID:   workerID,
		InstanceID: instanceID,
	}
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
