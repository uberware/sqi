// SPDX-License-Identifier: AGPL-3.0-only

package bus

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// ── Consumer name helpers ─────────────────────────────────────────────────────

// workConsumerName returns the durable JetStream consumer name for
// task-assignment messages targeting a specific queue.
//
// All workers serving queueID share this single durable pull consumer.
// JetStream delivers each assignment message to exactly one caller of
// Consumer.Fetch regardless of how many workers are pulling concurrently —
// no distributed locking needed.  This is the "consumer group" pattern for
// JetStream WorkQueuePolicy streams.
func workConsumerName(queueID string) string {
	return "sqi-work-" + sanitizeConsumerToken(queueID)
}

// taskStatusConsumerName is the durable name for the server-side push consumer
// that processes task-status transitions.  One server process runs this
// consumer at a time; in Phase 4 HA deployments, leader election gates startup.
const taskStatusConsumerName = "sqi-task-status-srv"

// workerConsumerName is the durable name for the server-side push consumer
// that processes worker-registration and heartbeat messages.
const workerConsumerName = "sqi-worker-srv"

// sanitizeConsumerToken replaces characters not allowed in JetStream consumer
// names (alphanumeric, dash, underscore, dot) with underscores.  In practice
// sqi IDs are UUIDs (hex + dashes), and all those characters are already
// allowed — the function exists to enforce the invariant explicitly.
func sanitizeConsumerToken(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			return r
		default:
			return '_'
		}
	}, s)
}

// ── Work assignment — pull consumer (task 37) ─────────────────────────────────

// EnsureWorkConsumer creates or updates the durable pull consumer for
// task-assignment messages on the given queue, then returns it.
//
// Consumer group semantics:
// All workers serving queueID call EnsureWorkConsumer with the same ID.
// They all receive the same durable consumer name, so each SQI_WORK message
// is delivered to exactly one worker — whichever calls Consumer.Fetch first.
// Workers then loop calling Fetch (or FetchNoWait) to pull their next
// assignment.
//
// Ack / redelivery policy (task 38):
//   - AckWait 30 s — worker must acknowledge within 30 s of fetching.
//     The worker protocol (task 56) sends an explicit nack immediately on
//     rejection so this is a safety net, not the normal path.
//   - MaxDeliver 5 — unacknowledged assignments are retried up to 5 times.
//     After that the stream's MaxAge (5 min) discards the message.  The
//     scheduler's heartbeat sweep (task 48) reclaims the task and re-publishes
//     a fresh assignment, so an exhausted consumer entry never causes permanent
//     loss.
//   - DeliverNewPolicy — only deliver messages published after the consumer
//     was created.  Stale assignments must not wake up a late-starting worker;
//     the scheduler will reassign as needed.
func (c *Client) EnsureWorkConsumer(ctx context.Context, queueID string) (jetstream.Consumer, error) {
	cfg := jetstream.ConsumerConfig{
		Durable:       workConsumerName(queueID),
		FilterSubject: WorkAssignSubject(queueID),
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       30 * time.Second,
		MaxDeliver:    5,
		DeliverPolicy: jetstream.DeliverNewPolicy,
		Description:   "Work-assignment pull consumer for queue " + queueID,
	}

	consumer, err := c.js.CreateOrUpdateConsumer(ctx, StreamWork, cfg)
	if err != nil {
		return nil, fmt.Errorf("bus: ensure work consumer %q: %w", queueID, err)
	}
	return consumer, nil
}

// ── Task status — server-side push consumer ───────────────────────────────────

// ConsumeTaskStatus creates or updates the server-side durable push consumer
// for task-status messages (SQI_TASK stream) and begins delivering them to
// handler.  The returned ConsumeContext is tracked internally and stopped
// automatically during [Client.Drain].
//
// Ack / redelivery policy (task 38):
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

	cc, err := consumer.Consume(handler)
	if err != nil {
		return nil, fmt.Errorf("bus: consume task-status: %w", err)
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
// Ack / redelivery policy (task 38):
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

	cc, err := consumer.Consume(handler)
	if err != nil {
		return nil, fmt.Errorf("bus: consume worker: %w", err)
	}

	c.trackConsumer(cc)
	return cc, nil
}
