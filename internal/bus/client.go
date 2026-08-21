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

	// mu guards cons; all other fields are immutable after construction.
	mu   sync.Mutex
	cons []jetstream.ConsumeContext // push consumers tracked for drain
}

// NewClient dials the embedded NATS server at url and returns a connected
// Client.  The connection is configured for unlimited reconnects with
// exponential backoff, so transient disruptions (e.g. a brief broker restart
// during development) recover automatically.
//
// extraOpts are appended after the default options, so a caller can supply
// authentication (e.g. an nkey credential) without disturbing the reconnect
// behavior above.
//
// Prefer [Broker.NewClient] over calling NewClient directly; the Broker
// supplies the correct loopback URL without the caller needing to know it.
func NewClient(url string, logger *slog.Logger, extraOpts ...nats.Option) (*Client, error) {
	opts := []nats.Option{
		// Reconnect indefinitely — this is an in-process loopback connection
		// and should always come back up if the embedded broker restarts.
		nats.MaxReconnects(-1),

		// Wait 2 s between reconnect attempts.  The embedded broker is on
		// loopback so it recovers quickly; a short fixed interval is sufficient.
		nats.ReconnectWait(2 * time.Second),

		// Log reconnect events through the server's structured logger so
		// operators can correlate them with other activity.
		nats.ReconnectHandler(func(nc *nats.Conn) {
			logger.InfoContext(context.Background(), "bus: client reconnected", slog.String("url", nc.ConnectedUrl()))
		}),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			if err != nil {
				logger.WarnContext(context.Background(), "bus: client disconnected", slog.Any("error", err))
			}
		}),
		nats.ClosedHandler(func(_ *nats.Conn) {
			logger.InfoContext(context.Background(), "bus: client connection closed")
		}),
	}
	opts = append(opts, extraOpts...)

	nc, err := nats.Connect(url, opts...)
	if err != nil {
		return nil, fmt.Errorf("bus: client connect %q: %w", url, err)
	}

	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("bus: client jetstream context: %w", err)
	}

	return &Client{nc: nc, js: js, logger: logger}, nil
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
// be delivered).  It respects the given context deadline.
//
// Call Drain before [Client.Close] or [Broker.Shutdown] to guarantee a clean
// shutdown — no in-flight messages are dropped and no pending publishes are
// lost.  After Drain returns, no further messages will arrive at handlers.
func (c *Client) Drain(ctx context.Context) error {
	c.mu.Lock()
	cons := c.cons
	c.cons = nil
	c.mu.Unlock()

	// Drain consumers and the NATS connection in a single goroutine so that
	// the caller's context deadline is honored without blocking here.
	//
	// Consumer.Drain() stops new message delivery and waits for any in-flight
	// handler invocations to complete before returning.  nc.Drain() then
	// flushes pending JetStream publish-acks and closes the connection.
	done := make(chan error, 1)
	go func() {
		for _, cc := range cons {
			cc.Drain()
		}
		done <- c.nc.Drain()
	}()

	select {
	case err := <-done:
		if err != nil && !errors.Is(err, nats.ErrConnectionClosed) {
			return fmt.Errorf("bus: drain: %w", err)
		}
		c.logger.InfoContext(ctx, "bus: client drained cleanly")
		return nil
	case <-ctx.Done():
		// Context expired before drain finished — stop remaining consumers
		// immediately and force-close the connection so the broker can shut down.
		c.logger.WarnContext(ctx, "bus: drain timed out — forcing close", slog.Any("error", ctx.Err()))
		for _, cc := range cons {
			cc.Stop()
		}
		c.nc.Close()
		return fmt.Errorf("bus: drain timed out: %w", ctx.Err())
	}
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
