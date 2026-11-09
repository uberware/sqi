// SPDX-License-Identifier: AGPL-3.0-or-later

package simworker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/uberware/sqi/internal/loadtest/internal/sleep"

	nats "github.com/nats-io/nats.go"

	"github.com/uberware/sqi/internal/loadtest/scenario"
	"github.com/uberware/sqi/internal/worker/cancel"
	"github.com/uberware/sqi/internal/worker/capabilities"
	workerconfig "github.com/uberware/sqi/internal/worker/config"
	"github.com/uberware/sqi/internal/worker/fmtres"
	"github.com/uberware/sqi/internal/worker/heartbeat"
	"github.com/uberware/sqi/internal/worker/lease"
	"github.com/uberware/sqi/internal/worker/logstreamer"
	"github.com/uberware/sqi/internal/worker/natsclient"
	"github.com/uberware/sqi/internal/worker/openjd"
	"github.com/uberware/sqi/internal/worker/registration"
	"github.com/uberware/sqi/internal/worker/status"
)

const (
	// reconnectWait is sqi-worker's default nats.reconnect_wait.
	reconnectWait = 2 * time.Second
	// defaultHeartbeat is sqi-worker's default heartbeat interval.
	defaultHeartbeat = 15 * time.Second
	// registerRetry and registerMaxGap pace Disconnect's own re-registration
	// attempts: the first wait, and the most it doubles to.
	registerRetry  = 100 * time.Millisecond
	registerMaxGap = 2 * time.Second
	// registerPoll is how often Disconnect checks for the re-registration.
	registerPoll = 10 * time.Millisecond
	// handlerGrace is how long Disconnect leaves the reconnect handler to
	// re-register once the connection is back before registering itself.
	handlerGrace = time.Second
	// disconnectBound caps Disconnect: natsclient's first redial waits about
	// reconnectWait (plus jitter), and one registration may take
	// registration.RetryBudget.
	disconnectBound = 2*reconnectWait + registration.RetryBudget
	// simArch is the architecture every simulated worker reports. Scenarios
	// have no arch field, and a fixed value keeps a run independent of the
	// generator host's.
	simArch = "amd64"
	// defaultOS is the OS reported by a group that names none.
	defaultOS = "linux"
)

var errNotRunning = errors.New("simworker: worker is not running")

// WorkerSpec identifies one simulated worker. Group.Queues holds queue IDs
// (the fleet resolves the scenario's names). Seed and PublicKey are its nkey
// broker credential, nil when broker auth is off. RandSeed seeds its
// behavior sampling; the fleet derives it from the run seed.
type WorkerSpec struct {
	ID, Hostname, FarmID string
	Group                scenario.WorkerGroup
	NATSURL              string
	Seed                 []byte
	PublicKey            string
	TLS                  TLSFiles
	RandSeed             uint64
}

// TLSFiles is the broker TLS material as file paths, the form natsclient
// takes: sqi-worker's nats.tls_enabled, nats.tls_ca_file, nats.tls_cert_file,
// nats.tls_key_file and nats.insecure_skip_verify. The zero value is "auto"
// with no files, which dials without TLS.
type TLSFiles struct {
	Enabled            string // "" or "auto", "true", "false"
	CAFile             string
	CertFile, KeyFile  string
	InsecureSkipVerify bool
}

// NATSConfig returns f as the TLS fields of sqi-worker's NATS configuration.
func (f TLSFiles) NATSConfig() workerconfig.NATSConfig {
	return workerconfig.NATSConfig{
		TLSEnabled:         f.Enabled,
		TLSCAFile:          f.CAFile,
		TLSCertFile:        f.CertFile,
		TLSKeyFile:         f.KeyFile,
		InsecureSkipVerify: f.InsecureSkipVerify,
	}
}

// Worker is one simulated sqi-worker: one NATS connection, and the real
// registration, heartbeat, lease, status, log and cancel packages driving a
// [Dispatcher]. Its lifecycle methods are serialized.
type Worker struct {
	spec     WorkerSpec
	behavior Behavior
	ledger   *Ledger
	logger   *slog.Logger
	rng      *rand.Rand

	opMu     sync.Mutex   // serializes Start, Stop, Crash, Restart, Disconnect
	cur      *incarnation // the running process; nil when stopped or crashed
	up       atomic.Bool  // cur != nil, readable without opMu (which Disconnect holds for seconds)
	instance atomic.Pointer[string]

	// registerFault, when set, runs before each registration publish and
	// fails it with its error. A test hook.
	registerFault atomic.Pointer[func() error]
}

