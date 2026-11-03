// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/uberware/sqi/internal/redact"
)

// Client is the typed in-process NATS client used by all sqi-server code.
//
// Rather than importing the raw nats package directly, every component that
// needs to publish or subscribe goes through Client.  This indirection keeps
// the raw nats API out of callers, making it easy to add instrumentation,
// swap transport, or substitute a [FakeClient] in unit tests.
//
// Obtain a Client via [Broker.NewClient] after the broker has been started.
// A Client should be shared across goroutines — its methods are safe for
// concurrent use.
type Client struct {
	nc     *nats.Conn
	js     jetstream.JetStream
	logger *slog.Logger

	// stateMu guards upSince and events. See [Client.BusUpSince].
	stateMu sync.Mutex
	// upSince is when the connection was last established or re-established;
	// zero while it is down.
	upSince time.Time
	// events counts the connection handlers that have run, so the constructor
	// can tell that one fired between nats.Connect returning and its own
	// bookkeeping, and not undo it.
	events int

	// mu guards cons; all other fields are immutable after construction.
	mu   sync.Mutex
	cons []jetstream.ConsumeContext // push consumers tracked for drain
}

// BusUpSince returns when the client's connection was last established or
// re-established, and the zero time while it is disconnected. Safe for
// concurrent use.
//
// The scheduler uses it to avoid judging workers by heartbeats that could not
// have reached it: a heartbeat travels over the bus, so a broker outage looks
// exactly like every worker going silent at once.
func (c *Client) BusUpSince() time.Time {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.upSince
}

// markUp records a (re)connect.
func (c *Client) markUp() {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	c.events++
	c.upSince = time.Now().UTC()
}

// markDown records a disconnect.
func (c *Client) markDown() {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	c.events++
	c.upSince = time.Time{}
}

// markInitialUp records the first connection, unless a connection handler has
// already run: whatever it recorded is newer than this connect.
func (c *Client) markInitialUp() {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if c.events == 0 {
		c.upSince = time.Now().UTC()
	}
}

// NewClient dials the broker at url, the embedded server in-process or an
// external cluster, and returns a connected Client. The connection reconnects
// without limit every 2 s, so a broker restart or a brief partition recovers
// on its own.
//
// extraOpts are appended after the default options, so a caller can supply
// authentication (e.g. an nkey credential) without disturbing the reconnect
// behavior above. NewClient owns the reconnect and disconnect handlers, which
// maintain [Client.BusUpSince]: one passed in extraOpts is replaced.
//
// Prefer [Broker.NewClient] over calling NewClient directly; the Broker
// supplies the URL, credential and TLS options for its mode.
func NewClient(url string, logger *slog.Logger, extraOpts ...nats.Option) (*Client, error) {
	// c's connection fields are set once nats.Connect returns; the handlers
	// below use only its connection-state fields.
	c := &Client{logger: logger}
	opts := []nats.Option{
		// Reconnect indefinitely: the server has no use without its bus, so
		// the connection must come back whenever the broker does.
		nats.MaxReconnects(-1),

		// Wait 2 s between reconnect attempts. A short fixed interval is
		// enough for sqi-server's own few connections; the reconnect grace
		// the scheduler applies afterwards covers the workers' slower redials.
		nats.ReconnectWait(2 * time.Second),

		// Detect a silent partition (a black-holed link, a dead broker host)
		// in about 15 s: the scheduler defers its liveness sweeps only while
		// BusUpSince is zero, and the default of two minutes of pings would
		// leave it reporting "up" while every heartbeat is being dropped.
		nats.PingInterval(5 * time.Second),
		nats.MaxPingsOutstanding(2),

		nats.ClosedHandler(func(_ *nats.Conn) {
			logger.InfoContext(context.Background(), "bus: client connection closed")
		}),
	}
	opts = append(opts, extraOpts...)
	// The handlers that log through the server's structured logger and
	// maintain BusUpSince come last: nats.go keeps only the last handler of
	// each kind, so a caller's own would otherwise silence BusUpSince.
	opts = append(
		opts,
		nats.ReconnectHandler(func(nc *nats.Conn) {
			c.markUp()
			logger.InfoContext(context.Background(), "bus: client reconnected", slog.String("url", redact.URL(nc.ConnectedUrl())))
		}),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			c.markDown()
			if err != nil {
				logger.WarnContext(context.Background(), "bus: client disconnected", slog.Any("error", err))
			}
		}),
	)

	nc, err := nats.Connect(url, opts...)
	if err != nil {
		return nil, fmt.Errorf("bus: client connect %q: %w", redact.List(url), err)
	}

	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("bus: client jetstream context: %w", err)
	}

	c.nc, c.js = nc, js
	if nc.IsConnected() {
		c.markInitialUp()
	}
	return c, nil
}

// ── Publish methods ──────────────────────────────────────────────────────────

// PublishTaskStatus publishes a task-status transition to the
// task.status.<workerID>.<jobID> subject.  Workers call this when a task
// transitions to running, succeeded, failed, or canceled.
func (c *Client) PublishTaskStatus(ctx context.Context, workerID, jobID string, data []byte) error {
	return c.publish(ctx, TaskStatusSubject(workerID, jobID), data)
}

