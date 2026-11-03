// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// ── Pull tuning shared by every server-side consumer ──────────────────────────

// consumePullExpiry is how long each pull request a consume context issues
// stays open on the broker before it is renewed.
const consumePullExpiry = 10 * time.Second

// consumeHeartbeat is the idle heartbeat the broker sends on an open pull.
// The consume context re-pulls once it misses two in a row, so a pull lost
// while the consumer had no leader (a broker restart, a node loss) is
// replaced within about 2×consumeHeartbeat. With the library defaults it took
// up to ~30s, longer than a worker's heartbeat timeout. The check is paused
// while a handler runs, so a slow handler does not trip it.
const consumeHeartbeat = 2 * time.Second

// consume starts consumer with sqi's pull tuning and an error handler that
// logs every interruption under the consumer's name.
func (c *Client) consume(consumer jetstream.Consumer, name string, handler jetstream.MessageHandler) (jetstream.ConsumeContext, error) {
	return consumer.Consume(
		handler,
		jetstream.PullExpiry(consumePullExpiry),
		jetstream.PullHeartbeat(consumeHeartbeat),
		jetstream.ConsumeErrHandler(c.consumeErrHandler(name)),
	)
}

// consumeErrHandler logs the errors a consume context reports. Without it the
// library swallows them, and a stalled consumer is silent.
func (c *Client) consumeErrHandler(name string) jetstream.ConsumeErrHandlerFunc {
	return func(_ jetstream.ConsumeContext, err error) {
		ctx := context.Background()
		attrs := []any{slog.String("consumer", name), slog.Any("error", err)}
		switch {
		case errors.Is(err, jetstream.ErrConnectionClosed), errors.Is(err, nats.ErrConnectionClosed):
			// Every Close and Drain ends here; it is not a fault.
			c.logger.DebugContext(ctx, "bus: consumer stopped with its connection", attrs...)
		case errors.Is(err, jetstream.ErrNoHeartbeat),
			errors.Is(err, nats.ErrNoResponders),
			errors.Is(err, jetstream.ErrConsumerLeadershipChanged):
			c.logger.WarnContext(ctx, "bus: consumer pull interrupted; pulling again", attrs...)
		default:
			c.logger.WarnContext(ctx, "bus: consumer error", attrs...)
		}
	}
}

// ── Consumer name helpers ─────────────────────────────────────────────────────

// taskStatusConsumerName is the durable name for the server-side push consumer
// that processes task-status transitions.  One server process runs this
// consumer at a time: in external mode the guard ([AcquireGuard]) refuses a
// second sqi-server on the same broker account, and the embedded broker is
// private to its one server.
const taskStatusConsumerName = "sqi-task-status-srv"

// taskLogsConsumerName is the durable name for the server-side push consumer
// that ingests log chunks from the SQI_LOGS stream.
const taskLogsConsumerName = "sqi-task-logs-srv"

// workerConsumerName is the durable name for the server-side push consumer
// that processes worker-registration and heartbeat messages.
const workerConsumerName = "sqi-worker-srv"

// ── Task status — server-side push consumer ───────────────────────────────────

// ConsumeTaskStatus creates or updates the server-side durable push consumer
// for task-status messages (SQI_TASK stream) and begins delivering them to
// handler.  The returned ConsumeContext is tracked internally and stopped
// automatically during [Client.Drain].
//
// Ack / redelivery policy:
//   - AckWait 30 s — the server has 30 s to persist a status update to SQLite
//     before NATS redelivers.
//   - MaxDeliver 10 — allows for transient DB errors without dropping updates.
//   - DeliverAllPolicy — on first consumer creation, replay any unprocessed
//     messages from before the last server restart so no state transitions are
//     silently lost.  NATS tracks the consumer sequence for existing durables,
//     so subsequent starts pick up where they left off.
func (c *Client) ConsumeTaskStatus(ctx context.Context, handler jetstream.MessageHandler) (jetstream.ConsumeContext, error) {
	cfg := jetstream.ConsumerConfig{
		Durable:       taskStatusConsumerName,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       30 * time.Second,
		MaxDeliver:    10,
		DeliverPolicy: jetstream.DeliverAllPolicy,
		Description:   "Server-side task-status consumer.",
	}

	consumer, err := c.js.CreateOrUpdateConsumer(ctx, StreamTask, cfg)
	if err != nil {
		return nil, fmt.Errorf("bus: ensure task-status consumer: %w", err)
	}

	cc, err := c.consume(consumer, taskStatusConsumerName, handler)
	if err != nil {
		return nil, fmt.Errorf("bus: consume task-status: %w", err)
	}

	c.trackConsumer(cc)
	return cc, nil
}