// NewWorker returns a stopped worker. A nil logger discards.
func NewWorker(spec WorkerSpec, b Behavior, l *Ledger, logger *slog.Logger) *Worker {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Worker{
		spec:     spec,
		behavior: b,
		ledger:   l,
		logger:   logger.With(slog.String("worker_id", spec.ID)),
		rng:      rand.New(rand.NewPCG(spec.RandSeed, spec.RandSeed^0x9e3779b97f4a7c15)), //nolint:gosec // G404: sampling must be reproducible from the run seed
	}
}

// setCur records the running process. The caller holds opMu.
func (w *Worker) setCur(inc *incarnation) {
	w.cur = inc
	w.up.Store(inc != nil)
}

// alive reports whether the worker has a running process, without waiting for
// a lifecycle operation in progress.
func (w *Worker) alive() bool { return w.up.Load() }

// InstanceID returns the instance ID of the current or most recent process,
// or "" before the first Start.
func (w *Worker) InstanceID() string {
	if p := w.instance.Load(); p != nil {
		return *p
	}
	return ""
}

// Start connects, registers, and starts the heartbeat and lease loop. ctx
// bounds connecting and registering only: the worker then runs until Stop,
// Crash or Restart.
func (w *Worker) Start(ctx context.Context) error {
	w.opMu.Lock()
	defer w.opMu.Unlock()
	if w.cur != nil {
		return errors.New("simworker: worker is already running")
	}
	inc, err := w.launch(ctx)
	if err != nil {
		return err
	}
	w.setCur(inc)
	return nil
}

// Stop shuts the worker down the way sqi-worker does: no new leases, in-flight
// tasks run on until ctx is done, any still running then end failed with
// "worker_shutdown", and a deregistration goes out before the connection
// closes. Stopping a worker that is not running is a no-op.
func (w *Worker) Stop(ctx context.Context) error {
	w.opMu.Lock()
	defer w.opMu.Unlock()
	inc := w.cur
	w.setCur(nil)
	if inc == nil {
		return nil
	}
	inc.stop(ctx)
	return nil
}

// Crash drops the connection without deregistering. Heartbeats stop, and
// every in-flight attempt is abandoned with no terminal status published
// (ledger kind crashed). Nothing is kept running.
func (w *Worker) Crash() {
	w.opMu.Lock()
	defer w.opMu.Unlock()
	if w.cur != nil {
		w.cur.crash()
		w.setCur(nil)
	}
}

// Restart is a new worker process on the same worker ID: a running worker
// crashes first, then it reconnects and registers with a new instance ID, so
// the server reclaims the old process's attempts.
func (w *Worker) Restart(ctx context.Context) error {
	w.opMu.Lock()
	defer w.opMu.Unlock()
	if w.cur != nil {
		w.cur.crash()
		w.setCur(nil)
	}
	inc, err := w.launch(ctx)
	if err != nil {
		return err
	}
	w.setCur(inc)
	return nil
}

// Disconnect drops the connection's socket and waits until NATS has
// reconnected it and the worker has re-registered, with the same instance ID,
// so the server keeps its attempts. The reconnect handler re-registers, the
// same path a broker-side drop takes; if its registration has not landed
// handlerGrace after the connection is back (it failed, or the stream ack
// timed out), Disconnect registers itself, retrying. The wait is bounded by
// disconnectBound as well as ctx, so a lost registration cannot hold the
// worker's lifecycle lock indefinitely; after that error the worker keeps
// running and its heartbeat watchdog may still re-register it.
func (w *Worker) Disconnect(ctx context.Context) error {
	w.opMu.Lock()
	defer w.opMu.Unlock()
	inc := w.cur
	if inc == nil {
		return errNotRunning
	}
	before := inc.reg.LastRegisteredAt()
	if err := inc.nc.ForceReconnect(); err != nil {
		return fmt.Errorf("simworker: force reconnect: %w", err)
	}
	ctx, cancelFn := context.WithTimeout(ctx, disconnectBound)
	defer cancelFn()
	return inc.awaitReregistration(ctx, before)
}

// incarnation is one worker process: one connection and instance ID.
type incarnation struct {
	w          *Worker
	instanceID string
	nc         *nats.Conn
	reg        *registration.Registrar
	disp       *Dispatcher

	stopHB, stopLease context.CancelFunc
	hbDone, leaseDone chan struct{}
	// ended is closed when this process is torn down on purpose, so the
	// closed-connection watcher can tell that from natsclient giving up.
	ended chan struct{}
}

