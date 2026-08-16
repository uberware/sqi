// SPDX-License-Identifier: AGPL-3.0-or-later

// Package registration manages worker registration with sqi-server over NATS.
//
// # Lifecycle
//
// A [Registrar] is created once at worker startup, after the NATS connection
// is established. Its [Registrar.Register] method publishes a [protocol.RegisterMsg]
// to the worker.register JetStream subject. On graceful shutdown,
// [Registrar.Deregister] publishes a [protocol.DeregisterMsg] to
// worker.deregister so the server marks the worker offline immediately.
//
// # Re-registration
//
// [Registrar.SetupReconnectHook] installs a NATS reconnect callback that
// re-publishes the registration message whenever the connection is restored
// after a drop. This ensures the server always has a current, online record
// for the worker without requiring a process restart.
//
// # Acknowledgment
//
// Registration is published with a JetStream ack, so [Registrar.Register]
// returns only once the SQI_WORKER stream has durably stored the message. A
// plain core-NATS publish would be silently discarded when no stream is behind
// the subject — which strands the worker permanently, since the server only
// learns of it via worker.register and NAKs the heartbeats of workers it has no
// record of.
//
// That "no stream yet" window is real and routinely hit: a worker started
// alongside its server (as in a compose stack) connects as soon as the NATS
// listener opens, which is a few milliseconds before the server finishes
// provisioning its streams. Register therefore retries while the stream is
// absent, up to [RetryBudget] or the caller's context deadline, whichever is
// sooner.
//
// The server processes worker.register via a JetStream push consumer and does
// not send a reply of its own; the stream ack is the acknowledgment. Explicit
// application-level accept/reject is a planned protocol enhancement.
//
// # In-memory capability map
//
// The Registrar stores the merged [capabilities.Capabilities] for the lifetime
// of the process. Heartbeat publishers and log formatters access it via
// [Registrar.Capabilities] so they can include capability context in their
// output without re-detecting hardware on every call.
package registration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync/atomic"
	"time"

	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/version"
	"github.com/uberware/sqi/internal/worker/capabilities"
	workerconfig "github.com/uberware/sqi/internal/worker/config"
	"github.com/uberware/sqi/internal/worker/fmtres"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// Retry pacing for the registration publish. The wait between attempts starts
// at retryInitial and doubles up to retryMax; RetryBudget caps the total time
// Register spends waiting for the SQI_WORKER stream to appear.
//
// RetryBudget is generous relative to the window it covers (a server provisions
// its streams milliseconds after opening its NATS listener). It exists to
// outlast a slow or restarting server, not to be reached: a worker that cannot
// register is useless, so waiting beats exiting.
const (
	RetryBudget  = 30 * time.Second
	retryInitial = 100 * time.Millisecond
	retryMax     = 2 * time.Second
)

// Registrar publishes worker registration and deregistration messages and
// stores the worker's capability map in memory for the process lifetime.
type Registrar struct {
	nc *nats.Conn

	// js publishes registration with a stream ack. Deregister deliberately
	// stays on the core-NATS path: it is best-effort by design and runs during
	// shutdown, where blocking on an ack would delay exit for a message the
	// heartbeat-timeout sweep makes redundant anyway.
	js jetstream.JetStream

	workerID string
	cfg      workerconfig.WorkerSettings

	// exprLimits are the EXPR evaluation caps this worker enforces, advertised
	// in every RegisterMsg so the server can refuse to send it work it cannot
	// run (see [protocol.ExprLimits]). Normalized at construction so a zero
	// value advertises the defaults it would actually enforce rather than
	// "unadvertised".
	exprLimits fmtres.ExprLimits

	caps   capabilities.Capabilities
	logger *slog.Logger

	// lastRegisteredAt records the Unix nanosecond timestamp of the most
	// recent successful Register call. Stored atomically so the heartbeat
	// watchdog can read it without a mutex.
	lastRegisteredAt atomic.Int64
}

