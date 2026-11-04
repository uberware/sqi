// SPDX-License-Identifier: AGPL-3.0-or-later

package simworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/loadtest/scenario"
	"github.com/uberware/sqi/internal/version"
	"github.com/uberware/sqi/internal/worker/cancel"
	workerconfig "github.com/uberware/sqi/internal/worker/config"
	"github.com/uberware/sqi/internal/worker/fmtres"
	"github.com/uberware/sqi/internal/worker/heartbeat"
	"github.com/uberware/sqi/internal/worker/lease"
	"github.com/uberware/sqi/internal/worker/logstreamer"
	"github.com/uberware/sqi/internal/worker/natsclient"
	"github.com/uberware/sqi/internal/worker/protocol"
	"github.com/uberware/sqi/internal/worker/status"
)

const (
	// reconnectWait is sqi-worker's default nats.reconnect_wait.
	reconnectWait = 2 * time.Second
	// defaultHeartbeat is sqi-worker's default heartbeat interval.
	defaultHeartbeat = 15 * time.Second
	// registerBudget bounds how long one registration waits for the
	// SQI_WORKER stream to exist (registration.RetryBudget).
	registerBudget = 30 * time.Second
	registerRetry  = 100 * time.Millisecond
	registerMaxGap = 2 * time.Second
	// registerPoll is how often Disconnect checks for the re-registration.
	registerPoll = 10 * time.Millisecond
	// handlerGrace is how long Disconnect leaves the reconnect handler to
	// re-register once the connection is back before registering itself.
	handlerGrace = time.Second
	// disconnectBound caps Disconnect: natsclient's first redial waits about
	// reconnectWait (plus jitter), and one registration may take
	// registerBudget.
	disconnectBound = 2*reconnectWait + registerBudget
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

// Worker is one simulated sqi-worker: one NATS connection, a hand-built
// registration, and the real heartbeat, lease, status, log and cancel
// packages driving a [Dispatcher]. Its lifecycle methods are serialized.
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
	return inc.stop(ctx)
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
	before := inc.registrations.Load()
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
	js         jetstream.JetStream
	disp       *Dispatcher

	stopHB, stopLease context.CancelFunc
	hbDone, leaseDone chan struct{}
	// ended is closed when this process is torn down on purpose, so the
	// closed-connection watcher can tell that from natsclient giving up.
	ended chan struct{}

	lastRegistered atomic.Int64 // UnixNano of the last successful registration
	registrations  atomic.Int64 // successful registrations so far
}