// ── Task logs — server-side push consumer ────────────────────────────────────

// ConsumeTaskLogs creates or updates the server-side durable push consumer for
// log-chunk messages (SQI_LOGS stream) and begins delivering them to handler.
// The returned ConsumeContext is tracked internally and stopped automatically
// during [Client.Drain].
//
// Ack / redelivery policy:
//   - AckWait 30 s — the server has 30 s to persist a log chunk to the store.
//     The write is a single INSERT.
//   - MaxDeliver 5 — allows for transient DB errors without re-filling the
//     stream indefinitely.  After exhaustion the chunk is discarded; the
//     stream is not read back by the API, so the gap stays.
//   - DeliverAllPolicy — redelivers unacked log chunks from before the last
//     server restart so no output is permanently lost across restarts.  A chunk
//     already stored is acked without being written again (task_logs is unique
//     on attempt and worker sequence).
func (c *Client) ConsumeTaskLogs(ctx context.Context, handler jetstream.MessageHandler) (jetstream.ConsumeContext, error) {
	cfg := jetstream.ConsumerConfig{
		Durable:       taskLogsConsumerName,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       30 * time.Second,
		MaxDeliver:    5,
		DeliverPolicy: jetstream.DeliverAllPolicy,
		Description:   "Server-side task log chunk ingestion consumer.",
	}

	consumer, err := c.js.CreateOrUpdateConsumer(ctx, StreamLogs, cfg)
	if err != nil {
		return nil, fmt.Errorf("bus: ensure task-logs consumer: %w", err)
	}

	cc, err := c.consume(consumer, taskLogsConsumerName, handler)
	if err != nil {
		return nil, fmt.Errorf("bus: consume task-logs: %w", err)
	}

	c.trackConsumer(cc)
	return cc, nil
}

// ── Worker registration / heartbeat — server-side push consumer ───────────────

// ConsumeWorker creates or updates the server-side durable push consumer for
// worker-registration and heartbeat messages (SQI_WORKER stream) and begins
// delivering them to handler.  The returned ConsumeContext is tracked
// internally and stopped automatically during [Client.Drain].
//
// Ack / redelivery policy:
//   - AckWait 10 s — heartbeats are time-sensitive; a 10 s window is well
//     within the scheduler's heartbeat-timeout sweep interval (≈30 s) so
//     delays are caught before workers are incorrectly marked offline.
//   - MaxDeliver 3 — heartbeats are cheap and frequent; a low retry limit
//     avoids piling up stale pings.
//   - DeliverAllPolicy — replays unprocessed registration/heartbeat messages
//     on startup, ensuring no worker comes online without the server noticing.
func (c *Client) ConsumeWorker(ctx context.Context, handler jetstream.MessageHandler) (jetstream.ConsumeContext, error) {
	cfg := jetstream.ConsumerConfig{
		Durable:       workerConsumerName,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       10 * time.Second,
		MaxDeliver:    3,
		DeliverPolicy: jetstream.DeliverAllPolicy,
		Description:   "Server-side worker registration and heartbeat consumer.",
	}

	consumer, err := c.js.CreateOrUpdateConsumer(ctx, StreamWorker, cfg)
	if err != nil {
		return nil, fmt.Errorf("bus: ensure worker consumer: %w", err)
	}

	cc, err := c.consume(consumer, workerConsumerName, handler)
	if err != nil {
		return nil, fmt.Errorf("bus: consume worker: %w", err)
	}

	c.trackConsumer(cc)
	return cc, nil
}