// New creates a Registrar over an established NATS connection. caps should
// already have manual tags merged in (via
// [capabilities.Capabilities.MergeManualTags]) before being passed here.
//
// exprLimits must be THE SAME value the worker's session manager enforces
// with, not a second mapping of the same configuration: what this worker
// advertises to the server is what the server assumes it will enforce, and a
// transposition between the two would make the server's dispatch gate compare
// against a number no evaluation ever uses. cmd/sqi-worker passes
// exprLimitsFromConfig(cfg.Expr) to both.
//
// It returns an error only if a JetStream context cannot be derived from nc.
func New(
	nc *nats.Conn,
	workerID string,
	cfg workerconfig.WorkerSettings,
	exprLimits fmtres.ExprLimits,
	caps capabilities.Capabilities,
	logger *slog.Logger,
) (*Registrar, error) {
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("registration: jetstream context: %w", err)
	}
	return &Registrar{
		nc:         nc,
		js:         js,
		workerID:   workerID,
		cfg:        cfg,
		exprLimits: exprLimits.Normalized(),
		caps:       caps,
		logger:     logger,
	}, nil
}

// Register publishes a RegisterMsg to worker.register and returns once the
// stream has acked it. It is safe to call multiple times (at boot and on NATS
// reconnect).
//
// It blocks while the SQI_WORKER stream does not exist yet, returning an error
// once [RetryBudget] or ctx expires (see package-level documentation).
func (r *Registrar) Register(ctx context.Context) error {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "" // best-effort; server accepts empty hostname
	}
	ip := outboundIP()

	// QueueID is set only when the worker is restricted to exactly one queue.
	// A wildcard worker (no queue_ids) or a multi-queue worker leaves it empty
	// so the scheduler treats it as "accepts all queues".
	var queueID string
	if len(r.cfg.QueueIDs) == 1 {
		queueID = r.cfg.QueueIDs[0]
	}

	msg := protocol.RegisterMsg{
		Version:         protocol.ProtocolVersion,
		Type:            protocol.TypeRegister,
		WorkerID:        r.workerID,
		FarmID:          r.cfg.FarmID,
		QueueID:         queueID,
		Name:            r.cfg.Name,
		Hostname:        hostname,
		IPAddress:       ip,
		ComputeLocation: r.cfg.ComputeLocation,
		OS:              r.caps.OS,
		Arch:            r.caps.Arch,
		OSVersion:       r.caps.OSVersion,
		WorkerVersion:   version.Version,
		CPUCount:        r.caps.CPUCount,
		RAMMb:           r.caps.RAMMb,
		GPUInfo: protocol.GPUInfo{
			Vendor: r.caps.GPU.Vendor,
			Model:  r.caps.GPU.Model,
			VRAMMb: r.caps.GPU.VRAMMb,
			Count:  r.caps.GPU.Count,
		},
		Tags: r.caps.Tags,
		ExprLimits: protocol.ExprLimits{
			OperationLimit:          r.exprLimits.OperationLimit,
			MemoryLimit:             r.exprLimits.MemoryLimit,
			AssignmentPositions:     r.exprLimits.AssignmentPositions,
			AssignmentRetainedBytes: r.exprLimits.AssignmentRetainedBytes,
			LetRetainedBytes:        r.exprLimits.LetRetainedBytes,
		},
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("registration: marshal RegisterMsg: %w", err)
	}

	if err := r.publishRegister(ctx, data); err != nil {
		return err
	}

	// Record successful registration time so the heartbeat watchdog can
	// determine whether it needs to re-register after a reconnect (i.e.,
	// whether the reconnect callback already handled it).
	r.lastRegisteredAt.Store(time.Now().UnixNano())

	r.logger.InfoContext(
		ctx, "registration: worker registered",
		slog.String("worker_id", r.workerID),
		slog.String("farm_id", r.cfg.FarmID),
		slog.String("queue_id", queueID),
		slog.String("name", r.cfg.Name),
		slog.String("hostname", hostname),
		slog.String("compute_location", r.cfg.ComputeLocation),
		slog.String("os", r.caps.OS),
		slog.Int("cpu_count", r.caps.CPUCount),
		slog.Int("ram_mb", r.caps.RAMMb),
		slog.Int("tag_count", len(r.caps.Tags)),
	)
	return nil
}

// publishRegister publishes data to worker.register and waits for the stream to
// ack it, retrying while the SQI_WORKER stream does not exist yet.
//
// Only a missing stream is retried. Every other failure (a closed connection,
// say) is returned immediately: it will not resolve by waiting.
func (r *Registrar) publishRegister(ctx context.Context, data []byte) error {
	ctx, cancel := context.WithTimeout(ctx, RetryBudget)
	defer cancel()

	backoff := retryInitial
	for attempt := 1; ; attempt++ {
		_, err := r.js.Publish(ctx, bus.SubjectWorkerRegister, data)
		if err == nil {
			return nil
		}
		if !errors.Is(err, jetstream.ErrNoStreamResponse) {
			return fmt.Errorf("registration: publish to %s: %w", bus.SubjectWorkerRegister, err)
		}

		r.logger.DebugContext(
			ctx, "registration: no stream for worker.register yet — retrying",
			slog.String("worker_id", r.workerID),
			slog.Int("attempt", attempt),
			slog.Duration("retry_in", backoff),
		)

		select {
		case <-ctx.Done():
			return fmt.Errorf(
				"registration: publish to %s: no stream after %d attempts (is the server provisioning JetStream?): %w",
				bus.SubjectWorkerRegister, attempt, ctx.Err(),
			)
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, retryMax)
	}
}

