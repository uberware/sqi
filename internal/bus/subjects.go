// SPDX-License-Identifier: AGPL-3.0-only

// Package bus provides the embedded NATS JetStream message broker and the
// subject/stream definitions used for sqi-server's internal messaging.
//
// # Subject naming
//
// Each subject class follows a hierarchical naming scheme so that JetStream
// stream subject-filters and consumer subject-bindings are intuitive:
//
//	work.assign.<queue>    — server → worker(s): task assignment payload
//	task.status.<job>      — worker → server: task state transitions
//	task.logs.<task>       — worker → server: log chunk ingestion
//	worker.heartbeat       — worker → server: periodic liveness pings
//	worker.register        — worker → server: capability advertisement
//
// The leaf token (<queue>, <job>, <task>) is the opaque string identifier of
// the corresponding entity in the SQLite store.  Callers build full subject
// strings using the helper functions below rather than constructing them by
// hand.
package bus

// Subject prefix and fixed-subject constants.
const (
	// SubjectWorkAssignPrefix is the prefix for task-assignment subjects.
	// Full subject: SubjectWorkAssignPrefix + "." + queueID.
	SubjectWorkAssignPrefix = "work.assign"

	// SubjectTaskStatusPrefix is the prefix for task-status subjects.
	// Full subject: SubjectTaskStatusPrefix + "." + jobID.
	SubjectTaskStatusPrefix = "task.status"

	// SubjectTaskLogsPrefix is the prefix for task-log subjects.
	// Full subject: SubjectTaskLogsPrefix + "." + taskID.
	SubjectTaskLogsPrefix = "task.logs"

	// SubjectWorkerHeartbeat is the subject workers publish liveness pings to.
	SubjectWorkerHeartbeat = "worker.heartbeat"

	// SubjectWorkerRegister is the subject workers publish registration
	// messages to when they first connect or reconnect.
	SubjectWorkerRegister = "worker.register"
)

// WorkAssignSubject returns the full NATS subject for task-assignment messages
// targeting workers subscribed to the given queue.
func WorkAssignSubject(queueID string) string {
	return SubjectWorkAssignPrefix + "." + queueID
}

// TaskStatusSubject returns the full NATS subject for task-status messages
// belonging to the given job.
func TaskStatusSubject(jobID string) string {
	return SubjectTaskStatusPrefix + "." + jobID
}

// TaskLogsSubject returns the full NATS subject for log-chunk messages
// belonging to the given task.
func TaskLogsSubject(taskID string) string {
	return SubjectTaskLogsPrefix + "." + taskID
}
