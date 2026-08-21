// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// Structured log ingestion that timestamps and persists log chunks
// with monotonic sequence numbers per task attempt.
//
// Workers publish [protocol.LogChunkMsg] values to task.logs.<workerID>.<taskID> as their
// task produces stdout/stderr output.  The server-side consumer here:
//
//  1. Decodes each [protocol.LogChunkMsg].
//  2. Inserts a [store.TaskLog] row with:
//       - worker-assigned SeqNum (monotonic within the attempt, from the message)
//       - NATS stream sequence number (NATSSeq, extracted from the JetStream
//         message metadata) as the stable cursor for the log-tail REST endpoint
//       - server-side ReceivedAt timestamp
//  3. Acks the NATS message so it is removed from the SQI_LOGS WorkQueue
//     stream.
//
// The SQI_LOGS stream uses LimitsPolicy (not WorkQueuePolicy) so all messages
// are retained for the configured MaxAge (96 h by default) even after ack.
// This allows the REST log-tail endpoint to replay chunks from the NATS stream
// directly without touching SQLite for the hot path — the SQLite rows are used
// for durable persistence beyond the JetStream retention window and for sorted
// paging.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/worker/protocol"
	"github.com/uberware/sqi/internal/ws"
)

// startTaskLogsConsumer creates the server-side JetStream push consumer for
// the SQI_LOGS stream and begins delivering messages to handleLogChunk.
// Called once from [Scheduler.Run].
func (s *Scheduler) startTaskLogsConsumer(ctx context.Context) error {
	_, err := s.bus.ConsumeTaskLogs(ctx, s.handleLogChunk)
	return err
}