// Deregister publishes a DeregisterMsg to worker.deregister on graceful
// shutdown so the server marks this worker offline immediately rather than
// waiting for the heartbeat timeout sweep.
//
// Deregister is best-effort: failures are logged as warnings since the server
// will detect the absence via heartbeat timeout regardless. The caller is
// responsible for flushing any buffered publishes before closing the NATS
// connection (typically via natsclient.Drain deferred in start.go).
func (r *Registrar) Deregister(reason string) {
	ctx := context.Background()

	msg := protocol.DeregisterMsg{
		Version:  protocol.ProtocolVersion,
		Type:     protocol.TypeDeregister,
		WorkerID: r.workerID,
		Reason:   reason,
	}

	data, err := json.Marshal(msg)
	if err != nil {
		r.logger.WarnContext(ctx, "registration: marshal DeregisterMsg failed",
			slog.Any("error", err))
		return
	}

	if err := r.nc.Publish(bus.SubjectWorkerDeregister, data); err != nil {
		r.logger.WarnContext(ctx, "registration: deregister publish failed — server will detect absence via heartbeat timeout",
			slog.String("worker_id", r.workerID),
			slog.Any("error", err))
		return
	}

	r.logger.InfoContext(ctx, "registration: worker deregistered",
		slog.String("worker_id", r.workerID),
		slog.String("reason", reason))
}

// SetupReconnectHook installs a NATS reconnect handler that re-publishes the
// registration message whenever the connection is restored. It replaces any
// previously set ReconnectHandler on the connection, so callers must install
// this after any other reconnect hooks they require.
//
// Re-registration failures are logged but do not cause a fatal error; the
// server's heartbeat sweep will time out the stale worker record regardless
// and the next successful heartbeat will re-establish liveness.
func (r *Registrar) SetupReconnectHook(_ context.Context) {
	r.nc.SetReconnectHandler(func(conn *nats.Conn) {
		// Use context.Background() rather than the caller's context: the
		// reconnect callback fires asynchronously (possibly after the caller's
		// ctx is already canceled during shutdown) and must not be gated on
		// the signal-notification context lifetime.
		ctx := context.Background()
		r.logger.InfoContext(
			ctx, "natsclient: reconnected — re-registering worker",
			slog.String("url", conn.ConnectedUrl()),
			slog.String("server_id", conn.ConnectedServerId()),
			slog.String("worker_id", r.workerID),
		)
		if err := r.Register(ctx); err != nil {
			r.logger.ErrorContext(
				ctx, "registration: re-registration after reconnect failed",
				slog.String("worker_id", r.workerID),
				slog.Any("error", err),
			)
		}
	})
}

// LastRegisteredAt returns the time of the most recent successful Register
// call, or the zero time if Register has never succeeded in this process
// lifetime. Safe for concurrent use; the heartbeat watchdog uses this to
// determine whether it needs to re-register after a reconnect.
func (r *Registrar) LastRegisteredAt() time.Time {
	ns := r.lastRegisteredAt.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

// Capabilities returns the merged capability set stored at construction time.
// The returned value is a copy; mutations do not affect the Registrar's state.
func (r *Registrar) Capabilities() capabilities.Capabilities {
	return r.caps
}

// outboundIP returns the preferred outbound IP address of the host as a string.
// It performs a UDP "connect" (no packets are sent) to determine which local
// interface would be used to reach an external address.
// Returns an empty string if the address cannot be determined.
func outboundIP() string {
	// Use DialContext with a background context so the noctx linter is
	// satisfied. No packets are sent — this is a kernel route-table lookup
	// that tells us which local IP the OS would use to reach an external host.
	conn, err := (&net.Dialer{}).DialContext(context.Background(), "udp", "8.8.8.8:53")
	if err != nil {
		return ""
	}
	defer conn.Close()
	localAddr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return ""
	}
	return localAddr.IP.String()
}
