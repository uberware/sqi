// SPDX-License-Identifier: AGPL-3.0-only

package bus

import (
	"context"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// Stream name constants.  Every JetStream stream owned by sqi-server uses the
// "SQI_" prefix so they are clearly identifiable if the embedded NATS instance
// is ever inspected externally.
const (
	// StreamWork captures task-assignment messages sent by the scheduler to
	// waiting workers.  WorkQueuePolicy ensures each assignment is delivered to
	// exactly one worker consumer.
	StreamWork = "SQI_WORK"

	// StreamTask captures task-status transitions reported by workers back to
	// the server.  WorkQueuePolicy ensures each update is processed exactly once
	// by the server-side status consumer.
	StreamTask = "SQI_TASK"

	// StreamLogs captures log chunks emitted by running tasks.  LimitsPolicy
	// retains chunks so they can be served via the REST API even after the
	// producing worker has moved on.
	StreamLogs = "SQI_LOGS"

	// StreamWorker captures worker-registration and worker-heartbeat messages.
	// WorkQueuePolicy ensures the server processes each registration and
	// heartbeat ping exactly once.
	StreamWorker = "SQI_WORKER"

	// StreamCancel captures task-cancellation signals published by the server
	// to assigned workers (task 54).  WorkQueuePolicy ensures each signal is
	// delivered exactly once; MaxAge of 5 min matches StreamWork so stale
	// signals for already-completed tasks are discarded automatically.
	StreamCancel = "SQI_CANCEL"
)

// streamDefs returns the canonical JetStream stream configurations for
// sqi-server.  Called once at startup by [ensureStreams]; also exported so
// tests can assert stream shape without booting a real broker.
func streamDefs() []jetstream.StreamConfig {
	return []jetstream.StreamConfig{
		{
			// SQI_WORK — work.assign.<queue>
			//
			// The scheduler publishes one message per task assignment.  Workers
			// consume via a durable pull consumer bound to their queue(s).
			// WorkQueuePolicy removes the message once it is acknowledged so the
			// same assignment is never delivered twice.
			//
			// MaxAge of 5 min: if no worker picks up an assignment within that
			// window the scheduler's heartbeat sweep will reclaim the task and
			// re-publish, so stale messages are discarded rather than delivered
			// to a late-waking worker.
			Name:        StreamWork,
			Description: "Task-assignment payloads delivered to workers per queue.",
			Subjects:    []string{SubjectWorkAssignPrefix + ".>"},
			Retention:   jetstream.WorkQueuePolicy,
			Storage:     jetstream.FileStorage,
			MaxAge:      5 * time.Minute,
			Discard:     jetstream.DiscardOld,
			Replicas:    1,
		},
		{
			// SQI_TASK — task.status.<job>
			//
			// Workers publish task-state transitions (running, succeeded, failed,
			// canceled) to this stream keyed by job ID.  The server's status
			// consumer reads and persists each update to SQLite.
			// WorkQueuePolicy removes the message after the server acknowledges.
			Name:        StreamTask,
			Description: "Task state transitions reported by workers.",
			Subjects:    []string{SubjectTaskStatusPrefix + ".>"},
			Retention:   jetstream.WorkQueuePolicy,
			Storage:     jetstream.FileStorage,
			MaxAge:      24 * time.Hour,
			Discard:     jetstream.DiscardOld,
			Replicas:    1,
		},
		{
			// SQI_LOGS — task.logs.<task>
			//
			// Workers publish structured log chunks as tasks run.  LimitsPolicy
			// retains all chunks so the REST and WebSocket log-tail endpoints can
			// replay them on demand.  Chunks are stored with a monotonic sequence
			// number; the REST handler pages through them using the NATS sequence
			// as an offset cursor.
			//
			// MaxAge of 96 h balances storage cost against the window in which
			// operators typically review task output.
			Name:        StreamLogs,
			Description: "Log chunks emitted by running tasks, retained for REST retrieval.",
			Subjects:    []string{SubjectTaskLogsPrefix + ".>"},
			Retention:   jetstream.LimitsPolicy,
			Storage:     jetstream.FileStorage,
			MaxAge:      96 * time.Hour,
			Discard:     jetstream.DiscardOld,
			Replicas:    1,
		},
		{
			// SQI_WORKER — worker.heartbeat, worker.register
			//
			// Workers publish registration payloads on connect and heartbeat pings
			// on a configurable interval.  The server's worker consumer processes
			// each message to update the worker table in SQLite.
			// WorkQueuePolicy ensures no duplicate processing.
			//
			// MaxAge of 2 min: heartbeats older than that are stale by definition
			// (the scheduler's heartbeat-timeout sweep fires at ~30 s intervals)
			// and should be discarded rather than processed out of order.
			Name:        StreamWorker,
			Description: "Worker registration and heartbeat messages.",
			Subjects:    []string{SubjectWorkerRegister, SubjectWorkerHeartbeat},
			Retention:   jetstream.WorkQueuePolicy,
			Storage:     jetstream.FileStorage,
			MaxAge:      2 * time.Minute,
			Discard:     jetstream.DiscardOld,
			Replicas:    1,
		},
		{
			// SQI_CANCEL — task.cancel.<taskID>
			//
			// The scheduler publishes one message per canceled task (task 54).
			// The worker assigned to that task consumes the message and interrupts
			// its running process.  WorkQueuePolicy ensures at-most-once delivery
			// so the same signal is never re-sent to a different worker.
			//
			// MaxAge of 5 min: a worker that has not consumed the cancel signal
			// within this window has either already completed the task (in which
			// case the signal is irrelevant) or gone offline (in which case the
			// heartbeat sweep will reclaim it).
			Name:        StreamCancel,
			Description: "Task-cancellation signals delivered to assigned workers.",
			Subjects:    []string{SubjectTaskCancelPrefix + ".>"},
			Retention:   jetstream.WorkQueuePolicy,
			Storage:     jetstream.FileStorage,
			MaxAge:      5 * time.Minute,
			Discard:     jetstream.DiscardOld,
			Replicas:    1,
		},
	}
}

// ensureStreams creates or updates all sqi-server JetStream streams.  It is
// idempotent: calling it against a broker that already has the streams defined
// is safe and will update any configuration drift.
func ensureStreams(ctx context.Context, js jetstream.JetStream) error {
	for _, cfg := range streamDefs() {
		if _, err := js.CreateOrUpdateStream(ctx, cfg); err != nil {
			return fmt.Errorf("stream %s: %w", cfg.Name, err)
		}
	}
	return nil
}
