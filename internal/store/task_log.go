// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"time"
)

// LogStream identifies the output stream a log chunk was captured from.
type LogStream string

const (
	// LogStreamStdout is the standard output stream.
	LogStreamStdout LogStream = "stdout"
	// LogStreamStderr is the standard error stream.
	LogStreamStderr LogStream = "stderr"
)

// TaskLog is one chunk of output produced by a running task.
//
// Workers publish log chunks to task.logs.<workerID>.<taskID> as output is produced.
// The server persists each chunk here with:
//   - A monotonic worker-side sequence number (SeqNum) per attempt, starting
//     at 1 and incrementing by 1.  This allows the REST log-tail endpoint to
//     return chunks in the order the worker produced them regardless of NATS
//     delivery order.
//   - A NATS stream sequence number (NATSSeq) used as the primary DB insertion
//     order and for efficient cursor-based pagination.
//   - A server-side receipt timestamp (ReceivedAt) for audit purposes.
type TaskLog struct {
	ID        string
	TaskID    string
	AttemptID string

	// SeqNum is the worker-assigned monotonic sequence number for this chunk
	// within the attempt.  Starts at 1; callers must not assume it is
	// contiguous — gaps can appear if chunks are dropped or reordered.
	SeqNum int64

	// NATSSeq is the JetStream message sequence number from the SQI_LOGS
	// stream.  Used as a stable, dense offset cursor for the log-tail REST
	// endpoint.  Stored as int64 to match SQLite's INTEGER affinity; NATS
	// stream sequences are uint64 but will never approach int64 max in practice.
	NATSSeq int64

	// At is the worker's local timestamp when the chunk was captured.
	At time.Time

	// ReceivedAt is the server's clock time when the chunk was persisted.
	// Monotonically increasing within a server instance; useful for
	// detecting clock skew between worker and server.
	ReceivedAt time.Time

	// Stream is "stdout" or "stderr".
	Stream LogStream

	// Data is the raw log text.
	Data string
}

// TaskLogStore is the persistence interface for [TaskLog] records.
type TaskLogStore interface {
	// CreateTaskLog inserts a new log chunk. The caller must populate all
	// fields including a unique ID.
	CreateTaskLog(ctx context.Context, log TaskLog) (TaskLog, error)

	// ListTaskLogs returns log chunks for a task attempt in NATSSeq ascending
	// order.  Pass afterNATSSeq = 0 to start from the beginning; pass the
	// NATSSeq of the last received chunk to page forward.  Limit controls the
	// maximum number of chunks returned.
	//
	// This method is used by the REST log-tail endpoint and by the WebSocket
	// log-streaming handler.
	ListTaskLogs(ctx context.Context, attemptID string, afterNATSSeq int64, limit int) ([]TaskLog, error)
}