// handleLogChunk is the JetStream message handler for task.logs.<worker>.<task> messages
// published by workers.
//
// [protocol.LogChunkMsg] carries no worker-identity field of its own, so the
// subject is the ONLY identity available: it is resolved to an attempt, and
// that attempt's recorded WorkerID and TaskID are both checked before a
// chunk is persisted or fanned out — the worker check alone would let a
// worker holding a live attempt of its own pair that attempt with a
// different task in the payload and inject log content there. See the
// auth-off note on [Scheduler.handleTaskStatusMessage]: it applies here too.
func (s *Scheduler) handleLogChunk(msg jetstream.Msg) {
	ctx := s.ctx

	var m protocol.LogChunkMsg
	if err := json.Unmarshal(msg.Data(), &m); err != nil {
		s.logger.WarnContext(
			ctx, "scheduler: malformed task.logs message",
			slog.Any("error", err),
		)
		s.ackMsg(ctx, msg) // discard; re-delivery cannot fix a bad payload
		return
	}
	if m.TaskID == "" || m.AttemptID == "" {
		s.logger.WarnContext(
			ctx, "scheduler: task.logs missing task_id or attempt_id — discarding",
			slog.String("task_id", m.TaskID),
			slog.String("attempt_id", m.AttemptID),
		)
		s.ackMsg(ctx, msg)
		return
	}

	// The subject is the only identity NATS itself can vouch for; a message
	// on a subject that does not carry one concrete worker ID cannot be
	// attributed to anyone and is discarded rather than acted on.
	subjectWorkerID, _, ok := bus.ParseWorkerSubject(msg.Subject())
	if !ok {
		s.logger.WarnContext(
			ctx, "scheduler: task.logs on unexpected subject — discarding",
			slog.String("subject", msg.Subject()),
		)
		s.ackMsg(ctx, msg)
		return
	}

	// Resolve the attempt this chunk claims to belong to so its recorded
	// WorkerID and TaskID can be checked below. Both fields are immutable for
	// the life of the attempt, so a cache hit is as good as a fresh store
	// read; on a miss, fall back to the store exactly as before — including
	// the transient-vs-permanent distinction below, since the store read
	// also confirms the attempt still exists (a chunk for a vanished attempt
	// must still be discarded, never assumed present).
	owner, ok := s.attemptCache.get(m.AttemptID)
	if !ok {
		attempt, err := s.store.GetTaskAttempt(ctx, m.AttemptID)
		if errors.Is(err, store.ErrNotFound) {
			s.logger.WarnContext(
				ctx, "scheduler: task.logs for unknown attempt — discarding",
				slog.String("attempt_id", m.AttemptID),
				slog.String("task_id", m.TaskID),
			)
			s.ackMsg(ctx, msg)
			return
		}
		if err != nil {
			s.logger.WarnContext(
				ctx, "scheduler: task.logs: lookup attempt failed — will redeliver",
				slog.String("attempt_id", m.AttemptID),
				slog.Any("error", err),
			)
			s.nakMsg(ctx, msg)
			return
		}
		owner = attemptOwner{workerID: attempt.WorkerID, taskID: attempt.TaskID}
		s.attemptCache.put(m.AttemptID, owner.workerID, owner.taskID)
	}
	// The attempt is real, but is it for the task this chunk claims? Without
	// this, a worker holding a live attempt of its own could pair that
	// attempt's ID with a different task's ID in the payload — the worker-ID
	// check below would pass (the attempt really is the subject worker's),
	// but the log would land against a task that worker was never assigned.
	if owner.taskID != m.TaskID {
		s.logger.WarnContext(
			ctx, "scheduler: task.logs attempt task_id mismatch — discarding",
			slog.String("attempt_id", m.AttemptID),
			slog.String("msg_task_id", m.TaskID),
			slog.String("attempt_task_id", owner.taskID),
		)
		s.ackMsg(ctx, msg)
		return
	}
	// The subject's worker ID was enforced by NATS when broker auth is on;
	// with LogChunkMsg carrying no worker field of its own, it is the only
	// identity this handler has to check at all. Treat a mismatch as
	// permanent — redelivery cannot make a forged message legal.
	if subjectWorkerID != owner.workerID {
		s.logger.WarnContext(
			ctx, "scheduler: task.logs from a worker that does not hold this task — discarding",
			slog.String("task_id", m.TaskID),
			slog.String("attempt_id", m.AttemptID),
			slog.String("subject_worker_id", subjectWorkerID),
			slog.String("attempt_worker_id", owner.workerID),
		)
		s.ackMsg(ctx, msg)
		return
	}

	// Extract the NATS JetStream sequence number from the message metadata.
	// This is the stable cursor used by the log-tail REST endpoint.
	// NATS stream sequences are uint64; int64 is used here to match SQLite's
	// INTEGER affinity. Sequences never approach int64 max in practice.
	var natsSeq int64
	if meta, err := msg.Metadata(); err == nil {
		natsSeq = int64(meta.Sequence.Stream) //nolint:gosec // uint64→int64: NATS sequences never approach int64 max
	}

	at := m.At
	if at.IsZero() {
		at = time.Now().UTC()
	}

	stream := store.LogStreamStdout
	if m.Stream == "stderr" {
		stream = store.LogStreamStderr
	}

	log := store.TaskLog{
		ID:        uuid.NewString(),
		TaskID:    m.TaskID,
		AttemptID: m.AttemptID,
		SeqNum:    m.SeqNum,
		NATSSeq:   natsSeq,
		At:        at,
		Stream:    stream,
		Data:      m.Data,
	}

	if _, err := s.store.CreateTaskLog(ctx, log); err != nil {
		s.logger.WarnContext(
			ctx, "scheduler: persist log chunk failed — will redeliver",
			slog.String("task_id", m.TaskID),
			slog.String("attempt_id", m.AttemptID),
			slog.Int64("seq_num", m.SeqNum),
			slog.Any("error", err),
		)
		s.nakMsg(ctx, msg)
		return
	}

	// Notify WebSocket hub so clients subscribed to "tasks/{taskID}/logs"
	// receive the chunk in real time.
	s.notifier.NotifyLog(ws.LogEvent{
		TaskID:    m.TaskID,
		AttemptID: m.AttemptID,
		SeqNum:    m.SeqNum,
		Stream:    m.Stream,
		Data:      m.Data,
		At:        at,
	})

	s.ackMsg(ctx, msg)
}