// PublishTaskLog publishes a log chunk to the task.logs.<workerID>.<taskID>
// subject.  Workers call this continuously as a task produces output; the
// server retains chunks in JetStream for later retrieval via the REST and
// WebSocket APIs.
func (c *Client) PublishTaskLog(ctx context.Context, workerID, taskID string, data []byte) error {
	return c.publish(ctx, TaskLogsSubject(workerID, taskID), data)
}

// PublishWorkerHeartbeat publishes a heartbeat ping on the
// worker.heartbeat.<workerID> subject.  Workers call this on a regular
// interval; the server-side heartbeat sweep marks workers offline when pings
// stop.
func (c *Client) PublishWorkerHeartbeat(ctx context.Context, workerID string, data []byte) error {
	return c.publish(ctx, WorkerHeartbeatSubject(workerID), data)
}

// PublishWorkerRegister publishes a capability advertisement on the
// worker.register.<workerID> subject.  Workers call this on first connect and
// after any reconnect so the server always has a current view of their
// capabilities.
func (c *Client) PublishWorkerRegister(ctx context.Context, workerID string, data []byte) error {
	return c.publish(ctx, WorkerRegisterSubject(workerID), data)
}

// PublishTaskCancel publishes a task-cancellation signal to the
// task.cancel.<taskID> subject. The scheduler calls this for each
// task that was actively assigned or running at the time a job is canceled.
// The worker holding the task consumes this message and interrupts the running
// process without waiting for the next heartbeat timeout.
func (c *Client) PublishTaskCancel(ctx context.Context, taskID string, data []byte) error {
	return c.publish(ctx, TaskCancelSubject(taskID), data)
}

// publish is the shared JetStream publish path.  Using jetstream.Publish
// (rather than nats.Conn.Publish) ensures the message is durably written to
// the stream and the server receives an ack before the call returns.
func (c *Client) publish(ctx context.Context, subject string, data []byte) error {
	if _, err := c.js.Publish(ctx, subject, data); err != nil {
		return fmt.Errorf("bus: publish %s: %w", subject, err)
	}
	return nil
}

// ── Drain and close ──────────────────────────────────────────────────────────

// Drain stops all push consumers started by this Client, waits for in-flight
// message handlers to finish, and then drains the underlying NATS connection
// (flushing pending publish-acks and waiting for all outstanding messages to
// be delivered) and waits for it to close. It respects the given context
// deadline.
//
// [jetstream.ConsumeContext.Drain] and [nats.Conn.Drain] only start a drain:
// both return at once and finish in the background. Drain waits for each
// consumer's Closed channel and then for the connection to reach CLOSED, so
// that when it returns the buffered messages have been handled. The server
// shuts the broker down, and on an external broker releases its account
// guard, as soon as Drain returns.
//
// Call Drain before [Client.Close] or [Broker.Shutdown] to guarantee a clean
// shutdown — no in-flight messages are dropped and no pending publishes are
// lost.  After Drain returns, no further messages will arrive at handlers.
// When ctx ends first, Drain stops the consumers, closes the connection and
// returns an error.
func (c *Client) Drain(ctx context.Context) error {
	c.mu.Lock()
	cons := c.cons
	c.cons = nil
	c.mu.Unlock()

	if err := c.drain(ctx, cons); err != nil {
		if ctx.Err() == nil {
			return err
		}
		// Context expired before drain finished — stop remaining consumers
		// immediately and force-close the connection so the broker can shut down.
		c.logger.WarnContext(ctx, "bus: drain timed out — forcing close", slog.Any("error", ctx.Err()))
		for _, cc := range cons {
			cc.Stop()
		}
		c.nc.Close()
		return fmt.Errorf("bus: drain timed out: %w", ctx.Err())
	}
	c.logger.InfoContext(ctx, "bus: client drained cleanly")
	return nil
}

// drain drains cons, then the connection, waiting for each to finish. It
// returns ctx's error when ctx ends first.
func (c *Client) drain(ctx context.Context, cons []jetstream.ConsumeContext) error {
	// ConsumeContext.Drain stops new deliveries; the messages already
	// buffered are still handed to the handler, and Closed closes once that
	// is done.
	for _, cc := range cons {
		cc.Drain()
	}
	for _, cc := range cons {
		select {
		case <-cc.Closed():
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	// Registered before Drain so the close cannot be missed; a connection
	// that was already closed sends nothing, which IsClosed covers.
	closed := c.nc.StatusChanged(nats.CLOSED)
	defer c.nc.RemoveStatusListener(closed)
	if err := c.nc.Drain(); err != nil && !errors.Is(err, nats.ErrConnectionClosed) {
		return fmt.Errorf("bus: drain: %w", err)
	}
	if !c.nc.IsClosed() {
		select {
		case <-closed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := c.nc.LastError(); errors.Is(err, nats.ErrDrainTimeout) {
		return fmt.Errorf("bus: drain: %w", err)
	}
	return nil
}

// Close closes the underlying NATS connection immediately without draining.
// Prefer [Client.Drain] for graceful shutdown; use Close only when draining
// has already completed or is not possible.
func (c *Client) Close() {
	if c.nc != nil && !c.nc.IsClosed() {
		c.nc.Close()
	}
}

// trackConsumer registers a ConsumeContext so it is stopped during [Drain].
// Called internally by consumer-creation helpers in consumers.go.
func (c *Client) trackConsumer(cc jetstream.ConsumeContext) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cons = append(c.cons, cc)
}
