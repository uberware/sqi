// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// Task 59: structured log ingestion that timestamps and persists log chunks
// with monotonic sequence numbers per task attempt.
//
// Workers publish [protocol.LogChunkMsg] values to task.logs.<taskID> as their
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
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"

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

// handleLogChunk is the JetStream message handler for task.logs.<task> messages
// published by workers.
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