// reregistered reports whether a registration has succeeded since before.
func (inc *incarnation) reregistered(before time.Time) bool {
	return inc.reg.LastRegisteredAt().After(before)
}

// awaitReregistration waits for a registration after the one at before,
// making it itself once the reconnect handler has had handlerGrace to.
func (inc *incarnation) awaitReregistration(ctx context.Context, before time.Time) error {
	tick := time.NewTicker(registerPoll)
	defer tick.Stop()
	var direct time.Time // when to stop waiting for the reconnect handler
	for !inc.reregistered(before) {
		if direct.IsZero() && inc.nc.IsConnected() {
			direct = time.Now().Add(handlerGrace)
		}
		if !direct.IsZero() && time.Now().After(direct) {
			return inc.registerUntil(ctx, before)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("simworker: waiting for re-registration: %w", ctx.Err())
		case <-tick.C:
		}
	}
	return nil
}

// registerUntil registers, with backoff, until a registration after the one
// at before has landed (its own or anyone's) or ctx ends.
func (inc *incarnation) registerUntil(ctx context.Context, before time.Time) error {
	backoff := registerRetry
	for !inc.reregistered(before) {
		err := inc.Register(ctx)
		if err == nil {
			return nil
		}
		inc.w.logger.DebugContext(ctx, "simworker: re-registration failed; retrying", slog.Any("error", err))
		if !sleep.For(ctx, backoff) {
			return fmt.Errorf("simworker: re-registering after disconnect: %w", err)
		}
		backoff = min(backoff*2, registerMaxGap)
	}
	return nil
}

// launch connects and registers a new process, then starts its loops.
func (w *Worker) launch(ctx context.Context) (*incarnation, error) {
	nc, closed, err := w.connect(ctx)
	if err != nil {
		return nil, err
	}
	reg, err := w.registrar(nc)
	if err != nil {
		nc.Close()
		return nil, err
	}
	inc := &incarnation{w: w, instanceID: reg.InstanceID(), nc: nc, reg: reg, ended: make(chan struct{})}
	nc.SetReconnectHandler(inc.onReconnect)
	if err := inc.Register(ctx); err != nil {
		nc.Close()
		return nil, err
	}
	w.instance.Store(&inc.instanceID)

	id, g := w.spec.ID, w.spec.Group
	st := status.New(nc, status.Config{WorkerID: id}, w.logger)
	logs := logstreamer.New(nc, id, logstreamer.DefaultConfig(), w.logger)
	d := NewDispatcher(id, st, logs, w.behavior, w.ledger, w.rng)
	d.instanceID = inc.instanceID
	d.logger = w.logger
	if _, err := cancel.Subscribe(nc, id, d, w.logger); err != nil {
		nc.Close()
		return nil, err
	}
	d.SetProgress(openjd.New(logs, nc, id, w.logger))
	inc.disp = d

	interval := g.Heartbeat.Duration
	if interval <= 0 {
		interval = defaultHeartbeat
	}
	hb := heartbeat.New(nc, id, g.CPU, interval, d, inc, w.logger)
	loop := lease.New(lease.NATSTransport{NC: nc}, d, lease.Config{
		QueueIDs: lease.QueueIDs(g.Queues), WorkerID: id, InstanceID: inc.instanceID,
	}, w.logger)

	hbCtx, stopHB := context.WithCancel(context.Background())
	leaseCtx, stopLease := context.WithCancel(hbCtx)
	inc.stopHB, inc.stopLease = stopHB, stopLease
	inc.hbDone, inc.leaseDone = make(chan struct{}), make(chan struct{})
	go func() {
		defer close(inc.hbDone)
		hb.Run(hbCtx)
	}()
	go func() {
		defer close(inc.leaseDone)
		loop.Run(leaseCtx)
	}()
	go w.watchClosed(inc, closed) //nolint:gosec // G118: it watches the connection for the process lifetime, not the Start call
	return inc, nil
}

// watchClosed crashes inc if its connection closes for good without the
// worker tearing it down: natsclient gave up after a credential was refused
// past its grace (reconnects are unlimited, so they are never exhausted). A
// worker left running on a closed connection would neither heartbeat nor
// lease, yet still count as live.
func (w *Worker) watchClosed(inc *incarnation, closed <-chan struct{}) {
	select {
	case <-inc.ended:
		return
	case <-closed:
	}
	w.opMu.Lock()
	defer w.opMu.Unlock()
	if w.cur != inc {
		return // torn down meanwhile, on purpose
	}
	w.logger.WarnContext(context.Background(), "simworker: connection closed for good; treating it as a crash")
	inc.crash()
	w.setCur(nil)
}

