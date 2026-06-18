// SPDX-License-Identifier: AGPL-3.0-or-later

package protocol

import "time"

// DiagLogMsg is a single diagnostic (operational) log record published by
// sqi-worker to the core-NATS subject worker.diag.<workerID>.  It carries the
// worker's own slog output — distinct from task process output, which flows via
// LogChunkMsg on task.logs.<taskID>.
//
// Published with core NATS (not JetStream): delivery is best-effort and nothing
// is retained on the broker.  The server holds a bounded in-memory ring buffer.
type DiagLogMsg struct {
	// Ts is the time the log record was emitted.
	Ts time.Time `json:"ts"`
	// Level is the slog level string: "DEBUG", "INFO", "WARN", or "ERROR".
	Level string `json:"level"`
	// Msg is the log message.
	Msg string `json:"msg"`
	// Attrs holds flattened slog attributes (including correlation keys such as
	// task_id, attempt_id, job_id, session_id) stringified for transport.
	Attrs map[string]string `json:"attrs,omitempty"`
}