// awaitReregistration waits for a registration after the before'th, making it
// itself once the reconnect handler has had handlerGrace to.
func (inc *incarnation) awaitReregistration(ctx context.Context, before int64) error {
	tick := time.NewTicker(registerPoll)
	defer tick.Stop()
	var direct time.Time // when to stop waiting for the reconnect handler
	for inc.registrations.Load() == before {
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

// registerUntil registers, with backoff, until a registration after the
// before'th has landed (its own or anyone's) or ctx ends.
func (inc *incarnation) registerUntil(ctx context.Context, before int64) error {
	backoff := registerRetry
	for inc.registrations.Load() == before {
		err := inc.Register(ctx)
		if err == nil {
			return nil
		}
		inc.w.logger.DebugContext(ctx, "simworker: re-registration failed; retrying", slog.Any("error", err))
		if !sleepCtx(ctx, backoff) {
			return fmt.Errorf("simworker: re-registering after disconnect: %w", err)
		}
		backoff = min(backoff*2, registerMaxGap)
	}
	return nil
}

// sleepCtx waits d and reports whether ctx was still live at the end.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// launch connects and registers a new process, then starts its loops.
func (w *Worker) launch(ctx context.Context) (*incarnation, error) {
	nc, closed, err := w.connect(ctx)
	if err != nil {
		return nil, err
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("simworker: jetstream: %w", err)
	}
	inc := &incarnation{w: w, instanceID: uuid.NewString(), nc: nc, js: js, ended: make(chan struct{})}
	nc.SetReconnectHandler(inc.onReconnect)
	if err := inc.Register(ctx); err != nil {
		nc.Close()
		return nil, err
	}
	w.instance.Store(&inc.instanceID)

	id, g := w.spec.ID, w.spec.Group
	st := status.New(nc, status.Config{WorkerID: id}, w.logger)
	logs := logstreamer.New(nc, id, logstreamer.DefaultConfig(), w.logger)
	d := NewDispatcher(id, st, logs, nil, w.behavior, w.ledger, w.rng)
	d.instanceID = inc.instanceID
	d.logger = w.logger
	d.cancels = cancel.New(nc, d, w.logger)
	inc.disp = d

	interval := g.Heartbeat.Duration
	if interval <= 0 {
		interval = defaultHeartbeat
	}
	hb := heartbeat.New(nc, id, g.CPU, interval, d, inc, w.logger)
	loop := lease.New(leaseTransport{nc: nc}, d, lease.Config{
		QueueIDs: leaseQueueIDs(g.Queues), WorkerID: id, InstanceID: inc.instanceID,
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
// worker tearing it down: natsclient gave up (a credential refused past its
// grace, or reconnects exhausted). A worker left running on a closed
// connection would neither heartbeat nor lease: a zombie in the fleet count.
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
	cfg := workerconfig.NATSConfig{
		URL:                  s.NATSURL,
		TLSEnabled:           s.TLS.Enabled,
		TLSCAFile:            s.TLS.CAFile,
		TLSCertFile:          s.TLS.CertFile,
		TLSKeyFile:           s.TLS.KeyFile,
		InsecureSkipVerify:   s.TLS.InsecureSkipVerify,
		MaxReconnectAttempts: -1,
		ReconnectWait:        reconnectWait,
	}
	nc, closed, err := natsclient.Connect(ctx, cfg, s.ID, s.Seed, s.PublicKey, w.logger)
	if err != nil {
		return nil, nil, fmt.Errorf("simworker: connect: %w", err)
	}
	return nc, closed, nil
}

// leaseQueueIDs is sqi-worker's mapping: no queues means the wildcard token.
func leaseQueueIDs(queues []string) []string {
	if len(queues) == 0 {
		return []string{bus.WildcardQueueToken}
	}
	return queues
}

// registerMsg builds the registration sqi-worker's registration package would
// send for this worker's group.
func (w *Worker) registerMsg(instanceID string) protocol.RegisterMsg {
	s, g := w.spec, w.spec.Group
	var queueID string
	if len(g.Queues) == 1 {
		queueID = g.Queues[0]
	}
	osName := g.OS
	if osName == "" {
		osName = defaultOS
	}
	lim := fmtres.DefaultExprLimits()
	return protocol.RegisterMsg{
		Version:       protocol.ProtocolVersion,
		Type:          protocol.TypeRegister,
		WorkerID:      s.ID,
		InstanceID:    instanceID,
		FarmID:        s.FarmID,
		QueueID:       queueID,
		Name:          s.Hostname,
		Hostname:      s.Hostname,
		OS:            osName,
		Arch:          simArch,
		WorkerVersion: version.Version,
		CPUCount:      g.CPU,
		RAMMb:         g.RAMMb,
		Tags:          g.Tags,
		ExprLimits: protocol.ExprLimits{
			OperationLimit:          lim.OperationLimit,
			MemoryLimit:             lim.MemoryLimit,
			AssignmentPositions:     lim.AssignmentPositions,
			AssignmentRetainedBytes: lim.AssignmentRetainedBytes,
			LetRetainedBytes:        lim.LetRetainedBytes,
		},
		Extensions: protocol.WorkerEnforcedExtensions(),
	}
}

// Register publishes this process's registration and waits for the stream
// ack, retrying while the SQI_WORKER stream does not exist yet. It implements
// heartbeat.Registrar, whose watchdog calls it after a reconnect the reconnect
// handler did not cover.
func (inc *incarnation) Register(ctx context.Context) error {
	data, err := json.Marshal(inc.w.registerMsg(inc.instanceID))
	if err != nil {
		return fmt.Errorf("simworker: marshal registration: %w", err)
	}
	if fault := inc.w.registerFault.Load(); fault != nil {
		if err := (*fault)(); err != nil {
			return err
		}
	}
	ctx, cancelFn := context.WithTimeout(ctx, registerBudget)
	defer cancelFn()
	subj := bus.WorkerRegisterSubject(inc.w.spec.ID)
	backoff := registerRetry
	for {
		_, err := inc.js.Publish(ctx, subj, data)
		if err == nil {
			break
		}
		if !errors.Is(err, jetstream.ErrNoStreamResponse) {
			return fmt.Errorf("simworker: register: %w", err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("simworker: register: no stream: %w", ctx.Err())
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, registerMaxGap)
	}
	inc.lastRegistered.Store(time.Now().UnixNano())
	inc.registrations.Add(1)
	return nil
}

// LastRegisteredAt implements heartbeat.Registrar.
func (inc *incarnation) LastRegisteredAt() time.Time {
	ns := inc.lastRegistered.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

// onReconnect re-registers with the same instance ID after NATS reconnects.
func (inc *incarnation) onReconnect(*nats.Conn) {
	if err := inc.Register(context.Background()); err != nil {
		inc.w.logger.WarnContext(context.Background(), "simworker: re-registration after reconnect failed", slog.Any("error", err))
	}
}

// deregister publishes the departure, best effort, as sqi-worker does.
func (inc *incarnation) deregister(reason string) error {
	data, err := json.Marshal(protocol.DeregisterMsg{
		Version:    protocol.ProtocolVersion,
		Type:       protocol.TypeDeregister,
		WorkerID:   inc.w.spec.ID,
		InstanceID: inc.instanceID,
		Reason:     reason,
	})
	if err != nil {
		return fmt.Errorf("simworker: marshal deregistration: %w", err)
	}
	if err := inc.nc.Publish(bus.WorkerDeregisterSubject(inc.w.spec.ID), data); err != nil {
		return fmt.Errorf("simworker: deregister: %w", err)
	}
	return nil
}

// stop is the graceful shutdown behind Worker.Stop.
func (inc *incarnation) stop(ctx context.Context) error {
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
	err := inc.deregister("graceful shutdown")
	inc.nc.Close() // flushes the deregistration and any terminal statuses
	return err
}

// crash is the abrupt end behind Crash and Restart: loops stop, attempts are
// abandoned without a terminal status, and the connection closes with no
// deregistration.
//
// It is not a severed socket, and deliberately so. Every message a task
// handed to the connection before the crash still goes out, because Close
// flushes the connection's buffer: a terminal status already being published
// (the ledger records that attempt as terminal, not crashed), a running
// status from a task that started as the crash began, and the log lines the
// log publisher still held, which FlushLogs drains while stopping its
// per-attempt goroutine. Keeping them means the ledger never claims a status
// the server did not get, and never misses one it did.
func (inc *incarnation) crash() {
	close(inc.ended)
	inc.stopHB() // also stops the lease loop
	<-inc.leaseDone
	<-inc.hbDone
	inc.disp.shutdown(endCrashed)
	inc.disp.wait(context.Background())
	inc.nc.Close()
}

// leaseTransport adapts a connection to lease.Transport, as sqi-worker's does.
type leaseTransport struct {
	nc *nats.Conn
}

// RequestLease asks for work on the worker's lease subject for queueID.
func (t leaseTransport) RequestLease(ctx context.Context, workerID, queueID string, data []byte, timeout time.Duration) ([]byte, error) {
	reqCtx, cancelReq := context.WithTimeout(ctx, timeout)
	defer cancelReq()
	msg, err := t.nc.RequestWithContext(reqCtx, bus.WorkLeaseSubject(workerID, queueID), data)
	if err != nil {
		return nil, fmt.Errorf("simworker: request lease for queue %q: %w", queueID, err)
	}
	return msg.Data, nil
}