// connect dials the broker through natsclient exactly as sqi-worker does, with
// or without a credential, so the inbox prefix, reconnect backoff and
// lifecycle handling are the real ones. The returned channel closes when the
// connection closes for good.
func (w *Worker) connect(ctx context.Context) (*nats.Conn, <-chan struct{}, error) {
	s := w.spec
	cfg := s.TLS.NATSConfig()
	cfg.URL = s.NATSURL
	cfg.MaxReconnectAttempts = -1
	cfg.ReconnectWait = reconnectWait
	nc, closed, err := natsclient.Connect(ctx, cfg, s.ID, s.Seed, s.PublicKey, w.logger)
	if err != nil {
		return nil, nil, fmt.Errorf("simworker: connect: %w", err)
	}
	return nc, closed, nil
}

// registrar builds the real registrar for one process of this worker: its
// group's queues, cores, memory, tags and OS, the default EXPR limits, and its
// own hostname rather than the generator host's.
func (w *Worker) registrar(nc *nats.Conn) (*registration.Registrar, error) {
	s, g := w.spec, w.spec.Group
	osName := g.OS
	if osName == "" {
		osName = defaultOS
	}
	settings := workerconfig.WorkerSettings{FarmID: s.FarmID, QueueIDs: g.Queues, Name: s.Hostname}
	caps := capabilities.Capabilities{OS: osName, Arch: simArch, CPUCount: g.CPU, RAMMb: g.RAMMb, Tags: g.Tags}
	reg, err := registration.New(nc, s.ID, settings, fmtres.DefaultExprLimits(), caps, w.logger, registration.WithHost(s.Hostname, ""), registration.WithInterfaces(nil))
	if err != nil {
		return nil, fmt.Errorf("simworker: %w", err)
	}
	return reg, nil
}

// Register publishes this process's registration through the real registrar.
// It implements heartbeat.Registrar, whose watchdog calls it after a
// reconnect the reconnect handler did not cover.
func (inc *incarnation) Register(ctx context.Context) error {
	if fault := inc.w.registerFault.Load(); fault != nil {
		if err := (*fault)(); err != nil {
			return err
		}
	}
	return inc.reg.Register(ctx)
}

// LastRegisteredAt implements heartbeat.Registrar.
func (inc *incarnation) LastRegisteredAt() time.Time { return inc.reg.LastRegisteredAt() }

// onReconnect re-registers with the same instance ID after NATS reconnects.
// It stands in for Registrar.SetupReconnectHook so the test fault applies to
// this path too.
func (inc *incarnation) onReconnect(*nats.Conn) {
	if err := inc.Register(context.Background()); err != nil {
		inc.w.logger.WarnContext(context.Background(), "simworker: re-registration after reconnect failed", slog.Any("error", err))
	}
}

// stop is the graceful shutdown behind Worker.Stop.
func (inc *incarnation) stop(ctx context.Context) {
	close(inc.ended)
	inc.stopLease()
	<-inc.leaseDone
	if !inc.disp.wait(ctx) {
		inc.disp.shutdown(endShutdown)
		inc.disp.wait(context.Background())
	}
	inc.disp.shutdown(endShutdown) // refuses any late dispatch
	inc.stopHB()
	<-inc.hbDone
	inc.reg.Deregister("graceful shutdown", "") // best effort, logged, as in sqi-worker
	inc.nc.Close()                              // flushes the deregistration and any terminal statuses
}

// crash is the abrupt end behind Crash and Restart: loops stop, attempts are
// abandoned without a terminal status, and the connection closes with no
// deregistration.
//
// The socket is not severed. Close flushes the connection's buffer, so every
// message a task handed to the connection before the crash still goes out: a
// terminal status already being published (the ledger records that attempt as
// terminal, not crashed), a running status from a task that started as the
// crash began, and the log lines the log publisher still held, which FlushLogs
// drains while stopping its per-attempt goroutine. The ledger therefore
// records no status the server did not get, and misses none it did.
func (inc *incarnation) crash() {
	close(inc.ended)
	inc.stopHB() // also stops the lease loop
	<-inc.leaseDone
	<-inc.hbDone
	inc.disp.shutdown(endCrashed)
	inc.disp.wait(context.Background())
	inc.nc.Close()
}
