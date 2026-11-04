// SPDX-License-Identifier: AGPL-3.0-or-later

package simworker

import (
	"context"
	"crypto/tls"
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

	"github.com/uberware/sqi/internal/brokerauth"
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
	TLS                  *tls.Config
	RandSeed             uint64
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
	instance atomic.Pointer[string]
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
	w.cur = inc
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
	w.cur = nil
	if inc == nil {
		return nil
	}
	return inc.stop(ctx)
}

// Crash drops the connection without deregistering. Heartbeats stop, and
// every in-flight attempt is abandoned with no status published (ledger kind
// crashed). Nothing is kept running.
func (w *Worker) Crash() {
	w.opMu.Lock()
	defer w.opMu.Unlock()
	if w.cur != nil {
		w.cur.crash()
		w.cur = nil
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
		w.cur = nil
	}
	inc, err := w.launch(ctx)
	if err != nil {
		return err
	}
	w.cur = inc
	return nil
}

// Disconnect drops the connection's socket and waits until NATS has
// reconnected it and the worker has re-registered, with the same instance ID,
// so the server keeps its attempts. The re-registration is the reconnect
// handler's, the same path a broker-side drop takes.
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
	tick := time.NewTicker(registerPoll)
	defer tick.Stop()
	for inc.registrations.Load() == before {
		select {
		case <-ctx.Done():
			return fmt.Errorf("simworker: waiting for re-registration: %w", ctx.Err())
		case <-tick.C:
		}
	}
	return nil
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

	lastRegistered atomic.Int64 // UnixNano of the last successful registration
	registrations  atomic.Int64 // successful registrations so far
}

// launch connects and registers a new process, then starts its loops.
func (w *Worker) launch(ctx context.Context) (*incarnation, error) {
	nc, err := w.connect(ctx)
	if err != nil {
		return nil, err
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("simworker: jetstream: %w", err)
	}
	inc := &incarnation{w: w, instanceID: uuid.NewString(), nc: nc, js: js}
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
	return inc, nil
}

// connect dials the broker on one connection with this worker's inbox prefix.
// With a credential and no TLS it goes through natsclient, as sqi-worker does;
// natsclient takes TLS only as files, so an in-memory TLS config dials with
// the same options directly.
func (w *Worker) connect(ctx context.Context) (*nats.Conn, error) {
	s := w.spec
	if len(s.Seed) > 0 && s.TLS == nil {
		cfg := workerconfig.NATSConfig{URL: s.NATSURL, MaxReconnectAttempts: -1, ReconnectWait: reconnectWait}
		nc, _, err := natsclient.Connect(ctx, cfg, s.ID, s.Seed, s.PublicKey, w.logger)
		if err != nil {
			return nil, fmt.Errorf("simworker: connect: %w", err)
		}
		return nc, nil
	}
	if !brokerauth.ValidWorkerIDToken(s.ID) {
		return nil, fmt.Errorf("simworker: worker id %q is not a single NATS subject token", s.ID)
	}
	opts := []nats.Option{
		nats.Name(s.Hostname),
		nats.CustomInboxPrefix(brokerauth.InboxPrefix(s.ID)),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(reconnectWait),
		nats.ReconnectJitter(reconnectWait/5, reconnectWait/5),
	}
	if s.TLS != nil {
		opts = append(opts, nats.Secure(s.TLS.Clone()))
	}
	if len(s.Seed) > 0 {
		opts = append(opts, brokerauth.NkeyOption(s.PublicKey, s.Seed))
	}
	nc, err := nats.Connect(s.NATSURL, opts...)
	if err != nil {
		return nil, fmt.Errorf("simworker: connect: %w", err)
	}
	return nc, nil
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
// abandoned unpublished, and the connection closes with no deregistration.
// Terminal statuses already being published when it starts still go out.
func (inc *incarnation) crash() {
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
