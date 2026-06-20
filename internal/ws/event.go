// SPDX-License-Identifier: AGPL-3.0-or-later

package ws

import "time"

// TaskEvent is published by the scheduler when a task transitions to a new
// status.  The Hub fans this out to:
//   - clients subscribed to [SubjectJobs] (job-summary updates)
//   - clients subscribed to "jobs/{jobID}/tasks" (per-job task updates)
type TaskEvent struct {
	JobID     string
	TaskID    string
	Name      string
	Status    string // store.TaskStatus value
	WorkerID  string // empty when unassigned
	UpdatedAt time.Time
}

// WorkerStatusRemoved is a synthetic WorkerEvent.Status value (not a persisted
// [store.WorkerStatus]) signaling that a worker row was hard-deleted — by a
// manual remove or the offline-retention sweep — so subscribers should drop it
// from their views rather than update it in place.
const WorkerStatusRemoved = "removed"

// WorkerEvent is published when a worker registers, sends a heartbeat,
// deregisters, has its status changed by the heartbeat sweep, or is removed.
// The Hub fans this out to clients subscribed to [SubjectWorkers].
type WorkerEvent struct {
	WorkerID string
	Name     string
	Hostname string
	FarmID   string
	Status   string // store.WorkerStatus value
}

// LogEvent is published when a log chunk is persisted by the log-ingestion
// consumer.  The Hub fans this out to clients subscribed to
// "tasks/{taskID}/logs".
type LogEvent struct {
	TaskID    string
	AttemptID string
	SeqNum    int64
	Stream    string // "stdout" or "stderr"
	Data      string
	At        time.Time
}

// JobEvent is published when a job transitions to a new aggregate status
// (running, completed, failed, canceled).
// The Hub fans this out to clients subscribed to [SubjectJobs].
type JobEvent struct {
	JobID     string
	Name      string
	Owner     string
	QueueID   string
	Status    string // store.JobStatus value
	UpdatedAt time.Time
}

// DiagEvent is published when a diagnostic log record is ingested (from the
// server's own logger or a worker's worker.diag stream).  The Hub fans this out
// to clients subscribed to [SubjectDiagnostics].
type DiagEvent struct {
	Component string // "server" or "worker:<id>"
	Level     string // DEBUG|INFO|WARN|ERROR
	Msg       string
	Attrs     map[string]string
	At        time.Time
}

// Notifier is implemented by [Hub].  Packages outside the ws package —
// primarily the scheduler — call these methods after persisting state changes
// to push live events to subscribed WebSocket clients.
//
// All methods must be safe for concurrent use and must not block the caller.
type Notifier interface {
	NotifyTask(e TaskEvent)
	NotifyWorker(e WorkerEvent)
	NotifyLog(e LogEvent)
	NotifyJob(e JobEvent)
	NotifyDiag(e DiagEvent)
}

// NoopNotifier is a [Notifier] that discards all events.  Use it in tests that
// exercise the scheduler without a running WebSocket hub.
type NoopNotifier struct{}

// NotifyTask discards the event.
func (NoopNotifier) NotifyTask(TaskEvent) {}

// NotifyWorker discards the event.
func (NoopNotifier) NotifyWorker(WorkerEvent) {}

// NotifyLog discards the event.
func (NoopNotifier) NotifyLog(LogEvent) {}

// NotifyJob discards the event.
func (NoopNotifier) NotifyJob(JobEvent) {}

// NotifyDiag discards the event.
func (NoopNotifier) NotifyDiag(DiagEvent) {}
