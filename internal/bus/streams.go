// SPDX-License-Identifier: AGPL-3.0-or-later

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
	// StreamTask captures task-status transitions reported by workers back to
	// the server.  WorkQueuePolicy ensures each update is processed exactly once
	// by the server-side status consumer.
	StreamTask = "SQI_TASK"

	// StreamLogs captures log chunks emitted by running tasks.  The server
	// persists each chunk to task_logs and acks it; REST and WebSocket read
	// task_logs.  LimitsPolicy keeps the chunks for MaxAge (96 h) after ack as
	// a delivery buffer, but nothing reads them back from the stream.
	StreamLogs = "SQI_LOGS"

	// StreamWorker captures worker-registration and worker-heartbeat messages.
	// WorkQueuePolicy ensures the server processes each registration and
	// heartbeat ping exactly once.
	StreamWorker = "SQI_WORKER"

	// StreamCancel captures task-cancellation signals published by the server
	// to assigned workers; see [streamDefs] for how they are delivered.
	StreamCancel = "SQI_CANCEL"
)

// streamDefs returns the canonical JetStream stream configurations for
// sqi-server, each with the given replica count. [ensureStreams] applies them
// at startup; tests use them to assert stream shape without booting a real
// broker.
func streamDefs(replicas int) []jetstream.StreamConfig {
	return []jetstream.StreamConfig{
		{
			// SQI_TASK — task.status.<worker>.<job>
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
			Replicas:    replicas,
		},
		{
			// SQI_LOGS — task.logs.<worker>.<task>
			//
			// Workers publish structured log chunks as tasks run.  The server
			// persists each chunk to task_logs and acks it; REST and WebSocket
			// read task_logs, paging with the chunk's NATS sequence as the
			// cursor.  LimitsPolicy keeps the chunks for MaxAge after ack, but
			// nothing reads them back from the stream.
			//
			// MaxAge of 96 h bounds the stream's disk use; a chunk the server
			// has not acked within it is lost.
			Name:        StreamLogs,
			Description: "Log chunks emitted by running tasks, persisted to task_logs by the server.",
			Subjects:    []string{SubjectTaskLogsPrefix + ".>"},
			Retention:   jetstream.LimitsPolicy,
			Storage:     jetstream.FileStorage,
			MaxAge:      96 * time.Hour,
			Discard:     jetstream.DiscardOld,
			Replicas:    replicas,
		},
		{
			// SQI_WORKER — worker.register.<worker>, worker.heartbeat.<worker>,
			// worker.deregister.<worker>
			//
			// Workers publish registration payloads on connect and reconnect,
			// heartbeat pings on a configurable interval, and a departure message
			// on graceful shutdown. The server's worker consumer processes each
			// message to update the worker table in SQLite.
			// WorkQueuePolicy ensures no duplicate processing.
			//
			// MaxAge of 2 min: heartbeats older than that are stale by definition
			// (the scheduler's heartbeat-timeout sweep fires at ~30 s intervals)
			// and should be discarded rather than processed out of order.
			// Deregister messages also expire quickly — they have no retry value
			// once processed.
			Name:        StreamWorker,
			Description: "Worker registration and heartbeat messages.",
			Subjects: []string{
				SubjectWorkerRegisterPrefix + ".>",
				SubjectWorkerHeartbeatPrefix + ".>",
				SubjectWorkerDeregisterPrefix + ".>",
			},
			Retention: jetstream.WorkQueuePolicy,
			Storage:   jetstream.FileStorage,
			MaxAge:    2 * time.Minute,
			Discard:   jetstream.DiscardOld,
			Replicas:  replicas,
		},
		{
			// SQI_CANCEL — task.cancel.<workerID>
			//
			// The scheduler publishes one message per canceled run, addressed
			// to the worker holding it. That worker receives it on the core
			// subscription to task.cancel.<workerID> it opens before asking
			// for work, and interrupts the run; no consumer reads the stream.
			// A worker disconnected when a cancel goes out misses it, and is
			// sent it again on every heartbeat sweep while its heartbeats
			// still list the attempt (internal/scheduler/attemptreconcile.go).
			//
			// MaxAge of 5 min bounds what the stream keeps.
			Name:        StreamCancel,
			Description: "Task-cancellation signals delivered to assigned workers.",
			Subjects:    []string{SubjectTaskCancelPrefix + ".>"},
			Retention:   jetstream.WorkQueuePolicy,
			Storage:     jetstream.FileStorage,
			MaxAge:      5 * time.Minute,
			Discard:     jetstream.DiscardOld,
			Replicas:    replicas,
		},
	}
}

// ensureStreams creates or updates all sqi-server JetStream streams with the
// given replica count. It is idempotent, and brings an existing stream's
// configuration back to its definition.
func ensureStreams(ctx context.Context, js jetstream.JetStream, replicas int) error {
	for _, cfg := range streamDefs(replicas) {
		if _, err := js.CreateOrUpdateStream(ctx, cfg); err != nil {
			return fmt.Errorf("stream %s: %w", cfg.Name, err)
		}
	}
	return nil
}

// StreamNames lists sqi's streams by name, in the order they are provisioned.
func StreamNames() []string {
	defs := streamDefs(1)
	names := make([]string, 0, len(defs))
	for _, def := range defs {
		names = append(names, def.Name)
	}
	return names
}
